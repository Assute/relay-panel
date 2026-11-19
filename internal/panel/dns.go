// language: Go 1.23, file: internal/panel/dns.go
// DNS Provider：Cloudflare。
//
// 设计要点：
//  1. Zone 可以填 32 位 zone id，也可以直接填 example.com，面板自动查 id
//  2. 记录不存在会自动创建，不用先去 Cloudflare 后台手建
//  3. 永远 proxied=false —— 转发流量走 CF 代理会被限速且拿不到真实 IP
package panel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var ErrDNSNotConfigured = errors.New("分组未配置 DNS")
var ErrDNSRecordMissing = errors.New("DNS 记录不存在")

type DNSProvider interface {
	SetA(g *Group, ip string, ttl int) error
	GetA(g *Group) (string, error)
	Verify() error
}

// NoopDNS 没配 token 时用：只打日志，不改真实记录。
type NoopDNS struct{}

func (NoopDNS) SetA(g *Group, ip string, ttl int) error {
	log.Printf("[dns-noop] 分组 %d 的 A 记录本应指向 %s (ttl=%d)", g.ID, ip, ttl)
	return nil
}
func (NoopDNS) GetA(g *Group) (string, error) { return "", nil }
func (NoopDNS) Verify() error                { return errors.New("未配置 Cloudflare Token") }

// ManagedDNS 读 Config 里的 token，token 变了立刻生效（不用重启面板）。
// 没配 token 时退化成 NoopDNS，只打日志不改记录。
type ManagedDNS struct {
	cnf *Config
	hc  *http.Client
}

func NewManagedDNS(cnf *Config) *ManagedDNS {
	return &ManagedDNS{cnf: cnf, hc: &http.Client{Timeout: 15 * time.Second}}
}

func (m *ManagedDNS) provider() DNSProvider {
	tok := strings.TrimSpace(m.cnf.Token())
	if tok == "" {
		return NoopDNS{}
	}
	return &Cloudflare{token: tok, hc: m.hc}
}

func (m *ManagedDNS) SetA(g *Group, ip string, ttl int) error {
	return m.provider().SetA(g, ip, ttl)
}

func (m *ManagedDNS) GetA(g *Group) (string, error) {
	return m.provider().GetA(g)
}

func (m *ManagedDNS) Verify() error {
	return m.provider().Verify()
}

// Zones 列出该 token 能管的域名，设置页用
func (m *ManagedDNS) Zones() ([]cfZone, error) {
	p, ok := m.provider().(*Cloudflare)
	if !ok {
		return nil, errors.New("未配置 Cloudflare Token")
	}
	return p.ListZones()
}

// NewDNSProvider 保留给命令行/测试用
func NewDNSProvider(name, token string) (DNSProvider, error) {
	switch strings.ToLower(name) {
	case "", "cloudflare", "cf":
		return &Cloudflare{token: token, hc: &http.Client{Timeout: 15 * time.Second}}, nil
	default:
		return nil, fmt.Errorf("不支持的 DNS 服务商: %s", name)
	}
}

type Cloudflare struct {
	token string
	hc    *http.Client
}

type cfRecord struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
}

