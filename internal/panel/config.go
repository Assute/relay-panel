// language: Go 1.23, file: internal/panel/config.go
package panel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// utf8bom Windows 记事本保存的 JSON 会带 BOM
var utf8bom = []byte{0xEF, 0xBB, 0xBF}

type Config struct {
	Listen          string `json:"listen"`
	DB              string `json:"db"`
	AdminUser       string `json:"admin_user"`
	AdminPassHash   string `json:"admin_pass_hash"`
	CloudflareToken string `json:"cloudflare_token"`
	PublicURL       string `json:"public_url"` // 生成一键命令用，如 https://panel.example.com
	NodeBinURL      string `json:"node_bin_url"`
	ProbeNoPort     bool   `json:"probe_no_port"`

	Path string `json:"-"` // 配置文件自身路径，Save 用

	mu sync.Mutex `json:"-"`
}

func DefaultConfig() *Config {
	return &Config{
		Listen:    "127.0.0.1:3070",
		DB:        "data.json",
		AdminUser: "123456",
		PublicURL: "http://127.0.0.1:3070",
	}
}

// LoadConfig 读配置。文件不存在就写一份默认的，并生成随机管理员密码。
// 返回 true 表示是首次创建，调用方需要把密码打印出来。
func LoadConfig(path string) (*Config, bool, string, error) {
	abs, _ := filepath.Abs(path)
	c := DefaultConfig()
	c.Path = abs

	b, err := os.ReadFile(abs)
	if err == nil {
		if err := json.Unmarshal(bytes.TrimPrefix(b, utf8bom), c); err != nil {
			return nil, false, "", err
		}
		c.Path = abs
		c.resolvePaths()
		if c.AdminPassHash == "" {
			pw := randHex(8)
			c.AdminPassHash = HashPassword(pw)
			if err := c.Save(); err != nil {
				return nil, false, "", err
			}
			return c, true, pw, nil
		}
		return c, false, "", nil
	}
	if !os.IsNotExist(err) {
		return nil, false, "", err
	}

	pw := randHex(8)
	c.AdminPassHash = HashPassword(pw)
	c.resolvePaths()
	if err := c.Save(); err != nil {
		return nil, false, "", err
	}
	return c, true, pw, nil
}

// resolvePaths 让相对路径相对配置文件所在目录解析，并立刻转成绝对路径。
// 必须转绝对，否则下次启动会把已经解析过的相对路径再拼一次（_test/_test/data.json）。
func (c *Config) resolvePaths() {
	dir := filepath.Dir(c.Path)
	if c.DB == "" {
		c.DB = "data.json"
	}
	if !filepath.IsAbs(c.DB) {
		c.DB = filepath.Join(dir, c.DB)
	}
	if abs, err := filepath.Abs(c.DB); err == nil {
		c.DB = abs
	}
}

// Save 写回配置文件
func (c *Config) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.Path
	if p == "" {
		return fmt.Errorf("配置缺少路径")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

// SetCloudflareToken 更新 token 并落盘
func (c *Config) SetCloudflareToken(tok string) error {
	c.mu.Lock()
	c.CloudflareToken = tok
	c.mu.Unlock()
	return c.Save()
}

func (c *Config) Token() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.CloudflareToken
}

func (c *Config) SetPassword(plain string) error {
	c.mu.Lock()
	c.AdminPassHash = HashPassword(plain)
	c.mu.Unlock()
	return c.Save()
}

func (c *Config) String() string {
	return fmt.Sprintf("listen=%s db=%s user=%s", c.Listen, c.DB, c.AdminUser)
}
