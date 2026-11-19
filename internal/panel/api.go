// language: Go 1.23, file: internal/panel/api.go
// REST 接口 + Agent 心跳接口。
package panel

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type API struct {
	store *Store
	cnf   *Config
	sess  *SessionStore
	dns   DNSProvider
	sched *Scheduler
}

func NewAPI(s *Store, c *Config, sess *SessionStore, dns DNSProvider, sched *Scheduler) *API {
	return &API{store: s, cnf: c, sess: sess, dns: dns, sched: sched}
}

// kick 让调度器立刻巡检一次，不用等下一个 30 秒周期
func (a *API) kick() {
	if a.sched != nil {
		a.sched.Kick()
	}
}

func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()

	// 公开
	mux.HandleFunc("POST /api/login", a.login)
	mux.HandleFunc("POST /agent/beat", a.agentBeat)
	mux.HandleFunc("POST /agent/stats", a.agentStats)
	mux.HandleFunc("GET /i.sh", a.installScript)
	mux.HandleFunc("GET /dl/{name}", a.dlHandler)

	// 需要登录
	mux.Handle("POST /api/logout", a.auth(http.HandlerFunc(a.logout)))
	mux.Handle("GET /api/me", a.auth(http.HandlerFunc(a.me)))
	mux.Handle("GET /api/overview", a.auth(http.HandlerFunc(a.overview)))

	mux.Handle("GET /api/groups", a.auth(http.HandlerFunc(a.listGroups)))
	mux.Handle("POST /api/groups", a.auth(http.HandlerFunc(a.createGroup)))
	mux.Handle("GET /api/groups/{id}", a.auth(http.HandlerFunc(a.getGroup)))
	mux.Handle("PATCH /api/groups/{id}", a.auth(http.HandlerFunc(a.patchGroup)))
	mux.Handle("DELETE /api/groups/{id}", a.auth(http.HandlerFunc(a.deleteGroup)))

	mux.Handle("GET /api/groups/{id}/nodes", a.auth(http.HandlerFunc(a.listNodes)))
	mux.Handle("POST /api/groups/{id}/nodes", a.auth(http.HandlerFunc(a.createNode)))
	mux.Handle("PATCH /api/nodes/{id}", a.auth(http.HandlerFunc(a.patchNode)))
	mux.Handle("DELETE /api/nodes/{id}", a.auth(http.HandlerFunc(a.deleteNode)))
	mux.Handle("POST /api/nodes/{id}/reset", a.auth(http.HandlerFunc(a.resetNode)))
	mux.Handle("POST /api/nodes/{id}/promote", a.auth(http.HandlerFunc(a.promoteNode)))

	mux.Handle("GET /api/groups/{id}/rules", a.auth(http.HandlerFunc(a.listRules)))
	mux.Handle("POST /api/groups/{id}/rules", a.auth(http.HandlerFunc(a.createRule)))
	mux.Handle("POST /api/groups/{id}/rules/import", a.auth(http.HandlerFunc(a.ImportRules)))
	mux.Handle("GET /api/groups/{id}/rules/export", a.auth(http.HandlerFunc(a.ExportRules)))
	mux.Handle("POST /api/groups/{id}/rules/delete-batch", a.auth(http.HandlerFunc(a.DeleteRulesBatch)))
	mux.Handle("POST /api/groups/{id}/rules/enable-batch", a.auth(http.HandlerFunc(a.EnableRulesBatch)))
	mux.Handle("PATCH /api/rules/{id}", a.auth(http.HandlerFunc(a.patchRule)))
	mux.Handle("DELETE /api/rules/{id}", a.auth(http.HandlerFunc(a.deleteRule)))

	mux.Handle("GET /api/events", a.auth(http.HandlerFunc(a.listEvents)))
	mux.Handle("GET /api/alerts", a.auth(http.HandlerFunc(a.listAlerts)))
	mux.Handle("POST /api/alerts/{id}/ack", a.auth(http.HandlerFunc(a.ackAlert)))

	mux.Handle("GET /api/settings", a.auth(http.HandlerFunc(a.getSettings)))
	mux.Handle("POST /api/settings", a.auth(http.HandlerFunc(a.saveSettings)))
	mux.Handle("POST /api/dns/verify", a.auth(http.HandlerFunc(a.verifyDNS)))
	mux.Handle("POST /api/dns/zones", a.auth(http.HandlerFunc(a.listZones)))
	mux.Handle("POST /api/groups/{id}/dns-test", a.auth(http.HandlerFunc(a.testGroupDNS)))
	mux.Handle("POST /api/groups/{id}/sync-dns", a.auth(http.HandlerFunc(a.syncGroupDNS)))
	mux.Handle("GET /api/rules", a.auth(http.HandlerFunc(a.AllRules)))
	mux.Handle("POST /api/nodes/{id}/cmd", a.auth(http.HandlerFunc(a.nodeCmd)))
	mux.Handle("POST /api/nodes/{id}/recover", a.auth(http.HandlerFunc(a.recoverNode)))

	mux.Handle("/", webHandler())

	return mux
}

// ---------- 认证 ----------

