// language: Go 1.23, file: internal/panel/scheduler.go
// 定时任务：节点健康检查 + 配额巡检 + 切流 + 月度重置。
//
// 切流的两个触发条件：
//   1. 当前节点用量到阈值（正常轮换）
//   2. 当前节点失联（心跳超时）—— 不等用量，立刻切走
package panel

import (
	"fmt"
	"log"
	"net"
	"sync"
	"time"
)

const (
	// Agent 15s 一次心跳，75 秒没动静算失联
	heartbeatTimeout = 75
	// 延迟探测间隔
	rttInterval = 120
)

type Scheduler struct {
	store *Store
	dns   DNSProvider
	cnf   *Config
	kick  chan struct{}
}

func NewScheduler(s *Store, dns DNSProvider, c *Config) *Scheduler {
	return &Scheduler{store: s, dns: dns, cnf: c, kick: make(chan struct{}, 1)}
}

// Kick 让面板在增删节点/规则后立刻巡检一次，不用等下一个周期
func (s *Scheduler) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func (s *Scheduler) Run() {
	time.Sleep(3 * time.Second)
	s.tick()

	t30 := time.NewTicker(30 * time.Second)
	defer t30.Stop()
	t1h := time.NewTicker(time.Hour)
	defer t1h.Stop()

	for {
		select {
		case <-t30.C:
			s.tick()
		case <-s.kick:
			s.tick()
		case <-t1h.C:
			s.cycleTick()
		}
	}
}

func (s *Scheduler) tick() {
	for _, g := range s.store.Groups() {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("group %d tick panic: %v", g.ID, r)
				}
			}()
			s.tickGroup(&g)
		}()
	}
}

// switchAt 返回触发正常轮换的用量比例。
// 入口换了目标后新连接立刻走新出口，但老连接还在旧节点上跑到自然结束，
// 余量要覆盖这段尾巴：按峰值 × 10 分钟估，至少留 5%。
func switchAt(n *Node, g *Group) float64 {
	if n.QuotaBytes <= 0 {
		return 1.0
	}
	reserve := float64(n.QuotaBytes) * 0.05
	if tail := float64(g.PeakBps) * 600; tail > reserve {
		reserve = tail
	}
	ratio := 1 - reserve/float64(n.QuotaBytes)
	if ratio < 0.5 {
		ratio = 0.5
	}
	return ratio
}

// isDead 判断节点是否失联。
// 只有「从来没连上过」和「心跳正常」算活着，心跳超时一律算死。
func (s *Scheduler) isDead(n *Node, g *Group) bool {
	if n.LastSeen == 0 {
		return false // pending，从没连过，交给候选过滤处理
	}
	if time.Now().Unix()-n.LastSeen <= heartbeatTimeout {
		return false
	}
	// 心跳超时了。有探活端口就再确认一次：能连上说明是 Agent 挂了或面板侧网络问题
	if g.ProbePort > 0 {
		return !probeTCP(n.IP, g.ProbePort, 3*time.Second)
	}
	return true
}

// usable 能不能当候选
func usable(n Node) bool {
	if n.IP == "" {
		return false // 还没上报过 IP，不知道域名该指向哪
	}
	if n.Unreachable {
		return false // 上游不通，等人工恢复
	}
	switch n.Status {
	case StatusDisabled, StatusExhausted, StatusDraining, StatusUnreachable:
		return false
	}
	return true
}

// candidates 按轮换顺序排好的候选列表
func candidates(nodes []Node, cur *Node) []Node {
	var tail, head, backup []Node
	for _, n := range nodes {
		if cur != nil && n.ID == cur.ID {
			continue
		}
		if !usable(n) {
			continue
		}
		switch {
		case n.IsBackup:
			backup = append(backup, n)
		case cur == nil || n.SortOrder > cur.SortOrder:
			tail = append(tail, n) // 当前节点后面的
		default:
			head = append(head, n) // 前面的：后面都不可用时才回头用
		}
	}
	// 顺序：后面的 → 前面的（循环）→ 备用
	return append(append(tail, head...), backup...)
}

