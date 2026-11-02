package pr

// PullRequest は PR の同一性と表示に使う情報。
type PullRequest struct {
	URL    string
	Number int
	Title  string
	Body   string
}

// Forge は PR の検索・作成・更新を行う。
type Forge interface {
	// OpenPR は branch に開いている PR を返す。無ければ ok=false。
	OpenPR(branch string) (pr PullRequest, ok bool, err error)
	// CreatePR は PR を作成して返す。
	CreatePR(branch, base, title, body string) (PullRequest, error)
	// EditPR は既存 PR のタイトル・本文を更新する。
	EditPR(pr PullRequest, title, body string) error
}