func (a *API) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ck, err := r.Cookie("sid")
		if err != nil {
			writeJSON(w, 401, map[string]string{"error": "未登录"})
			return
		}
		if _, ok := a.sess.Get(ck.Value); !ok {
			writeJSON(w, 401, map[string]string{"error": "会话已过期"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *API) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "请求格式错误"})
		return
	}
	if in.Username != a.cnf.AdminUser || !VerifyPassword(in.Password, a.cnf.AdminPassHash) {
		time.Sleep(300 * time.Millisecond) // 拖一下暴力破解
		writeJSON(w, 401, map[string]string{"error": "用户名或密码错误"})
		return
	}
	tok := a.sess.Issue(in.Username)
	http.SetCookie(w, &http.Cookie{
		Name:     "sid",
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   12 * 3600,
	})
	writeJSON(w, 200, map[string]any{"ok": true, "user": in.Username})
}

func (a *API) logout(w http.ResponseWriter, r *http.Request) {
	if ck, err := r.Cookie("sid"); err == nil {
		a.sess.Revoke(ck.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: "sid", Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *API) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"user": a.cnf.AdminUser})
}

func pathID(r *http.Request, name string) int64 {
	n, _ := strconv.ParseInt(r.PathValue(name), 10, 64)
	return n
}

// ---------- 分组 ----------

func (a *API) listGroups(w http.ResponseWriter, r *http.Request) {
	gs := a.store.Groups()
	out := make([]map[string]any, 0, len(gs))
	for i := range gs {
		g := &gs[i]
		nodes := a.store.Nodes(g.ID)
		var cur *Node
		var totalUsed int64
		for j := range nodes {
			if nodes[j].ID == g.CurrentNode {
				cur = &nodes[j]
			}
			totalUsed += nodes[j].UsedBytes
		}
		out = append(out, map[string]any{
			"group":      g,
			"node_count": len(nodes),
			"total_used": totalUsed,
			"current":    cur,
		})
	}
	writeJSON(w, 200, out)
}

func (a *API) getGroup(w http.ResponseWriter, r *http.Request) {
	g := a.store.Group(pathID(r, "id"))
	if g == nil {
		writeJSON(w, 404, map[string]string{"error": "分组不存在"})
		return
	}
	writeJSON(w, 200, g)
}

type groupIn struct {
	Name        string `json:"name"`
	Remark      string `json:"remark"`
	QuotaGB     int64  `json:"quota_gb"`
	PeakMbps    int64  `json:"peak_mbps"`
	ProbePort   int    `json:"probe_port"`
	// ProbeTarget 该组节点的连通性探测目标（host:port）
	ProbeTarget string `json:"probe_target"`
	DNSProvider string `json:"dns_provider"`
	DNSZone     string `json:"dns_zone"`
	DNSName     string `json:"dns_name"`
	DNSTTL      int    `json:"dns_ttl"`
	ResetDay    int    `json:"reset_day"`
}

func (in *groupIn) toGroup() Group {
	return Group{
		Name:        strings.TrimSpace(in.Name),
		Remark:      in.Remark,
		QuotaBytes:  in.QuotaGB * 1024 * 1024 * 1024,
		PeakBps:     in.PeakMbps * 1000 * 1000 / 8,
		ProbePort:   in.ProbePort,
		ProbeTarget: strings.TrimSpace(in.ProbeTarget),
		DNSProvider: in.DNSProvider,
		DNSZone:     strings.TrimSpace(in.DNSZone),
		DNSName:     strings.TrimSpace(in.DNSName),
		DNSTTL:      in.DNSTTL,
		ResetDay:    in.ResetDay,
	}
}

func (a *API) createGroup(w http.ResponseWriter, r *http.Request) {
	var in groupIn
	if err := readJSON(r, &in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "请求格式错误"})
		return
	}
	if in.Name == "" {
		writeJSON(w, 400, map[string]string{"error": "分组名不能为空"})
		return
	}
	if strings.TrimSpace(in.DNSName) == "" {
		writeJSON(w, 400, map[string]string{"error": "域名不能为空，切流靠它"})
		return
	}
	g, err := a.store.CreateGroup(in.toGroup())
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	a.kick()
	writeJSON(w, 200, g)
}

func (a *API) patchGroup(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	old := a.store.Group(id)
	if old == nil {
		writeJSON(w, 404, map[string]string{"error": "分组不存在"})
		return
	}
	var in groupIn
	if err := readJSON(r, &in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "请求格式错误"})
		return
	}
	if strings.TrimSpace(in.DNSName) == "" {
		writeJSON(w, 400, map[string]string{"error": "域名不能为空，切流靠它"})
		return
	}
	ng := in.toGroup()
	ng.ID = old.ID
	ng.CreatedAt = old.CreatedAt
	ng.CurrentNode = old.CurrentNode
	ng.LastSwitch = old.LastSwitch
	ng.DNSRecordID = old.DNSRecordID
	ng.ManualLockUntil = old.ManualLockUntil
	if ng.QuotaBytes == 0 {
		ng.QuotaBytes = old.QuotaBytes
	}
	if ng.PeakBps == 0 {
		ng.PeakBps = old.PeakBps
	}
	if ng.DNSTTL == 0 {
		ng.DNSTTL = old.DNSTTL
	}
	if ng.ProbePort == 0 && old.ProbePort != 0 {
		ng.ProbePort = old.ProbePort
	}
	if ng.ResetDay == 0 {
		ng.ResetDay = old.ResetDay
	}
	if err := a.store.UpdateGroup(ng); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	a.kick()
	writeJSON(w, 200, ng)
}

