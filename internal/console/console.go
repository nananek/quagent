// Package console は承認コンソール。quagent 本体 (Server) と、tmux のペインで
// 動く UI (RunClient) が unix socket 上の JSON 行でやり取りする。
//
// 本体は利用者の端末で動き続ける (秘密を渡す環境変数などを保持するため)。
// UI は表示と入力だけを受け持つ。エージェントのペインの __attach も同じ socket に
// つなぎ、VM が出した OSC 52 (クリップボードへの書き込み要求) を本体に渡す。
package console

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/nananek/quagent/internal/access"
)

// Msg は本体と UI (・__attach) の間のメッセージ。
//
//	本体 -> UI:      request / settled / clip / clipsettled / log
//	UI -> 本体:      ui (最初に名乗る) / decide / clipdecide / quit
//	__attach -> 本体: clipboard
type Msg struct {
	Type string `json:"type"`

	ID       int      `json:"id,omitempty"`
	Domains  []string `json:"domains,omitempty"`
	Reason   string   `json:"reason,omitempty"`
	Deadline string   `json:"deadline,omitempty"`

	Status   access.Status `json:"status,omitempty"`
	Kind     access.Kind   `json:"kind,omitempty"`
	Question string        `json:"question,omitempty"`

	Text string `json:"text,omitempty"`
	Data []byte `json:"data,omitempty"` // クリップボードに入れたい中身
	Size int    `json:"size,omitempty"`

	// PR の作成承認 (prrequest / prsettled) の内容。
	Branch string `json:"branch,omitempty"`
	Base   string `json:"base,omitempty"`
	Title  string `json:"title,omitempty"`
	Body   string `json:"body,omitempty"`

	// コンテンツガードの確認 (guardrequest / guardsettled) の内容。
	Provider string   `json:"provider,omitempty"`
	Method   string   `json:"method,omitempty"`
	URL      string   `json:"url,omitempty"`
	Evidence string   `json:"evidence,omitempty"`
	Headers  []string `json:"headers,omitempty"`
}

const (
	writeTimeout = 2 * time.Second // UI が受け取らなくても本体が止まらないように
	maxBacklog   = 200             // UI が繋がる前に溜めるログ
)

type client struct {
	c   net.Conn
	enc *json.Encoder
}

// Server は本体側。申請と DNS の拒否を UI に流し、UI の判断を Manager に渡す。
type Server struct {
	m    *access.Manager
	sock string
	l    net.Listener

	// Quit は UI から終了の指示が来ると閉じる。
	Quit     chan struct{}
	quitOnce sync.Once
	// Clipboard は承認されたクリップボードの中身を host に入れる (nil なら TmuxClipboard)。
	Clipboard func([]byte) error

	mu      sync.Mutex
	clients map[*client]bool
	shown   map[int]bool // UI に送った申請
	backlog []Msg        // UI が繋がる前のログ
	clip    clipState

	// PR の作成承認。access.Manager とは別に、この Server が直接待つ。
	prMu      sync.Mutex
	prNextID  int
	prPending []*prRequest

	// コンテンツガードの確認。同じくこの Server が直接待つ。
	guardMu      sync.Mutex
	guardNextID  int
	guardPending []*guardRequest
}

// PRInfo は承認コンソールに諮る PR 作成の内容。
type PRInfo struct {
	Branch string
	Base   string
	Title  string
	Body   string
}

type prRequest struct {
	id       int
	info     PRInfo
	created  time.Time
	done     chan struct{}
	status   access.Status
	approved bool
}

// GuardInfo はコンテンツガードが承認コンソールに諮るリクエストの要約。
type GuardInfo struct {
	Provider string
	Method   string
	URL      string
	Reason   string
	// Evidence はローカル LLM が「これが機密だ」と指摘した該当箇所。
	Evidence string
	Headers  []string
	Body     string
}

type guardRequest struct {
	id       int
	info     GuardInfo
	created  time.Time
	done     chan struct{}
	status   access.Status
	approved bool
}

// NewServer は sock で待ち受ける Server を作る。
func NewServer(m *access.Manager, sock string) (*Server, error) {
	_ = os.Remove(sock)
	l, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	s := &Server{m: m, sock: sock, l: l, Quit: make(chan struct{}),
		clients: map[*client]bool{}, shown: map[int]bool{}}
	m.Log = s.Log
	go s.accept()
	go s.watch()
	return s, nil
}

