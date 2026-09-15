// occupancy.go 守护进程的实例登记簿：出身、持有人数与回收判定。
//
// 判定规则（InstanceOrigin 三种出身的唯一差异点）：
//
//	autostart / explicit  永不因占用清零而停
//	demand                holders==0 → 应当优雅停止
package daemon

import "sync"

type session struct {
	Group   string
	Origin  InstanceOrigin
	Holders int
}

// Table 并发安全的占用表；键为实例名。
type Table struct {
	mu   sync.Mutex
	sess map[string]*session
}

func NewTable() *Table { return &Table{sess: map[string]*session{}} }

// Ensure 登记并返回会话（存在即复用）；origin 仅在首次创建时生效。
func (t *Table) Ensure(name, group string, o InstanceOrigin) *session {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.sess[name]; ok {
		return s
	}
	s := &session{Group: group, Origin: o}
	t.sess[name] = s
	return s
}

func (t *Table) Get(name string) (*session, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.sess[name]
	return s, ok
}

func (t *Table) Hold(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.sess[name]; ok {
		s.Holders++
	}
}

// Release 归还一个持有者；返回会话是否仍存在。
func (t *Table) Release(name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.sess[name]; ok && s.Holders > 0 {
		s.Holders--
	}
	s, ok := t.sess[name]
	return ok && s != nil && s.Holders > 0
}

// Drop 从表中移除实例。
func (t *Table) Drop(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.sess, name)
}

// ShouldAutoStop 占用清零时的回收判定：仅隐式按需实例返回 true。
func (s *session) ShouldAutoStop() bool { return s.Origin == OriginDemand && s.Holders == 0 }

// Snapshot 全量快照（ps 用）。
func (t *Table) Snapshot() []session {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]session, 0, len(t.sess))
	for _, s := range t.sess {
		out = append(out, *s)
	}
	return out
}
