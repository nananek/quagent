package netns

import (
	"errors"
)

// inspectResult は親から返ってきた点検の判定。
type inspectResult struct {
	allow  bool
	reason string
}

// inspect は透明プロキシが終端した 1 リクエストを親へ送り、判定を待つ。親が内容
// ガード (ローカル LLM と承認コンソール) にかけて通すか止めるかを返す。親との
// 接続が切れたら止める側で戻る。
func (c *child) inspect(req InspectRequest) error {
	c.inspectMu.Lock()
	c.inspectNext++
	id := c.inspectNext
	ch := make(chan inspectResult, 1)
	c.inspectWait[id] = ch
	c.inspectMu.Unlock()

	c.events.send(Event{Inspect: &req, InspectID: id})
	r := <-ch
	if !r.allow {
		if r.reason == "" {
			r.reason = "コンテンツガードが止めた"
		}
		return errors.New(r.reason)
	}
	return nil
}

// deliverInspect は親からの返答を、待っている点検へ届ける。
func (c *child) deliverInspect(id int, allow bool, reason string) {
	c.inspectMu.Lock()
	ch := c.inspectWait[id]
	delete(c.inspectWait, id)
	c.inspectMu.Unlock()
	if ch != nil {
		ch <- inspectResult{allow: allow, reason: reason}
	}
}

// failInspects は親との接続が切れたときに、待っている点検をすべて止める側で起こす。
func (c *child) failInspects() {
	c.inspectMu.Lock()
	wait := c.inspectWait
	c.inspectWait = map[int]chan inspectResult{}
	c.inspectMu.Unlock()
	for _, ch := range wait {
		ch <- inspectResult{allow: false, reason: "親との接続が切れた"}
	}
}