func (s *Scheduler) tickGroup(g *Group) {
	nodes := s.store.Exits(g.ID)
	if len(nodes) == 0 {
		return
	}

	// 顺手更新延迟（限频，不阻塞主流程）
	s.probeRTTs(g, nodes)

	// 全组都没心跳：多半是面板自己的网络出了问题，别乱切
	alive := 0
	for _, n := range nodes {
		if n.LastSeen > 0 && time.Now().Unix()-n.LastSeen <= heartbeatTimeout {
			alive++
		}
	}
	if alive == 0 && len(nodes) > 0 {
		allNeverConnected := true
		for _, n := range nodes {
			if n.LastSeen > 0 {
				allNeverConnected = false
				break
			}
		}
		if !allNeverConnected {
			s.store.Alert(g.ID, 0, "error", "所有节点心跳都超时，疑似面板侧网络问题，暂不切换")
			return
		}
	}

	cur := s.store.CurrentNode(g.ID)

	// 可用节点只剩两台：提前预警，别等到最后一台用完才发现没得切。
	// 只看本地状态（usable + 心跳），不在这里发 TCP 探测，
	// 那是 probeRTTs 的活，挂在每次巡检上会把 30 秒周期撑爆。
	now := time.Now().Unix()
	avail := 0
	for i := range nodes {
		n := &nodes[i]
		if !usable(*n) || n.LastSeen == 0 {
			continue
		}
		if now-n.LastSeen > heartbeatTimeout {
			continue
		}
		avail++
	}
	if avail <= 2 {
		if !g.LowNodes {
			s.store.Alert(g.ID, 0, "warn",
				fmt.Sprintf("分组「%s」可用节点只剩 %d 台，用完就没得切了", g.Name, avail))
			s.store.SetGroupLowNodes(g.ID, true)
		}
	} else if g.LowNodes {
		s.store.SetGroupLowNodes(g.ID, false)
	}

	// 1) 当前节点自检报「上游不通」：最先处理。
	// 这种情况机器还活着、心跳照发、网卡还在涨，只有节点自己拨号才知道，
	// 所以不能等 isDead——那永远不会成立。
	if cur != nil && cur.Unreachable {
		log.Printf("group %d: 节点 %d (%s) 上游不通，切走", g.ID, cur.ID, cur.Name)
		s.switchTo(g, cur, nodes, "unreachable")
		return
	}

	// 2) 当前节点失联：不等用量，立刻切
	if cur != nil && s.isDead(cur, g) {
		log.Printf("group %d: 节点 %d (%s) 心跳超时，立即切换", g.ID, cur.ID, cur.IP)
		s.store.SetStatus(cur.ID, StatusDead)
		s.store.Alert(g.ID, cur.ID, "error", "节点失联（心跳超时），已自动切走")
		s.switchTo(g, cur, nodes, "dead")
		return
	}

	// 2) 还没分配当前节点
	if cur == nil {
		s.switchTo(g, nil, nodes, "init")
		return
	}

	// 3) 用量到阈值：正常轮换
	//
	// 配额 0 表示不限量，这种节点永远不该因为「用量」被切走。
	// 原来直接比 used < quota*ratio，quota=0 时变成 0 < 0 = false，
	// 于是每一轮都被判成「到线了」——不限量的节点会被反复切来切去。
	if cur.QuotaBytes <= 0 {
		return
	}
	if float64(cur.UsedBytes) < float64(cur.QuotaBytes)*switchAt(cur, g) {
		return
	}
	// 用户手工切过来的，锁定期内不自动切走
	if lock := g.ManualLockUntil; lock > time.Now().Unix() {
		return
	}
	s.switchTo(g, cur, nodes, "quota")
}

