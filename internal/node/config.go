// language: Go 1.23, file: internal/node/config.go
package node

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// utf8bom Windows 记事本保存的 JSON 会带 BOM，json.Unmarshal 不认
var utf8bom = []byte{0xEF, 0xBB, 0xBF}

func stripBOM(b []byte) []byte { return bytes.TrimPrefix(b, utf8bom) }

type Config struct {
	Panel    string `json:"panel"`     // 面板地址，如 https://panel.example.com
	Token    string `json:"token"`     // 首次注册用，注册后自动换成 session_key
	KeyFile  string `json:"key_file"`  // session_key 落盘位置
	Iface    string `json:"iface"`     // 网卡名，面板下发的为空时用这个
	StateDir string `json:"state_dir"` // 累计量落盘目录
}

func LoadConfig(path string) (*Config, error) {
	c := &Config{
		KeyFile:  "/var/lib/relay/key",
		StateDir: "/var/lib/relay",
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(stripBOM(b), c); err != nil {
		return nil, err
	}
	c.Panel = strings.TrimRight(c.Panel, "/")
	if c.KeyFile == "" {
		c.KeyFile = filepath.Join(c.StateDir, "key")
	}
	return c, nil
}

// LoadKey 读已保存的 session_key；没有就返回 token（首次注册）
func (c *Config) LoadKey() string {
	if b, err := os.ReadFile(c.KeyFile); err == nil {
		if k := strings.TrimSpace(string(b)); k != "" {
			return k
		}
	}
	return c.Token
}

func (c *Config) SaveKey(k string) error {
	if err := os.MkdirAll(filepath.Dir(c.KeyFile), 0o755); err != nil {
		return err
	}
	return os.WriteFile(c.KeyFile, []byte(k), 0o600)
}
