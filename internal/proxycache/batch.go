package proxycache

import (
	"strings"
	"sync"
	"time"

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
	AskBatch func(info console.BatchDownloadInfo) error
	Window   time.Duration // デバウンス待機時間 (既定: 400ms)
	MaxSize  int           // 1 バッチの最大項目数 (既定: 50)

	mu            sync.Mutex
	approvedItems map[string]bool        // "ecosystem:name" -> true (正規化キー)
	pendingQueues map[string]*batchQueue // ecosystem -> queue
}

// normalizeItemKey はエコシステムとパッケージ名から統一的なキーを生成する。
func normalizeItemKey(ecosystem, name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	n = strings.ReplaceAll(n, "_", "-")
	return ecosystem + ":" + n
}

// NewBatchManager は新しい BatchManager を作成する。
func NewBatchManager(askBatch func(info console.BatchDownloadInfo) error) *BatchManager {
	return &BatchManager{
		AskBatch:      askBatch,
		Window:        400 * time.Millisecond,
		MaxSize:       50,
		approvedItems: make(map[string]bool),
		pendingQueues: make(map[string]*batchQueue),
	}
}

// IsApproved は指定されたパッケージが承認済みか確認する。
func (b *BatchManager) IsApproved(ecosystem, name string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.approvedItems[normalizeItemKey(ecosystem, name)]
}

// RequestApproval は指定されたエコシステム・パッケージのダウンロード承認を要求する。
// 短時間の後続リクエストをバッチに集約してコンソールに諮る。
func (b *BatchManager) RequestApproval(ecosystem, name, version string) error {
	return b.RequestApprovalItems(ecosystem, []BatchItem{{
		Ecosystem: ecosystem,
		Name:      name,
		Version:   version,
	}})
}

// RequestApprovalItems は事前解決等によって得られた複数項目を一括で承認申請する。
// すべて承認済みの場合は即座に nil を返す。未承認項目がある場合はバッチ承認に諮る。
func (b *BatchManager) RequestApprovalItems(ecosystem string, items []BatchItem) error {
	if b.AskBatch == nil || len(items) == 0 {
		return nil
	}

	b.mu.Lock()

	// 1. 未承認の項目のみ抽出
	var unapproved []BatchItem
	for _, it := range items {
		key := normalizeItemKey(ecosystem, it.Name)
		if !b.approvedItems[key] {
			// 重複チェック
			exists := false
			for _, u := range unapproved {
				if normalizeItemKey(ecosystem, u.Name) == key {
					exists = true
					break
				}
			}
			if !exists {
				unapproved = append(unapproved, it)
			}
		}
	}

	// 全て承認済みなら即終了
	if len(unapproved) == 0 {
		b.mu.Unlock()
		return nil
	}

	// 2. 事前解決等で複数項目 (>= 2) が指定された場合は、待機せず即座にバッチ実行
	if len(unapproved) > 1 {
		// 既存キューがあればマージして即時 flush
		q, ok := b.pendingQueues[ecosystem]
		if ok {
			q.timer.Stop()
			delete(b.pendingQueues, ecosystem)
			for _, it := range unapproved {
				exists := false
				for _, qit := range q.items {
					if normalizeItemKey(ecosystem, qit.Name) == normalizeItemKey(ecosystem, it.Name) {
						exists = true
						break
					}
				}
				if !exists {
					q.items = append(q.items, it)
				}
			}
		} else {
			q = &batchQueue{
				ecosystem: ecosystem,
				items:     unapproved,
			}
		}
		ch := make(chan error, 1)
		q.waiters = append(q.waiters, ch)
		b.mu.Unlock()
		b.executeBatch(q)
		return <-ch
	}

	// 3. 単一項目の場合はデバウンスキューに追加 (後続リクエスト集約のため)
	single := unapproved[0]
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

	itemExists := false
	for _, it := range q.items {
		if normalizeItemKey(ecosystem, it.Name) == normalizeItemKey(ecosystem, single.Name) {
			itemExists = true
			break
		}
	}
	if !itemExists {
		q.items = append(q.items, single)
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
	err := b.AskBatch(console.BatchDownloadInfo{
		Ecosystem: q.ecosystem,
		Items:     q.items,
	})

	b.mu.Lock()
	if err == nil {
		for _, item := range q.items {
			b.approvedItems[normalizeItemKey(q.ecosystem, item.Name)] = true
		}
	}
	b.mu.Unlock()

	for _, ch := range q.waiters {
		ch <- err
	}
}
