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
	bytes  atomic.Int64
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

	go func() {
		defer wg.Done()
		n, _ := io.Copy(up, c)
		cur.bytes.Add(n)
		f.acc.Add(n)
		closeWrite(up)
	}()
	go func() {
		defer wg.Done()
		n, _ := io.Copy(c, up)
		cur.bytes.Add(n)
		f.acc.Add(n)
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

// Stats 返回每个监听端口的连接数和字节数（调试用）
func (f *Forwarder) Stats() map[int]map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[int]map[string]any{}
	for port, v := range f.m {
		out[port] = map[string]any{
			"target": v.getTarget(),
			"conns":  v.conns.Load(),
			"bytes":  v.bytes.Load(),
		}
	}
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