func (a *API) deleteGroup(w http.ResponseWriter, r *http.Request) {
	if err := a.store.DeleteGroup(pathID(r, "id")); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- 节点 ----------

func (a *API) listNodes(w http.ResponseWriter, r *http.Request) {
	gid := pathID(r, "id")
	ns := a.store.Nodes(gid)
	out := make([]map[string]any, 0, len(ns))
	for _, n := range ns {
		online := n.LastSeen > 0 && time.Now().Unix()-n.LastSeen < 90
		out = append(out, map[string]any{
			"node":   n,
			"online": online,
			// 从没上报过的节点，显示「0 GB」会让人以为统计正常只是没流量，
			// 实际是根本没数据。前端拿这个标志区分。
			"never_reported": n.LastSeen == 0,
		})
	}
	writeJSON(w, 200, out)
}

func (a *API) createNode(w http.ResponseWriter, r *http.Request) {
	gid := pathID(r, "id")
	if a.store.Group(gid) == nil {
		writeJSON(w, 404, map[string]string{"error": "分组不存在"})
		return
	}
	var in struct {
		Name     string `json:"name"`
		IP       string `json:"ip"`
		Iface    string `json:"iface"`
		BillMode string `json:"bill_mode"`
		QuotaGB  int64  `json:"quota_gb"`
		IsBackup bool   `json:"is_backup"`
		Role     string `json:"role"`
		Forward  *bool  `json:"forward"`
		// ProbeTarget 留空则用第一条线路的上游
		ProbeTarget string `json:"probe_target_custom"`
	}
	if err := readJSON(r, &in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "请求格式错误"})
		return
	}
	if in.Name == "" {
		writeJSON(w, 400, map[string]string{"error": "名称必填"})
		return
	}
	// IP 可以留空：节点首次心跳时面板会从连接里识别并自动填入
	order := 0
	for _, n := range a.store.Nodes(gid) {
		if n.SortOrder >= order {
			order = n.SortOrder + 10
		}
	}
	n, err := a.store.CreateNode(Node{
		GroupID:           gid,
		Name:              in.Name,
		IP:                in.IP,
		Iface:             in.Iface,
		BillMode:          in.BillMode,
		QuotaBytes:        in.QuotaGB * 1024 * 1024 * 1024,
		SortOrder:         order,
		ProbeTargetCustom: strings.TrimSpace(in.ProbeTarget),
		Token:             randHex(24),
		Role:       "node",
		Forward:    true,
		IsBackup:   in.IsBackup,
	})
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	a.kick()
	writeJSON(w, 200, map[string]any{
		"node":  n,
		"token": n.Token,
		"cmd":   a.buildInstallCmd(n.Token),
	})
}

// nodeCmd 返回某个节点的对接命令。token 用一次就作废，
// 已接入的节点需要传 reset=1 重新生成（会让旧节点掉线）。
func (a *API) nodeCmd(w http.ResponseWriter, r *http.Request) {
	n := a.store.Node(pathID(r, "id"))
	if n == nil {
		writeJSON(w, 404, map[string]string{"error": "节点不存在"})
		return
	}
	if strings.TrimSpace(a.cnf.PublicURL) == "" {
		writeJSON(w, 400, map[string]string{"error": "先在设置页填 Public URL，命令里要用"})
		return
	}

	var in struct {
		Reset bool `json:"reset"`
	}
	_ = readJSON(r, &in)

	if strings.HasPrefix(n.Token, "used-") && !in.Reset {
		writeJSON(w, 200, map[string]any{
			"need_reset": true,
			"message":    "该节点已接入过，原命令已失效。重新生成会让它掉线，需要在机器上重装。",
		})
		return
	}
	if in.Reset {
		n = a.store.ResetNodeKey(n.ID)
		if n == nil {
			writeJSON(w, 500, map[string]string{"error": "重置失败"})
			return
		}
		log.Printf("node %d (%s): 重新生成对接 token", n.ID, n.Name)
	}

	writeJSON(w, 200, map[string]any{
		"cmd":   a.buildInstallCmd(n.Token),
		"token": n.Token,
		"reset": in.Reset,
	})
}

func (a *API) buildInstallCmd(token string) string {
	base := strings.TrimRight(a.cnf.PublicURL, "/")
	if base == "" {
		// 没配 PublicURL 时兜底，端口跟随监听配置
		port := "3070"
		if i := strings.LastIndex(a.cnf.Listen, ":"); i >= 0 && i < len(a.cnf.Listen)-1 {
			port = a.cnf.Listen[i+1:]
		}
		base = "http://127.0.0.1:" + port
	}
	return "curl -fsSL " + base + "/i.sh | bash -s -- --panel=" + base + " --token=" + token
}

