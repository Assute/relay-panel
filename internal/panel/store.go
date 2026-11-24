// language: Go 1.23, file: internal/panel/store.go
// 面板状态存储：全内存 + JSON 文件落盘。几百节点量级足够，
// 备份就是 cp 一份 data.json。
package panel

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	StatusPending   = "pending"   // 已建，从未连上
	StatusOnline    = "online"    // 在线且是当前节点
	StatusStandby   = "standby"   // 在线但不是当前节点（串行待命）
	StatusDraining  = "draining"  // 刚被切走，存量连接还在跑
	StatusExhausted = "exhausted" // 本周期配额用完
	StatusDead      = "dead"      // 探活失败
	StatusDisabled  = "disabled"  // 人工停用
	// StatusUnreachable 节点自检发现上游不通，已被切走。
	// 和 dead 的区别：dead 是「面板连不上它」，认由心跳/探活恢复；
	// unreachable 是「它自己连不上上游」，必须人工点「恢复」才重新参与调度，
	// 否则它每分钟都会再报一次不通，切来切去没完。
	StatusUnreachable = "unreachable"
)

type Group struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Remark      string `json:"remark"`

	QuotaBytes int64 `json:"quota_bytes"` // 0 = 不限
	PeakBps    int64 `json:"peak_bps"`    // 预估峰值字节/秒，算切换余量用
	ProbePort  int   `json:"probe_port"`  // 探活端口

	// ProbeTarget 分组级的连通性探测目标（host:port），该组所有节点默认探这个。
	// 填一个境内地址就能当「IP 是否被墙」的探针：被封的 IP 到境内通常双向不通。
	// 节点自己填了就以节点的为准。
	ProbeTarget string `json:"probe_target"`

	DNSProvider string `json:"dns_provider"`
	DNSZone     string `json:"dns_zone"`
	DNSName     string `json:"dns_name"`
	DNSRecordID string `json:"dns_record_id"`
	DNSTTL      int    `json:"dns_ttl"`

	CurrentNode int64 `json:"current_node"`
	LastSwitch  int64 `json:"last_switch"`
	// 手动切到某台后，这段时间内不自动切走。
	// 否则用户刚点「切到这台」，30 秒后被调度器又切回去，很蠢。
	ManualLockUntil int64 `json:"manual_lock_until"`
	ResetDay        int   `json:"reset_day"`

	// LowNodes 「可用节点只剩两台」的告警已经发过了。
	// 恢复到三台以上才复位，避免每 30 秒巡检都重复报同一条。
	LowNodes bool `json:"low_nodes"`

	CreatedAt int64 `json:"created_at"`
}

type Node struct {
	ID      int64  `json:"id"`
	GroupID int64  `json:"group_id"`
	Name    string `json:"name"`
	IP      string `json:"ip"`
	Iface   string `json:"iface"`
	BillMode string `json:"bill_mode"` // tx | rx | both

	// Role 旧字段：早期版本区分入口机/出口机，现在统一为转发节点。
	// 保留字段只为旧数据能继续解析，新节点一律写 "node"。
	Role    string `json:"role"`
	Forward bool   `json:"forward"` // 本机是否跑转发器

	QuotaBytes    int64 `json:"quota_bytes"`
	UsedBytes     int64 `json:"used_bytes"`      // 本周期已用 = NicReported - CycleStartNic
	CycleStartNic int64 `json:"cycle_start_nic"` // 周期起点的网卡累计值
	NicReported   int64 `json:"nic_reported"`    // Agent 上报的网卡累计（单调）
	RelayReported int64 `json:"relay_reported"`  // Agent 上报的转发累计（单调）

	// AccountMigrated 该节点是否已切到「绝对量账本」（见 AcceptBeat）。
	// 旧版本用增量累加，升级后第一次心跳靠它决定要不要反推周期起点。
	AccountMigrated bool `json:"account_migrated"`
	// NeedAlign 下一次心跳只对齐起点、不计用量。
	// 换网卡或改计费口径后节点端换了计数器，新计数器的当前值不能算成本周期用量。
	NeedAlign bool `json:"need_align"`
	// AgentStamp 节点端二进制标识，面板上直接能看到节点跑的是哪个构建，
	// 不用登机器看 journalctl。
	AgentStamp string `json:"agent_stamp"`

	// 心跳带回来的实时观测值，只用于展示，不参与计费。
	RxBps   int64 `json:"rx_bps"` // 下行，字节/秒
	TxBps   int64 `json:"tx_bps"` // 上行，字节/秒
	CPUPct  int   `json:"cpu_pct"`
	MemPct  int   `json:"mem_pct"`
	DiskPct int   `json:"disk_pct"`
	// FirstSeen 第一次心跳的时间，面板上的「在线 X 天」按它算
	FirstSeen int64 `json:"first_seen"`

	// 上游连通性：节点自己拨 TCP 得到的结果，面板据此判断它还能不能干活。
	// ProbeTargetCustom 人工指定的探测目标（host:port）。
	// 留空时用该分组第一条启用线路的上游——那才是这个节点真正要转发到的地方。
	ProbeTargetCustom string `json:"probe_target_custom"`
	ProbeTarget       string `json:"probe_target"` // host:port，空表示没配置
	ProbeOK     bool   `json:"probe_ok"`
	ProbeDone   bool   `json:"probe_done"` // 节点是否已经出过结果
	ProbeFails  int    `json:"probe_fails"` // 连续失败次数，成功即归零
	ProbeAt     int64  `json:"probe_at"`
	ProbeRTT    int64  `json:"probe_rtt_ms"`
	ProbeErr    string `json:"probe_err"`
	// Unreachable 已被判定上游不通并切走，等人工恢复。
	Unreachable bool `json:"unreachable"`

	SortOrder int    `json:"sort_order"`
	Status    string `json:"status"`

	// IsBackup 备用节点：只有所有主用节点都不可用时才轮得到它
	IsBackup bool `json:"is_backup"`
	// RTT 面板到该节点的 TCP 握手耗时（毫秒），-1 = 不可达
	RTT       int   `json:"rtt"`
	LastProbe int64 `json:"last_probe"`

	Token       string `json:"token"`        // 一次性，注册后作废
	SessionKey  string `json:"session_key"`  // 注册后换发的长期凭证
	Fingerprint string `json:"fingerprint"`

	ConfigVersion int64 `json:"config_version"`
	LastSeen      int64 `json:"last_seen"`
	LastCycleAt   int64 `json:"last_cycle_at"`
	CreatedAt     int64 `json:"created_at"`
}

