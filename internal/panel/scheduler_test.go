// language: Go 1.23, file: internal/panel/scheduler_test.go
package panel

import (
	"testing"
	"time"
)

func mkNode(id int64, order int, status string, backup bool, lastSeen int64) Node {
	return Node{
		ID: id, Name: "n" + itoa(int(id)), SortOrder: order,
		Status: status, IsBackup: backup, LastSeen: lastSeen,
		QuotaBytes: 100, IP: "10.0.0.1",
	}
}

// isDead：只有心跳超时才可能算死
func TestIsDead(t *testing.T) {
	s := &Scheduler{}
	g := &Group{ProbePort: 0} // 不探活，纯看心跳
	now := time.Now().Unix()

	cases := []struct {
		name     string
		lastSeen int64
		want     bool
	}{
		{"从没连过", 0, false},
		{"刚刚心跳", now, false},
		{"心跳过期前", now - heartbeatTimeout + 5, false},
		{"心跳刚超时", now - heartbeatTimeout - 1, true},
		{"很久没心跳", now - 3600, true},
	}
	for _, c := range cases {
		n := mkNode(1, 0, StatusOnline, false, c.lastSeen)
		if got := s.isDead(&n, g); got != c.want {
			t.Errorf("%s: isDead=%v, 期望 %v", c.name, got, c.want)
		}
	}
}

// candidates：顺序 = 后面的 → 前面的 → 备用
func TestCandidatesOrder(t *testing.T) {
	now := time.Now().Unix()
	nodes := []Node{
		mkNode(1, 10, StatusOnline, false, now),    // 当前
		mkNode(2, 20, StatusStandby, false, now),   // 后面
		mkNode(3, 30, StatusStandby, false, now),   // 后面
		mkNode(4, 5, StatusStandby, false, now),    // 前面
		mkNode(5, 99, StatusStandby, true, now),    // 备用
		mkNode(6, 40, StatusExhausted, false, now), // 用尽，排除
		mkNode(7, 50, StatusDisabled, false, now),  // 停用，排除
	}
	cur := &Node{ID: 1, SortOrder: 10}
	got := candidates(nodes, cur)

	var ids []int64
	for _, n := range got {
		ids = append(ids, n.ID)
	}
	want := []int64{2, 3, 4, 5} // 后面升序、前面的、最后备用
	if len(ids) != len(want) {
		t.Fatalf("候选数量 %d，期望 %d（%v）", len(ids), len(want), ids)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("候选顺序 %v，期望 %v", ids, want)
		}
	}
}

// 备用不能排在主用前面
func TestBackupLast(t *testing.T) {
	now := time.Now().Unix()
	nodes := []Node{
		mkNode(1, 10, StatusOnline, false, now),
		mkNode(2, 5, StatusStandby, true, now),  // 备用但顺序更小
		mkNode(3, 20, StatusStandby, false, now),
	}
	cur := &Node{ID: 1, SortOrder: 10}
	got := candidates(nodes, cur)
	if len(got) < 2 || got[0].ID != 3 {
		t.Fatalf("主用节点应排在备用前，实际顺序 %v", idsOf(got))
	}
	if got[len(got)-1].ID != 2 {
		t.Fatalf("备用节点应排最后，实际 %v", idsOf(got))
	}
}

func idsOf(ns []Node) []int64 {
	var ids []int64
	for _, n := range ns {
		ids = append(ids, n.ID)
	}
	return ids
}

// 全部耗尽时，候选里只剩备用
func TestOnlyBackupLeft(t *testing.T) {
	now := time.Now().Unix()
	nodes := []Node{
		mkNode(1, 10, StatusOnline, false, now),
		mkNode(2, 20, StatusExhausted, false, now),
		mkNode(3, 30, StatusStandby, true, now),
	}
	cur := &Node{ID: 1, SortOrder: 10}
	got := candidates(nodes, cur)
	if len(got) != 1 || got[0].ID != 3 {
		t.Fatalf("应该只剩备用节点 3，实际 %v", idsOf(got))
	}
}

// switchAt：留够余量，但不低于 50%
func TestSwitchAt(t *testing.T) {
	g := &Group{DNSTTL: 60, PeakBps: 25_000_000} // 200Mbps
	// 200G 配额：5% = 10G，峰值×600s = 15G，取 15G → 阈值 92.5%
	n := &Node{QuotaBytes: 200 * 1024 * 1024 * 1024}
	got := switchAt(n, g)
	if got < 0.90 || got > 0.94 {
		t.Fatalf("200G 阈值应在 92%% 附近，实际 %.3f", got)
	}
	// 小配额时兜底到 50%
	small := &Node{QuotaBytes: 1024 * 1024}
	if v := switchAt(small, g); v != 0.5 {
		t.Fatalf("极小配额应兜底 0.5，实际 %.3f", v)
	}
	// 不限量永不触发
	if v := switchAt(&Node{QuotaBytes: 0}, g); v != 1.0 {
		t.Fatalf("不限量应返回 1.0，实际 %.3f", v)
	}
}