func (a *API) patchNode(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	old := a.store.Node(id)
	if old == nil {
		writeJSON(w, 404, map[string]string{"error": "节点不存在"})
		return
	}
	var in struct {
		Name      string `json:"name"`
		IP        string `json:"ip"`
		Iface     string `json:"iface"`
		BillMode  string `json:"bill_mode"`
		QuotaGB   int64  `json:"quota_gb"`
		SortOrder int    `json:"sort_order"`
		Status    string `json:"status"`
		IsBackup  *bool  `json:"is_backup"`
		// ProbeTarget 指针：区分「没传」和「传了空字符串（清空自定义目标）」
		ProbeTarget *string `json:"probe_target_custom"`
	}
	if err := readJSON(r, &in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "请求格式错误"})
		return
	}
	n := *old
	if in.ProbeTarget != nil {
		n.ProbeTargetCustom = strings.TrimSpace(*in.ProbeTarget)
	}
	if in.IsBackup != nil {
		n.IsBackup = *in.IsBackup
	}
	if in.Name != "" {
		n.Name = in.Name
	}
	if in.IP != "" {
		n.IP = in.IP
	}
	if in.Iface != "" {
		n.Iface = in.Iface
	}
	if in.BillMode != "" {
		n.BillMode = in.BillMode
	}
	if in.QuotaGB > 0 {
		n.QuotaBytes = in.QuotaGB * 1024 * 1024 * 1024
	}
	if in.SortOrder != 0 {
		n.SortOrder = in.SortOrder
	}
	if in.Status != "" {
		n.Status = in.Status
	}
	// 换网卡 = 换了一把完全无关的计数器；改计费口径 = 节点端的累计从 0 重新算。
	// 这两种情况都必须把面板这侧的账一起归零，否则面板上的数字和服务器上的
	// /proc/net/dev 永远差一个定值，看着就像「算错了」。
	recal := (in.Iface != "" && in.Iface != old.Iface) ||
		(in.BillMode != "" && in.BillMode != old.BillMode)
	if err := a.store.UpdateNode(n); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if recal {
		a.store.ResetNodeAccounting(n.ID)
		log.Printf("node %d (%s): 网卡 %q->%q 口径 %q->%q，本周期统计从 0 重算",
			n.ID, n.Name, old.Iface, n.Iface, old.BillMode, n.BillMode)
	}
	// 改的正好是当前节点、而且 IP 变了：DNS 必须立刻跟上，否则域名还指着旧地址
	if old.IP != n.IP {
		if g := a.store.Group(n.GroupID); g != nil && g.CurrentNode == n.ID {
			if err := a.dns.SetA(g, n.IP, g.DNSTTL); err != nil {
				a.store.Alert(n.GroupID, n.ID, "error", "节点 IP 已改但 DNS 更新失败: "+err.Error())
			}
		}
	}
	a.kick()
	writeJSON(w, 200, n)
}

func (a *API) deleteNode(w http.ResponseWriter, r *http.Request) {
	if err := a.store.DeleteNode(pathID(r, "id")); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	a.kick()
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *API) resetNode(w http.ResponseWriter, r *http.Request) {
	a.store.ResetNodeCycle(pathID(r, "id"))
	a.kick()
	writeJSON(w, 200, map[string]any{"ok": true})
}

// promoteNode 手工切到指定节点，无视串行顺序
// syncNodeIP 用心跳连接的来源地址更新节点公网 IP。
// 只接受 IPv4（DNS A 记录用）；IPv6 连接保留原配置不动。
func (a *API) syncNodeIP(n *Node, r *http.Request) {
	pub := clientIP(r)
	ip := net.ParseIP(pub)
	if ip == nil || ip.To4() == nil {
		return
	}
	if pub == n.IP {
		return
	}
	old := a.store.SetNodeIP(n.ID, pub)

	if old == "" {
		log.Printf("node %d (%s): 自动识别公网 IP %s", n.ID, n.Name, pub)
		return
	}

	log.Printf("node %d (%s): 公网 IP %s -> %s", n.ID, n.Name, old, pub)
	a.store.Alert(n.GroupID, n.ID, "warn",
		"节点 "+n.Name+" 公网 IP 变更: "+old+" → "+pub)

	// 正好是当前节点，DNS 立刻跟上，别让域名指着旧地址
	if g := a.store.Group(n.GroupID); g != nil && g.CurrentNode == n.ID {
		if err := a.dns.SetA(g, pub, g.DNSTTL); err != nil {
			a.store.Alert(n.GroupID, n.ID, "error", "IP 变更后 DNS 更新失败: "+err.Error())
			log.Printf("group %d: IP 变更后 DNS 更新失败: %v", g.ID, err)
		} else {
			log.Printf("group %d: DNS 已跟随 IP 变更指向 %s", g.ID, pub)
		}
	}
}
func (a *API) promoteNode(w http.ResponseWriter, r *http.Request) {
	n := a.store.Node(pathID(r, "id"))
	if n == nil {
		writeJSON(w, 404, map[string]string{"error": "节点不存在"})
		return
	}
	g := a.store.Group(n.GroupID)
	if g == nil {
		writeJSON(w, 404, map[string]string{"error": "分组不存在"})
		return
	}
	if err := a.dns.SetA(g, n.IP, g.DNSTTL); err != nil {
		writeJSON(w, 500, map[string]string{"error": "DNS 更新失败: " + err.Error()})
		return
	}
	old := g.CurrentNode
	a.store.SetCurrentNode(g.ID, n.ID)
	// 锁 30 分钟：不然用户刚点「切到这台」，调度器 30 秒后又切回去
	a.store.LockGroup(g.ID, 30*time.Minute)
	a.store.Audit(g.ID, old, n.ID, "manual")
	writeJSON(w, 200, map[string]any{"ok": true, "node": n, "locked_minutes": 30})
}