type Rule struct {
	ID         int64  `json:"id"`
	GroupID    int64  `json:"group_id"`
	Name       string `json:"name"`        // 备注名，批量导入导出时带上
	ListenPort int    `json:"listen_port"` // 节点上的监听端口
	TargetHost string `json:"target_host"` // 转发目标，所有节点共用同一份
	TargetPort int    `json:"target_port"`
	Protocol   string `json:"protocol"`
	Enabled    bool   `json:"enabled"`
	// 旧字段，保留仅为兼容历史数据，不再使用
	ExitPort int `json:"exit_port,omitempty"`
}

const (
	RoleEntry = "entry" // 旧值，兼容
	RoleExit  = "exit"  // 旧值，兼容
)

type Event struct {
	ID       int64  `json:"id"`
	GroupID  int64  `json:"group_id"`
	FromNode int64  `json:"from_node"`
	ToNode   int64  `json:"to_node"`
	Reason   string `json:"reason"`
	At       int64  `json:"at"`
}

type Alert struct {
	ID      int64  `json:"id"`
	GroupID int64  `json:"group_id"`
	NodeID  int64  `json:"node_id"`
	Level   string `json:"level"`
	Message string `json:"message"`
	Acked   bool   `json:"acked"`
	At      int64  `json:"at"`
}

type User struct {
	Username  string `json:"username"`
	PassHash  string `json:"pass_hash"`
	CreatedAt int64  `json:"created_at"`
}

type Ball struct {
	Users  []User  `json:"users"`
	Groups []Group `json:"groups"`
	Nodes  []Node  `json:"nodes"`
	Rules  []Rule  `json:"rules"`
	Events []Event `json:"events"`
	Alerts []Alert `json:"alerts"`
	Creds  []Cred  `json:"creds"`
	Seq    int64   `json:"seq"`
}