type cfZone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (c *Cloudflare) do(method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "https://api.cloudflare.com/client/v4"+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("请求 Cloudflare 失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var env struct {
		Success bool            `json:"success"`
		Errors  []cfErr         `json:"errors"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("Cloudflare 返回无法解析 (%d): %s", resp.StatusCode, trunc(string(raw), 200))
	}
	if !env.Success {
		if len(env.Errors) == 0 {
			return fmt.Errorf("Cloudflare 返回失败 (%d)", resp.StatusCode)
		}
		var msgs []string
		for _, e := range env.Errors {
			msgs = append(msgs, fmt.Sprintf("%d %s", e.Code, e.Message))
		}
		joined := strings.Join(msgs, "; ")
		if looksLikeAuthError(joined) {
			return fmt.Errorf("%s —— Token 权限不足，需要 Zone.DNS 的 Edit 权限", joined)
		}
		return fmt.Errorf("Cloudflare 错误: %s", joined)
	}
	if out != nil && len(env.Result) > 0 {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

type cfErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func looksLikeAuthError(s string) bool {
	l := strings.ToLower(s)
	return strings.Contains(l, "authentication") || strings.Contains(l, "unauthorized") ||
		strings.Contains(l, "forbidden") || strings.Contains(l, "permission") ||
		strings.Contains(l, "10000") || strings.Contains(l, "9109") || strings.Contains(l, "6111")
}

var zoneIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// resolveZone 把用户填的 zone（可能是域名，也可能是 32 位 id）转成 zone id
func (c *Cloudflare) resolveZone(zone string) (string, error) {
	zone = strings.TrimSpace(zone)
	if zone == "" {
		return "", errors.New("未填 Zone")
	}
	if zoneIDRe.MatchString(strings.ToLower(zone)) {
		return zone, nil
	}
	var zones []cfZone
	if err := c.do("GET", "/zones?name="+zone, nil, &zones); err != nil {
		return "", err
	}
	if len(zones) == 0 {
		return "", fmt.Errorf("Cloudflare 账号里找不到 zone %q，检查域名拼写或 token 权限", zone)
	}
	return zones[0].ID, nil
}

// findRecord 只查不建。找不到返回 ErrDNSRecordMissing。
func (c *Cloudflare) findRecord(g *Group, zoneID string) (*cfRecord, error) {
	if g.DNSName == "" {
		return nil, fmt.Errorf("%w: 分组未填域名", ErrDNSNotConfigured)
	}
	if g.DNSRecordID != "" {
		var rec cfRecord
		err := c.do("GET", "/zones/"+zoneID+"/dns_records/"+g.DNSRecordID, nil, &rec)
		if err == nil {
			return &rec, nil
		}
		// 记录被删了，往下按名字重新找
	}
	var list []cfRecord
	if err := c.do("GET", "/zones/"+zoneID+"/dns_records?type=A&name="+g.DNSName, nil, &list); err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrDNSRecordMissing
	}
	return &list[0], nil
}

func (c *Cloudflare) createA(zoneID, name, ip string, ttl int) (*cfRecord, error) {
	var rec cfRecord
	body := map[string]any{
		"type": "A", "name": name, "content": ip, "ttl": ttl, "proxied": false,
	}
	if err := c.do("POST", "/zones/"+zoneID+"/dns_records", body, &rec); err != nil {
		return nil, err
	}
	log.Printf("[dns] 已创建 A 记录 %s -> %s", name, ip)
	return &rec, nil
}

func (c *Cloudflare) SetA(g *Group, ip string, ttl int) error {
	if ttl <= 0 {
		ttl = 60
	}
	if ttl < 60 {
		ttl = 60 // Cloudflare 免费版最小值；1 表示 Auto
	}
	zoneID, err := c.resolveZone(g.DNSZone)
	if err != nil {
		return err
	}
	rec, err := c.findRecord(g, zoneID)
	if errors.Is(err, ErrDNSRecordMissing) {
		_, cerr := c.createA(zoneID, g.DNSName, ip, ttl)
		return cerr
	}
	if err != nil {
		return err
	}
	if rec.Content == ip && !rec.Proxied {
		return nil // 已经是目标，不浪费 API 配额
	}
	body := map[string]any{
		"type": "A", "name": rec.Name, "content": ip, "ttl": ttl, "proxied": false,
	}
	return c.do("PUT", "/zones/"+zoneID+"/dns_records/"+rec.ID, body, nil)
}

func (c *Cloudflare) GetA(g *Group) (string, error) {
	zoneID, err := c.resolveZone(g.DNSZone)
	if err != nil {
		return "", err
	}
	rec, err := c.findRecord(g, zoneID)
	if err != nil {
		return "", err
	}
	return rec.Content, nil
}

// Verify 校验 token 有效性，并顺手把账号下的 zone 数量报出来
func (c *Cloudflare) Verify() error {
	if strings.TrimSpace(c.token) == "" {
		return errors.New("Token 为空")
	}
	var st struct {
		Status string `json:"status"`
	}
	if err := c.do("GET", "/user/tokens/verify", nil, &st); err != nil {
		return err
	}
	if st.Status != "active" {
		return fmt.Errorf("Token 状态异常: %s", st.Status)
	}
	return nil
}

// ListZones 方便前端在设置页显示这个 token 能管哪些域名
func (c *Cloudflare) ListZones() ([]cfZone, error) {
	var zones []cfZone
	if err := c.do("GET", "/zones?per_page=50", nil, &zones); err != nil {
		return nil, err
	}
	return zones, nil
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