// Close は待ち受けを止める。
func (s *Server) Close() { s.l.Close() }

// Log は UI にログを流す。
func (s *Server) Log(text string) {
	s.broadcast(Msg{Type: "log", Text: time.Now().Format("15:04:05 ") + text})
}

func (s *Server) broadcast(msg Msg) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.clients) == 0 && msg.Type == "log" {
		if len(s.backlog) < maxBacklog {
			s.backlog = append(s.backlog, msg)
		}
		return
	}
	for cl := range s.clients {
		if s.send(cl, msg) != nil {
			delete(s.clients, cl)
			cl.c.Close()
		}
	}
}

// send は 1 つの UI に送る (s.mu を持って呼ぶ)。
func (s *Server) send(cl *client, msg Msg) error {
	_ = cl.c.SetWriteDeadline(time.Now().Add(writeTimeout))
	return cl.enc.Encode(msg)
}

func (s *Server) accept() {
	for {
		c, err := s.l.Accept()
		if err != nil {
			return
		}
		go s.serve(c)
	}
}

func (s *Server) serve(c net.Conn) {
	defer c.Close()
	cl := &client{c: c, enc: json.NewEncoder(c)}
	defer func() {
		s.mu.Lock()
		delete(s.clients, cl)
		s.mu.Unlock()
	}()
	dec := json.NewDecoder(c)
	for {
		var msg Msg
		if err := dec.Decode(&msg); err != nil {
			return
		}
		switch msg.Type {
		case "ui":
			s.register(cl)
		case "decide":
			err := s.m.Decide(msg.ID, access.Decision{Status: msg.Status, Kind: msg.Kind, Question: msg.Question})
			if err != nil {
				s.Log("判断を反映できない: " + err.Error())
			}
		case "clipboard":
			s.onClipboard(msg)
		case "clipdecide":
			s.clipDecide(msg.ID, msg.Status == access.Approved)
		case "prdecide":
			s.settlePR(msg.ID, msg.Status)
		case "guarddecide":
			s.settleGuard(msg.ID, msg.Status)
		case "quit":
			s.quitOnce.Do(func() { close(s.Quit) })
		}
	}
}

// register は UI として名乗った接続に、溜めたログと承認待ちを送り、以後の通知先にする。
func (s *Server) register(cl *client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, msg := range s.backlog {
		_ = s.send(cl, msg)
	}
	s.backlog = nil
	for _, r := range s.m.Pending() {
		_ = s.send(cl, requestMsg(r))
	}
	if s.clip.pending != nil {
		_ = s.send(cl, clipMsg(s.clip.pending))
	}
	s.prMu.Lock()
	for _, r := range s.prPending {
		_ = s.send(cl, prRequestMsg(r))
	}
	s.prMu.Unlock()
	s.guardMu.Lock()
	for _, r := range s.guardPending {
		_ = s.send(cl, guardRequestMsg(r))
	}
	s.guardMu.Unlock()
	s.clients[cl] = true
}

func requestMsg(r *access.Request) Msg {
	return Msg{Type: "request", ID: r.ID, Domains: r.Domains, Reason: r.Reason,
		Deadline: r.Created.Add(access.DecisionTimeout).Format("15:04:05")}
}

// AskPR は PR の作成を承認コンソールに諮り、承認されるまで待つ。拒否・時間切れ・
// 終了ならエラーを返し、呼び出し側 (pr.Publisher) は push しない。
func (s *Server) AskPR(info PRInfo) error {
	req := &prRequest{info: info, created: time.Now(), done: make(chan struct{})}
	s.prMu.Lock()
	s.prNextID++
	req.id = s.prNextID
	s.prPending = append(s.prPending, req)
	s.prMu.Unlock()
	s.broadcast(prRequestMsg(req))

	t := time.NewTimer(access.DecisionTimeout)
	defer t.Stop()
	select {
	case <-req.done:
	case <-t.C:
		s.settlePR(req.id, access.TimedOut)
		<-req.done
	case <-s.Quit:
		s.settlePR(req.id, access.Denied)
		return fmt.Errorf("終了したので PR を作らなかった")
	}
	if req.approved {
		return nil
	}
	if req.status == access.TimedOut {
		return fmt.Errorf("%s 以内に承認されなかったので PR を作らなかった", access.DecisionTimeout)
	}
	return fmt.Errorf("PR の作成は承認されなかった")
}