// Cred 一台可以被面板远程安装的服务器凭据。
//
// 密码是明文存的（data.json 权限 0600）——面板要能重复登录就必须拿到原文。
// 这份文件本来就是面板的最高权限，谁读到它谁就能改面板，加密也只是防一眼看，
// 所以不做假安全：写清楚，靠文件权限和机器权限兜。
type Cred struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	IP       string `json:"ip"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	// HostKey 首次连接时记下的主机指纹。之后指纹变了就拒绝连接，
	// 防止中途被换成另一台机器把密码骗走。
	HostKey string `json:"host_key"`
	UsedAt  int64  `json:"used_at"`
}

type Store struct {
	mu   sync.RWMutex
	path string
	db   Ball
	// onAlert 记完告警后回调，用来推企业微信。
	// 只在锁外调用，回调里可以安全地再查 store。
	onAlert func(Alert)
}

// SetAlertHook 设置告警回调。传 nil 表示不推送。
func (s *Store) SetAlertHook(fn func(Alert)) {
	s.mu.Lock()
	s.onAlert = fn
	s.mu.Unlock()
}

func OpenStore(path string) (*Store, error) {
	s := &Store{path: path}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &s.db); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// save 调用方必须已持锁
func (s *Store) save() error {
	b, err := json.MarshalIndent(&s.db, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) nextID() int64 {
	s.db.Seq++
	return s.db.Seq
}

// ---------- 读 ----------

func (s *Store) Groups() []Group {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Group, len(s.db.Groups))
	copy(out, s.db.Groups)
	return out
}

func (s *Store) Group(id int64) *Group {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.db.Groups {
		if s.db.Groups[i].ID == id {
			g := s.db.Groups[i]
			return &g
		}
	}
	return nil
}

func (s *Store) Nodes(groupID int64) []Node {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Node, 0)
	for _, n := range s.db.Nodes {
		if n.GroupID == groupID {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SortOrder < out[j].SortOrder })
	return out
}

func (s *Store) AllNodes() []Node {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Node, len(s.db.Nodes))
	copy(out, s.db.Nodes)
	return out
}

// Exits 返回该分组下所有转发节点，按 sort_order 升序。
// 名称沿用 exit 是为了少改调用方；语义现在是「可轮换的转发节点」。
func (s *Store) Exits(groupID int64) []Node {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Node, 0)
	for _, n := range s.db.Nodes {
		if n.GroupID != groupID {
			continue
		}
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SortOrder < out[j].SortOrder })
	return out
}

// Entries 已废弃：现在所有节点都是转发节点，没有入口/出口之分。
func (s *Store) Entries(groupID int64) []Node { return nil }

func (s *Store) Node(id int64) *Node {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.db.Nodes {
		if s.db.Nodes[i].ID == id {
			n := s.db.Nodes[i]
			return &n
		}
	}
	return nil
}

func (s *Store) CurrentNode(groupID int64) *Node {
	g := s.Group(groupID)
	if g == nil || g.CurrentNode == 0 {
		return nil
	}
	return s.Node(g.CurrentNode)
}

func (s *Store) Rules(groupID int64) []Rule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Rule, 0)
	for _, r := range s.db.Rules {
		if r.GroupID == groupID {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ListenPort < out[j].ListenPort })
	return out
}

func (s *Store) Rule(id int64) *Rule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.db.Rules {
		if s.db.Rules[i].ID == id {
			r := s.db.Rules[i]
			return &r
		}
	}
	return nil
}

func (s *Store) Events(groupID int64, limit int) []Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Event, 0)
	for i := len(s.db.Events) - 1; i >= 0; i-- {
		e := s.db.Events[i]
		if groupID == 0 || e.GroupID == groupID {
			out = append(out, e)
			if len(out) >= limit {
				break
			}
		}
	}
	return out
}

func (s *Store) Alerts(onlyOpen bool) []Alert {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Alert, 0)
	for i := len(s.db.Alerts) - 1; i >= 0; i-- {
		a := s.db.Alerts[i]
		if !onlyOpen || !a.Acked {
			out = append(out, a)
		}
		if len(out) >= 200 {
			break
		}
	}
	return out
}

// ---------- 写 ----------

func (s *Store) CreateGroup(g Group) (Group, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.db.Groups {
		if x.Name == g.Name {
			return Group{}, errors.New("分组名已存在")
		}
	}
	g.ID = s.nextID()
	if g.CreatedAt == 0 {
		g.CreatedAt = time.Now().Unix()
	}
	if g.DNSTTL <= 0 {
		g.DNSTTL = 60
	}
	if g.ProbePort < 0 {
		g.ProbePort = 0
	}
	// ProbePort == 0 表示不探活（完全信任节点心跳）
	if g.ResetDay < 1 || g.ResetDay > 28 {
		g.ResetDay = 1
	}
	if g.PeakBps <= 0 {
		g.PeakBps = 25_000_000 // 200Mbps / 8
	}
	s.db.Groups = append(s.db.Groups, g)
	return g, s.save()
}

func (s *Store) UpdateGroup(g Group) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.db.Groups {
		if s.db.Groups[i].ID == g.ID {
			s.db.Groups[i] = g
			return s.save()
		}
	}
	return errors.New("分组不存在")
}

func (s *Store) DeleteGroup(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ks []int64
	for _, r := range s.db.Rules {
		if r.GroupID == id {
			ks = append(ks, r.ID)
		}
	}
	_ = ks
	fg := s.db.Groups[:0]
	for _, g := range s.db.Groups {
		if g.ID != id {
			fg = append(fg, g)
		}
	}
	s.db.Groups = fg
	fn := s.db.Nodes[:0]
	for _, n := range s.db.Nodes {
		if n.GroupID != id {
			fn = append(fn, n)
		}
	}
	s.db.Nodes = fn
	fr := s.db.Rules[:0]
	for _, r := range s.db.Rules {
		if r.GroupID != id {
			fr = append(fr, r)
		}
	}
	s.db.Rules = fr
	return s.save()
}

func (s *Store) CreateNode(n Node) (Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.db.Nodes {
		if x.GroupID == n.GroupID && x.IP == n.IP {
			return Node{}, errors.New("该分组下已有相同 IP 的节点")
		}
	}
	n.ID = s.nextID()
	if n.CreatedAt == 0 {
		n.CreatedAt = time.Now().Unix()
	}
	if n.LastCycleAt == 0 {
		n.LastCycleAt = time.Now().Unix()
	}
	if n.Iface == "" {
		n.Iface = "eth0"
	}
	if n.BillMode == "" {
		// 中转卖流量基本都是只算出向，双向会把入向也算进去、比商家后台多一倍
		n.BillMode = "tx"
	}
	n.Status = StatusPending
	n.SessionKey = ""
	n.ConfigVersion = 1
	s.db.Nodes = append(s.db.Nodes, n)
	return n, s.save()
}

func (s *Store) UpdateNode(n Node) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.db.Nodes {
		if s.db.Nodes[i].ID == n.ID {
			// 不允许通过这个口改身份字段
			n.Token = s.db.Nodes[i].Token
			n.SessionKey = s.db.Nodes[i].SessionKey
			n.Fingerprint = s.db.Nodes[i].Fingerprint
			n.NicReported = s.db.Nodes[i].NicReported
			n.RelayReported = s.db.Nodes[i].RelayReported
			n.CycleStartNic = s.db.Nodes[i].CycleStartNic
			// 账本字段只由 AcceptBeat/ResetNodeAccounting 写。
			// 编辑节点时拿到的是一份旧副本，不挡回去会把心跳刚写进去的
			// 用量和构建标识覆盖成上一秒的值。
			n.UsedBytes = s.db.Nodes[i].UsedBytes
			n.AccountMigrated = s.db.Nodes[i].AccountMigrated
			n.AgentStamp = s.db.Nodes[i].AgentStamp
			// 实时观测值同理，编辑不该把它们清成 0
			n.RxBps = s.db.Nodes[i].RxBps
			n.TxBps = s.db.Nodes[i].TxBps
			n.CPUPct = s.db.Nodes[i].CPUPct
			n.MemPct = s.db.Nodes[i].MemPct
			n.DiskPct = s.db.Nodes[i].DiskPct
			n.FirstSeen = s.db.Nodes[i].FirstSeen
			n.ProbeTarget = s.db.Nodes[i].ProbeTarget
			n.ProbeOK = s.db.Nodes[i].ProbeOK
			n.ProbeDone = s.db.Nodes[i].ProbeDone
			n.ProbeFails = s.db.Nodes[i].ProbeFails
			n.ProbeAt = s.db.Nodes[i].ProbeAt
			n.ProbeRTT = s.db.Nodes[i].ProbeRTT
			n.ProbeErr = s.db.Nodes[i].ProbeErr
			n.Unreachable = s.db.Nodes[i].Unreachable
			s.db.Nodes[i] = n
			return s.save()
		}
	}
	return errors.New("节点不存在")
}

func (s *Store) DeleteNode(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.db.Nodes[:0]
	for _, n := range s.db.Nodes {
		if n.ID != id {
			out = append(out, n)
		}
	}
	s.db.Nodes = out
	return s.save()
}

// BumpConfigVersion 规则变动后让该组所有节点重新拉配置
func (s *Store) BumpConfigVersion(groupID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.db.Nodes {
		if s.db.Nodes[i].GroupID == groupID {
			s.db.Nodes[i].ConfigVersion++
		}
	}
	_ = s.save()
}

func (s *Store) CreateRule(r Rule) (Rule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.db.Rules {
		if x.GroupID == r.GroupID && x.ListenPort == r.ListenPort && x.Protocol == r.Protocol {
			return Rule{}, errors.New("该分组下端口+协议已被占用")
		}
	}
	r.ID = s.nextID()
	if r.Protocol == "" {
		r.Protocol = "tcp"
	}
	s.db.Rules = append(s.db.Rules, r)
	for i := range s.db.Nodes {
		if s.db.Nodes[i].GroupID == r.GroupID {
			s.db.Nodes[i].ConfigVersion++
		}
	}
	return r, s.save()
}

func (s *Store) UpdateRule(r Rule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 改端口后不能和同组其他线路撞车，否则节点上监听会失败
	for _, x := range s.db.Rules {
		if x.ID != r.ID && x.GroupID == r.GroupID &&
			x.ListenPort == r.ListenPort && x.Protocol == r.Protocol {
			return errors.New("该端口已被同组其他线路占用")
		}
	}
	for i := range s.db.Rules {
		if s.db.Rules[i].ID == r.ID {
			s.db.Rules[i] = r
			for j := range s.db.Nodes {
				if s.db.Nodes[j].GroupID == r.GroupID {
					s.db.Nodes[j].ConfigVersion++
				}
			}
			return s.save()
		}
	}
	return errors.New("规则不存在")
}

func (s *Store) DeleteRule(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var gid int64
	out := s.db.Rules[:0]
	for _, r := range s.db.Rules {
		if r.ID == id {
			gid = r.GroupID
			continue
		}
		out = append(out, r)
	}
	s.db.Rules = out
	if gid != 0 {
		for i := range s.db.Nodes {
			if s.db.Nodes[i].GroupID == gid {
				s.db.Nodes[i].ConfigVersion++
			}
		}
	}
	return s.save()
}

// ---------- 远程安装用的服务器凭据 ----------

// Creds 返回全部凭据（含密码，仅供服务端内部使用）
func (s *Store) Creds() []Cred {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Cred, len(s.db.Creds))
	copy(out, s.db.Creds)
	return out
}

// CredByIP 按 IP 找已记住的凭据，用来实现「密码填一次就够了」
func (s *Store) CredByIP(ip string) *Cred {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.db.Creds {
		if s.db.Creds[i].IP == ip {
			c := s.db.Creds[i]
			return &c
		}
	}
	return nil
}

// LastCred 返回最近用过的一条带密码的凭据。
// 用途是「所有服务器同一个 SSH 密码」：新机器留空就直接复用这一条。
func (s *Store) LastCred() *Cred {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var best *Cred
	for i := range s.db.Creds {
		if s.db.Creds[i].Password == "" {
			continue
		}
		if best == nil || s.db.Creds[i].UsedAt > best.UsedAt {
			c := s.db.Creds[i]
			best = &c
		}
	}
	return best
}

// SaveCred 按 IP 覆盖写入，没有就新增
func (s *Store) SaveCred(c Cred) (Cred, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c.UsedAt = time.Now().Unix()
	for i := range s.db.Creds {
		if s.db.Creds[i].IP == c.IP {
			c.ID = s.db.Creds[i].ID
			// 指纹沿用旧的，除非这次拿到了新的
			if c.HostKey == "" {
				c.HostKey = s.db.Creds[i].HostKey
			}
			s.db.Creds[i] = c
			return c, s.save()
		}
	}
	c.ID = s.nextID()
	s.db.Creds = append(s.db.Creds, c)
	return c, s.save()
}

// DeleteCred 忘记某台服务器的密码
func (s *Store) DeleteCred(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.db.Creds[:0]
	for _, c := range s.db.Creds {
		if c.ID != id {
			out = append(out, c)
		}
	}
	s.db.Creds = out
	return s.save()
}

// ---------- Agent 侧 ----------

// AuthStats 实时指标的鉴权：只校验，不兑换 token、不写库。
// 这条路径每两秒就走一次，不能碰 AuthAgent 里那套 token → session_key 的兑换，
// 否则节点还没收到新 key 就被换掉了，之后所有请求全 401。
func (s *Store) AuthStats(key, fp string) (int64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.db.Nodes {
		n := &s.db.Nodes[i]
		switch {
		case n.SessionKey != "" && n.SessionKey == key:
			// 正常情况：注册后用的都是 session_key
		case strings.HasPrefix(n.Token, "used-"):
			continue // 已兑换过，旧 token 作废
		case n.Token == key:
			// 还没走完第一次心跳，先放行
		default:
			continue
		}
		if n.Fingerprint != "" && fp != "" && n.Fingerprint != fp {
			return 0, false
		}
		return n.ID, true
	}
	return 0, false
}

// probeFailLimit 连续三次不通才认定节点不能干活。
// 跟节点端 probeMaxFails 对应：一次抖动不算故障。
const probeFailLimit = 3

// Metrics 一次 /agent/stats 带回来的观测值。
type Metrics struct {
	RxBps   int64
	TxBps   int64
	CPUPct  int
	MemPct  int
	DiskPct int
	Iface   string

	ProbeTarget string
	ProbeOK     bool
	ProbeDone   bool
	ProbeFails  int
	ProbeAt     int64
	ProbeRTT    int64
	ProbeErr    string
}

// UpdateMetrics 只更新展示用的实时字段，不落盘。
// 两秒一次的高频上报如果每次都写 JSON，磁盘会被无意义地刷。
// 下一次心跳（15 秒）会把内存里的值顺带写进 data.json。
//
// 返回 true 表示这一轮把节点判成了「上游不通」，调用方要立刻让调度器巡检。
func (s *Store) UpdateMetrics(nodeID int64, m Metrics) (becameUnreachable bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.db.Nodes {
		if s.db.Nodes[i].ID != nodeID {
			continue
		}
		n := &s.db.Nodes[i]
		n.RxBps, n.TxBps = m.RxBps, m.TxBps
		n.CPUPct, n.MemPct, n.DiskPct = m.CPUPct, m.MemPct, m.DiskPct
		if m.Iface != "" {
			n.Iface = m.Iface
		}

		n.ProbeTarget = m.ProbeTarget
		n.ProbeOK, n.ProbeDone = m.ProbeOK, m.ProbeDone
		n.ProbeFails = m.ProbeFails
		n.ProbeAt, n.ProbeRTT, n.ProbeErr = m.ProbeAt, m.ProbeRTT, m.ProbeErr

		// 连续三次不通：这个节点接不了流量了，标成待恢复等人工处理。
		// 只标一次——之后它每分钟都会继续报不通，不重复告警也不反复切。
		// 注意：探通了也不自动解封，这是有意的，避免在临界状态来回切。
		if m.ProbeFails >= probeFailLimit && !n.Unreachable {
			n.Unreachable = true
			n.Status = StatusUnreachable
			becameUnreachable = true
		}

		now := time.Now().Unix()
		if n.FirstSeen == 0 {
			n.FirstSeen = now
		}
		n.LastSeen = now // 掉线判断也跟着这个走，不用等下一次心跳
		return
	}
	return false
}

// RecoverNode 人工恢复：解除「上游不通」的封禁，重新参与调度。
// 不自动切回当前节点——恢复只是让它重新变成可用候选。
func (s *Store) RecoverNode(nodeID int64) *Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.db.Nodes {
		n := &s.db.Nodes[i]
		if n.ID != nodeID {
			continue
		}
		n.Unreachable = false
		n.ProbeFails = 0
		n.ProbeErr = ""
		if n.Status == StatusUnreachable {
			n.Status = StatusStandby
		}
		_ = s.save()
		out := *n
		return &out
	}
	return nil
}

// AuthAgent 用 token 或 session_key 认证，token 首次使用时换发 session_key
func (s *Store) AuthAgent(key, fp string) (nodeID int64, sessionKey string, fresh bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.db.Nodes {
		n := &s.db.Nodes[i]
		if n.Token != key && n.SessionKey != key {
			continue
		}
		// token 首次使用：换 key、作废 token、绑指纹
		if n.Token == key {
			n.SessionKey = randHex(24)
			n.Token = "used-" + randHex(8)
			if n.Fingerprint == "" {
				n.Fingerprint = fp
			}
			fresh = true
			sessionKey = n.SessionKey
		}
		// 指纹不一致：拒绝（换机器必须后台重绑）
		if n.Fingerprint != "" && fp != "" && n.Fingerprint != fp {
			return 0, "", false, errors.New("fingerprint_mismatch")
		}
		if n.Status == StatusPending {
			n.Status = StatusStandby
		}
		// 心跳能到，说明节点活着：把之前误判的 dead 拉回来
		if n.Status == StatusDead {
			n.Status = StatusStandby
		}
		n.LastSeen = time.Now().Unix()
		_ = s.save()
		return n.ID, sessionKey, fresh, nil
	}
	return 0, "", false, errors.New("token_invalid")
}

// ResetNodeKey 重新生成对接 token，清掉会话和指纹，让节点可以重新接入。
// 节点原来的 session_key 会立刻失效，需要重装。
func (s *Store) ResetNodeKey(id int64) *Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.db.Nodes {
		if s.db.Nodes[i].ID != id {
			continue
		}
		s.db.Nodes[i].Token = randHex(24)
		s.db.Nodes[i].SessionKey = ""
		s.db.Nodes[i].Fingerprint = ""
		s.db.Nodes[i].Status = StatusPending
		s.db.Nodes[i].LastSeen = 0
		_ = s.save()
		n := s.db.Nodes[i]
		return &n
	}
	return nil
}

// AcceptBeat 处理一次心跳上报，返回需要下发的配置版本。
//
// 用量只有一个算法：本周期已用 = 节点上报的网卡累计 - 周期起点。
//
// 旧版本是「面板自己攒增量」(used += nicAcc - lastNic)，看着等价，实际上
// 只要节点侧的计数器重置过一次（改计费口径、换网卡、重装、/var/lib/relay/nic.state
// 丢了），两边的基准就永久错位：要么用量一直不动，要么一次把网卡开机以来的
// 全部流量算进来。改成减法后，节点那个单调累加值是唯一真相，面板只负责
// 记起点，天然不会两本账打架。
// Beat 一次心跳带回来的观测值。
// 用结构体而不是一长串位置参数：字段只会越加越多，按位置传迟早接错。
type Beat struct {
	NodeID   int64
	NicAcc   int64
	RelayAcc int64
	Version  int64
	Agent    string
	RxBps    int64 // 实时下行（字节/秒）
	TxBps    int64 // 实时上行（字节/秒）
	CPUPct   int
	MemPct   int
	DiskPct  int
}

func (s *Store) AcceptBeat(b Beat) (wantVersion int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.db.Nodes {
		n := &s.db.Nodes[i]
		if n.ID != b.NodeID {
			continue
		}
		nicAcc := b.NicAcc

		// 账本初始化。
		// 旧版本用增量累加、库里已经有数的节点：用「已上报 - 已用」反推周期起点，
		// 保证升级这一刻面板上的数字不跳。
		// 全新节点（还没上报过，两个值都是 0）：第一次心跳只对齐起点。
		if !n.AccountMigrated {
			if n.NicReported > 0 || n.UsedBytes > 0 {
				start := n.NicReported - n.UsedBytes
				if start < 0 {
					start = 0
				}
				n.CycleStartNic = start
			} else {
				n.NeedAlign = true
			}
			n.AccountMigrated = true
		}

		// 首跳、或刚换网卡/改口径：把当前位置当周期起点。
		// 不能从 0 起算，否则网卡（或新计数器）开机以来跑了多少就一次性算多少。
		if n.NeedAlign {
			n.CycleStartNic = nicAcc
			n.NeedAlign = false
		}

		// 节点计数器归零（重装 / 删了 state 文件 / 换了计数器）：
		// 起点跟着归零，从 0 重新开始记，不倒扣也不跳涨。
		if nicAcc < n.CycleStartNic {
			n.CycleStartNic = 0
		}

		n.NicReported = nicAcc
		n.RelayReported = b.RelayAcc
		// 实时速率和负载：只用于展示，不参与计费。
		n.RxBps, n.TxBps = b.RxBps, b.TxBps
		n.CPUPct, n.MemPct, n.DiskPct = b.CPUPct, b.MemPct, b.DiskPct
		if used := nicAcc - n.CycleStartNic; used > 0 {
			n.UsedBytes = used
		} else {
			n.UsedBytes = 0
		}
		if b.Agent != "" {
			n.AgentStamp = b.Agent
		}
		now := time.Now().Unix()
		if n.FirstSeen == 0 {
			n.FirstSeen = now // 面板上「在线 X 天」从这里算
		}
		n.LastSeen = now
		// 心跳到达说明在线，pending/dead 都拉回 standby
		switch n.Status {
		case StatusPending, StatusDead:
			n.Status = StatusStandby
		}
		_ = s.save()
		return n.ConfigVersion
	}
	return 0
}

// ResetNodeAccounting 把节点这侧的账归零。
// 换网卡 / 改计费口径时调：节点端的累计值也跟着从 0 重新算，
// 面板要是留着旧值，显示的数字就再也对不上服务器了。
func (s *Store) ResetNodeAccounting(nodeID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.db.Nodes {
		if s.db.Nodes[i].ID == nodeID {
			s.db.Nodes[i].CycleStartNic = 0
			s.db.Nodes[i].NicReported = 0
			s.db.Nodes[i].UsedBytes = 0
			s.db.Nodes[i].NeedAlign = true
			s.db.Nodes[i].LastCycleAt = time.Now().Unix()
			_ = s.save()
			return
		}
	}
}

// SetStatus / SetCurrentNode 等小操作
// SetNodeIface 记录节点实际在用的网卡，避免面板反复下发错误配置
func (s *Store) SetNodeIface(nodeID int64, iface string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.db.Nodes {
		if s.db.Nodes[i].ID == nodeID {
			s.db.Nodes[i].Iface = iface
			_ = s.save()
			return
		}
	}
}

// SetRTTs 批量更新延迟，只落盘一次
// SetNodeIP 更新节点公网 IP，返回旧值
func (s *Store) SetNodeIP(nodeID int64, ip string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.db.Nodes {
		if s.db.Nodes[i].ID == nodeID {
			old := s.db.Nodes[i].IP
			if old == ip {
				return old
			}
			s.db.Nodes[i].IP = ip
			_ = s.save()
			return old
		}
	}
	return ""
}

func (s *Store) SetRTTs(rtts map[int64]int) {
	if len(rtts) == 0 {
		return
	}
	now := time.Now().Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.db.Nodes {
		if v, ok := rtts[s.db.Nodes[i].ID]; ok {
			s.db.Nodes[i].RTT = v
			s.db.Nodes[i].LastProbe = now
		}
	}
	_ = s.save()
}

func (s *Store) SetStatus(nodeID int64, st string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.db.Nodes {
		if s.db.Nodes[i].ID == nodeID {
			s.db.Nodes[i].Status = st
			_ = s.save()
			return
		}
	}
}

// SetGroupLowNodes 记下「只剩两台」这个告警发过没有
func (s *Store) SetGroupLowNodes(groupID int64, v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.db.Groups {
		if s.db.Groups[i].ID == groupID {
			if s.db.Groups[i].LowNodes == v {
				return
			}
			s.db.Groups[i].LowNodes = v
			_ = s.save()
			return
		}
	}
}

// LockGroup 手动切换后锁定一段时间，期间调度器不自动切走
func (s *Store) LockGroup(groupID int64, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.db.Groups {
		if s.db.Groups[i].ID == groupID {
			s.db.Groups[i].ManualLockUntil = time.Now().Add(d).Unix()
			_ = s.save()
			return
		}
	}
}

func (s *Store) SetCurrentNode(groupID, nodeID int64) {	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.db.Groups {
		if s.db.Groups[i].ID == groupID {
			s.db.Groups[i].CurrentNode = nodeID
			s.db.Groups[i].LastSwitch = time.Now().Unix()
		}
	}
	for i := range s.db.Nodes {
		if s.db.Nodes[i].GroupID == groupID {
			switch s.db.Nodes[i].ID {
			case nodeID:
				s.db.Nodes[i].Status = StatusOnline
			default:
				if s.db.Nodes[i].Status == StatusOnline {
					s.db.Nodes[i].Status = StatusStandby
				}
			}
			// 所有节点配置完全一致，切节点只改 DNS，不需要重发配置
		}
	}
	_ = s.save()
}

func (s *Store) Audit(groupID, from, to int64, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db.Events = append(s.db.Events, Event{
		ID: s.nextID(), GroupID: groupID, FromNode: from, ToNode: to,
		Reason: reason, At: time.Now().Unix(),
	})
	if len(s.db.Events) > 5000 {
		s.db.Events = s.db.Events[len(s.db.Events)-5000:]
	}
	_ = s.save()
}

func (s *Store) Alert(groupID, nodeID int64, level, msg string) {
	s.mu.Lock()
	// 同内容 10 分钟内不重复
	for i := len(s.db.Alerts) - 1; i >= 0 && i > len(s.db.Alerts)-20; i-- {
		a := s.db.Alerts[i]
		if a.Message == msg && a.NodeID == nodeID && time.Now().Unix()-a.At < 600 {
			s.mu.Unlock()
			return
		}
	}
	a := Alert{
		ID: s.nextID(), GroupID: groupID, NodeID: nodeID,
		Level: level, Message: msg, At: time.Now().Unix(),
	}
	s.db.Alerts = append(s.db.Alerts, a)
	if len(s.db.Alerts) > 1000 {
		s.db.Alerts = s.db.Alerts[len(s.db.Alerts)-1000:]
	}
	_ = s.save()
	hook := s.onAlert
	s.mu.Unlock()

	// 推送放到锁外：企微那边超时不能把整个面板卡住，
	// 回调里也可能反过来查 store（查分组名），持锁调就是死锁。
	if hook != nil {
		hook(a)
	}
}

func (s *Store) AckAlert(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.db.Alerts {
		if s.db.Alerts[i].ID == id {
			s.db.Alerts[i].Acked = true
		}
	}
	_ = s.save()
}

// ResetNodeCycle 手动清零本周期用量：把周期起点对齐到当前网卡累计值。
func (s *Store) ResetNodeCycle(nodeID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.db.Nodes {
		if s.db.Nodes[i].ID == nodeID {
			s.db.Nodes[i].CycleStartNic = s.db.Nodes[i].NicReported
			s.db.Nodes[i].UsedBytes = 0
			s.db.Nodes[i].AccountMigrated = true
			s.db.Nodes[i].LastCycleAt = time.Now().Unix()
			if s.db.Nodes[i].Status == StatusExhausted {
				s.db.Nodes[i].Status = StatusStandby
			}
			_ = s.save()
			return
		}
	}
}

// CycleReset 月度重置：把周期起点对齐到当前网卡累计值
func (s *Store) CycleReset(nodeID int64) {
	s.ResetNodeCycle(nodeID)
}
