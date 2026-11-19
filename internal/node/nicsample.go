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
	//
	// 但 n.mode 为空表示「刚启动、还不知道上次用的是哪个口径」
	// （老版本 state 文件没记口径）。这种只补上口径，不能清 acc——
	// 否则每次重启都会被当成「用户改了口径」，历史用量全丢。
	if mode != "" && n.mode != "" && mode != n.mode {
		n.base = 0
		n.acc = 0
		n.ready = false
	}
	if mode != "" {
		n.mode = mode
	}
}

func (n *NicMeter) Iface() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.iface
}

// Mode 返回当前计费口径（both / tx / rx），空表示还没确定
func (n *NicMeter) Mode() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.mode
}

// Acc 返回本进程累计的字节数，也就是落盘的那个值。
func (n *NicMeter) Acc() uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.acc
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

// Restore 从磁盘恢复累计值，以及上次用的计费口径。
//
// 口径必须一起恢复：进程重启时 NewNicMeter 把 mode 初始化成 "both"，
// 而面板下发的可能是 "tx"——SetIface 看到不一样就当成「用户改了口径」，
// 直接把 acc 清零。结果就是每重启一次丢一次历史用量，
// 面板上表现为「更新完流量被清零了」。
func (n *NicMeter) Restore() {
	b, err := os.ReadFile(n.state)
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	v, err := strconv.ParseUint(strings.TrimSpace(lines[0]), 10, 64)
	if err != nil {
		return
	}
	// 老格式只有一行数字，没有口径记录：留空表示「未知」，
	// 下一次 SetIface 只补口径，不清 acc。
	mode := ""
	if len(lines) > 1 {
		mode = strings.TrimSpace(lines[1])
	}
	n.mu.Lock()
	n.acc, n.base = v, 0
	n.mode = mode
	n.ready = false
	n.mu.Unlock()
}

// Persist 把累计值和计费口径一起落盘。
// 格式：第一行字节数，第二行口径（tx/rx/both）。
func (n *NicMeter) Persist() error {
	n.mu.Lock()
	v := n.acc
	mode := n.mode
	path := n.state
	n.mu.Unlock()
	_ = os.MkdirAll(dirOf(path), 0o755)
	body := strconv.FormatUint(v, 10)
	if mode != "" {
		body += "\n" + mode
	}
	return os.WriteFile(path, []byte(body), 0o600)
}

func dirOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i > 0 {
		return p[:i]
	}
	return "."
}
