// language: Go 1.23, file: internal/node/restart_test.go
// 重启不丢账的回归测试。
// 之前的真实故障：进程重启时 NewNicMeter 把口径初始化成 both，
// 面板下发 tx，SetIface 当成「用户改口径」把 acc 清零——每重启一次丢一次历史。
package node

import (
	"os"
	"path/filepath"
	"testing"
)

func newMeterWithState(t *testing.T, body string) (*NicMeter, string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "nic.state")
	if body != "" {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return NewNicMeter("ens5", "", dir), p
}

// 重启后口径一致：acc 必须原样保留
func TestRestartKeepsAccWhenModeMatches(t *testing.T) {
	m, _ := newMeterWithState(t, "5368709120\ntx\n") // 5 GiB，口径 tx
	m.Restore()

	if got := m.Acc(); got != 5368709120 {
		t.Fatalf("恢复后 acc 应为 5 GiB，实际 %d", got)
	}
	if got := m.Mode(); got != "tx" {
		t.Fatalf("恢复后口径应为 tx，实际 %q", got)
	}

	// 面板心跳下发同样的口径：不能清零
	m.SetIface("ens5", "tx")
	if got := m.Acc(); got != 5368709120 {
		t.Fatalf("重启后下发同口径不该清零，实际 acc=%d", got)
	}
}

// 老版本 state 文件只有一行数字，没记口径：补口径时也不能清 acc
func TestRestartKeepsAccWithLegacyStateFile(t *testing.T) {
	m, _ := newMeterWithState(t, "4294967296\n") // 4 GiB，旧格式
	m.Restore()

	if got := m.Acc(); got != 4294967296 {
		t.Fatalf("恢复后 acc 应为 4 GiB，实际 %d", got)
	}
	if got := m.Mode(); got != "" {
		t.Fatalf("旧格式文件口径应未知，实际 %q", got)
	}

	m.SetIface("ens5", "tx") // 第一次拿到口径
	if got := m.Acc(); got != 4294967296 {
		t.Fatalf("补口径不该清零，实际 acc=%d", got)
	}
	if got := m.Mode(); got != "tx" {
		t.Fatalf("口径没补上，实际 %q", got)
	}
}

// 真的改口径（运行中从 tx 换成 both）：必须清零，否则用量会翻倍
func TestModeChangeStillResets(t *testing.T) {
	m, _ := newMeterWithState(t, "5368709120\ntx\n")
	m.Restore()
	m.SetIface("ens5", "tx")
	if got := m.Acc(); got != 5368709120 {
		t.Fatalf("前置：acc 应保留")
	}

	m.SetIface("ens5", "both") // 用户真的改了口径
	if got := m.Acc(); got != 0 {
		t.Fatalf("改口径必须清零，实际 acc=%d", got)
	}
}

// 落盘再读回：口径要跟着一起存
func TestPersistWritesMode(t *testing.T) {
	m, p := newMeterWithState(t, "")
	m.SetIface("ens5", "rx")

	m.mu.Lock()
	m.acc = 123456789
	m.mu.Unlock()

	if err := m.Persist(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("state 文件内容: %q", string(b))

	m2, _ := newMeterWithState(t, string(b))
	m2.Restore()
	if got := m2.Acc(); got != 123456789 {
		t.Fatalf("acc 没恢复: %d", got)
	}
	if got := m2.Mode(); got != "rx" {
		t.Fatalf("口径没恢复: %q", got)
	}
}
