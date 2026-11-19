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
)

type Group struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Remark      string `json:"remark"`

	QuotaBytes int64 `json:"quota_bytes"` // 0 = 不限
	PeakBps    int64 `json:"peak_bps"`    // 预估峰值字节/秒，算切换余量用
	ProbePort  int   `json:"probe_port"`  // 探活端口

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
	UsedBytes     int64 `json:"used_bytes"`      // 本周期已用
	CycleStartNic int64 `json:"cycle_start_nic"` // 周期起点的网卡累计值
	NicReported   int64 `json:"nic_reported"`    // Agent 上报的网卡累计（单调）
	RelayReported int64 `json:"relay_reported"`  // Agent 上报的转发累计（单调）

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
	Seq    int64   `json:"seq"`
}

type Store struct {
	mu   sync.RWMutex
	path string
	db   Ball
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
		n.BillMode = "both"
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

// ---------- Agent 侧 ----------

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

// AcceptBeat 处理一次心跳上报，返回需要下发的配置版本
func (s *Store) AcceptBeat(nodeID, nicAcc, relayAcc, ver int64) (wantVersion int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.db.Nodes {
		n := &s.db.Nodes[i]
		if n.ID != nodeID {
			continue
		}
		delta := nicAcc - n.NicReported
		if delta < 0 {
			delta = 0 // Agent 重装 / 换机，不倒扣
		}
		n.NicReported = nicAcc
		n.RelayReported = relayAcc
		if delta > 0 {
			n.UsedBytes += delta
		}
		n.LastSeen = time.Now().Unix()
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

// SetStatus / SetCurrentNode 等小操作
// SetRTTs 批量更新延迟，只落盘一次
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
	defer s.mu.Unlock()
	// 同内容 10 分钟内不重复
	for i := len(s.db.Alerts) - 1; i >= 0 && i > len(s.db.Alerts)-20; i-- {
		a := s.db.Alerts[i]
		if a.Message == msg && a.NodeID == nodeID && time.Now().Unix()-a.At < 600 {
			return
		}
	}
	s.db.Alerts = append(s.db.Alerts, Alert{
		ID: s.nextID(), GroupID: groupID, NodeID: nodeID,
		Level: level, Message: msg, At: time.Now().Unix(),
	})
	if len(s.db.Alerts) > 1000 {
		s.db.Alerts = s.db.Alerts[len(s.db.Alerts)-1000:]
	}
	_ = s.save()
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

// ResetNodeCycle 手动清零本周期用量
func (s *Store) ResetNodeCycle(nodeID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.db.Nodes {
		if s.db.Nodes[i].ID == nodeID {
			s.db.Nodes[i].CycleStartNic = s.db.Nodes[i].NicReported
			s.db.Nodes[i].UsedBytes = 0
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
