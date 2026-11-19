// language: Go 1.23, file: cmd/panel/main.go
// 中转面板：分组 / 节点 / 规则 / 配额 / DNS 自动切流。
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"relay/internal/panel"
)

func main() {
	cfgPath := flag.String("config", "config.json", "配置文件路径")
	listen := flag.String("listen", "", "覆盖监听地址")
	passwd := flag.String("passwd", "", "设置管理员密码后退出")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[panel] ")

	cnf, fresh, pw, err := panel.LoadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	if listen != nil && *listen != "" {
		cnf.Listen = *listen
	}
	if fresh {
		log.Printf("配置文件已生成: %s", *cfgPath)
		log.Printf("========================================")
		log.Printf("  管理员账号: %s", cnf.AdminUser)
		log.Printf("  初始密码  : %s", pw)
		log.Printf("  登录后请立刻改密码")
		log.Printf("========================================")
		_ = os.Stdout.Sync()
	}

	// -passwd：改完密码就退出，不启动服务
	if *passwd != "" {
		if err := cnf.SetPassword(*passwd); err != nil {
			log.Fatalf("修改密码失败: %v", err)
		}
		log.Printf("管理员 %s 的密码已更新", cnf.AdminUser)
		return
	}

	store, err := panel.OpenStore(cnf.DB)
	if err != nil {
		log.Fatalf("打开数据文件失败: %v", err)
	}

	// ManagedDNS 每次读配置里的 token，所以在面板里改 token 立刻生效
	dns := panel.NewManagedDNS(cnf)
	if cnf.Token() != "" {
		log.Printf("DNS: Cloudflare Token 已配置")
	} else {
		log.Printf("提示: 还没配 Cloudflare Token，切流只打日志不改记录。登录后在「设置」里填。")
	}

	sess := panel.NewSessionStore()
	sched := panel.NewScheduler(store, dns, cnf)
	api := panel.NewAPI(store, cnf, sess, dns, sched)

	go sched.Run()

	srv := &http.Server{
		Addr:              cnf.Listen,
		Handler:           api.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
	}

	log.Printf("面板启动: http://%s", cnf.Listen)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("服务退出: %v", err)
	}
}
