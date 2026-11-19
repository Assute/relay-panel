// language: Go 1.23, file: cmd/node/main.go
// 节点 Agent：向面板注册、拉规则、转发 TCP 流量、上报网卡用量。
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"relay/internal/node"
)

func main() {
	cfgPath := flag.String("config", "/etc/relay/config.json", "配置文件路径")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[node] ")

	cnf, err := node.LoadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	if cnf.Panel == "" {
		log.Fatalf("配置缺少 panel 地址")
	}

	meter := node.NewNicMeter(cnf.Iface, "", cnf.StateDir)
	meter.Restore()

	// 网卡自检：配错了直接说清楚，别让人瞎猜
	if _, _, err := node.NicCounters(meter.Iface()); err != nil {
		log.Printf("警告: 网卡 %q 读不到，当前可用: %v", meter.Iface(), node.ListIfaces())
		log.Printf("       面板下发的 iface 会覆盖本地配置，稍后自动纠正")
	}

	fwd := node.NewForwarder()
	ag := node.NewAgent(cnf, meter, fwd)

	// 优雅退出：先落盘累计量，避免重启丢账
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Printf("收到退出信号，保存状态")
		_ = meter.Persist()
		os.Exit(0)
	}()

	log.Printf("节点启动，面板: %s", cnf.Panel)
	ag.Run()
}
