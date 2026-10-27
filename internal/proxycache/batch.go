package proxycache

import (
	"strings"
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
	// AskBatch は承認コンソールに判断を諮る。戻り値の Kind が Session なら、
	// そのエコシステムの取得はこの run のあいだ確認なしで通す。
	AskBatch func(info console.BatchDownloadInfo) (access.Kind, error)
	// Logf はまとめて承認したことを記録する (nil なら記録しない)。
	Logf    func(string)
	Window  time.Duration // デバウンス待機時間 (既定: 400ms)
	MaxSize int           // 1 バッチの最大項目数 (既定: 50)

	mu              sync.Mutex
	approvedItems   map[string]bool // "ecosystem:name" -> true (正規化キー)
	sessionAllowed  map[string]bool // ecosystem -> この run 中は確認しない
	metadataAllowed map[string]bool // ecosystem -> メタデータ参照を許可済み
	metadataPending map[string]*metadataWait
	pendingQueues   map[string]*batchQueue // ecosystem -> queue
}

// metadataWait は同じエコシステムのメタデータ許可待ちを 1 回の承認にまとめる。
type metadataWait struct {
	done chan struct{}
	err  error
}

// normalizeItemKey はエコシステムごとの規則でパッケージ名を正規化し、承認キーを作る。
// 規則をまたいで潰すと、承認ダイアログに出ていない別パッケージが承認扱いになる。
func normalizeItemKey(ecosystem, name string) string {
	n := strings.TrimSpace(name)
	if ecosystem == "pypi" {
		// PyPI は PEP 503 に従い小文字化し、- . _ を - に畳む。
		n = normalizePyPIName(n)
	}
	return ecosystem + ":" + n
}

// NewBatchManager は新しい BatchManager を作成する。
func NewBatchManager(askBatch func(info console.BatchDownloadInfo) (access.Kind, error)) *BatchManager {
	return &BatchManager{
		AskBatch:        askBatch,
		Window:          400 * time.Millisecond,
		MaxSize:         50,
		approvedItems:   make(map[string]bool),
		sessionAllowed:  make(map[string]bool),
		metadataAllowed: make(map[string]bool),
		metadataPending: make(map[string]*metadataWait),
		pendingQueues:   make(map[string]*batchQueue),
	}
}

// AllowMetadata は依存解決のためのメタデータ参照を、この run 中まとめて許可する。
// 未許可なら承認コンソールに 1 回だけ諮り、同じエコシステムの並行要求はその結果を
// 共有する。承認は run のメモリにだけ保持する。
func (b *BatchManager) AllowMetadata(ecosystem string) error {
	if b.AskBatch == nil {
		return nil
	}

	b.mu.Lock()
	if b.metadataAllowed[ecosystem] {
		b.mu.Unlock()
		return nil
	}
	if wait, ok := b.metadataPending[ecosystem]; ok {
		b.mu.Unlock()
		<-wait.done
		return wait.err
	}
	wait := &metadataWait{done: make(chan struct{})}
	b.metadataPending[ecosystem] = wait
	b.mu.Unlock()

	_, err := b.AskBatch(console.BatchDownloadInfo{
		Ecosystem: ecosystem,
		Metadata:  true,
	})

	b.mu.Lock()
	if err == nil {
		b.metadataAllowed[ecosystem] = true
	}
	wait.err = err
	delete(b.metadataPending, ecosystem)
	logf := b.Logf
	b.mu.Unlock()
	close(wait.done)

	if err == nil && logf != nil {
		logf(ecosystem + ": この run 中のメタデータ参照を許可した")
	}
	return err
}

// LogMetadata は上流へ問い合わせたパッケージ名を承認コンソールのログに残す。
// 名前は VM 由来なので制御文字と Bidi を無害化する。
func (b *BatchManager) LogMetadata(ecosystem, name string) {
	b.mu.Lock()
	logf := b.Logf
	b.mu.Unlock()
	if logf == nil {
		return
	}
	logf(ecosystem + ": メタデータ参照: " + console.Sanitize(truncateRunes(name, 200)))
}

// truncateRunes はログを埋め尽くさないように文字数で切る。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// IsApproved は指定されたパッケージが承認済みか確認する。この run 中まとめて
// 許可されたエコシステムなら、パッケージ名を問わず承認済みとして扱う。
func (b *BatchManager) IsApproved(ecosystem, name string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessionAllowed[ecosystem] || b.approvedItems[normalizeItemKey(ecosystem, name)]
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

	// 0. この run 中まとめて許可されたエコシステムなら確認しない。
	if b.sessionAllowed[ecosystem] {
		b.mu.Unlock()
		return nil
	}

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
	// 順番待ちのあいだにまとめて許可された、あるいは別の経路で承認された項目は
	// もう諮らない。承認直後に古いキューがダイアログを出し直すのを防ぐ。
	b.mu.Lock()
	if b.sessionAllowed[q.ecosystem] {
		b.mu.Unlock()
		for _, ch := range q.waiters {
			ch <- nil
		}
		return
	}
	items := q.items[:0]
	for _, it := range q.items {
		if !b.approvedItems[normalizeItemKey(q.ecosystem, it.Name)] {
			items = append(items, it)
		}
	}
	q.items = items
	if len(q.items) == 0 {
		b.mu.Unlock()
		for _, ch := range q.waiters {
			ch <- nil
		}
		return
	}
	b.mu.Unlock()

	kind, err := b.AskBatch(console.BatchDownloadInfo{
		Ecosystem: q.ecosystem,
		Items:     q.items,
	})

	b.mu.Lock()
	if err == nil {
		for _, item := range q.items {
			b.approvedItems[normalizeItemKey(q.ecosystem, item.Name)] = true
		}
		if kind == access.Session {
			b.sessionAllowed[q.ecosystem] = true
		}
	}
	logf := b.Logf
	b.mu.Unlock()

	if err == nil && kind == access.Session && logf != nil {
		logf(q.ecosystem + ": この run 中のパッケージ取得をまとめて承認した")
	}

	for _, ch := range q.waiters {
		ch <- err
	}
}