// ---------- 规则 ----------

func (a *API) listRules(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, a.store.Rules(pathID(r, "id")))
}

func (a *API) createRule(w http.ResponseWriter, r *http.Request) {
	gid := pathID(r, "id")
	var in struct {
		Name       string `json:"name"`
		ListenPort int    `json:"listen_port"`
		TargetHost string `json:"target_host"`
		TargetPort int    `json:"target_port"`
		Protocol   string `json:"protocol"`
	}
	if err := readJSON(r, &in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "请求格式错误"})
		return
	}
	if in.ListenPort <= 0 {
		writeJSON(w, 400, map[string]string{"error": "监听端口必填"})
		return
	}
	if in.TargetHost == "" || in.TargetPort <= 0 {
		writeJSON(w, 400, map[string]string{"error": "目标地址和端口必填"})
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = fmt.Sprintf("%s:%d", in.TargetHost, in.TargetPort)
	}
	r2, err := a.store.CreateRule(Rule{
		GroupID:    gid,
		Name:       name,
		ListenPort: in.ListenPort,
		TargetHost: strings.TrimSpace(in.TargetHost),
		TargetPort: in.TargetPort,
		Protocol:   in.Protocol,
		Enabled:    true,
	})
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, r2)
}

func (a *API) patchRule(w http.ResponseWriter, r *http.Request) {
	target := a.store.Rule(pathID(r, "id"))
	if target == nil {
		writeJSON(w, 404, map[string]string{"error": "规则不存在"})
		return
	}
	var in struct {
		Name       string `json:"name"`
		GroupID    int64  `json:"group_id"`
		ListenPort int    `json:"listen_port"`
		TargetHost string `json:"target_host"`
		TargetPort int    `json:"target_port"`
		Protocol   string `json:"protocol"`
		Enabled    *bool  `json:"enabled"`
	}
	if err := readJSON(r, &in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "请求格式错误"})
		return
	}
	// 换分组：端口冲突会在 UpdateRule 里按新分组重新校验
	if in.GroupID > 0 && in.GroupID != target.GroupID {
		if a.store.Group(in.GroupID) == nil {
			writeJSON(w, 400, map[string]string{"error": "目标分组不存在"})
			return
		}
		target.GroupID = in.GroupID
	}
	if in.Name != "" {
		target.Name = strings.TrimSpace(in.Name)
	}
	if in.ListenPort != 0 {
		target.ListenPort = in.ListenPort
	}
	if in.TargetHost != "" {
		target.TargetHost = in.TargetHost
	}
	if in.TargetPort != 0 {
		target.TargetPort = in.TargetPort
	}
	if in.Protocol != "" {
		target.Protocol = in.Protocol
	}
	if in.Enabled != nil {
		target.Enabled = *in.Enabled
	}
	if err := a.store.UpdateRule(*target); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, target)
}

