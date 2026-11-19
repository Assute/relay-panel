// language: Go 1.23, file: internal/panel/beat_e2e_test.go
// 走完整 HTTP 链路的心跳测试：节点上报 → 面板记账 → 接口返回值。
// 前面几轮「流量涨几 GB」都是改完直接推上去让人肉看，这次把序列写死成用例。
package panel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

type beatPayload struct {
	Key         string `json:"key"`
	Fingerprint string `json:"fingerprint"`
	Iface       string `json:"iface"`
	NicAcc      int64  `json:"nic_acc"`
	RelayAcc    int64  `json:"relay_acc"`
	Version     int64  `json:"version"`
	Agent       string `json:"agent"`
	RxBps       int64  `json:"rx_bps"`
	TxBps       int64  `json:"tx_bps"`
	CPUPct      int    `json:"cpu_pct"`
	MemPct      int    `json:"mem_pct"`
	DiskPct     int    `json:"disk_pct"`
}

func (p beatPayload) post(t *testing.T, url string) (sessionKey string) {
	t.Helper()
	body, _ := json.Marshal(p)
	resp, err := http.Post(url+"/agent/beat", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("心跳请求失败: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		OK         bool   `json:"ok"`
		Err        string `json:"err"`
		SessionKey string `json:"session_key"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("解析心跳响应: %v", err)
	}
	if !out.OK {
		t.Fatalf("面板拒绝心跳: %s", out.Err)
	}
	return out.SessionKey
}

func loginSID(t *testing.T, base, user, pw string) string {
	t.Helper()
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, user, pw)
	resp, err := http.Post(base+"/api/login", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("登录请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("登录失败 HTTP %d", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "sid" {
			return c.Value
		}
	}
	t.Fatal("登录没有拿到 sid cookie")
	return ""
}

// patchNodeHTTP 等价于在面板上编辑节点改「计费口径」。
func patchNodeHTTP(t *testing.T, base, sid string, id int64, payload string) {
	t.Helper()
	req, err := http.NewRequest("PATCH", fmt.Sprintf("%s/api/nodes/%d", base, id), strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "sid", Value: sid})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("改口径请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("改口径 HTTP %d", resp.StatusCode)
	}
}

// 实时指标快通道：2 秒一条轻请求，只刷展示字段。
// 关键点是不能让这条高频路径碰 token 兑换，否则节点会在收到 session_key
// 之前就把它换掉，之后所有请求全 401。
func TestStatsChannel(t *testing.T) {
	dir := t.TempDir()
	cnf := DefaultConfig()
	cnf.DB = filepath.Join(dir, "data.json")
	cnf.Path = filepath.Join(dir, "config.json")
	store, err := OpenStore(cnf.DB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateGroup(Group{Name: "g1", DNSName: "t.example.com", QuotaBytes: 200 * 1 << 30, DNSTTL: 60}); err != nil {
		t.Fatal(err)
	}
	n, err := store.CreateNode(Node{GroupID: 1, Name: "n1", Iface: "ens5", BillMode: "tx", Token: "tok-stats"})
	if err != nil {
		t.Fatal(err)
	}
	api := NewAPI(store, cnf, NewSessionStore(), NewManagedDNS(cnf), NewScheduler(store, NewManagedDNS(cnf), cnf))
	srv := httptest.NewServer(api.Routes())
	defer srv.Close()

	postStats := func(key string, rx, tx int64) int {
		body := fmt.Sprintf(`{"key":%q,"fingerprint":"fp","iface":"ens5","rx_bps":%d,"tx_bps":%d,"cpu_pct":12,"mem_pct":40,"disk_pct":55}`, key, rx, tx)
		resp, err := http.Post(srv.URL+"/agent/stats", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("stats 请求失败: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	// 还没走心跳：token 直接用于 stats 应该放行，且不能把 token 兑换掉
	if code := postStats("tok-stats", 13040, 6960); code != 200 {
		t.Fatalf("stats 应放行，HTTP %d", code)
	}
	if got := store.Node(n.ID); got.SessionKey != "" {
		t.Fatal("stats 不该触发 token 兑换，否则节点还没拿到 session_key 就被换掉了")
	}
	live := *store.Node(n.ID)
	if live.RxBps != 13040 || live.TxBps != 6960 || live.CPUPct != 12 || live.DiskPct != 55 {
		t.Fatalf("实时指标没写进去: %+v", live)
	}
	if live.FirstSeen == 0 || live.LastSeen == 0 {
		t.Fatal("stats 也应该建立首次心跳时间，否则面板上一片空白")
	}

	// 走一次心跳，token 兑换成 session_key
	p := beatPayload{Key: "tok-stats", Fingerprint: "fp", Iface: "ens5", NicAcc: 100, Version: 1, Agent: "stamp"}
	sk := p.post(t, srv.URL)
	if sk == "" {
		t.Fatal("心跳没换发 session_key")
	}

	// 旧 token 这时已经作废
	if code := postStats("tok-stats", 1, 1); code != 401 {
		t.Fatalf("作废的 token 应该 401，实际 %d", code)
	}
	if code := postStats(sk, 20480, 8192); code != 200 {
		t.Fatalf("session_key 应该放行，实际 %d", code)
	}
	if got := *store.Node(n.ID); got.RxBps != 20480 || got.TxBps != 8192 {
		t.Fatalf("速率没更新: rx=%d tx=%d", got.RxBps, got.TxBps)
	}
}

// 指纹不一致时，stats 也要拒绝
func TestStatsRejectsWrongFingerprint(t *testing.T) {
	dir := t.TempDir()
	cnf := DefaultConfig()
	cnf.DB = filepath.Join(dir, "data.json")
	cnf.Path = filepath.Join(dir, "config.json")
	store, err := OpenStore(cnf.DB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateGroup(Group{Name: "g1", DNSName: "t.example.com"}); err != nil {
		t.Fatal(err)
	}
	n, err := store.CreateNode(Node{GroupID: 1, Name: "n1", Token: "tok-fp", Fingerprint: "right"})
	if err != nil {
		t.Fatal(err)
	}
	api := NewAPI(store, cnf, NewSessionStore(), NewManagedDNS(cnf), NewScheduler(store, NewManagedDNS(cnf), cnf))
	srv := httptest.NewServer(api.Routes())
	defer srv.Close()

	body := `{"key":"tok-fp","fingerprint":"wrong","iface":"ens5","rx_bps":1,"tx_bps":1}`
	resp, err := http.Post(srv.URL+"/agent/stats", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("指纹不符应 401，实际 %d", resp.StatusCode)
	}
	if got := store.Node(n.ID).LastSeen; got != 0 {
		t.Fatal("被拒的请求不该更新节点状态")
	}
}
func TestBeatOverHTTP(t *testing.T) {
	const GiB = int64(1024 * 1024 * 1024)
	const MiB = int64(1024 * 1024)

	dir := t.TempDir()
	cnf := DefaultConfig()
	cnf.DB = filepath.Join(dir, "data.json")
	cnf.Path = filepath.Join(dir, "config.json")
	cnf.PublicURL = "http://example.test"
	cnf.AdminUser = "admin"
	cnf.AdminPassHash = HashPassword("testpw")
	store, err := OpenStore(cnf.DB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateGroup(Group{Name: "g1", DNSName: "t.example.com", QuotaBytes: 200 * GiB, DNSTTL: 60}); err != nil {
		t.Fatal(err)
	}
	n, err := store.CreateNode(Node{
		GroupID: 1, Name: "n1", Iface: "ens5", BillMode: "tx",
		QuotaBytes: 200 * GiB, Token: "tok-e2e",
	})
	if err != nil {
		t.Fatal(err)
	}

	api := NewAPI(store, cnf, NewSessionStore(), NewManagedDNS(cnf), NewScheduler(store, NewManagedDNS(cnf), cnf))
	srv := httptest.NewServer(api.Routes())
	defer srv.Close()

	key := n.Token
	beat := func(nic int64, tag string) {
		t.Helper()
		p := beatPayload{
			Key: key, Fingerprint: "fp-e2e", Iface: "ens5",
			NicAcc: nic, Version: 1, Agent: "20261119-203105",
			// 55.68Kbps 上行 / 104.32Kbps 下行（字节/秒），跟截图里一个量级
			TxBps: 6960, RxBps: 13040, CPUPct: 7, MemPct: 35, DiskPct: 58,
		}
		if sk := p.post(t, srv.URL); sk != "" {
			key = sk
		}
		got := *store.Node(n.ID)
		t.Logf("%-18s used=%-12d nic=%-12d start=%-12d", tag, got.UsedBytes, got.NicReported, got.CycleStartNic)
	}
	check := func(want int64, tag string) {
		t.Helper()
		if got := store.Node(n.ID).UsedBytes; got != want {
			t.Fatalf("%s: 用量 %d，期望 %d", tag, got, want)
		}
	}

	// 网卡开机以来已经跑了 5 GiB：这是历史，不是本周期用量
	beat(5*GiB, "首跳")
	check(0, "首跳")

	beat(5*GiB+100*MiB, "+100MiB")
	check(100*MiB, "+100MiB")

	beat(6*GiB, "+1GiB")
	check(1*GiB, "+1GiB")

	if got := store.Node(n.ID).AgentStamp; got != "20261119-203105" {
		t.Fatalf("面板应记下节点端构建标识，实际 %q", got)
	}
	// 实时观测值：速率/负载/首次心跳时间都要原样落库，前端才能显示
	live := *store.Node(n.ID)
	if live.TxBps != 6960 || live.RxBps != 13040 {
		t.Fatalf("速率没落库: tx=%d rx=%d", live.TxBps, live.RxBps)
	}
	if live.CPUPct != 7 || live.MemPct != 35 || live.DiskPct != 58 {
		t.Fatalf("负载没落库: cpu=%d mem=%d disk=%d", live.CPUPct, live.MemPct, live.DiskPct)
	}
	if live.FirstSeen == 0 {
		t.Fatal("首次心跳时间没记，面板上「在线 X 天」会一直是空的")
	}

	// 手动清零
	store.ResetNodeCycle(n.ID)
	check(0, "清零")

	beat(6*GiB+50*MiB, "清零后 +50MiB")
	check(50*MiB, "清零后 +50MiB")

	// 改计费口径：节点端换了口径，面板这侧必须重新对齐
	sid := loginSID(t, srv.URL, "admin", "testpw")
	patchNodeHTTP(t, srv.URL, sid, n.ID, `{"bill_mode":"both"}`)
	check(0, "改口径")

	beat(2*GiB, "改口径后首跳")
	check(0, "改口径后首跳")

	beat(2*GiB+30*MiB, "改口径后 +30MiB")
	check(30*MiB, "改口径后 +30MiB")

	// 节点重装：计数器从 0 开始
	beat(0, "重装首跳")
	check(0, "重装首跳")

	beat(12*MiB, "重装后 +12MiB")
	check(12*MiB, "重装后 +12MiB")
}
