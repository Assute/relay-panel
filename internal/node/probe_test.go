// language: Go 1.23, file: internal/node/probe_test.go
// 探针的三种状态：通、不通、目标变更。
package node

import (
	"net"
	"testing"
	"time"
)

// 上游通：算成功，连续失败清零
func TestProberSuccess(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	p := NewProber()
	p.SetTarget(ln.Addr().String())
	p.round()

	ok, done, fails, at, rtt, target, errMsg := p.Status()
	if !ok || !done {
		t.Fatalf("应该探测成功: ok=%v done=%v err=%q", ok, done, errMsg)
	}
	if fails != 0 {
		t.Fatalf("成功后失败次数应为 0，实际 %d", fails)
	}
	if at == 0 {
		t.Fatal("应该记录探测时间")
	}
	if target != ln.Addr().String() {
		t.Fatalf("目标记录不对: %q", target)
	}
	t.Logf("rtt=%dms", rtt)
}

// 上游不通：连续三次失败累加，每轮最多加 3
func TestProberFails(t *testing.T) {
	// 127.0.0.1:1 没有监听，连接会被立刻拒绝，不用等 3 秒超时
	p := NewProber()
	p.retryGap = 5 * time.Millisecond // 测试不等真 5 秒
	p.timeout = 500 * time.Millisecond
	p.SetTarget("127.0.0.1:1")

	start := time.Now()
	p.round()
	elapsed := time.Since(start)

	ok, done, fails, _, _, _, errMsg := p.Status()
	if ok || !done {
		t.Fatalf("应该探测失败: ok=%v done=%v", ok, done)
	}
	if fails != probeMaxFails {
		t.Fatalf("一轮应该是 %d 次失败，实际 %d", probeMaxFails, fails)
	}
	if errMsg == "" {
		t.Fatal("应该记录失败原因")
	}
	// 三次尝试之间有两段 retryGap 间隔
	if elapsed < 2*p.retryGap {
		t.Fatalf("重试间隔没生效，只用了 %v", elapsed)
	}

	// 再来一轮，计数继续累加（面板靠它判断是否超过阈值）
	p.round()
	if _, _, f, _, _, _, _ := p.Status(); f != 2*probeMaxFails {
		t.Fatalf("第二轮后应为 %d 次，实际 %d", 2*probeMaxFails, f)
	}

	// 成功后清零
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	p.SetTarget(ln.Addr().String()) // 换目标会先清零
	p.round()
	if ok, _, f, _, _, _, _ := p.Status(); !ok || f != 0 {
		t.Fatalf("换到可用目标后应成功且清零: ok=%v fails=%d", ok, f)
	}
}

// 目标被清空（面板没下发）：不探测，也不算失败
func TestProberNoTarget(t *testing.T) {
	p := NewProber()
	p.retryGap = 5 * time.Millisecond
	p.timeout = 500 * time.Millisecond
	p.SetTarget("127.0.0.1:1")
	p.round()
	if _, _, f, _, _, _, _ := p.Status(); f == 0 {
		t.Fatal("前置：应该有失败计数")
	}

	p.SetTarget("") // 面板清空了目标
	if p.Target() != "" {
		t.Fatal("目标没清掉")
	}
	if _, _, f, _, _, _, _ := p.Status(); f != 0 {
		t.Fatalf("换目标应清零，实际 %d", f)
	}
	p.round()
	if ok, done, f, _, _, _, _ := p.Status(); !ok || done || f != 0 {
		t.Fatalf("没目标时不该判失败: ok=%v done=%v fails=%d", ok, done, f)
	}
}

// 转发器要把进出两个方向分开记：商家按出向计费，
// 混成一个总数就看不出「哪条规则吃带宽」了。
func TestPortStatsDirections(t *testing.T) {
	f := NewForwarder()
	const port = 45123
	if err := f.Sync([]Rule{{ListenPort: port, TargetHost: "127.0.0.1", TargetPort: 1}}); err != nil {
		t.Skipf("端口 %d 占用，跳过: %v", port, err)
	}
	defer f.Sync(nil)

	f.mu.Lock()
	cur := f.m[port]
	f.mu.Unlock()
	if cur == nil {
		t.Fatal("没建出转发器")
	}
	cur.rx.Add(1000)
	cur.tx.Add(2500)

	sts := f.PortStats()
	if len(sts) != 1 {
		t.Fatalf("应有 1 条，实际 %d", len(sts))
	}
	if sts[0].Rx != 1000 || sts[0].Tx != 2500 {
		t.Fatalf("进出应分开记：rx=%d tx=%d", sts[0].Rx, sts[0].Tx)
	}
	if sts[0].Port != port {
		t.Fatalf("端口对不上: %d", sts[0].Port)
	}
}
