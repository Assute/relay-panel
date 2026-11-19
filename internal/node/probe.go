// language: Go 1.23, file: internal/node/probe.go
// 上游连通性探针：每分钟拨一次 TCP，连续三次不通就报给面板。
//
// 为什么在节点上测，而不是面板测：
// 面板测的是「面板 → 节点」，节点测的是「节点 → 上游」。
// 中转节点最常见的故障不是机器死掉，而是机器活得好好的、
// 到回源的链路断了——那种情况下心跳照发、网卡照涨，
// 面板从外部完全看不出来，只有从节点自己这边拨号才知道。
package node

import (
	"log"
	"net"
	"sync"
	"time"
)

const (
	probeInterval = 60 * time.Second // 正常情况下一分钟一次
	probeTimeout  = 3 * time.Second  // 单次拨号超时
	probeRetryGap = 2 * time.Second  // 超时后隔 2 秒再试
	probeMaxFails = 3                // 连续三次都超时才认定不通
)

// Prober 记录连续失败次数和最近一次结果。
// 面板下发新的探测目标时立刻重测一轮，不用等满一分钟。
type Prober struct {
	mu       sync.RWMutex
	target   string
	gen      uint64 // 目标版本号：每换一次 +1，用来判断「这一轮跑到一半目标被换了」
	changed  chan struct{}
	retryGap time.Duration
	timeout  time.Duration

	fails  int
	ok     bool
	done   bool // 是否已经出过结果（启动瞬间还没测过）
	at     int64
	rttMs  int64
	errMsg string
}

func NewProber() *Prober {
	return &Prober{
		changed:  make(chan struct{}, 1),
		retryGap: probeRetryGap,
		timeout:  probeTimeout,
	}
}

// SetTarget 更新探测目标（host:port）。值变了会打断当前这一轮并立刻重测。
func (p *Prober) SetTarget(t string) {
	p.mu.Lock()
	changed := p.target != t
	p.target = t
	if changed {
		// 换了目标，旧的连续次数作废
		p.fails = 0
		p.gen++
	}
	p.mu.Unlock()
	if changed {
		select {
		case p.changed <- struct{}{}:
		default:
		}
	}
}

// generation 当前目标版本。round 用它判断本轮是否已经作废。
func (p *Prober) generation() uint64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.gen
}

func (p *Prober) Target() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.target
}

// Status 返回给面板的观测值。
func (p *Prober) Status() (ok bool, done bool, fails int, at int64, rttMs int64, target, errMsg string) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.ok, p.done, p.fails, p.at, p.rttMs, p.target, p.errMsg
}

func (p *Prober) Run() {
	p.round() // 启动先测一次，别干等一分钟

	t := time.NewTicker(probeInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			p.round()
		case <-p.changed:
			p.round()
		}
	}
}

// round 一轮探测：最多三次，中间隔 2 秒。
// 任意一次成功就清零连续失败次数——「后面任意一次正常就重置」。
//
// 注意这里按版本号判断目标是否变更，而不是去读 changed 通道：
// 通道里的信号可能来自更早的一次下发，读它会把别人的信号吃掉，
// 让本轮误以为「目标在中途变了」而提前退出。
func (p *Prober) round() {
	target := p.Target()
	gen := p.generation()
	if target == "" {
		p.mu.Lock()
		p.fails, p.ok, p.done, p.errMsg = 0, true, false, ""
		p.mu.Unlock()
		return
	}

	for i := 0; i < probeMaxFails; i++ {
		if i > 0 {
			time.Sleep(p.retryGap)
			if p.generation() != gen {
				return // 目标换了，这一轮作废，新一轮马上开始
			}
		}
		rtt, err := dialTCP(target, p.timeout)
		if err == nil {
			p.record(true, rtt, "")
			return
		}
		p.record(false, 0, err.Error())
	}
}

func (p *Prober) record(ok bool, rtt time.Duration, errMsg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done = true
	p.at = time.Now().Unix()
	if ok {
		// 之前是不通、现在通了：打一行，方便在 journalctl 里对上时间点
		wasBad := p.fails > 0 || !p.ok
		p.ok, p.fails, p.rttMs, p.errMsg = true, 0, rtt.Milliseconds(), ""
		if wasBad {
			log.Printf("连通性恢复: %s %dms", p.target, rtt.Milliseconds())
		}
		return
	}
	p.ok, p.rttMs, p.errMsg = false, 0, errMsg
	p.fails++
	log.Printf("连通性探测失败 %d/%d: %s (%s)", p.fails, probeMaxFails, p.target, errMsg)
}

func dialTCP(addr string, timeout time.Duration) (time.Duration, error) {
	start := time.Now()
	c, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return 0, err
	}
	_ = c.Close()
	return time.Since(start), nil
}