func (a *API) deleteRule(w http.ResponseWriter, r *http.Request) {
	if err := a.store.DeleteRule(pathID(r, "id")); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- 事件 / 告警 ----------

func (a *API) listEvents(w http.ResponseWriter, r *http.Request) {
	gid, _ := strconv.ParseInt(r.URL.Query().Get("group_id"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	writeJSON(w, 200, a.store.Events(gid, limit))
}

func (a *API) listAlerts(w http.ResponseWriter, r *http.Request) {
	onlyOpen := r.URL.Query().Get("acked") != "1"
	writeJSON(w, 200, a.store.Alerts(onlyOpen))
}

func (a *API) ackAlert(w http.ResponseWriter, r *http.Request) {
	a.store.AckAlert(pathID(r, "id"))
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *API) overview(w http.ResponseWriter, r *http.Request) {
	gs := a.store.Groups()
	nodes := a.store.AllNodes()
	var used, quota int64
	online := 0
	now := time.Now().Unix()
	for _, n := range nodes {
		used += n.UsedBytes
		quota += n.QuotaBytes
		if n.LastSeen > 0 && now-n.LastSeen < 90 {
			online++
		}
	}
	writeJSON(w, 200, map[string]any{
		"groups":       len(gs),
		"nodes":        len(nodes),
		"nodes_online": online,
		"used_bytes":   used,
		"quota_bytes":  quota,
		"alerts_open":  len(a.store.Alerts(true)),
	})
}

// ---------- 设置 / DNS ----------

func maskToken(t string) string {
	t = strings.TrimSpace(t)
	if t == "" {
		return ""
	}
	if len(t) <= 12 {
		return strings.Repeat("•", len(t))
	}
	return t[:6] + strings.Repeat("•", 10) + t[len(t)-4:]
}

func (a *API) getSettings(w http.ResponseWriter, r *http.Request) {
	tok := a.cnf.Token()
	writeJSON(w, 200, map[string]any{
		"build":                 BuildStamp,
		"public_url":            a.cnf.PublicURL,
		"db_path":               a.cnf.DB,
		"cloudflare_token_set":  tok != "",
		"cloudflare_token_mask": maskToken(tok),
	})
}

func (a *API) saveSettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CloudflareToken *string `json:"cloudflare_token"`
		PublicURL       *string `json:"public_url"`
	}
	if err := readJSON(r, &in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "请求格式错误"})
		return
	}
	if in.CloudflareToken != nil {
		if err := a.cnf.SetCloudflareToken(strings.TrimSpace(*in.CloudflareToken)); err != nil {
			writeJSON(w, 500, map[string]string{"error": "保存 Token 失败: " + err.Error()})
			return
		}
	}
	if in.PublicURL != nil {
		a.cnf.PublicURL = strings.TrimRight(strings.TrimSpace(*in.PublicURL), "/")
		if err := a.cnf.Save(); err != nil {
			writeJSON(w, 500, map[string]string{"error": "保存配置失败: " + err.Error()})
			return
		}
	}
	a.kick() // token 变了，让调度器重新试一次
	writeJSON(w, 200, map[string]any{"ok": true})
}

// verifyDNS 校验 token 是否有效
func (a *API) verifyDNS(w http.ResponseWriter, r *http.Request) {
	if err := a.dns.Verify(); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "message": "Token 有效"})
}

// listZones 列出 token 能管的域名，前端做下拉
func (a *API) listZones(w http.ResponseWriter, r *http.Request) {
	m, ok := a.dns.(*ManagedDNS)
	if !ok {
		writeJSON(w, 400, map[string]string{"error": "未配置 DNS"})
		return
	}
	zones, err := m.Zones()
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	out := make([]map[string]string, 0, len(zones))
	for _, z := range zones {
		out = append(out, map[string]string{"id": z.ID, "name": z.Name})
	}
	writeJSON(w, 200, out)
}

// syncGroupDNS 手动把域名指到当前节点的 IP，用于 IP 变动或记录被手工改坏后拉起
func (a *API) syncGroupDNS(w http.ResponseWriter, r *http.Request) {
	g := a.store.Group(pathID(r, "id"))
	if g == nil {
		writeJSON(w, 404, map[string]string{"error": "分组不存在"})
		return
	}
	if strings.TrimSpace(g.DNSName) == "" {
		writeJSON(w, 400, map[string]string{"error": "分组没填域名"})
		return
	}
	n := a.store.Node(g.CurrentNode)
	if n == nil {
		writeJSON(w, 400, map[string]string{"error": "分组还没分配节点，先在节点页添加并等它上线"})
		return
	}
	if strings.TrimSpace(n.IP) == "" {
		writeJSON(w, 400, map[string]string{"error": "当前节点还没上报 IP，等它心跳一次"})
		return
	}
	if err := a.dns.SetA(g, n.IP, g.DNSTTL); err != nil {
		writeJSON(w, 500, map[string]string{"error": "DNS 更新失败: " + err.Error()})
		return
	}
	got, _ := a.dns.GetA(g)
	if got != "" && got != n.IP {
		a.store.Alert(g.ID, n.ID, "warn", "同步后记录仍指向 "+got+"，可能有 CDN 缓存")
	}
	log.Printf("group %d: 手动同步 DNS -> %s (%s)", g.ID, n.IP, g.DNSName)
	a.store.Alert(g.ID, n.ID, "info", "已手动同步 DNS 指向 "+n.IP)
	writeJSON(w, 200, map[string]any{"ok": true, "ip": n.IP, "current": got, "node": n.Name})
}

// testGroupDNS 用该分组的 zone/域名跑一次真实查询，看配置对不对
func (a *API) testGroupDNS(w http.ResponseWriter, r *http.Request) {
	g := a.store.Group(pathID(r, "id"))
	if g == nil {
		writeJSON(w, 404, map[string]string{"error": "分组不存在"})
		return
	}
	m, ok := a.dns.(*ManagedDNS)
	if !ok {
		writeJSON(w, 400, map[string]string{"error": "DNS 未初始化"})
		return
	}
	if strings.TrimSpace(a.cnf.Token()) == "" {
		writeJSON(w, 400, map[string]string{"error": "还没配置 Cloudflare Token，先去设置页填"})
		return
	}
	cf, _ := m.provider().(*Cloudflare)
	if cf == nil {
		writeJSON(w, 400, map[string]string{"error": "DNS 未配置"})
		return
	}
	zoneID, err := cf.zoneFor(g)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	rec, err := cf.findRecord(g, zoneID)
	if errors.Is(err, ErrDNSRecordMissing) {
		// 记录还不存在，这是正常的：第一次切流时会自动建
		writeJSON(w, 200, map[string]any{
			"ok": true, "state": "missing",
			"zone_id": zoneID,
			"message": "Zone 已识别，记录 " + g.DNSName + " 还不存在。第一次切流时面板会自动创建。",
		})
		return
	}
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	msg := "记录存在，当前指向 " + rec.Content
	if rec.Proxied {
		msg += "（注意：开了 CF 代理，转发流量必须关掉云朵）"
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "state": "found", "zone_id": zoneID,
		"record_id": rec.ID, "content": rec.Content,
		"proxied": rec.Proxied, "ttl": rec.TTL,
		"message": msg,
	})
}

