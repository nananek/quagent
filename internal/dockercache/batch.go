package dockercache

import (
	"sync"
	"time"

	"github.com/nananek/quagent/internal/access"
	"github.com/nananek/quagent/internal/console"
)

// BatchItem はバッチ承認される項目 (console.BatchItem のエイリアス)。
type BatchItem = console.BatchItem

type batchQueue struct {
	ecosystem string
	items     []BatchItem
	waiters   []chan error
	timer     *time.Timer
}

// BatchManager はパッケージマネージャーの未キャッシュリクエストを集約し、
// バッチ単位で承認コンソールに諮るマネージャー。
type BatchManager struct {
	AskBatch func(info console.BatchDownloadInfo) (access.Kind, error)
	Window   time.Duration // デバウンス待機時間 (既定: 400ms)
	MaxSize  int           // 1 バッチの最大項目数 (既定: 50)

	mu              sync.Mutex
	sessionApproved map[string]bool        // ecosystem -> true
	approvedItems   map[string]bool        // "ecosystem:name" -> true
	pendingQueues   map[string]*batchQueue // ecosystem -> queue
}

// NewBatchManager は新しい BatchManager を作成する。
func NewBatchManager(askBatch func(info console.BatchDownloadInfo) (access.Kind, error)) *BatchManager {
	return &BatchManager{
		AskBatch:        askBatch,
		Window:          400 * time.Millisecond,
		MaxSize:         50,
		sessionApproved: make(map[string]bool),
		approvedItems:   make(map[string]bool),
		pendingQueues:   make(map[string]*batchQueue),
	}
}

// RequestApproval は指定されたエコシステム・パッケージのダウンロード承認を要求する。
// 短時間の後続リクエストをバッチに集約してコンソールに諮る。
func (b *BatchManager) RequestApproval(ecosystem, name, version string) error {
	if b.AskBatch == nil {
		return nil
	}

	b.mu.Lock()

	// 1. セッション承認済みチェック
	if b.sessionApproved[ecosystem] || b.sessionApproved["all"] {
		b.mu.Unlock()
		return nil
	}

	// 2. 同一パッケージ名が承認済みチェック
	key := ecosystem + ":" + name
	if b.approvedItems[key] {
		b.mu.Unlock()
		return nil
	}

	// 3. キュー取得または作成
	q, ok := b.pendingQueues[ecosystem]
	if !ok {
		q = &batchQueue{
			ecosystem: ecosystem,
		}
		b.pendingQueues[ecosystem] = q
		q.timer = time.AfterFunc(b.Window, func() {
			b.flushQueue(ecosystem)
		})
	}

	// 重複チェック
	itemExists := false
	for _, it := range q.items {
		if it.Name == name && it.Version == version {
			itemExists = true
			break
		}
	}
	if !itemExists {
		q.items = append(q.items, BatchItem{
			Ecosystem: ecosystem,
			Name:      name,
			Version:   version,
		})
	}

	ch := make(chan error, 1)
	q.waiters = append(q.waiters, ch)

	// 上限到達なら即時 flush
	if len(q.items) >= b.MaxSize {
		q.timer.Stop()
		delete(b.pendingQueues, ecosystem)
		b.mu.Unlock()
		b.executeBatch(q)
		return <-ch
	}

	b.mu.Unlock()
	return <-ch
}

func (b *BatchManager) flushQueue(ecosystem string) {
	b.mu.Lock()
	q, ok := b.pendingQueues[ecosystem]
	if !ok {
		b.mu.Unlock()
		return
	}
	delete(b.pendingQueues, ecosystem)
	b.mu.Unlock()

	b.executeBatch(q)
}

func (b *BatchManager) executeBatch(q *batchQueue) {
	kind, err := b.AskBatch(console.BatchDownloadInfo{
		Ecosystem: q.ecosystem,
		Items:     q.items,
	})

	b.mu.Lock()
	if err == nil {
		if kind == access.Session {
			b.sessionApproved[q.ecosystem] = true
		}
		for _, item := range q.items {
			b.approvedItems[q.ecosystem+":"+item.Name] = true
		}
	}
	b.mu.Unlock()

	for _, ch := range q.waiters {
		ch <- err
	}
}
