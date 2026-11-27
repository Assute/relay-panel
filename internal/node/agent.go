// language: Go 1.23, file: internal/node/agent.go
package node

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type agentConfig struct {
	Version int64       `json:"version"`
	Rules   []Rule      `json:"rules"`
	// ProbeTarget 上游连通性探测目标（host:port），面板按规则算好下发。
	// 空字符串表示不探（还没配规则）。
	ProbeTarget string `json:"probe_target,omitempty"`
}

type beatReq struct {
	Key         string `json:"key"`
	Fingerprint string `json:"fingerprint"`
	Iface       string `json:"iface"`
	NicAcc      int64  `json:"nic_acc"`
	// CycleStart 本周期起点。面板删掉节点记录再加回来时靠它把用量接上，
	// 所以这个值必须跟着 acc 一起存、一起报。
	CycleStart int64  `json:"cycle_start"`
	RelayAcc   int64  `json:"relay_acc"`
	Version    int64  `json:"version"`
	Agent      string `json:"agent"` // 构建标识，面板上直接显示
	RxBps      int64  `json:"rx_bps"`
	TxBps      int64  `json:"tx_bps"`
	CPUPct     int    `json:"cpu_pct"`
	MemPct     int    `json:"mem_pct"`
	DiskPct    int    `json:"disk_pct"`
}

type beatResp struct {
	OK         bool   `json:"ok"`
	Err        string `json:"err,omitempty"`
	SessionKey string `json:"session_key,omitempty"`
	Iface      string `json:"iface,omitempty"`
	BillMode   string `json:"bill_mode,omitempty"`
	// CycleStart 非 0 表示面板要求把本周期起点改成这个值（首次对齐 / 周期重置）
	CycleStart *int64 `json:"cycle_start,omitempty"`
	// ProbeTarget 每次都带（不像规则那样只在版本变化时下发）：
	// 探测目标跟着规则走，改一条规则就该立刻换目标重测。
	ProbeTarget string       `json:"probe_target"`
	Config      *agentConfig `json:"config,omitempty"`
}

// statsReq 实时指标快通道：只有展示用的数字，不动账本也不要配置。
type statsReq struct {
	Key         string `json:"key"`
	Fingerprint string `json:"fingerprint"`
	Iface       string `json:"iface"`
	RxBps       int64  `json:"rx_bps"`
	TxBps       int64  `json:"tx_bps"`
	CPUPct      int    `json:"cpu_pct"`
	MemPct      int    `json:"mem_pct"`
	DiskPct     int    `json:"disk_pct"`

	// 上游连通性探测结果，面板据此判断这个节点还能不能干活
	ProbeTarget string `json:"probe_target"`
	ProbeOK     bool   `json:"probe_ok"`
	ProbeDone   bool   `json:"probe_done"`
	ProbeFails  int    `json:"probe_fails"`
	ProbeAt     int64  `json:"probe_at"`
	ProbeRTT    int64  `json:"probe_rtt_ms"`
	ProbeErr    string `json:"probe_err"`

	// Rules 每条规则的实时速率，用来看「这台机器的带宽被哪条规则吃了」
	Rules []ruleStat `json:"rules,omitempty"`
}

// ruleStat 单条转发规则的实时状态
type ruleStat struct {
	Port   int    `json:"port"`
	Target string `json:"target"`
	RxBps  int64  `json:"rx_bps"`
	TxBps  int64  `json:"tx_bps"`
	Conns  int64  `json:"conns"`
}

type Agent struct {
	cnf   *Config
	meter *NicMeter
	fwd   *Forwarder

	// 按规则算速率需要上一次的累计值和时刻
	ruleMu   sync.Mutex
	rulePrev map[int][2]int64 // port -> {rx, tx}
	ruleAt   time.Time
	probe *Prober
	keyMu sync.RWMutex
	key   string
	ver   atomic.Int64
	hc    *http.Client
	fp    string
	sys   SysSampler
}

func (a *Agent) currentKey() string {
	a.keyMu.RLock()
	defer a.keyMu.RUnlock()
	return a.key
}

func (a *Agent) setKey(k string) {
	a.keyMu.Lock()
	a.key = k
	a.keyMu.Unlock()
}

func NewAgent(c *Config, m *NicMeter, f *Forwarder) *Agent {
	return &Agent{
		cnf:   c,
		meter: m,
		fwd:   f,
		probe: NewProber(),
		key:   c.LoadKey(),
		hc:    &http.Client{Timeout: 20 * time.Second},
		fp:    Fingerprint(),
	}
}

