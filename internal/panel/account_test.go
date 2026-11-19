// language: Go 1.23, file: internal/panel/account_test.go
// 用量账本的回归测试。
// 前几轮「流量涨几 GB」都是靠人肉看面板发现的，这次把每种计数器状态写死成用例。
package panel

import (
	"path/filepath"
	"testing"
	"time"
)

const g1 = int64(1024 * 1024 * 1024) // 1 GiB

func newTestStore(t *testing.T) (*Store, int64) {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "data.json"))
	if err != nil {
		t.Fatalf("打开存储: %v", err)
	}
	n, err := s.CreateNode(Node{GroupID: 1, Name: "n1", Token: "tok", Iface: "ens5", BillMode: "tx", QuotaBytes: 200 * g1})
	if err != nil {
		t.Fatalf("建节点: %v", err)
	}
	return s, n.ID
}

func beat(s *Store, id, nic int64) Node {
	s.AcceptBeat(id, nic, 0, 1, "test-stamp")
	return *s.Node(id)
}

// 第一次心跳只对齐周期起点，不能把网卡开机以来的流量算成本周期用量。
// 老实现就是在这里把 50 GB 一次性加进去的，表现为「每秒涨几 GB」。
func TestFirstBeatAlignsInsteadOfAdding(t *testing.T) {
	s, id := newTestStore(t)

	got := beat(s, id, 50*g1)
	if got.UsedBytes != 0 {
		t.Fatalf("首跳用量应为 0，实际 %d", got.UsedBytes)
	}
	if got.CycleStartNic != 50*g1 || got.NicReported != 50*g1 {
		t.Fatalf("首跳应对齐起点，实际 start=%d nic=%d", got.CycleStartNic, got.NicReported)
	}
	if got.AgentStamp != "test-stamp" {
		t.Fatalf("应记录节点端构建标识，实际 %q", got.AgentStamp)
	}

	// 之后的增量才计入
	got = beat(s, id, 50*g1+100*1024*1024)
	if got.UsedBytes != 100*1024*1024 {
		t.Fatalf("第二次心跳用量应为 100 MiB，实际 %d", got.UsedBytes)
	}
}

// 清零后从当前累计值重新起算，不会因为 NicReported 还留着大值而立刻涨回去。
func TestResetThenGrow(t *testing.T) {
	s, id := newTestStore(t)

	beat(s, id, 10*g1)
	beat(s, id, 11*g1)
	if got := *s.Node(id); got.UsedBytes != g1 {
		t.Fatalf("清零前应为 1 GiB，实际 %d", got.UsedBytes)
	}

	s.ResetNodeCycle(id)
	if got := *s.Node(id); got.UsedBytes != 0 {
		t.Fatalf("清零后应为 0，实际 %d", got.UsedBytes)
	}

	got := beat(s, id, 11*g1+5*1024*1024)
	if got.UsedBytes != 5*1024*1024 {
		t.Fatalf("清零后应只算新增量 5 MiB，实际 %d", got.UsedBytes)
	}
}

// 旧版本写下的数据（增量账本）升级：用量数字不能跳。
func TestMigrateOldLedgerKeepsNumber(t *testing.T) {
	s, id := newTestStore(t)

	// 模拟旧版本留下的状态：已上报 8 GiB，已用 3 GiB，还没迁移过
	s.mu.Lock()
	for i := range s.db.Nodes {
		s.db.Nodes[i].NicReported = 8 * g1
		s.db.Nodes[i].UsedBytes = 3 * g1
		s.db.Nodes[i].CycleStartNic = 0
		s.db.Nodes[i].AccountMigrated = false
		s.db.Nodes[i].LastSeen = time.Now().Unix() - 30
	}
	s.mu.Unlock()

	got := beat(s, id, 8*g1)
	if got.UsedBytes != 3*g1 {
		t.Fatalf("升级瞬间用量应保持 3 GiB，实际 %d", got.UsedBytes)
	}
	if got.CycleStartNic != 5*g1 {
		t.Fatalf("周期起点应反推为 5 GiB，实际 %d", got.CycleStartNic)
	}

	got = beat(s, id, 8*g1+200*1024*1024)
	if got.UsedBytes != 3*g1+200*1024*1024 {
		t.Fatalf("升级后应继续累加，实际 %d", got.UsedBytes)
	}
}

// 节点侧计数器归零（重装 / 删了 /var/lib/relay/nic.state）：不倒扣，不跳涨。
func TestCounterResetDoesNotGoNegative(t *testing.T) {
	s, id := newTestStore(t)

	beat(s, id, 4*g1)
	beat(s, id, 5*g1)
	if got := *s.Node(id); got.UsedBytes != g1 {
		t.Fatalf("前置：用量应为 1 GiB，实际 %d", got.UsedBytes)
	}

	got := beat(s, id, 0)
	if got.UsedBytes != 0 || got.CycleStartNic != 0 {
		t.Fatalf("计数器归零后应从 0 重算，实际 used=%d start=%d", got.UsedBytes, got.CycleStartNic)
	}

	got = beat(s, id, 64*1024*1024)
	if got.UsedBytes != 64*1024*1024 {
		t.Fatalf("归零后应只算新值 64 MiB，实际 %d", got.UsedBytes)
	}
}

// 换网卡 / 改口径：面板的账必须跟着归零，否则和服务器上的数字永远差一个定值。
func TestResetAccounting(t *testing.T) {
	s, id := newTestStore(t)

	beat(s, id, 2*g1)
	beat(s, id, 3*g1)
	s.ResetNodeAccounting(id)

	got := *s.Node(id)
	if got.UsedBytes != 0 || got.NicReported != 0 || got.CycleStartNic != 0 {
		t.Fatalf("账本应全清，实际 used=%d nic=%d start=%d", got.UsedBytes, got.NicReported, got.CycleStartNic)
	}

	// 节点端换口径后从 0 起算，下一次心跳不该被当成「从 3 GiB 掉到 0」
	got = beat(s, id, 10*1024*1024)
	if got.UsedBytes != 0 {
		t.Fatalf("归零后的第一次心跳应重新对齐，实际 %d", got.UsedBytes)
	}
	got = beat(s, id, 20*1024*1024)
	if got.UsedBytes != 10*1024*1024 {
		t.Fatalf("对齐后应算 10 MiB，实际 %d", got.UsedBytes)
	}
}

// 编辑节点不能把心跳刚写进去的账覆盖掉。
func TestUpdateNodeKeepsLedger(t *testing.T) {
	s, id := newTestStore(t)

	beat(s, id, g1)
	beat(s, id, 2*g1)

	old := *s.Node(id) // 面板编辑弹窗拿到的是这份
	stale := old
	stale.Name = "改名了"

	// 模拟编辑期间又来了一次心跳
	beat(s, id, 3*g1)

	if err := s.UpdateNode(stale); err != nil {
		t.Fatal(err)
	}
	got := *s.Node(id)
	if got.Name != "改名了" {
		t.Fatalf("名称没改上")
	}
	if got.UsedBytes != 2*g1 {
		t.Fatalf("编辑不应回退用量，实际 %d 期望 %d", got.UsedBytes, 2*g1)
	}
	if got.AgentStamp == "" {
		t.Fatalf("编辑不应清掉构建标识")
	}
}