// settlePR は承認待ちの PR を決着させ、UI に知らせる。既に決着していれば何もしない。
func (s *Server) settlePR(id int, status access.Status) {
	s.prMu.Lock()
	idx := -1
	for i, r := range s.prPending {
		if r.id == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.prMu.Unlock()
		return
	}
	req := s.prPending[idx]
	s.prPending = append(s.prPending[:idx], s.prPending[idx+1:]...)
	req.status = status
	req.approved = status == access.Approved
	s.prMu.Unlock()
	close(req.done)
	s.broadcast(Msg{Type: "prsettled", ID: id, Status: status})
}

func prRequestMsg(r *prRequest) Msg {
	return Msg{Type: "prrequest", ID: r.id, Branch: r.info.Branch, Base: r.info.Base,
		Title: r.info.Title, Body: r.info.Body,
		Deadline: r.created.Add(access.DecisionTimeout).Format("15:04:05")}
}

// AskGuard はコンテンツガードが疑わしいと判定したリクエストを承認コンソールに諮り、
// 通すか止めるかを待つ。通すなら nil、止める (拒否・時間切れ・終了・ctx 終了) なら理由を返す。
func (s *Server) AskGuard(ctx context.Context, info GuardInfo) error {
	req := &guardRequest{info: info, created: time.Now(), done: make(chan struct{})}
	s.guardMu.Lock()
	s.guardNextID++
	req.id = s.guardNextID
	s.guardPending = append(s.guardPending, req)
	s.guardMu.Unlock()
	s.broadcast(guardRequestMsg(req))

	t := time.NewTimer(access.DecisionTimeout)
	defer t.Stop()
	select {
	case <-req.done:
	case <-t.C:
		s.settleGuard(req.id, access.TimedOut)
		<-req.done
	case <-ctx.Done():
		s.settleGuard(req.id, access.Denied)
		return ctx.Err()
	case <-s.Quit:
		s.settleGuard(req.id, access.Denied)
		return fmt.Errorf("終了したので通さなかった")
	}
	if req.approved {
		return nil
	}
	if req.status == access.TimedOut {
		return fmt.Errorf("%s 以内に応答がなかったので止めた", access.DecisionTimeout)
	}
	return fmt.Errorf("通さないと決めた")
}

// settleGuard は承認待ちのコンテンツガードの確認を決着させ、UI に知らせる。既に決着していれば何もしない。
func (s *Server) settleGuard(id int, status access.Status) {
	s.guardMu.Lock()
	idx := -1
	for i, r := range s.guardPending {
		if r.id == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.guardMu.Unlock()
		return
	}
	req := s.guardPending[idx]
	s.guardPending = append(s.guardPending[:idx], s.guardPending[idx+1:]...)
	req.status = status
	req.approved = status == access.Approved
	s.guardMu.Unlock()
	close(req.done)
	s.broadcast(Msg{Type: "guardsettled", ID: id, Status: status})
}

func guardRequestMsg(r *guardRequest) Msg {
	return Msg{Type: "guardrequest", ID: r.id, Provider: r.info.Provider, Method: r.info.Method,
		URL: r.info.URL, Reason: r.info.Reason, Evidence: r.info.Evidence,
		Headers: r.info.Headers, Body: r.info.Body,
		Deadline: r.created.Add(access.DecisionTimeout).Format("15:04:05")}
}

// watch は新しい申請を UI に送り、決着した申請 (時間切れを含む) を知らせる。
func (s *Server) watch() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-s.m.Notify:
		case <-tick.C:
		}
		pending := map[int]bool{}
		for _, r := range s.m.Pending() {
			pending[r.ID] = true
			s.mu.Lock()
			isNew := !s.shown[r.ID]
			s.shown[r.ID] = true
			s.mu.Unlock()
			if isNew {
				s.broadcast(requestMsg(r))
			}
		}
		s.mu.Lock()
		var settled []int
		for id := range s.shown {
			if !pending[id] {
				settled = append(settled, id)
				delete(s.shown, id)
			}
		}
		s.mu.Unlock()
		for _, id := range settled {
			msg := Msg{Type: "settled", ID: id}
			if res, ok := s.m.Settled(id); ok {
				msg.Status, msg.Kind = res.Status, res.Kind
			}
			s.broadcast(msg)
		}
	}
}
