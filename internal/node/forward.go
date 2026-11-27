// language: Go 1.23, file: internal/node/forward.go
// TCP 转发器。自己实现，不依赖 gost，静态编译单文件。
//
// 关键设计：转发目标是 atomic 的，可以热改。
// 出口一换，面板下发新配置，entry 上的监听端口不动，
// 只有新连接去新目标，存量连接继续跑完 —— 客户完全无感。
package node

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type Rule struct {
	ListenPort int    `json:"listen_port"`
	TargetHost string `json:"target_host"`
	TargetPort int    `json:"target_port"`
	Protocol   string `json:"protocol"`
}

type fwd struct {
	port   int
	target atomic.Pointer[string]
	ln     net.Listener
	// rx / tx 分开记：商家按出向(tx)计费，面板上要能看出「哪条规则吃流量」，
	// 混成一个总数就失去意义了。
	rx     atomic.Int64
	tx     atomic.Int64
	conns  atomic.Int64
	closed atomic.Bool
}

func (fd *fwd) setTarget(t string) { fd.target.Store(&t) }

func (fd *fwd) getTarget() string {
	if p := fd.target.Load(); p != nil {
		return *p
	}
	return ""
}

type Forwarder struct {
	mu  sync.Mutex
	m   map[int]*fwd // port -> fwd
	acc atomic.Int64 // 累计转发字节（跨规则重启也单调）
}

func NewForwarder() *Forwarder {
	return &Forwarder{m: map[int]*fwd{}}
}

// Sync 把监听集合对齐到 rules。
// 端口不变只改目标时不重开 listener，只原子替换 target。
func (f *Forwarder) Sync(rules []Rule) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	want := map[int]string{}
	for _, r := range rules {
		if r.Protocol != "" && r.Protocol != "tcp" {
			log.Printf("跳过不支持的协议 %s (端口 %d)", r.Protocol, r.ListenPort)
			continue
		}
		if r.ListenPort <= 0 || r.TargetHost == "" || r.TargetPort <= 0 {
			continue
		}
		want[r.ListenPort] = net.JoinHostPort(r.TargetHost, strconv.Itoa(r.TargetPort))
	}

	// 关掉不再需要的端口
	for port, cur := range f.m {
		if _, ok := want[port]; !ok {
			cur.closed.Store(true)
			_ = cur.ln.Close()
			delete(f.m, port)
			log.Printf("停止转发 :%d", port)
		}
	}

	for port, tgt := range want {
		if cur, ok := f.m[port]; ok {
			if old := cur.getTarget(); old != tgt {
				cur.setTarget(tgt)
				log.Printf("热切换 :%d  %s -> %s（存量连接不受影响）", port, old, tgt)
			}
			continue
		}
		ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
		if err != nil {
			return fmt.Errorf("监听 :%d 失败: %w", port, err)
		}
		cur := &fwd{port: port, ln: ln}
		cur.setTarget(tgt)
		f.m[port] = cur
		go f.serve(cur)
		log.Printf("开始转发 :%d -> %s", port, tgt)
	}
	return nil
}

func (f *Forwarder) serve(cur *fwd) {
	for {
		c, err := cur.ln.Accept()
		if err != nil {
			if cur.closed.Load() || errors.Is(err, net.ErrClosed) {
				return
			}
			log.Printf(": %d accept: %v", cur.port, err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go f.handle(cur, c)
	}
}

func (f *Forwarder) handle(cur *fwd, c net.Conn) {
	defer c.Close()
	cur.conns.Add(1)
	defer cur.conns.Add(-1)

	// 建连瞬间读一次目标：这条连接从头到尾走同一台出口
	tgt := cur.getTarget()
	if tgt == "" {
		return
	}
	up, err := net.DialTimeout("tcp", tgt, 5*time.Second)
	if err != nil {
		return
	}
	defer up.Close()

	var wg sync.WaitGroup
	wg.Add(2)

	// 必须边转边计，不能等 io.Copy 返回。
	// io.Copy 是阻塞的，字节数要到连接断开那一刻才抛出来；
	// 一条持续几小时的隧道在期间全不计数，
	// 面板上就会出现「节点 115 Mbps、规则加起来 3 Mbps」这种对不上的情况。
	go func() {
		defer wg.Done()
		_, _ = io.Copy(meterWriter{w: up, n: &cur.rx, acc: &f.acc}, c) // 客户端 -> 上游（入向）
		closeWrite(up)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(meterWriter{w: c, n: &cur.tx, acc: &f.acc}, up) // 上游 -> 客户端（出向）
		closeWrite(c)
	}()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(12 * time.Hour):
		_ = c.Close()
		_ = up.Close()
		<-done
	}
}

func closeWrite(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
}

// BytesRead 累计转发字节数
func (f *Forwarder) BytesRead() int64 { return f.acc.Load() }

// PortStat 一个监听端口的快照。字节数是累计值（单调增），
// 速率由调用方两次采样求差得到。
type PortStat struct {
	Port   int    `json:"port"`
	Target string `json:"target"`
	Rx     int64  `json:"-"`
	Tx     int64  `json:"-"`
	Conns  int64  `json:"conns"`
}

// PortStats 返回每个监听端口的累计收发量和当前连接数。
func (f *Forwarder) PortStats() []PortStat {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]PortStat, 0, len(f.m))
	for port, v := range f.m {
		out = append(out, PortStat{
			Port:   port,
			Target: v.getTarget(),
			Rx:     v.rx.Load(),
			Tx:     v.tx.Load(),
			Conns:  v.conns.Load(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}

// ActiveConns 当前活跃连接总数
func (f *Forwarder) ActiveConns() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, v := range f.m {
		n += v.conns.Load()
	}
	return n
}

// meterWriter 边写边累加，让规则速率是实时的。
// 直接用 io.Copy 的返回值只能在连接结束时拿到总数。
type meterWriter struct {
	w   io.Writer
	n   *atomic.Int64 // 这条规则在这个方向上的累计
	acc *atomic.Int64 // 整个转发器的累计（RelayAcc）
}

func (m meterWriter) Write(p []byte) (int, error) {
	k, err := m.w.Write(p)
	if k > 0 {
		m.n.Add(int64(k))
		if m.acc != nil {
			m.acc.Add(int64(k))
		}
	}
	return k, err
}
