// language: Go 1.23, file: internal/node/nicsample.go
// 网卡流量采样。口径与商家后台一致（商家读的就是这个计数器）。
package node

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// NicCounters 读 /proc/net/dev，返回指定网卡的累计接收/发送字节。
func NicCounters(iface string) (rx, tx uint64, err error) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		i := strings.IndexByte(line, ':')
		if i < 0 {
			continue
		}
		if strings.TrimSpace(line[:i]) != iface {
			continue
		}
		flds := strings.Fields(line[i+1:])
		if len(flds) < 9 {
			return 0, 0, os.ErrInvalid
		}
		rx, _ = strconv.ParseUint(flds[0], 10, 64) // 第 1 列 rx_bytes
		tx, _ = strconv.ParseUint(flds[8], 10, 64) // 第 9 列 tx_bytes
		return rx, tx, nil
	}
	return 0, 0, os.ErrNotExist
}

// ListIfaces 列出所有网卡名（首次配错网卡时方便排查）
func ListIfaces() []string {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		i := strings.IndexByte(line, ':')
		if i < 0 {
			continue
		}
		name := strings.TrimSpace(line[:i])
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

// NicMeter 把"重启会归零的绝对值"转成"跨重启单调递增的累计值"。
type NicMeter struct {
	mu    sync.Mutex
	iface string
	mode  string // tx | rx | both
	base  uint64
	acc   uint64
	ready bool // 基准是否已对齐；换网卡/换口径后置 false
	state string

	// 实时速率用：上一次的原始计数器值和采样时刻。
	// 累计量和速率是两回事，速率永远取 rx/tx 原始值，不受计费口径影响。
	lastRx, lastTx uint64
	lastAt         time.Time
	rxBps, txBps   int64
}

// PickBusiestIface 挑当前流量最大的非回环网卡。
// 网卡命名各发行版不同（eth0 / ens5 / enp0s3 / venet0），
// 面板里填错的代价是流量完全不统计，所以给个自动兜底。
func PickBusiestIface() string {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return ""
	}
	defer f.Close()

	var best string
	var maxBytes uint64
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		i := strings.IndexByte(line, ':')
		if i < 0 {
			continue
		}
		name := strings.TrimSpace(line[:i])
		if name == "" || name == "lo" {
			continue
		}
		flds := strings.Fields(line[i+1:])
		if len(flds) < 9 {
			continue
		}
		rx, _ := strconv.ParseUint(flds[0], 10, 64)
		tx, _ := strconv.ParseUint(flds[8], 10, 64)
		if sum := rx + tx; sum > maxBytes {
			maxBytes, best = sum, name
		}
	}
	return best
}

func NewNicMeter(iface, mode, stateDir string) *NicMeter {
	if mode == "" {
		mode = "both"
	}
	if iface == "" {
		iface = "eth0"
	}
	return &NicMeter{
		iface: iface,
		mode:  mode,
		state: stateDir + "/nic.state",
	}
}

func (n *NicMeter) SetIface(iface, mode string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if iface != "" && iface != n.iface {
		n.iface = iface
		n.base = 0
		n.ready = false // 换网卡，下次采样只对齐
		n.lastAt = time.Time{} // 速率基准也一起重来，否则第一次差值会算成几 MB/s
		n.lastRx, n.lastTx = 0, 0
		n.rxBps, n.txBps = 0, 0
	}
	// 改计费口径会让 raw 突变（both -> tx 时砍掉一半），
	// base 和 acc 都归零重新起算，否则下一次 Sample 会把整个
	// tx 累计值当成增量加进去，用量瞬间多算几十 GB。
	// 面板侧的 UsedBytes 是独立累加的，不受影响。
	if mode != "" && mode != n.mode {
		n.mode = mode
		n.base = 0
		n.acc = 0
		n.ready = false
	}
}

func (n *NicMeter) Iface() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.iface
}

// Mode 返回当前计费口径（both / tx / rx）
func (n *NicMeter) Mode() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.mode
}

// Sample 返回单调递增的累计字节数
func (n *NicMeter) Sample() uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()

	rx, tx, err := NicCounters(n.iface)
	if err != nil {
		return n.acc
	}
	n.updateRate(rx, tx)

	var raw uint64
	switch n.mode {
	case "tx":
		raw = tx
	case "rx":
		raw = rx
	default:
		raw = rx + tx
	}

	// 首次采样、刚换网卡、刚换口径：只对齐基准，绝不累加。
	// 否则会把网卡开机以来的累计值整块算进本周期用量 —— 表现为
	// 「面板用量每秒涨几 GB」，而且每轮心跳都会重来一次。
	if !n.ready {
		n.base = raw
		n.ready = true
		return n.acc
	}

	if raw >= n.base {
		n.acc += raw - n.base
	} else {
		n.acc += raw // 计数器归零（重启 / 网卡重载），从零重算
	}
	n.base = raw
	return n.acc
}

// updateRate 按两次采样之间的时间差算实时速率（字节/秒）。
// 调用方必须持有 n.mu。计数器归零时差值会变负，直接按 0 处理。
func (n *NicMeter) updateRate(rx, tx uint64) {
	now := time.Now()
	if !n.lastAt.IsZero() {
		if d := now.Sub(n.lastAt).Seconds(); d >= 0.5 {
			n.rxBps = rateOf(rx, n.lastRx, d)
			n.txBps = rateOf(tx, n.lastTx, d)
		}
	}
	n.lastRx, n.lastTx, n.lastAt = rx, tx, now
}

func rateOf(cur, prev uint64, seconds float64) int64 {
	if cur < prev {
		return 0 // 计数器归零 / 换了网卡
	}
	return int64(float64(cur-prev) / seconds)
}

// Rates 返回最近一次采样算出来的上下行速率（字节/秒），面板显示用。
func (n *NicMeter) Rates() (rxBps, txBps int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.rxBps, n.txBps
}

func (n *NicMeter) Restore() {
	b, err := os.ReadFile(n.state)
	if err != nil {
		return
	}
	if v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64); err == nil {
		n.mu.Lock()
		n.acc, n.base = v, 0
		n.ready = false
		n.mu.Unlock()
	}
}

func (n *NicMeter) Persist() error {
	n.mu.Lock()
	v := n.acc
	path := n.state
	n.mu.Unlock()
	_ = os.MkdirAll(dirOf(path), 0o755)
	return os.WriteFile(path, []byte(strconv.FormatUint(v, 10)), 0o600)
}

func dirOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i > 0 {
		return p[:i]
	}
	return "."
}
