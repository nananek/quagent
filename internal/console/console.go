// Package console は承認コンソール。quagent 本体 (Server) と、tmux のペインで
// 動く UI (RunClient) が unix socket 上の JSON 行でやり取りする。
//
// 本体は利用者の端末で動き続ける (秘密を渡す環境変数などを保持するため)。
// UI は表示と入力だけを受け持つ。
package console

import (
	"encoding/json"
	"net"
	"os"
	"sync"
	"time"

	"github.com/nananek/quagent/internal/access"
)

// Msg は本体と UI の間のメッセージ。
type Msg struct {
	Type string `json:"type"` // request / settled / log (本体 -> UI)、decide / quit (UI -> 本体)

	ID       int      `json:"id,omitempty"`
	Domains  []string `json:"domains,omitempty"`
	Reason   string   `json:"reason,omitempty"`
	Deadline string   `json:"deadline,omitempty"`

	Status   access.Status `json:"status,omitempty"`
	Kind     access.Kind   `json:"kind,omitempty"`
	Question string        `json:"question,omitempty"`

	Text string `json:"text,omitempty"`
}

// Server は本体側。申請と DNS の拒否を UI に流し、UI の判断を Manager に渡す。
type Server struct {
	m    *access.Manager
	sock string
	l    net.Listener

	// Quit は UI から終了の指示が来ると閉じる。
	Quit     chan struct{}
	quitOnce sync.Once

	mu      sync.Mutex
	clients map[*json.Encoder]bool
	shown   map[int]bool // UI に送った申請
	backlog []Msg        // UI が繋がる前のログ
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
		clients: map[*json.Encoder]bool{}, shown: map[int]bool{}}
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
		s.backlog = append(s.backlog, msg)
		return
	}
	for enc := range s.clients {
		if err := enc.Encode(msg); err != nil {
			delete(s.clients, enc)
		}
	}
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
	enc := json.NewEncoder(c)
	s.mu.Lock()
	for _, msg := range s.backlog {
		_ = enc.Encode(msg)
	}
	s.backlog = nil
	s.clients[enc] = true
	// 繋ぎ直した UI にも承認待ちを全部見せる
	for _, r := range s.m.Pending() {
		_ = enc.Encode(requestMsg(r))
	}
	s.mu.Unlock()

	dec := json.NewDecoder(c)
	for {
		var msg Msg
		if err := dec.Decode(&msg); err != nil {
			s.mu.Lock()
			delete(s.clients, enc)
			s.mu.Unlock()
			return
		}
		switch msg.Type {
		case "decide":
			err := s.m.Decide(msg.ID, access.Decision{Status: msg.Status, Kind: msg.Kind, Question: msg.Question})
			if err != nil {
				s.Log("判断を反映できない: " + err.Error())
			}
		case "quit":
			s.quitOnce.Do(func() { close(s.Quit) })
		}
	}
}

func requestMsg(r *access.Request) Msg {
	return Msg{Type: "request", ID: r.ID, Domains: r.Domains, Reason: r.Reason,
		Deadline: r.Created.Add(access.DecisionTimeout).Format("15:04:05")}
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