// ---------- Agent 心跳 ----------

type beatReq struct {
	Key         string `json:"key"`
	Fingerprint string `json:"fingerprint"`
	Iface       string `json:"iface"`
	NicAcc      int64  `json:"nic_acc"`
	RelayAcc    int64  `json:"relay_acc"`
	Version     int64  `json:"version"`
	// Agent 节点端构建标识，面板上直接显示，省得每次都登机器看日志
	Agent string `json:"agent"`
	// 实时观测值：上下行速率（字节/秒）和资源占用，只用于面板展示
	RxBps   int64 `json:"rx_bps"`
	TxBps   int64 `json:"tx_bps"`
	CPUPct  int   `json:"cpu_pct"`
	MemPct  int   `json:"mem_pct"`
	DiskPct int   `json:"disk_pct"`
}

type agentRule struct {
	ListenPort int    `json:"listen_port"`
	TargetHost string `json:"target_host"`
	TargetPort int    `json:"target_port"`
	Protocol   string `json:"protocol"`
}

type agentConfig struct {
	Version int64       `json:"version"`
	Rules   []agentRule `json:"rules"`
}

type beatResp struct {
	OK         bool         `json:"ok"`
	Err        string       `json:"err,omitempty"`
	SessionKey string       `json:"session_key,omitempty"`
	Iface      string       `json:"iface,omitempty"`
	BillMode   string       `json:"bill_mode,omitempty"`
	ProbeTarget string      `json:"probe_target"`
	Config     *agentConfig `json:"config,omitempty"`
}

// defaultProbeTarget 默认连通性探测目标。
//
// 用来判断这个 IP 还能不能被境内正常访问：被封掉的 IP 到境内通常双向不通，
// 节点每分钟拨一次这个境内地址，超时就当「被墙」处理。
// 挑 CDN 域名而不是裸 IP——CDN 会轮换节点，单个 IP 挂了不代表你被墙。
const defaultProbeTarget = "gd-cu-v4.ip.zstaticcdn.com:80"

// probeTargetFor 决定这个节点该探哪个目标。优先级：
//  1. 节点上手工填的（probe_target_custom）
//  2. 分组上填的（一组一条线路，填一次全组生效）
//  3. 内置默认目标（defaultProbeTarget）
//
// 默认探境内地址是为了测「IP 是否被墙」；想改成探上游也可以，
// 在分组或节点上填 target_host:target_port 就覆盖掉了。
func (a *API) probeTargetFor(n *Node) string {
	if s := strings.TrimSpace(n.ProbeTargetCustom); s != "" {
		return s
	}
	if g := a.store.Group(n.GroupID); g != nil {
		if s := strings.TrimSpace(g.ProbeTarget); s != "" {
			return s
		}
	}
	return defaultProbeTarget
}

// statsReq 实时指标快通道：只有展示用的数字，不带配置也不动账本。
// 心跳 15 秒一次，要「实时」就得单独走一条 2 秒的轻请求。
type statsReq struct {
	Key         string `json:"key"`
	Fingerprint string `json:"fingerprint"`
	Iface       string `json:"iface"`
	RxBps       int64  `json:"rx_bps"`
	TxBps       int64  `json:"tx_bps"`
	CPUPct      int    `json:"cpu_pct"`
	MemPct      int    `json:"mem_pct"`
	DiskPct     int    `json:"disk_pct"`

	// 上游连通性自检：节点自己拨 TCP 的结果
	ProbeTarget string `json:"probe_target"`
	ProbeOK     bool   `json:"probe_ok"`
	ProbeDone   bool   `json:"probe_done"`
	ProbeFails  int    `json:"probe_fails"`
	ProbeAt     int64  `json:"probe_at"`
	ProbeRTT    int64  `json:"probe_rtt_ms"`
	ProbeErr    string `json:"probe_err"`
}