// ExitFatal 凭证被面板拒绝时的退出码。
// systemd unit 里配了 RestartPreventExitStatus=2：这种情况重启也没用，
// 只会把 journal 刷满，不如停下来等人到面板上重新添加节点。
const ExitFatal = 2

// Run 一直跑到凭证被拒为止，返回进程退出码。
func (a *Agent) Run() int {
	// 上游连通性探针：默认一分钟一轮，连续三次不通才报给面板
	go a.probe.Run()

	// 速率按 1 秒采样，面板上的数字才有「实时」的感觉。
	// 顺带把账本采样频率提上来，计数器中途归零丢的量也更少。
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for range t.C {
			a.meter.Sample()
		}
	}()

	// 实时指标单独走 2 秒的快通道。心跳 15 秒一次，掺不了这个。
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for range t.C {
			a.pushStats()
		}
	}()

	backoff := time.Second
	for {
		err := a.loop()
		if err != nil {
			if errors.Is(err, errFatal) {
				log.Printf("致命错误，停止: %v", err)
				return ExitFatal
			}
			log.Printf("连接中断: %v，%s 后重试", err, backoff)
		}
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

var errFatal = errors.New("fatal")

// loop 跑一次"心跳会话"，失败就返回让外层退避重连。
func (a *Agent) loop() error {
	// 首轮立刻发一次，之后每 15s
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()

	if err := a.beat(); err != nil {
		return err
	}
	for range tick.C {
		if err := a.beat(); err != nil {
			a.meter.Persist()
			return err
		}
	}
	return nil
}

func (a *Agent) beat() error {
	nicAcc := a.meter.Sample()
	rxBps, txBps := a.meter.Rates()
	req := beatReq{
		Key:         a.currentKey(),
		Fingerprint: a.fp,
		Iface:       a.meter.Iface(),
		NicAcc:      int64(nicAcc),
		CycleStart:  a.meter.CycleStart(),
		RelayAcc:    a.fwd.BytesRead(),
		Version:     a.ver.Load(),
		Agent:       BuildStamp,
		RxBps:       rxBps,
		TxBps:       txBps,
		CPUPct:      a.sys.CPU(),
		MemPct:      MemPct(),
		DiskPct:     DiskPct(),
	}
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequest("POST", a.cnf.Panel+"/agent/beat", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := a.hc.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var br beatResp
	if err := json.Unmarshal(raw, &br); err != nil {
		return fmt.Errorf("面板返回无法解析 (%d): %s", resp.StatusCode, trunc(string(raw), 200))
	}
	if !br.OK {
		switch br.Err {
		case "token_invalid", "fingerprint_mismatch", "node_gone":
			return fmt.Errorf("%w: %s（请到面板重新添加节点）", errFatal, br.Err)
		}
		return fmt.Errorf("面板拒绝: %s", br.Err)
	}

	// token 换成了 session_key，落盘
	if br.SessionKey != "" {
		a.setKey(br.SessionKey)
		if err := a.cnf.SaveKey(br.SessionKey); err != nil {
			log.Printf("保存 session key 失败: %v", err)
		} else {
			log.Printf("注册成功，已保存凭证")
		}
	}

	// 面板算出的周期起点：抄到自己身上，下次它记不住的时候（节点被删了重加）
	// 还能把用量接回去。只在本值和面板给的不一样时才落盘。
	if br.CycleStart != nil && *br.CycleStart != a.meter.CycleStart() {
		a.meter.SetCycleStart(*br.CycleStart)
	}

	// 面板下发的网卡配置
	if br.Iface != "" || br.BillMode != "" {
		before := a.meter.Iface()
		a.meter.SetIface(br.Iface, br.BillMode)
		after := a.meter.Iface()
		if after != before {
			log.Printf("网卡切换: %s -> %s（计费口径 %s）", before, after, a.meter.Mode())
		}
		// 换完立刻验一次：读不到就自动挑一张，否则用量会一直是 0 且没人知道为什么
		if _, _, err := NicCounters(after); err != nil {
			if auto := PickBusiestIface(); auto != "" && auto != after {
				log.Printf("网卡 %q 读不到，自动改测 %q（可用: %v）", after, auto, ListIfaces())
				a.meter.SetIface(auto, "")
			} else {
				log.Printf("警告: 网卡 %q 读不到，当前可用: %v —— 用量将一直是 0", after, ListIfaces())
			}
		}
	}

	// 上游探测目标：面板按规则算好，每次心跳都带。
	// 目标变了立刻重测一轮，不用等下一个一分钟。
	if br.ProbeTarget != a.probe.Target() {
		if br.ProbeTarget == "" {
			log.Printf("连通性探测: 面板未下发目标，暂停探测")
		} else {
			log.Printf("连通性探测目标: %s", br.ProbeTarget)
		}
		a.probe.SetTarget(br.ProbeTarget)
	}

	// 配置有更新
	if br.Config != nil && br.Config.Version > a.ver.Load() {
		if err := a.fwd.Sync(br.Config.Rules); err != nil {
			log.Printf("应用配置 v%d 失败，保持旧配置: %v", br.Config.Version, err)
			return nil // 配置错误不该断连接，下轮再试
		}
		a.ver.Store(br.Config.Version)
		log.Printf("配置 v%d 已应用: %d 条规则", br.Config.Version, len(br.Config.Rules))
	}

	a.meter.Persist()
	return nil
}

// pushStats 每两秒把实时速率和负载推给面板。
// 失败一律静默：这是纯展示用的辅助通道，不能因为它打断主循环。
// key 失效时下一轮心跳会重新注册或退出，这里不用管。
func (a *Agent) pushStats() {
	rx, tx := a.meter.Rates()
	pOK, pDone, pFails, pAt, pRTT, pTarget, pErr := a.probe.Status()
	body, _ := json.Marshal(statsReq{
		Key:         a.currentKey(),
		Fingerprint: a.fp,
		Iface:       a.meter.Iface(),
		RxBps:       rx,
		TxBps:       tx,
		CPUPct:      a.sys.CPU(),
		MemPct:      MemPct(),
		DiskPct:     DiskPct(),
		ProbeTarget: pTarget,
		ProbeOK:     pOK,
		ProbeDone:   pDone,
		ProbeFails:  pFails,
		ProbeAt:     pAt,
		ProbeRTT:    pRTT,
		ProbeErr:    pErr,
		Rules:       a.ruleStats(),
	})
	resp, err := a.hc.Post(a.cnf.Panel+"/agent/stats", "application/json", bytes.NewReader(body))
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
}

// Fingerprint 机器指纹，换机器就变，防止 token 被复用
// ruleStats 把转发器的累计字节换成实时速率（两次采样求差）。
// 没有上一次采样时返回零速率，不能拿累计值当速率。
func (a *Agent) ruleStats() []ruleStat {
	now := time.Now()
	cur := a.fwd.PortStats()

	a.ruleMu.Lock()
	defer a.ruleMu.Unlock()
	if a.rulePrev == nil {
		a.rulePrev = map[int][2]int64{}
	}
	dt := now.Sub(a.ruleAt).Seconds()

	out := make([]ruleStat, 0, len(cur))
	for _, st := range cur {
		prev, seen := a.rulePrev[st.Port]
		var rxBps, txBps int64
		if seen && dt > 0.2 {
			if d := st.Rx - prev[0]; d > 0 {
				rxBps = int64(float64(d) / dt)
			}
			if d := st.Tx - prev[1]; d > 0 {
				txBps = int64(float64(d) / dt)
			}
		}
		a.rulePrev[st.Port] = [2]int64{st.Rx, st.Tx}
		out = append(out, ruleStat{
			Port: st.Port, Target: st.Target,
			RxBps: rxBps, TxBps: txBps, Conns: st.Conns,
		})
	}
	// 规则被删了就把它的旧记录一并清掉
	alive := map[int]bool{}
	for _, st := range cur {
		alive[st.Port] = true
	}
	for port := range a.rulePrev {
		if !alive[port] {
			delete(a.rulePrev, port)
		}
	}
	a.ruleAt = now
	return out
}

func Fingerprint() string {
	h := sha256.New()
	if b, err := os.ReadFile("/etc/machine-id"); err == nil {
		h.Write([]byte(strings.TrimSpace(string(b))))
	} else if b, err := os.ReadFile("/var/lib/dbus/machine-id"); err == nil {
		h.Write([]byte(strings.TrimSpace(string(b))))
	} else {
		if hn, err := os.Hostname(); err == nil {
			h.Write([]byte(hn))
		}
		if ifs, err := net.Interfaces(); err == nil {
			for _, i := range ifs {
				if i.Flags&net.FlagLoopback != 0 || len(i.HardwareAddr) == 0 {
					continue
				}
				h.Write(i.HardwareAddr)
				break
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
