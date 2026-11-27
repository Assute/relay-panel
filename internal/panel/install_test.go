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
	"strconv"
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

// 同一台机器重装：复用节点记录、换新 token，不报 IP 冲突
func TestInstallReusesExistingNode(t *testing.T) {
	store, srv, sid := newInstallFixture(t)
	// 先建一个节点占住这个 IP
	old, err := store.CreateNode(Node{GroupID: 1, Name: "old", IP: "127.0.0.1", Token: "tok-old"})
	if err != nil {
		t.Fatal(err)
	}
	oldTok := old.Token

	body := `{"group_id":1,"ip":"127.0.0.1","ssh_port":1,"user":"root","password":"x","name":"renamed","quota_gb":150}`
	code, out := postInstall(t, srv.URL, sid, body)
	if code != 200 {
		t.Fatalf("重装不该报 IP 冲突，实际 %d (%v)", code, out)
	}

	ns := store.Nodes(1)
	if len(ns) != 1 {
		t.Fatalf("不该新增节点，实际 %d 个", len(ns))
	}
	got := ns[0]
	if got.ID != old.ID {
		t.Fatalf("应该复用同一个节点记录，ID %d != %d", got.ID, old.ID)
	}
	if got.Token == oldTok {
		t.Fatal("重装必须换新 token，否则节点还拿旧凭证")
	}
	if strings.HasPrefix(got.Token, "used-") {
		t.Fatal("新 token 不该是已作废的样子")
	}
	if got.Name != "renamed" {
		t.Fatalf("名称应更新，实际 %q", got.Name)
	}
	if got.QuotaBytes != 150*1024*1024*1024 {
		t.Fatalf("配额应更新，实际 %d", got.QuotaBytes)
	}
	if got.Status != StatusPending {
		t.Fatalf("重装后应回到 pending，实际 %s", got.Status)
	}
}

// 更新脚本不能碰配置和账本，只换二进制
func TestUpdateScriptKeepsState(t *testing.T) {
	for _, want := range []string{
		"/usr/local/bin/relay-node",
		"/tmp/relay-node.bin",
		"systemctl stop relay-node",
		"systemctl start relay-node",
	} {
		if !strings.Contains(updateScript, want) {
			t.Fatalf("更新脚本缺少 %q", want)
		}
	}
	// 这两个一碰就等于把节点重置了：凭证没了会重新注册，账本没了流量归零
	for _, forbidden := range []string{"/var/lib/relay/key", "/var/lib/relay/nic.state", "/etc/relay/config.json"} {
		if strings.Contains(updateScript, forbidden) {
			t.Fatalf("更新脚本不该碰 %q", forbidden)
		}
	}
}

// 一键更新的入参校验
func TestUpdateNodesValidation(t *testing.T) {
	store, srv, sid := newInstallFixture(t)
	n, err := store.CreateNode(Node{GroupID: 1, Name: "n1", IP: "1.2.3.4", Token: "t1"})
	if err != nil {
		t.Fatal(err)
	}

	call := func(body string) (int, map[string]any) {
		req, _ := http.NewRequest("POST", srv.URL+"/api/nodes/update", strings.NewReader(body))
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

	if code, out := call(`{"ids":[]}`); code != 400 {
		t.Fatalf("空选择应 400，实际 %d (%v)", code, out)
	}
	big := make([]string, 51)
	for i := range big {
		big[i] = "1"
	}
	if code, _ := call(`{"ids":[` + strings.Join(big, ",") + `]}`); code != 400 {
		t.Fatalf("超过 50 台应 400，实际 %d", code)
	}

	// 没有记住密码：应该逐台报错，而不是整个请求失败
	code, out := call(`{"ids":[` + itoa64(n.ID) + `]}`)
	if code != 200 {
		t.Fatalf("应返回 200 + 明细，实际 %d", code)
	}
	results, _ := out["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("应有 1 条结果，实际 %d", len(results))
	}
	first := results[0].(map[string]any)
	if ok, _ := first["ok"].(bool); ok {
		t.Fatal("没有 SSH 密码不该报成功")
	}
	if msg, _ := first["error"].(string); !strings.Contains(msg, "SSH 密码") {
		t.Fatalf("错误信息应该点明缺 SSH 密码，实际 %q", msg)
	}
}

func itoa64(v int64) string {
	return strconv.FormatInt(v, 10)
}

// 安装脚本该有的护栏：失败能定位、先停旧进程、装前看磁盘
func TestInstallScriptGuards(t *testing.T) {
	s := installTmpl("https://panel.example.test")
	for _, want := range []string{
		"trap 'echo \"[错误] 第 $LINENO 行失败", // 失败能指出是哪一行
		"systemctl stop relay-node",            // 先停旧进程，否则覆盖运行中的二进制会 Text file busy
		"df -P /usr/local",                     // 装前看磁盘
		"/tmp/relay-node.bin",                  // 优先用面板推上来的二进制
		"rm -f /var/lib/relay/key",             // 重装要清旧 session_key，否则一直 token_invalid
		"RestartPreventExitStatus=2",           // 凭证被拒时别让 systemd 无限拉起
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("安装脚本缺少 %q", want)
		}
	}
	// 面板地址必须替换进脚本
	if !strings.Contains(s, `PANEL="https://panel.example.test"`) {
		t.Fatal("面板地址没替换进去")
	}
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

// 节点换了 IP，凭据要能找回来。三级回退逐个验。
func TestPickCredFallbacks(t *testing.T) {
	store, srv, _ := newInstallFixture(t)
	api := NewAPI(store, nil, NewSessionStore(), nil, nil)
	_ = srv

	// 没任何凭据
	n := &Node{ID: 1, IP: "10.0.0.1"}
	if c := api.pickCred(n); c != nil {
		t.Fatalf("没密码时应返回 nil，实际 %+v", c)
	}

	// 录一条（旧 IP）
	old, err := store.SaveCred(Cred{IP: "10.0.0.1", Port: 22, User: "root", Password: "pw-old"})
	if err != nil {
		t.Fatal(err)
	}

	// 1) 按 CredID：节点 IP 已经变成 10.0.0.2，但记得凭据主键
	n = &Node{ID: 1, IP: "10.0.0.2", CredID: old.ID}
	if c := api.pickCred(n); c == nil || c.Password != "pw-old" {
		t.Fatalf("应按 CredID 命中，实际 %+v", c)
	}

	// 2) 按当前 IP：没记 CredID 的老节点，IP 没变
	n = &Node{ID: 1, IP: "10.0.0.1"}
	if c := api.pickCred(n); c == nil || c.Password != "pw-old" {
		t.Fatalf("应按 IP 命中，实际 %+v", c)
	}

	// 3) 都对不上：IP 变了、也没记 CredID —— 回退到通用密码
	n = &Node{ID: 1, IP: "10.0.0.9"}
	if c := api.pickCred(n); c == nil || c.Password != "pw-old" {
		t.Fatalf("应回退到通用密码，实际 %+v", c)
	}
}
