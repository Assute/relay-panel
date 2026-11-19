// language: Go 1.23, file: internal/node/agent.go
package node

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

type agentConfig struct {
	Version int64  `json:"version"`
	Rules   []Rule `json:"rules"`
}

type beatReq struct {
	Key         string `json:"key"`
	Fingerprint string `json:"fingerprint"`
	Iface       string `json:"iface"`
	NicAcc      int64  `json:"nic_acc"`
	RelayAcc    int64  `json:"relay_acc"`
	Version     int64  `json:"version"`
}

type beatResp struct {
	OK         bool         `json:"ok"`
	Err        string       `json:"err,omitempty"`
	SessionKey string       `json:"session_key,omitempty"`
	Iface      string       `json:"iface,omitempty"`
	BillMode   string       `json:"bill_mode,omitempty"`
	Config     *agentConfig `json:"config,omitempty"`
}

type Agent struct {
	cnf   *Config
	meter *NicMeter
	fwd   *Forwarder
	key   string
	ver   atomic.Int64
	hc    *http.Client
	fp    string
}

func NewAgent(c *Config, m *NicMeter, f *Forwarder) *Agent {
	return &Agent{
		cnf:   c,
		meter: m,
		fwd:   f,
		key:   c.LoadKey(),
		hc:    &http.Client{Timeout: 20 * time.Second},
		fp:    Fingerprint(),
	}
}

func (a *Agent) Run() {
	backoff := time.Second
	for {
		err := a.loop()
		if err != nil {
			if errors.Is(err, errFatal) {
				log.Printf("致命错误，停止: %v", err)
				return
			}
			log.Printf("连接中断: %v，%s 后重试", err, backoff)
		}
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

var errFatal = errors.New("fatal")

// loop 跑一次"心跳会话"，失败就返回让外层退避重连。
func (a *Agent) loop() error {
	// 首轮立刻发一次，之后每 15s
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()

	if err := a.beat(); err != nil {
		return err
	}
	for range tick.C {
		if err := a.beat(); err != nil {
			a.meter.Persist()
			return err
		}
	}
	return nil
}

func (a *Agent) beat() error {
	req := beatReq{
		Key:         a.key,
		Fingerprint: a.fp,
		Iface:       a.meter.Iface(),
		NicAcc:      int64(a.meter.Sample()),
		RelayAcc:    a.fwd.BytesRead(),
		Version:     a.ver.Load(),
	}
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequest("POST", a.cnf.Panel+"/agent/beat", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := a.hc.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var br beatResp
	if err := json.Unmarshal(raw, &br); err != nil {
		return fmt.Errorf("面板返回无法解析 (%d): %s", resp.StatusCode, trunc(string(raw), 200))
	}
	if !br.OK {
		switch br.Err {
		case "token_invalid", "fingerprint_mismatch", "node_gone":
			return fmt.Errorf("%w: %s（请到面板重新添加节点）", errFatal, br.Err)
		}
		return fmt.Errorf("面板拒绝: %s", br.Err)
	}

	// token 换成了 session_key，落盘
	if br.SessionKey != "" {
		a.key = br.SessionKey
		if err := a.cnf.SaveKey(br.SessionKey); err != nil {
			log.Printf("保存 session key 失败: %v", err)
		} else {
			log.Printf("注册成功，已保存凭证")
		}
	}

	// 面板下发的网卡配置
	if br.Iface != "" || br.BillMode != "" {
		before := a.meter.Iface()
		a.meter.SetIface(br.Iface, br.BillMode)
		after := a.meter.Iface()
		if after != before {
			log.Printf("网卡切换: %s -> %s（计费口径 %s）", before, after, a.meter.Mode())
		}
		// 换完立刻验一次：读不到就自动挑一张，否则用量会一直是 0 且没人知道为什么
		if _, _, err := NicCounters(after); err != nil {
			if auto := PickBusiestIface(); auto != "" && auto != after {
				log.Printf("网卡 %q 读不到，自动改测 %q（可用: %v）", after, auto, ListIfaces())
				a.meter.SetIface(auto, "")
			} else {
				log.Printf("警告: 网卡 %q 读不到，当前可用: %v —— 用量将一直是 0", after, ListIfaces())
			}
		}
	}

	// 配置有更新
	if br.Config != nil && br.Config.Version > a.ver.Load() {
		if err := a.fwd.Sync(br.Config.Rules); err != nil {
			log.Printf("应用配置 v%d 失败，保持旧配置: %v", br.Config.Version, err)
			return nil // 配置错误不该断连接，下轮再试
		}
		a.ver.Store(br.Config.Version)
		log.Printf("配置 v%d 已应用: %d 条规则", br.Config.Version, len(br.Config.Rules))
	}

	a.meter.Persist()
	return nil
}

// Fingerprint 机器指纹，换机器就变，防止 token 被复用
func Fingerprint() string {
	h := sha256.New()
	if b, err := os.ReadFile("/etc/machine-id"); err == nil {
		h.Write([]byte(strings.TrimSpace(string(b))))
	} else if b, err := os.ReadFile("/var/lib/dbus/machine-id"); err == nil {
		h.Write([]byte(strings.TrimSpace(string(b))))
	} else {
		if hn, err := os.Hostname(); err == nil {
			h.Write([]byte(hn))
		}
		if ifs, err := net.Interfaces(); err == nil {
			for _, i := range ifs {
				if i.Flags&net.FlagLoopback != 0 || len(i.HardwareAddr) == 0 {
					continue
				}
				h.Write(i.HardwareAddr)
				break
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