// agentStats 每两秒收一次节点的实时速率和负载。
// 只写内存，不落盘、不返回配置——这条路径唯一的作用是让面板上的数字动起来。
// 另外它也是连通性自检的入口：节点连报三次上游不通，这里立刻把它封掉。
func (a *API) agentStats(w http.ResponseWriter, r *http.Request) {
	var in statsReq
	if err := readJSON(r, &in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad_request"})
		return
	}
	nodeID, ok := a.store.AuthStats(in.Key, in.Fingerprint)
	if !ok {
		writeJSON(w, 401, map[string]string{"error": "token_invalid"})
		return
	}
	becameBad := a.store.UpdateMetrics(nodeID, Metrics{
		RxBps: in.RxBps, TxBps: in.TxBps,
		CPUPct: in.CPUPct, MemPct: in.MemPct, DiskPct: in.DiskPct,
		Iface:       in.Iface,
		ProbeTarget: in.ProbeTarget,
		ProbeOK:     in.ProbeOK,
		ProbeDone:   in.ProbeDone,
		ProbeFails:  in.ProbeFails,
		ProbeAt:     in.ProbeAt,
		ProbeRTT:    in.ProbeRTT,
		ProbeErr:    in.ProbeErr,
	})
	if becameBad {
		if n := a.store.Node(nodeID); n != nil {
			log.Printf("node %d (%s): 上游连续 %d 次不通（%s），标记为待恢复",
				n.ID, n.Name, in.ProbeFails, in.ProbeTarget)
			a.store.Alert(n.GroupID, n.ID, "error",
				fmt.Sprintf("节点 %s 连不上上游 %s（连续 %d 次），已切走，需人工恢复",
					n.Name, in.ProbeTarget, in.ProbeFails))
		}
		a.kick() // 立刻巡检，不用等下一个 30 秒
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// recoverNode 人工恢复被连通性自检封掉的节点。
func (a *API) recoverNode(w http.ResponseWriter, r *http.Request) {
	n := a.store.RecoverNode(pathID(r, "id"))
	if n == nil {
		writeJSON(w, 404, map[string]string{"error": "节点不存在"})
		return
	}
	log.Printf("node %d (%s): 人工恢复，重新参与调度", n.ID, n.Name)
	a.kick()
	writeJSON(w, 200, n)
}

// clientIP 从请求里取对端真实公网 IP，处理反代和 Cloudflare
func clientIP(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); v != "" {
		return v
	}
	if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
		return v
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.IndexByte(v, ','); i > 0 {
			return strings.TrimSpace(v[:i])
		}
		return strings.TrimSpace(v)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (a *API) agentBeat(w http.ResponseWriter, r *http.Request) {
	var in beatReq
	if err := readJSON(r, &in); err != nil {
		writeJSON(w, 400, beatResp{OK: false, Err: "bad_request"})
		return
	}
	nodeID, newKey, fresh, err := a.store.AuthAgent(in.Key, in.Fingerprint)
	if err != nil {
		writeJSON(w, 401, beatResp{OK: false, Err: err.Error()})
		return
	}
	before := a.store.Node(nodeID)
	ver := a.store.AcceptBeat(Beat{
		NodeID:   nodeID,
		NicAcc:   in.NicAcc,
		RelayAcc: in.RelayAcc,
		Version:  in.Version,
		Agent:    in.Agent,
		RxBps:    in.RxBps,
		TxBps:    in.TxBps,
		CPUPct:   in.CPUPct,
		MemPct:   in.MemPct,
		DiskPct:  in.DiskPct,
	})
	if before != nil && in.Agent != "" && before.AgentStamp != in.Agent {
		log.Printf("node %d (%s): 节点端 %q -> %q", nodeID, before.Name, before.AgentStamp, in.Agent)
	}

	resp := beatResp{OK: true}
	if fresh {
		resp.SessionKey = newKey
	}
	n := a.store.Node(nodeID)
	if n == nil {
		writeJSON(w, 410, beatResp{OK: false, Err: "node_gone"})
		return
	}
	a.syncNodeIP(n, r)
	// 节点把实际在用的网卡报上来，覆盖面板里可能填错的那个，
	// 否则每轮心跳都会把错的网卡推下去，node 端反复切换、基准反复重置，
	// 结果就是流量一点都不累加。
	if in.Iface != "" && in.Iface != n.Iface {
		log.Printf("node %d (%s): 网卡 %q -> %q（节点上报）", n.ID, n.Name, n.Iface, in.Iface)
		a.store.SetNodeIface(n.ID, in.Iface)
		n.Iface = in.Iface
	}
	resp.Iface = n.Iface
	resp.BillMode = n.BillMode
	resp.ProbeTarget = a.probeTargetFor(n)

	// 版本对不上才下发配置（心跳 15s 一次，配置一般不变）
	// 所有节点拿到的规则完全一致，切节点只改 DNS，不重发配置
	if ver > in.Version {
		cfg := agentConfig{Version: ver, Rules: []agentRule{}}
		for _, rl := range a.store.Rules(n.GroupID) {
			if !rl.Enabled || rl.TargetHost == "" || rl.TargetPort <= 0 || rl.ListenPort <= 0 {
				continue
			}
			cfg.Rules = append(cfg.Rules, agentRule{
				ListenPort: rl.ListenPort,
				TargetHost: rl.TargetHost,
				TargetPort: rl.TargetPort,
				Protocol:   rl.Protocol,
			})
		}
		resp.Config = &cfg
	}
	writeJSON(w, 200, resp)
}
