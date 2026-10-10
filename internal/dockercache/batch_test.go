package dockercache

import (
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

func TestBatchManagerSessionApproval(t *testing.T) {
	var callCount int32

	askBatch := func(info console.BatchDownloadInfo) (access.Kind, error) {
		atomic.AddInt32(&callCount, 1)
		return access.Session, nil // セッション承認
	}

	bm := NewBatchManager(askBatch)
	bm.Window = 30 * time.Millisecond

	// 1回目
	if err := bm.RequestApproval("npm", "lodash", "4.17.21"); err != nil {
		t.Fatal(err)
	}
	if c := atomic.LoadInt32(&callCount); c != 1 {
		t.Fatalf("expected 1 call, got %d", c)
	}

	// 2回目 (別のパッケージ) -> セッション承認済みなので即座に通るはず (AskBatch 呼び出しなし)
	if err := bm.RequestApproval("npm", "express", "4.18.2"); err != nil {
		t.Fatal(err)
	}
	if c := atomic.LoadInt32(&callCount); c != 1 {
		t.Fatalf("expected still 1 call due to session approval, got %d", c)
	}

	// 別のエコシステム (go) -> まだセッション承認されていないので AskBatch されるはず
	if err := bm.RequestApproval("go", "github.com/gin-gonic/gin", "v1.9.1"); err != nil {
		t.Fatal(err)
	}
	if c := atomic.LoadInt32(&callCount); c != 2 {
		t.Fatalf("expected 2 calls, got %d", c)
	}
}
