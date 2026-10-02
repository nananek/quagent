package access

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nananek/quagent/internal/netns"
)

type fakeApplier struct{ last []netns.Grant }

func (f *fakeApplier) SetGrants(gs []netns.Grant) error { f.last = gs; return nil }

func (f *fakeApplier) has(pattern string) (netns.Grant, bool) {
	for _, g := range f.last {
		if g.Pattern == pattern {
			return g, true
		}
	}
	return netns.Grant{}, false
}

func newTestManager(t *testing.T) (*Manager, *fakeApplier) {
	t.Helper()
	f := &fakeApplier{}
	m, err := NewManager(f, filepath.Join(t.TempDir(), "always.json"))
	if err != nil {
		t.Fatal(err)
	}
	return m, f
}

func TestNormalizeDomains(t *testing.T) {
	got, err := NormalizeDomains([]string{"PyPI.org.", "files.pythonhosted.org", "pypi.org", "*.githubusercontent.com"})
	if err != nil {
		t.Fatal(err)
	}
	want := "*.githubusercontent.com,files.pythonhosted.org,pypi.org"
	if strings.Join(got, ",") != want {
		t.Fatalf("got %v", got)
	}
	for _, bad := range []string{"", "localhost", "1.2.3.4", "*", "a..b", "exa mple.com", "*.*.com", "-a.com"} {
		if _, err := NormalizeDomains([]string{bad}); err == nil {
			t.Errorf("%q が通ってしまう", bad)
		}
	}
}

func TestOnceApproval(t *testing.T) {
	m, f := newTestManager(t)
	r, err := m.Submit([]string{"pypi.org", "files.pythonhosted.org"}, "依存の取得")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Pending()) != 1 {
		t.Fatal("承認待ちに積まれていない")
	}
	res, _ := m.Wait(context.Background(), r.ID, 10*time.Millisecond)
	if res.Status != Pending {
		t.Fatalf("決まる前に %s になった", res.Status)
	}
	if err := m.Decide(r.ID, Decision{Status: Approved, Kind: Once}); err != nil {
		t.Fatal(err)
	}
	res, _ = m.Wait(context.Background(), r.ID, time.Second)
	if res.Status != Approved || res.ExpiresAt == "" {
		t.Fatalf("unexpected %+v", res)
	}
	g, ok := f.has("pypi.org")
	if !ok || g.Expires == 0 {
		t.Fatalf("時限付きで反映されていない: %+v", f.last)
	}
	if len(m.Pending()) != 0 {
		t.Fatal("決着後も承認待ちに残っている")
	}
	// Once は次の申請で再確認される
	r2, _ := m.Submit([]string{"pypi.org"}, "再度")
	if len(m.Pending()) != 1 {
		t.Fatalf("Once 承認後の再申請が確認なしで通った: %+v", r2)
	}
}

func TestSessionApprovalSkipsLaterRequests(t *testing.T) {
	m, f := newTestManager(t)
	r, _ := m.Submit([]string{"registry.npmjs.org"}, "npm install")
	_ = m.Decide(r.ID, Decision{Status: Approved, Kind: Session})
	if g, ok := f.has("registry.npmjs.org"); !ok || g.Expires != 0 {
		t.Fatal("期限なしで反映されていない")
	}
	if _, err := m.Release([]string{"registry.npmjs.org"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.has("registry.npmjs.org"); ok {
		t.Fatal("放棄しても許可が残っている")
	}
	r2, _ := m.Submit([]string{"registry.npmjs.org"}, "もう一度")
	res, _ := m.Wait(context.Background(), r2.ID, time.Second)
	if res.Status != Approved || !res.Auto {
		t.Fatalf("セッション中の再申請が自動で通らない: %+v", res)
	}
	// 一部でも未確認のドメインが混じれば確認に回る
	_, _ = m.Submit([]string{"registry.npmjs.org", "example.com"}, "混在")
	if len(m.Pending()) != 1 {
		t.Fatal("未確認ドメイン混じりの申請が自動で通った")
	}
}

func TestAlwaysPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "always.json")
	m, _ := NewManager(&fakeApplier{}, path)
	r, _ := m.Submit([]string{"pypi.org"}, "pip")
	if err := m.Decide(r.ID, Decision{Status: Approved, Kind: Always}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("保存されていない")
	}
	m2, err := NewManager(&fakeApplier{}, path)
	if err != nil {
		t.Fatal(err)
	}
	r2, _ := m2.Submit([]string{"pypi.org"}, "別プロジェクト")
	res, _ := m2.Wait(context.Background(), r2.ID, time.Second)
	if res.Status != Approved || !res.Auto {
		t.Fatalf("以後確認しないが効いていない: %+v", res)
	}
}

