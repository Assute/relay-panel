// language: Go 1.23, file: internal/panel/install_test.go
// 远程安装的回归测试：密码记忆、参数校验、SSH 失败时不炸。
// 真 SSH 登录不在这里测（需要一台机器），测的是「连接不上时行为是否正确」。
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

func newInstallFixture(t *testing.T) (*Store, *httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	cnf := DefaultConfig()
	cnf.DB = filepath.Join(dir, "data.json")
	cnf.Path = filepath.Join(dir, "config.json")
	cnf.AdminUser = "admin"
	cnf.AdminPassHash = HashPassword("pw")
	cnf.PublicURL = "http://panel.example.test"
	store, err := OpenStore(cnf.DB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateGroup(Group{Name: "g1", DNSName: "t.example.com"}); err != nil {
		t.Fatal(err)
	}
	api := NewAPI(store, cnf, NewSessionStore(), NewManagedDNS(cnf), NewScheduler(store, NewManagedDNS(cnf), cnf))
	srv := httptest.NewServer(api.Routes())
	t.Cleanup(srv.Close)
	return store, srv, loginSID(t, srv.URL, "admin", "pw")
}

func postInstall(t *testing.T, base, sid, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", base+"/api/servers/install", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "sid", Value: sid})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// 通用密码：新机器留空时复用最近一次记住的那个
func TestInstallReusesUniversalPassword(t *testing.T) {
	store, srv, sid := newInstallFixture(t)
	// 先记住一台的密码
	if _, err := store.SaveCred(Cred{IP: "10.0.0.9", Port: 22, User: "root", Password: "shared-pw"}); err != nil {
		t.Fatal(err)
	}

	// 另一台从没见过的 IP，密码留空 —— 应该复用，而不是报「第一次要填密码」
	body := `{"group_id":1,"ip":"127.0.0.1","ssh_port":1,"user":"root","password":"","name":"new","quota_gb":200}`
	code, out := postInstall(t, srv.URL, sid, body)
	if code != 200 {
		t.Fatalf("应复用通用密码，实际 HTTP %d (%v)", code, out)
	}
	// SSH 会失败（端口 1 没人监听），但失败原因必须是连接被拒，不是「没密码」
	if msg, _ := out["error"].(string); strings.Contains(msg, "密码") {
		t.Fatalf("不该报缺密码，实际 %q", msg)
	}
	// 新 IP 的凭据也存下来了，密码跟着通用那个
	c := store.CredByIP("127.0.0.1")
	if c == nil || c.Password != "shared-pw" {
		t.Fatalf("新机器应记住通用密码: %+v", c)
	}
}

// 新节点的计费口径默认只算出向
func TestNewNodeDefaultsToTx(t *testing.T) {
	s, _ := newTestStore(t)
	n, err := s.CreateNode(Node{GroupID: 1, Name: "x2", IP: "1.2.3.4", Token: "t2"})
	if err != nil {
		t.Fatal(err)
	}
	if n.BillMode != "tx" {
		t.Fatalf("默认口径应为 tx，实际 %q", n.BillMode)
	}
}

// 面板里应该嵌着节点二进制（make assets 产出的）。
// 没嵌也能跑——会退回让目标机自己下载——所以这里只提醒，不算失败。
func TestEmbeddedNodeBinaries(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		b := nodeBinary(arch)
		if len(b) == 0 {
			t.Logf("%s: 面板里没有嵌入节点二进制（构建时没跑 make assets），远程安装会退回自下载", arch)
			continue
		}
		t.Logf("%s: 已嵌入 %d 字节", arch, len(b))
	}
	if b := nodeBinary("nonsense-arch"); b != nil {
		t.Fatalf("未知架构不该返回二进制，实际 %d 字节", len(b))
	}
	if b := nodeBinary(""); b != nil {
		t.Fatal("空架构不该返回二进制")
	}
}

// 密码按 IP 记住：第二次留空能取到，指纹也保留
func TestCredRememberPassword(t *testing.T) {
	s, _ := newTestStore(t)
	if _, err := s.SaveCred(Cred{Name: "a", IP: "1.2.3.4", Port: 22, User: "root", Password: "p1", HostKey: "SHA256:abc"}); err != nil {
		t.Fatal(err)
	}
	got := s.CredByIP("1.2.3.4")
	if got == nil || got.Password != "p1" || got.HostKey != "SHA256:abc" {
		t.Fatalf("凭据没存对: %+v", got)
	}

	// 再存一次，这次没带指纹（比如重装了，还没连过）——旧的指纹要留着做校验
	if _, err := s.SaveCred(Cred{Name: "a", IP: "1.2.3.4", Port: 22, User: "root", Password: "p2"}); err != nil {
		t.Fatal(err)
	}
	got = s.CredByIP("1.2.3.4")
	if got.Password != "p2" {
		t.Fatalf("密码应更新: %+v", got)
	}
	if got.HostKey != "SHA256:abc" {
		t.Fatalf("指纹不该被清掉: %+v", got)
	}
	if n := len(s.Creds()); n != 1 {
		t.Fatalf("同一 IP 不该重复插入，实际 %d 条", n)
	}
}

// 参数校验：该拦的都要拦在 SSH 之前
func TestInstallServerValidation(t *testing.T) {
	_, srv, sid := newInstallFixture(t)
	cases := []struct {
		name string
		body string
		want string
	}{
		{"缺 IP", `{"group_id":1,"ssh_port":22,"user":"root","password":"x"}`, "IP"},
		{"分组不存在", `{"group_id":99,"ip":"1.2.3.4","password":"x"}`, "分组"},
		{"第一次没密码", `{"group_id":1,"ip":"1.2.3.4","name":"a"}`, "密码"},
	}
	for _, c := range cases {
		code, out := postInstall(t, srv.URL, sid, c.body)
		if code != 400 {
			t.Fatalf("%s: 期望 400，实际 %d (%v)", c.name, code, out)
		}
		if msg, _ := out["error"].(string); !strings.Contains(msg, c.want) {
			t.Fatalf("%s: 错误信息应提到 %q，实际 %q", c.name, c.want, msg)
		}
	}
}

// PublicURL 是回环地址时不能装：节点根本拉不到安装包
func TestInstallRejectsLoopbackPublicURL(t *testing.T) {
	dir := t.TempDir()
	cnf := DefaultConfig()
	cnf.DB = filepath.Join(dir, "data.json")
	cnf.Path = filepath.Join(dir, "config.json")
	cnf.AdminUser = "admin"
	cnf.AdminPassHash = HashPassword("pw")
	cnf.PublicURL = "http://127.0.0.1:3070"
	store, err := OpenStore(cnf.DB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateGroup(Group{Name: "g1", DNSName: "t.example.com"}); err != nil {
		t.Fatal(err)
	}
	api := NewAPI(store, cnf, NewSessionStore(), NewManagedDNS(cnf), NewScheduler(store, NewManagedDNS(cnf), cnf))
	srv := httptest.NewServer(api.Routes())
	defer srv.Close()
	sid := loginSID(t, srv.URL, "admin", "pw")

	body := `{"group_id":1,"ip":"1.2.3.4","password":"x"}`
	code, out := postInstall(t, srv.URL, sid, body)
	if code != 400 {
		t.Fatalf("回环 PublicURL 应拒绝，实际 %d (%v)", code, out)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "Public URL") {
		t.Fatalf("错误信息应提到 Public URL，实际 %q", msg)
	}
}

// SSH 连不上：节点记录要建出来（方便手动重试），凭据要记住，接口不能 500
func TestInstallSSHFailureIsGraceful(t *testing.T) {
	store, srv, sid := newInstallFixture(t)
	// 127.0.0.1:1 没人监听，连上去立刻被拒
	body := fmt.Sprintf(`{"group_id":1,"ip":"127.0.0.1","ssh_port":1,"user":"root","password":"pw","name":"bad","quota_gb":200}`)
	code, out := postInstall(t, srv.URL, sid, body)
	if code != 200 {
		t.Fatalf("SSH 失败不该回 500，实际 %d (%v)", code, out)
	}
	if ok, _ := out["ok"].(bool); ok {
		t.Fatal("连不上还说成功")
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "失败") && !strings.Contains(msg, "refused") {
		t.Fatalf("应带回失败原因，实际 %q", msg)
	}

	// 节点记录留下了，状态是 pending，用户可以点「复制对接命令」手动装
	nodes := store.Nodes(1)
	if len(nodes) != 1 {
		t.Fatalf("应该建出一个节点记录，实际 %d 个", len(nodes))
	}
	if nodes[0].Status != StatusPending {
		t.Fatalf("节点状态应为 pending，实际 %s", nodes[0].Status)
	}
	if nodes[0].Token == "" {
		t.Fatal("节点应该有 token，手动安装要用")
	}

	// 凭据记住了：下次这台机器密码可以留空
	c := store.CredByIP("127.0.0.1")
	if c == nil || c.Password != "pw" {
		t.Fatalf("失败也要记住密码，方便重试: %+v", c)
	}
}

// 列表接口不能把密码吐给前端
func TestListServersHidesPassword(t *testing.T) {
	store, srv, sid := newInstallFixture(t)
	if _, err := store.SaveCred(Cred{IP: "1.2.3.4", Port: 22, User: "root", Password: "secret"}); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/api/servers", nil)
	req.AddCookie(&http.Cookie{Name: "sid", Value: sid})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) != 1 {
		t.Fatalf("应有 1 条，实际 %d", len(raw))
	}
	if _, leaked := raw[0]["password"]; leaked {
		t.Fatal("列表里不该出现密码字段")
	}
	if has, _ := raw[0]["has_password"].(bool); !has {
		t.Fatal("应该告诉前端「记住了一个密码」")
	}
}
