// Package console は承認コンソール。quagent 本体 (Server) と、tmux のペインで
// 動く UI (RunClient) が unix socket 上の JSON 行でやり取りする。
//
// 本体は利用者の端末で動き続ける (秘密を渡す環境変数などを保持するため)。
// UI は表示と入力だけを受け持つ。エージェントのペインの __attach も同じ socket に
// つなぎ、VM が出した OSC 52 (クリップボードへの書き込み要求) を本体に渡す。
package console

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
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

	// Web 遠隔操作 (hub) のアクセス URL 再発行 (hubreissue / hubreissued)。
	URL   string `json:"url,omitempty"`
	Error string `json:"error,omitempty"`

	// PR の作成承認 (prrequest / prsettled) の内容。
	Branch string `json:"branch,omitempty"`
	Base   string `json:"base,omitempty"`
	Title  string `json:"title,omitempty"`
	Body   string `json:"body,omitempty"`

	// 動的ヘッダ・メソッド緩和 (relaxrequest / relaxsettled) の内容。
	RelaxHost    string   `json:"relax_host,omitempty"`
	RelaxHeaders []string `json:"relax_headers,omitempty"`
	RelaxMethods []string `json:"relax_methods,omitempty"`
	AllowBody    bool     `json:"allow_body,omitempty"`

	// Docker イメージの取得承認 (dockerrequest / dockersettled) の内容。
	DockerRegistry   string `json:"docker_registry,omitempty"`
	DockerRepository string `json:"docker_repository,omitempty"`
	DockerReference  string `json:"docker_reference,omitempty"`

	// パッケージ一括取得承認 (batchrequest / batchsettled) の内容。
	BatchEcosystem string      `json:"batch_ecosystem,omitempty"`
	BatchItems     []BatchItem `json:"batch_items,omitempty"`
	// BatchMetadata はパッケージ取得ではなく、依存解決のためのメタデータ参照の
	// 許可を求めるダイアログであることを示す。
	BatchMetadata bool `json:"batch_metadata,omitempty"`
	// BatchRevision は表示中の項目一覧の版。合流で内容が変わるたびに増える。
	// 判断は自分が見た版を返し、サーバーは版が一致しない判断を拒否する。
	BatchRevision int `json:"batch_revision,omitempty"`
}

// BatchItem はバッチ承認でダウンロードされる 1 つの項目。
type BatchItem struct {
	Ecosystem string `json:"ecosystem"` // "go", "cargo", "npm", "pypi", "docker"
	Name      string `json:"name"`      // パッケージ名 / クレート名
	Version   string `json:"version"`   // バージョン / タグ
}

// BatchDownloadInfo はバッチ承認リクエスト情報。
type BatchDownloadInfo struct {
	Ecosystem string
	Items     []BatchItem
	// Metadata はパッケージそのものではなく、依存解決のためのメタデータ参照の
	// 許可を求める。承認はこの run 中の参照すべてに効く。
	Metadata bool
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

	// hubReissue は Web 遠隔操作 (hub) のアクセス URL を再発行する。run が hub に
	// 登録したときに設定する (nil なら hub 未登録)。s.mu で保護する。
	hubReissue func() (string, error)

	mu         sync.Mutex
	clients    map[*client]bool
	shown      map[int]bool // UI に送った申請
	shownRelax map[int]bool // UI に送った緩和申請
	backlog    []Msg        // UI が繋がる前のログ
	clip       clipState

	// PR の作成承認。access.Manager とは別に、この Server が直接待つ。
	prMu      sync.Mutex
	prNextID  int
	prPending []*prRequest

	// Docker イメージの取得承認。
	dockerMu      sync.Mutex
	dockerNextID  int
	dockerPending []*dockerRequest

	// パッケージ一括取得承認 (バッチ承認)。
	batchMu      sync.Mutex
	batchNextID  int
	batchPending []*batchRequest
}

type batchRequest struct {
	id       int
	info     BatchDownloadInfo
	created  time.Time
	done     chan struct{}
	status   access.Status
	kind     access.Kind
	revision int
	metadata bool
	approved bool
}

