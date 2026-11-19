//go:build !linux

// language: Go 1.23, file: internal/node/stats_other.go
// 非 Linux 平台（本机 Windows/macOS 调试）没有 /proc，资源占用一律报 0。
// 这些数字只在面板上显示，不影响转发和计费，所以缺了不报错。
package node

type cpuTimes struct {
	idle  uint64
	total uint64
}

type SysSampler struct {
	last   cpuTimes
	primed bool
}

func (s *SysSampler) CPU() int { return 0 }

func MemPct() int { return 0 }

func DiskPct() int { return 0 }

func Uptime() int64 { return 0 }
