//go:build linux

// language: Go 1.23, file: internal/node/stats_linux.go
// 节点机器的资源占用：CPU / 内存 / 磁盘 / 开机时长。
// 全部读 /proc 和 statfs，不引入任何依赖，也不跑外部命令。
package node

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// CPU 占用需要两次采样之间的差值，所以采样器得记住上一次的累计值。
type cpuTimes struct {
	idle  uint64
	total uint64
}

// SysSampler 保存上一次 CPU 累计值，用于算两次心跳之间的平均占用。
type SysSampler struct {
	last   cpuTimes
	primed bool
}

// CPU 返回上次采样以来的平均 CPU 占用百分比。
// 第一次调用只建立基准，返回 0。
func (s *SysSampler) CPU() int {
	cur, ok := readCPUTimes()
	if !ok {
		return 0
	}
	if !s.primed {
		s.last, s.primed = cur, true
		return 0
	}
	dTotal := float64(cur.total - s.last.total)
	dIdle := float64(cur.idle - s.last.idle)
	s.last = cur
	if dTotal <= 0 {
		return 0
	}
	pct := (dTotal - dIdle) / dTotal * 100
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return int(pct + 0.5)
}

func readCPUTimes() (cpuTimes, bool) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuTimes{}, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		f := strings.Fields(line)[1:]
		if len(f) < 5 {
			return cpuTimes{}, false
		}
		// user nice system idle iowait irq softirq steal
		var sum uint64
		for i := 0; i < len(f) && i < 8; i++ {
			v, _ := strconv.ParseUint(f[i], 10, 64)
			sum += v
		}
		idle, _ := strconv.ParseUint(f[3], 10, 64)
		iowait, _ := strconv.ParseUint(f[4], 10, 64)
		return cpuTimes{idle: idle + iowait, total: sum}, true
	}
	return cpuTimes{}, false
}

// MemPct 内存占用：按 MemAvailable 算，跟 free/htop 的口径一致。
func MemPct() int {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	var total, avail uint64
	for _, line := range strings.Split(string(b), "\n") {
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			total = meminfoValue(line)
		case strings.HasPrefix(line, "MemAvailable:"):
			avail = meminfoValue(line)
		}
	}
	if total == 0 {
		return 0
	}
	used := total - avail
	if avail > total { // 老内核没有 MemAvailable 时会拿到 0
		used = 0
	}
	return int(float64(used)/float64(total)*100 + 0.5)
}

func meminfoValue(line string) uint64 {
	f := strings.Fields(line)
	if len(f) < 2 {
		return 0
	}
	v, _ := strconv.ParseUint(f[1], 10, 64)
	return v
}

// DiskPct 根分区占用。用 Bfree 而不是 Bavail：面板看的是「盘用了多少」，
// 不是「root 还能写多少」，两者差的是给 root 保留的那 5%。
func DiskPct() int {
	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err != nil {
		return 0
	}
	if st.Blocks == 0 {
		return 0
	}
	used := st.Blocks - st.Bfree
	return int(float64(used)/float64(st.Blocks)*100 + 0.5)
}

// Uptime 开机时长（秒），面板上显示成「在线 2 天」。
func Uptime() int64 {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0
	}
	return int64(v)
}
