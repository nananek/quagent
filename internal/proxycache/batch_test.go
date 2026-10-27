package proxycache

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nananek/quagent/internal/access"
	"github.com/nananek/quagent/internal/console"
)

func TestBatchManagerAggregation(t *testing.T) {
	var callCount int32
	var lastInfo console.BatchDownloadInfo
	var mu sync.Mutex

	askBatch := func(info console.BatchDownloadInfo) (access.Kind, error) {
		atomic.AddInt32(&callCount, 1)
		mu.Lock()
		lastInfo = info
		mu.Unlock()
		return access.Once, nil
	}

	bm := NewBatchManager(askBatch)
	bm.Window = 50 * time.Millisecond // テスト用に短縮

	// 5つのリクエストをほぼ同時に送信
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		name := string(rune('a' + i))
		go func(pkg string) {
			defer wg.Done()
			if err := bm.RequestApproval("cargo", pkg, "1.0.0"); err != nil {
				t.Errorf("RequestApproval error: %v", err)
			}
		}(name)
	}
	wg.Wait()

	// 1回の AskBatch 呼び出しに 5 つのアイテムが集約されているはず
	if c := atomic.LoadInt32(&callCount); c != 1 {
		t.Fatalf("expected 1 AskBatch call, got %d", c)
	}

	mu.Lock()
	defer mu.Unlock()
	if lastInfo.Ecosystem != "cargo" {
		t.Errorf("expected ecosystem 'cargo', got %s", lastInfo.Ecosystem)
	}
	if len(lastInfo.Items) != 5 {
		t.Errorf("expected 5 items, got %d", len(lastInfo.Items))
	}
}

func TestBatchManagerPreResolvedItems(t *testing.T) {
	var callCount int32
	var lastInfo console.BatchDownloadInfo
	var mu sync.Mutex

	askBatch := func(info console.BatchDownloadInfo) (access.Kind, error) {
		atomic.AddInt32(&callCount, 1)
		mu.Lock()
		lastInfo = info
		mu.Unlock()
		return access.Once, nil
	}

	bm := NewBatchManager(askBatch)

	// 事前解決で得られた 3 つのアイテムを一括承認
	items := []BatchItem{
		{Ecosystem: "pypi", Name: "jupyterlab", Version: "4.6.4"},
		{Ecosystem: "pypi", Name: "async-lru", Version: "2.0.4"},
		{Ecosystem: "pypi", Name: "httpx", Version: "0.27.0"},
	}

	if err := bm.RequestApprovalItems("pypi", items); err != nil {
		t.Fatal(err)
	}

	if c := atomic.LoadInt32(&callCount); c != 1 {
		t.Fatalf("expected 1 call, got %d", c)
	}
	if len(lastInfo.Items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(lastInfo.Items))
	}

	// 承認済みの各アイテムは個別の RequestApproval でも即座に通過する (AskBatch 呼び出しが増えない)
	for _, it := range items {
		if !bm.IsApproved("pypi", it.Name) {
			t.Errorf("expected %s to be approved", it.Name)
		}
		if err := bm.RequestApproval("pypi", it.Name, it.Version); err != nil {
			t.Errorf("RequestApproval error: %v", err)
		}
	}

	if c := atomic.LoadInt32(&callCount); c != 1 {
		t.Fatalf("expected still 1 call, got %d", c)
	}
}

// TestBatchManagerMetadataConsent は、同じエコシステムのメタデータ参照許可が
// 並行要求でも 1 回の承認にまとまり、run 中は再確認しないことを確かめる。
func TestBatchManagerMetadataConsent(t *testing.T) {
	var calls int32
	askBatch := func(info console.BatchDownloadInfo) (access.Kind, error) {
		if !info.Metadata {
			t.Errorf("メタデータ以外の承認が来た: %+v", info)
		}
		atomic.AddInt32(&calls, 1)
		return access.Once, nil
	}
	bm := NewBatchManager(askBatch)

	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := bm.AllowMetadata("npm"); err != nil {
				t.Errorf("AllowMetadata: %v", err)
			}
		}()
	}
	wg.Wait()
	if c := atomic.LoadInt32(&calls); c != 1 {
		t.Fatalf("並行要求が 1 回の承認にまとまっていない: %d", c)
	}

	// 2 回目以降は確認しない。
	if err := bm.AllowMetadata("npm"); err != nil {
		t.Fatal(err)
	}
	if c := atomic.LoadInt32(&calls); c != 1 {
		t.Fatalf("許可済みのメタデータ参照で再確認した: %d", c)
	}

	// 別エコシステムは別に確認する。
	if err := bm.AllowMetadata("pypi"); err != nil {
		t.Fatal(err)
	}
	if c := atomic.LoadInt32(&calls); c != 2 {
		t.Fatalf("別エコシステムの確認回数が違う: %d", c)
	}
}

// TestBatchManagerSessionScope は、Session 承認したエコシステムだけが以後
// 確認なしで通り、他のエコシステムは従来どおり確認されることを確かめる。
func TestBatchManagerSessionScope(t *testing.T) {
	var callCount int32
	var logged []string
	var mu sync.Mutex
	askBatch := func(info console.BatchDownloadInfo) (access.Kind, error) {
		atomic.AddInt32(&callCount, 1)
		return access.Session, nil
	}

	bm := NewBatchManager(askBatch)
	bm.Window = 10 * time.Millisecond
	bm.Logf = func(s string) {
		mu.Lock()
		logged = append(logged, s)
		mu.Unlock()
	}

	if err := bm.RequestApproval("go", "example.com/a", "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	if c := atomic.LoadInt32(&callCount); c != 1 {
		t.Fatalf("expected 1 call, got %d", c)
	}

	// go はまとめて許可済みなので、新しいパッケージでも確認しない。
	if err := bm.RequestApproval("go", "example.com/b", "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	if !bm.IsApproved("go", "example.com/c") {
		t.Fatal("go がまとめて許可されていない")
	}
	if c := atomic.LoadInt32(&callCount); c != 1 {
		t.Fatalf("session 許可後も確認された: %d", c)
	}

	// 別エコシステムは影響を受けない。
	if bm.IsApproved("npm", "express") {
		t.Fatal("npm まで許可されている")
	}
	if err := bm.RequestApproval("npm", "express", "4.19.2"); err != nil {
		t.Fatal(err)
	}
	if c := atomic.LoadInt32(&callCount); c != 2 {
		t.Fatalf("npm は確認されるはず: %d", c)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(logged) != 2 || !strings.Contains(logged[0], "go") {
		t.Fatalf("まとめて承認の記録が残っていない: %v", logged)
	}
}
