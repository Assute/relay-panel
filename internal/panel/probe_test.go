// language: Go 1.23, file: internal/panel/probe_test.go
// 连通性自检的回归测试：三次不通 → 封节点 + 切走；人工恢复 → 重新可用。
package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func newProbeFixture(t *testing.T) (*Store, *httptest.Server, *Node) {
	t.Helper()
	dir := t.TempDir()
	cnf := DefaultConfig()
	cnf.DB = filepath.Join(dir, "data.json")
	cnf.Path = filepath.Join(dir, "config.json")
	cnf.AdminUser = "admin"
	cnf.AdminPassHash = HashPassword("pw")
	store, err := OpenStore(cnf.DB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateGroup(Group{Name: "g1", DNSName: "t.example.com", DNSTTL: 60}); err != nil {
		t.Fatal(err)
	}
	n, err := store.CreateNode(Node{GroupID: 1, Name: "n1", Iface: "ens5", Token: "tok-probe"})
	if err != nil {
		t.Fatal(err)
	}
	api := NewAPI(store, cnf, NewSessionStore(), NewManagedDNS(cnf), NewScheduler(store, NewManagedDNS(cnf), cnf))
	srv := httptest.NewServer(api.Routes())
	t.Cleanup(srv.Close)
	return store, srv, &n
}

func postProbeStats(t *testing.T, base, key string, fails int, target string) int {
	t.Helper()
	body := fmt.Sprintf(`{"key":%q,"fingerprint":"fp","iface":"ens5","probe_target":%q,"probe_ok":%t,"probe_done":true,"probe_fails":%d,"probe_at":1,"probe_rtt_ms":12,"probe_err":"dial timeout"}`,
		key, target, fails == 0, fails)
	resp, err := http.Post(base+"/agent/stats", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("stats 请求失败: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// 连续三次不通才封，前两次不能动
func TestProbeMarksUnreachableAfterThreeFails(t *testing.T) {
	store, srv, n := newProbeFixture(t)

	for _, fails := range []int{1, 2} {
		if code := postProbeStats(t, srv.URL, n.Token, fails, "1.2.3.4:8080"); code != 200 {
			t.Fatalf("HTTP %d", code)
		}
		if got := store.Node(n.ID); got.Unreachable {
			t.Fatalf("连续 %d 次不该封", fails)
		}
	}

	postProbeStats(t, srv.URL, n.Token, 3, "1.2.3.4:8080")
	got := *store.Node(n.ID)
	if !got.Unreachable {
		t.Fatal("连续三次不通应该标成待恢复")
	}
	if got.Status != StatusUnreachable {
		t.Fatalf("状态应为 %s，实际 %s", StatusUnreachable, got.Status)
	}
	if got.ProbeFails != 3 || got.ProbeTarget != "1.2.3.4:8080" {
		t.Fatalf("探测结果没落库: fails=%d target=%q", got.ProbeFails, got.ProbeTarget)
	}

	// 封了之后即使某次探通了也不自动解封（必须人工点恢复）
	postProbeStats(t, srv.URL, n.Token, 0, "1.2.3.4:8080")
	if got := store.Node(n.ID); !got.Unreachable {
		t.Fatal("探通不该自动解封，否则会在临界状态来回切")
	}
}

// 成功一次就清零连续失败次数：节点的失败计数归零，面板也不封
func TestProbeSuccessResetsFails(t *testing.T) {
	store, srv, n := newProbeFixture(t)

	postProbeStats(t, srv.URL, n.Token, 2, "1.2.3.4:8080")
	postProbeStats(t, srv.URL, n.Token, 0, "1.2.3.4:8080") // 第三次前探通
	got := *store.Node(n.ID)
	if got.ProbeFails != 0 || !got.ProbeOK || got.Unreachable {
		t.Fatalf("探通应清零并保持可用: fails=%d ok=%v unreachable=%v", got.ProbeFails, got.ProbeOK, got.Unreachable)
	}
}

// 人工恢复：解封、回到 standby、重新可选
func TestRecoverNode(t *testing.T) {
	store, srv, n := newProbeFixture(t)

	postProbeStats(t, srv.URL, n.Token, 3, "1.2.3.4:8080")
	if !store.Node(n.ID).Unreachable {
		t.Fatal("前置：应该已被封")
	}

	sid := loginSID(t, srv.URL, "admin", "pw")
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/nodes/%d/recover", srv.URL, n.ID), nil)
	req.AddCookie(&http.Cookie{Name: "sid", Value: sid})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("恢复接口 HTTP %d", resp.StatusCode)
	}

	got := *store.Node(n.ID)
	if got.Unreachable {
		t.Fatal("恢复后应解封")
	}
	if got.Status == StatusUnreachable {
		t.Fatalf("恢复后状态不该还是 %s", StatusUnreachable)
	}
	// usable 还要求有 IP（域名得有地方指），这个节点从没走过心跳，补一个
	store.SetNodeIP(n.ID, "1.2.3.4")
	if !usable(*store.Node(n.ID)) {
		t.Fatal("恢复后应该能重新当候选")
	}
	if got.ProbeFails != 0 {
		t.Fatalf("恢复后失败计数应清零，实际 %d", got.ProbeFails)
	}
}

// 封掉的节点不能当候选
func TestUnreachableNodeNotCandidate(t *testing.T) {
	now := 1
	ok := mkNode(1, 10, StatusOnline, false, int64(now))
	bad := mkNode(2, 20, StatusStandby, false, int64(now))
	bad.Status = StatusUnreachable
	bad.Unreachable = true
	if usable(bad) {
		t.Fatal("待恢复的节点不该被当成候选")
	}
	got := candidates([]Node{ok, bad}, &ok)
	if len(got) != 0 {
		t.Fatalf("候选里不该出现待恢复节点，实际 %v", idsOf(got))
	}
}

// 面板要按规则算出探测目标下发给节点
func TestProbeTargetFromRules(t *testing.T) {
	store, srv, n := newProbeFixture(t)
	if _, err := store.CreateRule(Rule{
		GroupID: 1, Name: "r1", ListenPort: 20001,
		TargetHost: "up.example.com", TargetPort: 443, Protocol: "tcp", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	body := `{"key":"tok-probe","fingerprint":"fp","iface":"ens5","nic_acc":0,"relay_acc":0,"version":0,"agent":"x"}`
	resp, err := http.Post(srv.URL+"/agent/beat", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		ProbeTarget string `json:"probe_target"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.ProbeTarget != "up.example.com:443" {
		t.Fatalf("心跳下发的探测目标应该是第一条启用线路的上游，实际 %q", out.ProbeTarget)
	}
	_ = n
}