// maxBatchReview は 1 枚のダイアログでレビューさせる項目数の上限。表示しきれない
// 項目を同じ承認で通さないため、超えた分は次のダイアログに分ける。
const maxBatchReview = 10

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

// DockerImageInfo は承認コンソールに諮る Docker イメージ取得の内容。
type DockerImageInfo struct {
	Registry   string // "docker.io" または "ghcr.io"
	Repository string // "library/golang", "astral-sh/uv" など
	Reference  string // "1.24", "latest", または digest
}

type dockerRequest struct {
	id       int
	info     DockerImageInfo
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
		clients: map[*client]bool{}, shown: map[int]bool{}, shownRelax: map[int]bool{}}
	m.Log = s.Log
	go s.accept()
	go s.watch()
	return s, nil
}

// Close は待ち受けを止め、待機中の承認と監視 goroutine も終わらせる。
func (s *Server) Close() {
	s.TriggerQuit()
	s.l.Close()
}

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
		case "relaxdecide":
			err := s.m.DecideRelaxation(msg.ID, access.Decision{Status: msg.Status, Kind: msg.Kind, Question: msg.Question})
			if err != nil {
				s.Log("緩和判断を反映できない: " + err.Error())
			}
		case "clipboard":
			s.onClipboard(msg)
		case "clipdecide":
			s.clipDecide(msg.ID, msg.Status == access.Approved)
		case "prdecide":
			s.settlePR(msg.ID, msg.Status)
		case "dockerdecide":
			s.settleDocker(msg.ID, msg.Status)
		case "batchdecide":
			s.settleBatch(msg.ID, msg.Status, msg.Kind, msg.BatchRevision)
		case "hubreissue":
			s.reissueHubURL()
		case "quit":
			s.TriggerQuit()
		}
	}
}

// TriggerQuit はコンソールの終了シグナルを発火する。
func (s *Server) TriggerQuit() {
	s.quitOnce.Do(func() { close(s.Quit) })
}

// SetHubReissue は hub のアクセス URL を再発行する関数を設定する (run が hub に
// 登録したときに呼ぶ)。nil なら hub 未登録として扱う。
func (s *Server) SetHubReissue(f func() (string, error)) {
	s.mu.Lock()
	s.hubReissue = f
	s.mu.Unlock()
}

// reissueHubURL は UI からの要求で hub のアクセス URL を再発行し、結果を全 UI に配る。
func (s *Server) reissueHubURL() {
	s.mu.Lock()
	f := s.hubReissue
	s.mu.Unlock()
	if f == nil {
		s.broadcast(Msg{Type: "hubreissued", Error: "hub に登録されていない"})
		return
	}
	url, err := f()
	if err != nil {
		s.Log("hub のアクセス URL を再発行できない: " + err.Error())
		s.broadcast(Msg{Type: "hubreissued", Error: err.Error()})
		return
	}
	// URL 自体はログに残さない (ログは hub 経由でブラウザにも届くため)。
	s.Log("hub のアクセス URL を再発行した")
	s.broadcast(Msg{Type: "hubreissued", URL: url})
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
	for _, r := range s.m.PendingRelaxations() {
		_ = s.send(cl, relaxRequestMsg(r))
	}
	if s.clip.pending != nil {
		_ = s.send(cl, clipMsg(s.clip.pending))
	}
	s.prMu.Lock()
	for _, r := range s.prPending {
		_ = s.send(cl, prRequestMsg(r))
	}
	s.prMu.Unlock()
	s.dockerMu.Lock()
	for _, r := range s.dockerPending {
		_ = s.send(cl, dockerRequestMsg(r))
	}
	s.dockerMu.Unlock()
	s.batchMu.Lock()
	for _, r := range s.batchPending {
		_ = s.send(cl, batchRequestMsg(r))
	}
	s.batchMu.Unlock()
	s.clients[cl] = true
}

