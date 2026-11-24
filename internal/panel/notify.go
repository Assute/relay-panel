// language: Go 1.23, file: internal/panel/notify.go
// 企业微信告警推送。
//
// 走的是「应用消息」这条路（corpid + secret 换 access_token，再按 agentid 发），
// 不需要用户在企微后台建群机器人，也不用暴露面板地址。
//
// 两个必须知道的限制：
//   - 企业微信要求调用方 IP 在应用的「可信 IP」白名单里。
//     面板所在机器 IP 变了就会报 60020，这时去后台改白名单，不用改代码。
//   - access_token 有效期 7200 秒，且同一个应用重复取会把上一个挤掉，
//     所以必须缓存、提前 5 分钟续，不能每条消息都去换。
package panel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	wecomTokenURL = "https://qyapi.weixin.qq.com/cgi-bin/gettoken"
	wecomSendURL  = "https://qyapi.weixin.qq.com/cgi-bin/message/send"
)

// Notifier 把告警推到企业微信。队列满了就丢，绝不阻塞调度器。
type Notifier struct {
	cnf *Config
	hc  *http.Client
	ch  chan string

	mu     sync.Mutex
	token  string
	expire time.Time
	lastErr string
}

func NewNotifier(cnf *Config) *Notifier {
	return &Notifier{
		cnf: cnf,
		hc:  &http.Client{Timeout: 10 * time.Second},
		ch:  make(chan string, 64),
	}
}

// Enabled 四项都填全了才推送
func (n *Notifier) Enabled() bool {
	c := n.cnf
	return strings.TrimSpace(c.WecomCorpID) != "" &&
		strings.TrimSpace(c.WecomSecret) != "" &&
		strings.TrimSpace(c.WecomAgentID) != "" &&
		strings.TrimSpace(c.WecomToUser) != ""
}

// Push 非阻塞投递。没配企业微信就什么都不做。
func (n *Notifier) Push(text string) {
	if !n.Enabled() || strings.TrimSpace(text) == "" {
		return
	}
	select {
	case n.ch <- text:
	default: // 队列满了说明在刷屏，丢掉比堵住调度器强
	}
}

// Run 在后台消费队列，由 main 起一个 goroutine。
func (n *Notifier) Run() {
	for text := range n.ch {
		if err := n.Send(text); err != nil {
			n.mu.Lock()
			n.lastErr = err.Error()
			n.mu.Unlock()
		}
	}
}

// LastError 最近一次发送失败的原因，设置页上显示，方便排查白名单之类的问题。
func (n *Notifier) LastError() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.lastErr
}

// Send 同步发一条文本消息。设置页的「发送测试」也走这里。
func (n *Notifier) Send(text string) error {
	if !n.Enabled() {
		return fmt.Errorf("企业微信没配全（企业ID/密钥/应用ID/接收者 四项都要填）")
	}
	tok, err := n.accessToken()
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{
		"touser":  strings.TrimSpace(n.cnf.WecomToUser),
		"msgtype": "text",
		"agentid": mustAtoi(n.cnf.WecomAgentID),
		"text":    map[string]string{"content": text},
		"safe":    0,
	})
	resp, err := n.hc.Post(wecomSendURL+"?access_token="+tok, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("发送失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	var out struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	_ = json.Unmarshal(raw, &out)
	if out.ErrCode != 0 {
		return fmt.Errorf("企业微信返回 %d: %s%s", out.ErrCode, out.ErrMsg, hintFor(out.ErrCode))
	}
	return nil
}

// accessToken 带缓存：7200 秒有效期，提前 5 分钟换。
// 重复换 token 会让上一个失效，高频告警下必须复用。
func (n *Notifier) accessToken() (string, error) {
	n.mu.Lock()
	if n.token != "" && time.Now().Before(n.expire) {
		t := n.token
		n.mu.Unlock()
		return t, nil
	}
	n.mu.Unlock()

	url := fmt.Sprintf("%s?corpid=%s&corpsecret=%s",
		wecomTokenURL, n.cnf.WecomCorpID, n.cnf.WecomSecret)
	resp, err := n.hc.Get(url)
	if err != nil {
		return "", fmt.Errorf("获取 access_token 失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	var out struct {
		ErrCode     int    `json:"errcode"`
		ErrMsg      string `json:"errmsg"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("解析 token 响应失败: %w", err)
	}
	if out.ErrCode != 0 || out.AccessToken == "" {
		return "", fmt.Errorf("企业微信返回 %d: %s%s", out.ErrCode, out.ErrMsg, hintFor(out.ErrCode))
	}

	ttl := time.Duration(out.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 7200 * time.Second
	}
	n.mu.Lock()
	n.token = out.AccessToken
	n.expire = time.Now().Add(ttl - 5*time.Minute)
	n.mu.Unlock()
	return out.AccessToken, nil
}

// hintFor 给几个最常撞的错误码补一句人话，省得对着 errcode 猜。
func hintFor(code int) string {
	switch code {
	case 60020:
		return "（调用方 IP 不在企业微信应用的「可信 IP」白名单里）"
	case 40001, 40091:
		return "（Secret 不对，或者被重置过）"
	case 40013, 40056:
		return "（企业ID 或 应用AgentId 不对）"
	case 81013:
		return "（接收者不在应用可见范围内）"
	case 45009:
		return "（调用太频繁，触发了频率限制）"
	}
	return ""
}

// FormatAlert 把一条告警拼成适合手机看的一小段文字。
func FormatAlert(a Alert, groupName string) string {
	lv := map[string]string{"error": "错误", "warn": "警告", "info": "提示"}[a.Level]
	if lv == "" {
		lv = a.Level
	}
	var b strings.Builder
	fmt.Fprintf(&b, "【中转面板】%s\n", lv)
	if groupName != "" {
		fmt.Fprintf(&b, "分组：%s\n", groupName)
	}
	fmt.Fprintf(&b, "%s\n", a.Message)
	fmt.Fprintf(&b, "%s", time.Unix(a.At, 0).Format("01-02 15:04:05"))
	return b.String()
}

func mustAtoi(s string) int {
	v := 0
	for _, c := range strings.TrimSpace(s) {
		if c < '0' || c > '9' {
			return v
		}
		v = v*10 + int(c-'0')
	}
	return v
}
