// language: Go 1.23, file: internal/panel/notify_test.go
// 告警推送与「可用节点只剩两台」的回归测试。
package panel

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 消息体要能一眼看出是哪台、什么级别
func TestFormatAlert(t *testing.T) {
	a := Alert{Level: "error", Message: "节点 香港 连不上上游 1.2.3.4:443", At: 1795000000}
	got := FormatAlert(a, "阿里云")
	for _, want := range []string{"【中转面板】错误", "分组：阿里云", "连不上上游"} {
		if !strings.Contains(got, want) {
			t.Fatalf("消息里缺少 %q，实际：%s", want, got)
		}
	}
	// 没有分组名（全组级告警）时不该冒出空行
	got = FormatAlert(a, "")
	if strings.Contains(got, "分组：\n") || strings.Contains(got, "分组：") {
		t.Fatalf("无分组时不该有分组行：%s", got)
	}
}

// 四项没填全就不推，免得半配置状态一直失败
func TestNotifierDisabledWithoutConfig(t *testing.T) {
	c := DefaultConfig()
	n := NewNotifier(c)
	if n.Enabled() {
		t.Fatal("默认配置不该是启用状态")
	}
	n.Push("hello")
	if len(n.ch) != 0 {
		t.Fatalf("没配置时不该入队，实际队列 %d", len(n.ch))
	}
	if err := n.Send("hello"); err == nil {
		t.Fatal("没配置时 Send 应该报错")
	}

	// 只填三项，仍然不算配全
	c.WecomCorpID = "ww123"
	c.WecomSecret = "sec"
	c.WecomAgentID = "1000002"
	if n.Enabled() {
		t.Fatal("接收者没填不该启用")
	}
	c.WecomToUser = "1"
	if !n.Enabled() {
		t.Fatal("四项齐了应该启用")
	}
}

func lowNodesFixture(t *testing.T) (*Store, *Group, int64) {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "data.json"))
	if err != nil {
		t.Fatal(err)
	}
	g, err := s.CreateGroup(Group{Name: "阿里云", DNSName: "t.example.com", DNSTTL: 60})
	if err != nil {
		t.Fatal(err)
	}
	// 一台能用（有心跳、不是当前以外的状态）
	ok, err := s.CreateNode(Node{GroupID: g.ID, Name: "ok", IP: "1.1.1.1", Token: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	// 三台已用尽
	for i := 0; i < 3; i++ {
		n, err := s.CreateNode(Node{GroupID: g.ID, Name: "dead", IP: "2.2.2." + string(rune('1'+i)), Token: "t" + string(rune('2'+i))})
		if err != nil {
			t.Fatal(err)
		}
		s.SetStatus(n.ID, StatusExhausted)
	}
	// 都打上新鲜心跳
	now := time.Now().Unix()
	s.mu.Lock()
	for i := range s.db.Nodes {
		s.db.Nodes[i].LastSeen = now
		if s.db.Nodes[i].Status == StatusPending {
			s.db.Nodes[i].Status = StatusStandby
		}
	}
	s.mu.Unlock()
	s.SetCurrentNode(g.ID, ok.ID)
	return s, s.Group(g.ID), ok.ID
}

func alertCount(s *Store, sub string) int {
	n := 0
	for _, a := range s.Alerts(true) {
		if strings.Contains(a.Message, sub) {
			n++
		}
	}
	return n
}

// 可用节点掉到 2 台及以下要告警，而且只报一次
func TestLowNodesAlertOnce(t *testing.T) {
	s, g, _ := lowNodesFixture(t)
	sched := NewScheduler(s, nil, DefaultConfig()) // 不会走到 DNS，dns 可以是 nil

	sched.tickGroup(g)
	if n := alertCount(s, "可用节点只剩"); n != 1 {
		t.Fatalf("应该报一次「可用节点只剩」，实际 %d 条", n)
	}
	if !s.Group(g.ID).LowNodes {
		t.Fatal("标记没置上，下一轮会重复告警")
	}

	// 再巡检一次不该重复报
	sched.tickGroup(g)
	if n := alertCount(s, "可用节点只剩"); n != 1 {
		t.Fatalf("不该重复告警，实际 %d 条", n)
	}
}

// 节点恢复以后标记要复位，下次再掉下来还能再报
func TestLowNodesAlertResets(t *testing.T) {
	s, g, _ := lowNodesFixture(t)
	sched := NewScheduler(s, nil, DefaultConfig())
	// 每一轮都要重新取分组：tickGroup 用的是传进去的那份快照，
	// 拿旧副本去判断 LowNodes 永远是 false，复位就测不出来。
	sched.tickGroup(s.Group(g.ID))
	if !s.Group(g.ID).LowNodes {
		t.Fatal("前置：应该已经置位")
	}

	// 把两台用尽的拉回可用
	back := 0
	for _, n := range s.Nodes(g.ID) {
		if n.Status == StatusExhausted && back < 2 {
			s.SetStatus(n.ID, StatusStandby)
			back++
		}
	}
	sched.tickGroup(s.Group(g.ID))
	if s.Group(g.ID).LowNodes {
		t.Fatal("可用数回到 3 台后应该复位")
	}

	// 再掉回 2 台，应该重新报
	for _, n := range s.Nodes(g.ID) {
		if n.Status == StatusStandby && n.ID != s.Group(g.ID).CurrentNode {
			s.SetStatus(n.ID, StatusExhausted)
			break
		}
	}
	sched.tickGroup(s.Group(g.ID))
	if n := alertCount(s, "可用节点只剩"); n < 2 {
		t.Fatalf("复位后应能再报一次，实际 %d 条", n)
	}
}

// 告警要能触发回调（企业微信就挂在这里）
func TestAlertHookFires(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "data.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got []Alert
	s.SetAlertHook(func(a Alert) { got = append(got, a) })

	s.Alert(1, 2, "warn", "测试告警")
	if len(got) != 1 || got[0].Message != "测试告警" {
		t.Fatalf("回调没收到告警: %+v", got)
	}
	// 去重窗口内同一条不该再触发
	s.Alert(1, 2, "warn", "测试告警")
	if len(got) != 1 {
		t.Fatalf("重复告警不该再回调，实际 %d 次", len(got))
	}
}

// 快通道上报的规则速率要能存进节点，且不进 data.json
func TestRuleStatsStoredNotPersisted(t *testing.T) {
	s, id := newTestStore(t)
	s.UpdateMetrics(id, Metrics{
		RxBps: 100, TxBps: 200,
		Rules: []RuleStat{
			{Port: 25210, Target: "1.2.3.4:443", RxBps: 11, TxBps: 22, Conns: 3},
		},
	})
	n := s.Node(id)
	if len(n.RuleStats) != 1 || n.RuleStats[0].TxBps != 22 {
		t.Fatalf("规则速率没存进去: %+v", n.RuleStats)
	}
}