func TestDeniedAndQuestion(t *testing.T) {
	m, f := newTestManager(t)
	r, _ := m.Submit([]string{"example.com"}, "x")
	_ = m.Decide(r.ID, Decision{Status: Denied})
	res, _ := m.Wait(context.Background(), r.ID, time.Second)
	if res.Status != Denied || len(f.last) != 0 {
		t.Fatalf("%+v %+v", res, f.last)
	}
	r2, _ := m.Submit([]string{"example.org"}, "y")
	_ = m.Decide(r2.ID, Decision{Status: Question, Question: "何に使う?"})
	res, _ = m.Wait(context.Background(), r2.ID, time.Second)
	if res.Status != Question || res.Question != "何に使う?" {
		t.Fatalf("%+v", res)
	}
}

func TestTimeout(t *testing.T) {
	m, f := newTestManager(t)
	r, _ := m.Submit([]string{"example.com"}, "x")
	r.Created = time.Now().Add(-DecisionTimeout - time.Second)
	res, _ := m.Wait(context.Background(), r.ID, time.Second)
	if res.Status != TimedOut {
		t.Fatalf("時間切れにならない: %+v", res)
	}
	if len(m.Pending()) != 0 {
		t.Fatal("時間切れ後も承認待ちに残っている")
	}
	// 時間切れ後の承認は効かない
	if err := m.Decide(r.ID, Decision{Status: Approved, Kind: Once}); err == nil {
		t.Fatal("時間切れ後の承認がエラーにならない")
	}
	res, _ = m.Wait(context.Background(), r.ID, time.Second)
	if res.Status != TimedOut {
		t.Fatalf("時間切れ後に結果が変わった: %+v", res)
	}
	if _, ok := f.has("example.com"); ok {
		t.Fatal("時間切れ後の承認で許可が入った")
	}
}

