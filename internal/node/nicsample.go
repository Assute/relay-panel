// language: Go 1.23, file: internal/node/nicsample.go
// 网卡流量采样。口径与商家后台一致（商家读的就是这个计数器）。
package node

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"sync"
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
	state string
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
		n.base = 0 // 换网卡，重新对齐
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

// Sample 返回单调递增的累计字节数
func (n *NicMeter) Sample() uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()

	rx, tx, err := NicCounters(n.iface)
	if err != nil {
		return n.acc
	}
	var raw uint64
	switch n.mode {
	case "tx":
		raw = tx
	case "rx":
		raw = rx
	default:
		raw = rx + tx
	}

	if raw >= n.base {
		n.acc += raw - n.base
	} else {
		n.acc += raw // 计数器归零（重启 / 网卡重载），从零重算
	}
	n.base = raw
	return n.acc
}

func (n *NicMeter) Restore() {
	b, err := os.ReadFile(n.state)
	if err != nil {
		return
	}
	if v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64); err == nil {
		n.mu.Lock()
		n.acc, n.base = v, 0
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
