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

// 整条链路：首次心跳对齐 → 增量 → 清零 → 改口径 → 节点侧重装
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