func TestRemoveAlways(t *testing.T) {
	path := filepath.Join(t.TempDir(), "always.json")
	m, _ := NewManager(&fakeApplier{}, path)
	r, _ := m.Submit([]string{"pypi.org", "registry.npmjs.org"}, "x")
	if err := m.Decide(r.ID, Decision{Status: Approved, Kind: Always}); err != nil {
		t.Fatal(err)
	}
	removed, err := RemoveAlways(path, []string{"pypi.org", "nothere.example"})
	if err != nil || len(removed) != 1 || removed[0] != "pypi.org" {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
	list, _ := LoadAlways(path)
	if len(list) != 1 || list[0] != "registry.npmjs.org" {
		t.Fatalf("残りが違う: %v", list)
	}
	// 取り消したものは次の Manager で再び確認に回る
	m2, _ := NewManager(&fakeApplier{}, path)
	_, _ = m2.Submit([]string{"pypi.org"}, "y")
	if len(m2.Pending()) != 1 {
		t.Fatal("取り消したドメインが確認なしで通った")
	}
}

func TestOnlyOnePending(t *testing.T) {
	m, _ := newTestManager(t)
	r, err := m.Submit([]string{"a.example"}, "x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Submit([]string{"b.example"}, "y"); err == nil {
		t.Fatal("承認待ちがあるのに次の申請が通った")
	}
	if len(m.Pending()) != 1 {
		t.Fatal("承認待ちが増えた")
	}
	_ = m.Decide(r.ID, Decision{Status: Question, Question: "?"})
	if _, err := m.Submit([]string{"b.example"}, "y"); err != nil {
		t.Fatalf("決着後の申請が通らない: %v", err)
	}
}

func TestDenyCooldown(t *testing.T) {
	m, _ := newTestManager(t)
	now := time.Now()
	m.now = func() time.Time { return now }
	r, _ := m.Submit([]string{"a.example", "b.example"}, "x")
	_ = m.Decide(r.ID, Decision{Status: Denied})
	if _, err := m.Submit([]string{"b.example"}, "言い換え"); err == nil {
		t.Fatal("拒否直後の再申請が通った")
	}
	if _, err := m.Submit([]string{"c.example"}, "別件"); err != nil {
		t.Fatalf("無関係なドメインまで止まった: %v", err)
	}
	r3 := m.Pending()[0]
	_ = m.Decide(r3.ID, Decision{Status: Denied})
	now = now.Add(DenyCooldown + time.Second)
	if _, err := m.Submit([]string{"b.example"}, "時間が経った"); err != nil {
		t.Fatalf("待ち時間の後も再申請できない: %v", err)
	}
}

func TestDeniedForgetsExpired(t *testing.T) {
	m, _ := newTestManager(t)
	now := time.Now()
	m.now = func() time.Time { return now }
	r, _ := m.Submit([]string{"a.example"}, "x")
	_ = m.Decide(r.ID, Decision{Status: Denied})
	now = now.Add(DenyCooldown + time.Second)
	r, _ = m.Submit([]string{"b.example"}, "y")
	_ = m.Decide(r.ID, Decision{Status: Denied})
	if _, ok := m.denied["a.example"]; ok || len(m.denied) != 1 {
		t.Fatalf("再申請できるようになった記録が残っている: %v", m.denied)
	}
}

func TestAlwaysFileIsPrivate(t *testing.T) {
	m, _ := newTestManager(t)
	r, _ := m.Submit([]string{"pypi.org"}, "x")
	if err := m.Decide(r.ID, Decision{Status: Approved, Kind: Always}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(m.alwaysPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("権限が %o", perm)
	}
}

func TestTimeoutAlsoCoolsDown(t *testing.T) {
	m, _ := newTestManager(t)
	r, _ := m.Submit([]string{"a.example"}, "x")
	r.Created = time.Now().Add(-DecisionTimeout - time.Second)
	_, _ = m.Wait(context.Background(), r.ID, time.Second)
	if _, err := m.Submit([]string{"a.example"}, "again"); err == nil {
		t.Fatal("時間切れ直後の再申請が通った")
	}
}

func TestLimits(t *testing.T) {
	m, _ := newTestManager(t)
	var many []string
	for i := range MaxDomains + 1 {
		many = append(many, fmt.Sprintf("d%d.example", i))
	}
	if _, err := m.Submit(many, "x"); err == nil {
		t.Fatal("ドメイン数の上限が効かない")
	}
	if _, err := m.Submit([]string{"a.example"}, strings.Repeat("あ", MaxReasonRunes+1)); err == nil {
		t.Fatal("理由の長さの上限が効かない")
	}
}

func TestSettledAreForgotten(t *testing.T) {
	m, _ := newTestManager(t)
	_ = m.Preallow([]string{"a.example"})
	first, _ := m.Submit([]string{"a.example"}, "x")
	for range keepSettled + 5 {
		if _, err := m.Submit([]string{"a.example"}, "x"); err != nil {
			t.Fatal(err)
		}
	}
	if len(m.byID) > keepSettled {
		t.Fatalf("決着済みの申請が溜まり続ける: %d", len(m.byID))
	}
	if _, ok := m.Settled(first.ID); ok {
		t.Fatal("古い申請を忘れていない")
	}
}