func requestMsg(r *access.Request) Msg {
	return Msg{Type: "request", ID: r.ID, Domains: r.Domains, Reason: r.Reason,
		Deadline: r.Created.Add(access.DecisionTimeout).Format("15:04:05")}
}

func relaxRequestMsg(r *access.RelaxRequest) Msg {
	return Msg{Type: "relaxrequest", ID: r.ID,
		RelaxHost:    r.Relaxation.Host,
		RelaxHeaders: r.Relaxation.Headers,
		RelaxMethods: r.Relaxation.Methods,
		AllowBody:    r.Relaxation.AllowBody,
		Reason:       r.Reason,
		Deadline:     r.Created.Add(access.DecisionTimeout).Format("15:04:05")}
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

// AskDockerDownload は Docker イメージのキャッシュ取得を承認コンソールに諮り、承認されるまで待つ。
func (s *Server) AskDockerDownload(info DockerImageInfo) error {
	req := &dockerRequest{info: info, created: time.Now(), done: make(chan struct{})}
	s.dockerMu.Lock()
	s.dockerNextID++
	req.id = s.dockerNextID
	s.dockerPending = append(s.dockerPending, req)
	s.dockerMu.Unlock()
	s.broadcast(dockerRequestMsg(req))

	t := time.NewTimer(access.DecisionTimeout)
	defer t.Stop()
	select {
	case <-req.done:
	case <-t.C:
		s.settleDocker(req.id, access.TimedOut)
		<-req.done
	case <-s.Quit:
		s.settleDocker(req.id, access.Denied)
		return fmt.Errorf("終了したので Docker イメージを取得しなかった")
	}
	if req.approved {
		return nil
	}
	if req.status == access.TimedOut {
		return fmt.Errorf("%s 以内に承認されなかったので Docker イメージを取得しなかった", access.DecisionTimeout)
	}
	return fmt.Errorf("Docker イメージの取得は承認されなかった")
}

// settleDocker は承認待ちの Docker イメージ取得を決着させ、UI に知らせる。
func (s *Server) settleDocker(id int, status access.Status) {
	s.dockerMu.Lock()
	idx := -1
	for i, r := range s.dockerPending {
		if r.id == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.dockerMu.Unlock()
		return
	}
	req := s.dockerPending[idx]
	s.dockerPending = append(s.dockerPending[:idx], s.dockerPending[idx+1:]...)
	req.status = status
	req.approved = status == access.Approved
	s.dockerMu.Unlock()
	close(req.done)
	s.broadcast(Msg{Type: "dockersettled", ID: id, Status: status})
}

func dockerRequestMsg(r *dockerRequest) Msg {
	return Msg{
		Type:             "dockerrequest",
		ID:               r.id,
		DockerRegistry:   r.info.Registry,
		DockerRepository: r.info.Repository,
		DockerReference:  r.info.Reference,
		Deadline:         r.created.Add(access.DecisionTimeout).Format("15:04:05"),
	}
}

// AskBatchDownload はパッケージ一括取得を承認コンソールに諮り、承認されるまで待つ。
// 既に未決のダイアログがあれば新しい項目をそこへ合流させ、1 枚にまとめる。
// 戻り値の kind は承認の種類で、Session なら呼び出し側は同じエコシステムの取得を
// この run のあいだ確認なしで通してよい。
func (s *Server) AskBatchDownload(info BatchDownloadInfo) (access.Kind, error) {
	if info.Metadata {
		return s.askBatchMetadata(info)
	}

	info.Items = normalizeBatchItems(info.Ecosystem, info.Items)
	if len(info.Items) == 0 {
		return access.Once, nil
	}
	info.Ecosystem = batchEcosystems(info.Items, info.Ecosystem)

	// 1 枚に表示できる件数に分ける。Session が選ばれたら残りは諮らない。
	for start := 0; start < len(info.Items); start += maxBatchReview {
		end := min(start+maxBatchReview, len(info.Items))
		kind, err := s.askBatchChunk(BatchDownloadInfo{
			Ecosystem: info.Ecosystem,
			Items:     info.Items[start:end],
		})
		if err != nil {
			return "", err
		}
		if kind == access.Session {
			return access.Session, nil
		}
	}
	return access.Once, nil
}

// askBatchChunk は 1 枚分の項目を未決ダイアログへ合流させ、判断を待つ。
func (s *Server) askBatchChunk(info BatchDownloadInfo) (access.Kind, error) {
	req, msg, merged := s.addOrMergeBatch(info)
	// 新規でも合流でも、表示中のダイアログを最新の内容に更新する (同じ ID で再送)。
	s.broadcast(msg)
	return s.waitBatch(req, merged)
}

// askBatchMetadata はメタデータ参照の許可を 1 回だけ諮る。合流はしない。
// 承認は run 全体に効くので、UI が once / session のどちらを返しても Session を返す。
func (s *Server) askBatchMetadata(info BatchDownloadInfo) (access.Kind, error) {
	info.Items = nil
	if strings.TrimSpace(info.Ecosystem) == "" {
		return "", fmt.Errorf("メタデータ参照の対象エコシステムが空")
	}

	s.batchMu.Lock()
	s.batchNextID++
	req := &batchRequest{
		id:       s.batchNextID,
		info:     info,
		created:  time.Now(),
		done:     make(chan struct{}),
		revision: 1,
		metadata: true,
	}
	s.batchPending = append(s.batchPending, req)
	s.batchMu.Unlock()
	s.broadcast(batchRequestMsg(req))

	if _, err := s.waitBatch(req, false); err != nil {
		return "", err
	}
	return access.Session, nil
}

// waitBatch はダイアログの決着を待つ。merged のときは、ダイアログを出した側の
// タイマーと終了処理に任せる。
func (s *Server) waitBatch(req *batchRequest, merged bool) (access.Kind, error) {
	if !merged {
		t := time.NewTimer(access.DecisionTimeout)
		defer t.Stop()
		select {
		case <-req.done:
		case <-t.C:
			s.settleBatch(req.id, access.TimedOut, "", 0)
			<-req.done
		case <-s.Quit:
			s.settleBatch(req.id, access.Denied, "", 0)
			return "", fmt.Errorf("終了したのでパッケージを取得しなかった")
		}
	} else {
		<-req.done
	}

	if req.approved {
		if req.kind == access.Session {
			return access.Session, nil
		}
		return access.Once, nil
	}
	if req.status == access.TimedOut {
		if req.metadata {
			return "", fmt.Errorf("%s 以内に承認されなかったのでメタデータを参照しなかった", access.DecisionTimeout)
		}
		return "", fmt.Errorf("%s 以内に承認されなかったのでパッケージを取得しなかった", access.DecisionTimeout)
	}
	if req.metadata {
		return "", fmt.Errorf("メタデータの参照は承認されなかった")
	}
	return "", fmt.Errorf("パッケージの取得は承認されなかった")
}

// addOrMergeBatch は未決のダイアログに項目を合流させる。合流先が無ければ新しい
// 申請を作る。merged は既存のダイアログへ合流したとき true。msg は表示用の
// スナップショット (合流で内容が変わるため、ロックを離す前に作る)。
func (s *Server) addOrMergeBatch(info BatchDownloadInfo) (req *batchRequest, msg Msg, merged bool) {
	s.batchMu.Lock()
	defer s.batchMu.Unlock()
	for _, r := range s.batchPending {
		if r.metadata {
			continue
		}
		if len(r.info.Items)+len(info.Items) > maxBatchReview {
			continue
		}
		r.info.Items = appendDedupBatchItems(r.info.Items, info.Items)
		r.info.Ecosystem = batchEcosystems(r.info.Items, r.info.Ecosystem)
		r.revision++
		return r, batchRequestMsg(r), true
	}
	s.batchNextID++
	req = &batchRequest{info: info, created: time.Now(), done: make(chan struct{}), revision: 1}
	req.id = s.batchNextID
	s.batchPending = append(s.batchPending, req)
	return req, batchRequestMsg(req), false
}

// normalizeBatchItems は項目のエコシステムを補い、重複を落とす。
func normalizeBatchItems(ecosystem string, items []BatchItem) []BatchItem {
	out := make([]BatchItem, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, it := range items {
		if it.Ecosystem == "" {
			it.Ecosystem = ecosystem
		}
		key := it.Ecosystem + "\x00" + it.Name
		if it.Name == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, it)
	}
	return out
}

// appendDedupBatchItems は dst に無い項目だけを足す。
func appendDedupBatchItems(dst, add []BatchItem) []BatchItem {
	seen := make(map[string]bool, len(dst)+len(add))
	for _, it := range dst {
		seen[it.Ecosystem+"\x00"+it.Name] = true
	}
	for _, it := range add {
		key := it.Ecosystem + "\x00" + it.Name
		if it.Name == "" || seen[key] {
			continue
		}
		seen[key] = true
		dst = append(dst, it)
	}
	return dst
}

// batchEcosystems は項目に含まれるエコシステムを列挙する。複数エコシステムを
// 1 枚のダイアログにまとめたとき、表示用に "npm, pypi" のように返す。
func batchEcosystems(items []BatchItem, fallback string) string {
	seen := make(map[string]bool, len(items))
	var list []string
	for _, it := range items {
		if it.Ecosystem == "" || seen[it.Ecosystem] {
			continue
		}
		seen[it.Ecosystem] = true
		list = append(list, it.Ecosystem)
	}
	if len(list) == 0 {
		return fallback
	}
	return strings.Join(list, ", ")
}

// settleBatch は承認待ちのバッチ取得を決着させ、UI に知らせる。
// 項目一覧が更新された後に古い版の承認が届いた場合は決着させない。
func (s *Server) settleBatch(id int, status access.Status, kind access.Kind, revision int) {
	s.batchMu.Lock()
	idx := -1
	for i, r := range s.batchPending {
		if r.id == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.batchMu.Unlock()
		return
	}
	req := s.batchPending[idx]
	if status == access.Approved && !req.metadata && revision != req.revision {
		s.batchMu.Unlock()
		s.Log("パッケージ一覧が更新されたので、内容を確認して再承認してください")
		return
	}
	s.batchPending = append(s.batchPending[:idx], s.batchPending[idx+1:]...)
	req.status = status
	req.approved = status == access.Approved
	if req.approved && kind == access.Session {
		req.kind = access.Session
	}
	s.batchMu.Unlock()
	close(req.done)
	s.broadcast(Msg{Type: "batchsettled", ID: id, Status: status, Kind: req.kind})
}

func batchRequestMsg(r *batchRequest) Msg {
	return Msg{
		Type:           "batchrequest",
		ID:             r.id,
		BatchEcosystem: r.info.Ecosystem,
		BatchItems:     r.info.Items,
		BatchMetadata:  r.metadata,
		BatchRevision:  r.revision,
		Deadline:       r.created.Add(access.DecisionTimeout).Format("15:04:05"),
	}
}

// watch は新しい申請を UI に送り、決着した申請 (時間切れを含む) を知らせる。
func (s *Server) watch() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-s.m.Notify:
		case <-tick.C:
		case <-s.Quit:
			return
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

		pendingRelax := map[int]bool{}
		for _, r := range s.m.PendingRelaxations() {
			pendingRelax[r.ID] = true
			s.mu.Lock()
			isNew := !s.shownRelax[r.ID]
			s.shownRelax[r.ID] = true
			s.mu.Unlock()
			if isNew {
				s.broadcast(relaxRequestMsg(r))
			}
		}
		s.mu.Lock()
		var settledRelax []int
		for id := range s.shownRelax {
			if !pendingRelax[id] {
				settledRelax = append(settledRelax, id)
				delete(s.shownRelax, id)
			}
		}
		s.mu.Unlock()
		for _, id := range settledRelax {
			msg := Msg{Type: "relaxsettled", ID: id}
			if res, ok := s.m.SettledRelaxation(id); ok {
				msg.Status, msg.Kind = res.Status, res.Kind
			}
			s.broadcast(msg)
		}
	}
}
