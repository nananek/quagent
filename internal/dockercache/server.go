package dockercache

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mdlayher/vsock"
	"github.com/nananek/quagent/internal/console"
)

// Server は OCI / Docker Registry Pull-through Cache の HTTP サーバー。
type Server struct {
	storage   *Storage
	dockerHub Upstream
	ghcr      Upstream

	// Port は host で待ち受ける vsock ポート。
	Port uint32
	cid  uint32

	// AskApproval は upstream ダウンロード時に人間の承認を求めるコールバック。
	// nil なら常に許可。
	AskApproval func(info console.DockerImageInfo) error

	httpSrv       *http.Server
	mu            sync.Mutex
	approvedRepos map[string]bool // key: "registry/repository"
}


// NewServer は新しい Server を作成する。
func NewServer(storage *Storage, dockerHub, ghcr Upstream) *Server {
	if dockerHub == nil {
		dockerHub = NewDockerHubUpstream(nil)
	}
	if ghcr == nil {
		ghcr = NewGHCRUpstream(nil)
	}
	return &Server{
		storage:       storage,
		dockerHub:     dockerHub,
		ghcr:          ghcr,
		approvedRepos: make(map[string]bool),
	}
}

// parsePath は /v2/<repo...>/<action>/<ref> を解析し、レジストリ・リポジトリ・参照・アクションを返す。
func parsePath(p string) (registry, repository, action, reference string, ok bool) {
	p = strings.TrimPrefix(p, "/v2/")
	parts := strings.Split(p, "/")
	if len(parts) < 3 {
		return "", "", "", "", false
	}

	actionIdx := -1
	for i, part := range parts {
		if part == "manifests" || part == "blobs" {
			actionIdx = i
			break
		}
	}
	if actionIdx <= 0 || actionIdx >= len(parts)-1 {
		return "", "", "", "", false
	}

	action = parts[actionIdx]
	reference = strings.Join(parts[actionIdx+1:], "/")
	fullRepo := strings.Join(parts[:actionIdx], "/")

	if strings.HasPrefix(fullRepo, "ghcr.io/") {
		registry = "ghcr.io"
		repository = strings.TrimPrefix(fullRepo, "ghcr.io/")
	} else if strings.HasPrefix(fullRepo, "docker.io/") {
		registry = "docker.io"
		repository = strings.TrimPrefix(fullRepo, "docker.io/")
	} else {
		registry = "docker.io"
		repository = fullRepo
	}

	return registry, repository, action, reference, true
}