// switchTo 从候选里挑一台把域名切过去。任何一步失败都保持原样，等下一轮重试。
func (s *Scheduler) switchTo(g *Group, from *Node, nodes []Node, reason string) {
	cands := candidates(nodes, from)
	if len(cands) == 0 {
		// 新建的节点还没心跳过、IP 未知，等它们上来就行，不算故障
		pending := 0
		for _, n := range nodes {
			if n.IP == "" {
				pending++
			}
		}
		if pending == len(nodes) {
			return
		}
		s.store.Alert(g.ID, 0, "error", "分组内没有可用节点")
		return
	}

	for _, cand := range cands {
		if cand.LastSeen == 0 && reason != "init" {
			continue // 从没连过的节点不拿去接流量
		}
		if s.isDead(&cand, g) {
			s.store.SetStatus(cand.ID, StatusDead)
			s.store.Alert(g.ID, cand.ID, "error", "探活失败，已剔除")
			continue
		}
		if g.ProbePort > 0 && !probeTCP(cand.IP, g.ProbePort, 3*time.Second) {
			log.Printf("group %d: node %d probe failed", g.ID, cand.ID)
			s.store.SetStatus(cand.ID, StatusDead)
			s.store.Alert(g.ID, cand.ID, "error", "探活失败，已剔除")
			continue
		}

		// 改 DNS。失败就不动 current，下一个 tick 自动重试
		if err := s.dns.SetA(g, cand.IP, g.DNSTTL); err != nil {
			log.Printf("group %d: dns update failed: %v", g.ID, err)
			s.store.Alert(g.ID, 0, "error", "DNS 更新失败: "+err.Error())
			return
		}

		var fromID int64
		if from != nil {
			fromID = from.ID
			// 上游不通的节点保持 unreachable：draining 会在 30 分钟后变 exhausted，
			// 那样人工恢复之后它还回不来。
			if !from.Unreachable {
				s.store.SetStatus(from.ID, StatusDraining)
				old := from.ID
				// 旧节点 30 分钟后标记收尾完成：给存量长连接时间自然结束
				time.AfterFunc(30*time.Minute, func() {
					if n := s.store.Node(old); n != nil && n.Status == StatusDraining {
						s.store.SetStatus(old, StatusExhausted)
					}
				})
			}
		}
		s.store.SetCurrentNode(g.ID, cand.ID)
		s.store.Audit(g.ID, fromID, cand.ID, reason)

		tag := ""
		if cand.IsBackup {
			tag = " [备用]"
		}
		log.Printf("group %d switch(%s): %v -> node %d (%s)%s",
			g.ID, reason, fromName(from), cand.ID, cand.IP, tag)

		switch reason {
		case "quota":
			s.store.Alert(g.ID, cand.ID, "info", "配额到线，已切到 "+cand.Name)
		case "dead":
			s.store.Alert(g.ID, cand.ID, "warn", "上一台失联，已切到 "+cand.Name+tag)
		case "unreachable":
			s.store.Alert(g.ID, cand.ID, "warn", "上一台连不上上游，已切到 "+cand.Name+tag+"（原节点需人工恢复）")
		}
		return
	}

	s.store.Alert(g.ID, 0, "error", "分组内已无可用节点，域名停在原处")
}

func fromName(n *Node) any {
	if n == nil {
		return "—"
	}
	return n.Name
}

// probeRTTs 并发测一轮延迟，每个节点 2 分钟一次
func (s *Scheduler) probeRTTs(g *Group, nodes []Node) {
	if g.ProbePort <= 0 {
		return
	}
	now := time.Now().Unix()
	var todo []Node
	for _, n := range nodes {
		if now-n.LastProbe >= rttInterval {
			todo = append(todo, n)
		}
	}
	if len(todo) == 0 {
		return
	}

	res := make(map[int64]int, len(todo))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, n := range todo {
		wg.Add(1)
		go func(n Node) {
			defer wg.Done()
			rtt := measureRTT(n.IP, g.ProbePort, 2*time.Second)
			mu.Lock()
			res[n.ID] = rtt
			mu.Unlock()
		}(n)
	}
	wg.Wait()
	s.store.SetRTTs(res)
}

// measureRTT 返回 TCP 握手毫秒数，失败返回 -1。
// 本机 loopback 常常不足 1ms，向上取整到 1，避免和「还没测过」(0) 混淆。
func measureRTT(host string, port int, timeout time.Duration) int {
	addr := net.JoinHostPort(host, itoa(port))
	start := time.Now()
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return -1
	}
	d := time.Since(start)
	_ = conn.Close()
	ms := int(d.Milliseconds())
	if ms < 1 {
		ms = 1
	}
	return ms
}

// cycleTick 月度重置。用「上次重置时间 + 一个月」判断，面板中途停机也不会漏。
func (s *Scheduler) cycleTick() {
	now := time.Now()
	for _, n := range s.store.AllNodes() {
		g := s.store.Group(n.GroupID)
		if g == nil {
			continue
		}
		base := n.LastCycleAt
		if base == 0 {
			base = n.CreatedAt
		}
		bt := time.Unix(base, 0)
		next := nextCycleStart(bt, g.ResetDay)
		if now.Before(next) {
			continue
		}
		s.store.CycleReset(n.ID)
		log.Printf("node %d: 周期重置，已用量清零", n.ID)
		s.store.Alert(n.GroupID, n.ID, "info", "流量周期重置，本周期用量已清零")
	}
}

func nextCycleStart(anchor time.Time, resetDay int) time.Time {
	if resetDay < 1 {
		resetDay = 1
	}
	if resetDay > 28 {
		resetDay = 28
	}
	y, m := anchor.Year(), anchor.Month()
	cand := time.Date(y, m, resetDay, 0, 10, 0, 0, anchor.Location())
	if !cand.After(anchor) {
		cand = time.Date(y, m+1, resetDay, 0, 10, 0, 0, anchor.Location())
	}
	return cand
}