func (s *Server) checkApproval(registry, repository, reference string) error {
	if s.AskApproval == nil {
		return nil
	}

	key := registry + "/" + repository
	s.mu.Lock()
	if s.approvedRepos[key] {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	err := s.AskApproval(console.DockerImageInfo{
		Registry:   registry,
		Repository: repository,
		Reference:  reference,
	})
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.approvedRepos[key] = true
	s.mu.Unlock()

	return nil
}

func (s *Server) getUpstream(registry string) Upstream {
	if registry == "ghcr.io" {
		return s.ghcr
	}
	return s.dockerHub
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 読み取り (GET / HEAD) 以外は拒否
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method Not Allowed (Read-only cache)", http.StatusMethodNotAllowed)
		return
	}

	// 1. /v2/ 互換性チェック
	if r.URL.Path == "/v2" || r.URL.Path == "/v2/" {
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "{}")
		return
	}

	registry, repository, action, ref, ok := parsePath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")

	switch action {
	case "manifests":
		s.handleManifest(w, r, registry, repository, ref)
	case "blobs":
		s.handleBlob(w, r, registry, repository, ref)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request, registry, repository, reference string) {
	// 1. キャッシュヒット確認 (承認不要)
	if data, meta, err := s.storage.GetManifest(registry, repository, reference); err == nil {
		if meta.ContentType != "" {
			w.Header().Set("Content-Type", meta.ContentType)
		}
		if meta.Digest != "" {
			w.Header().Set("Docker-Content-Digest", meta.Digest)
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(data)
		}
		return
	}

	// 2. キャッシュミス: upstream ダウンロード前に承認を求める
	if err := s.checkApproval(registry, repository, reference); err != nil {
		http.Error(w, fmt.Sprintf("Forbidden: %v", err), http.StatusForbidden)
		return
	}

	upstream := s.getUpstream(registry)
	resp, err := upstream.GetManifest(r.Context(), repository, reference, r.Header)
	if err != nil {
		http.Error(w, fmt.Sprintf("Upstream error: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to read manifest: %v", err), http.StatusInternalServerError)
		return
	}

	contentType := resp.Header.Get("Content-Type")
	digest := resp.Header.Get("Docker-Content-Digest")

	// キャッシュに保存 (エラーでもレスポンスは返す)
	_ = s.storage.SaveManifest(registry, repository, reference, contentType, digest, data)

	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	if digest != "" {
		w.Header().Set("Docker-Content-Digest", digest)
	}
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

func (s *Server) handleBlob(w http.ResponseWriter, r *http.Request, registry, repository, digest string) {
	// 1. キャッシュヒット確認 (承認不要)
	if has, size := s.storage.HasBlob(digest); has {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Docker-Content-Digest", digest)
		w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			f, _, err := s.storage.OpenBlob(digest)
			if err != nil {
				http.Error(w, fmt.Sprintf("Failed to open blob: %v", err), http.StatusInternalServerError)
				return
			}
			defer f.Close()
			_, _ = io.Copy(w, f)
		}
		return
	}

	// 2. キャッシュミス: 承認チェック
	if err := s.checkApproval(registry, repository, digest); err != nil {
		http.Error(w, fmt.Sprintf("Forbidden: %v", err), http.StatusForbidden)
		return
	}

	upstream := s.getUpstream(registry)
	resp, err := upstream.GetBlob(r.Context(), repository, digest)
	if err != nil {
		http.Error(w, fmt.Sprintf("Upstream error: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	// 一時ファイルに保存して SHA256 検証
	size, err := s.storage.SaveBlob(digest, resp.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to save blob: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
	w.WriteHeader(http.StatusOK)

	if r.Method != http.MethodHead {
		f, _, err := s.storage.OpenBlob(digest)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to open cached blob: %v", err), http.StatusInternalServerError)
			return
		}
		defer f.Close()
		_, _ = io.Copy(w, f)
	}
}

// Start は host の vsock で待ち受けを始める (ポートは空いているものを選ぶ)。
func (s *Server) Start(cid uint32) error {
	s.cid = cid
	var l net.Listener
	var err error
	for range 20 {
		n, _ := rand.Int(rand.Reader, big.NewInt(40000))
		port := uint32(20000 + n.Int64())
		if l, err = vsock.Listen(port, nil); err == nil {
			s.Port = port
			break
		}
	}
	if err != nil {
		return fmt.Errorf("vsock で待ち受けられない: %w", err)
	}

	s.httpSrv = &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	gl := &guardListener{
		Listener: l,
		cid:      cid,
		sem:      make(chan struct{}, 64),
	}

	go func() { _ = s.httpSrv.Serve(gl) }()
	return nil
}

// Stop はサーバーを停止する。
func (s *Server) Stop() {
	if s.httpSrv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.httpSrv.Shutdown(ctx)
}

type guardListener struct {
	net.Listener
	cid uint32
	sem chan struct{}
}

func (l *guardListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if a, ok := c.RemoteAddr().(*vsock.Addr); ok && a.ContextID == l.cid {
			select {
			case l.sem <- struct{}{}:
				return &guardConn{Conn: c, sem: l.sem}, nil
			default:
				c.Close()
				continue
			}
		}
		c.Close()
	}
}

type guardConn struct {
	net.Conn
	sem  chan struct{}
	once sync.Once
}

func (c *guardConn) Close() error {
	c.once.Do(func() { <-c.sem })
	return c.Conn.Close()
}

