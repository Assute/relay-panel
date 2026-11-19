// language: Go 1.23, file: internal/panel/rules_io.go
// 转发线路的批量导入 / 导出 / 批量操作。
//
// 导入导出格式对齐 Nyanpass，一行一个 JSON，方便两边互相迁移：
//
//	{"dest":["1.2.3.4:443"],"listen_port":25380,"name":"日本"}
//
// dest 是数组是为了兼容对方的故障转移格式，我们只取第一个，
// 多的会在导入结果里提示。
package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
)

// importRule 兼容两种写法：
//   Nyanpass: {"dest":["host:port"],"listen_port":25380,"name":"日本"}
//   本面板:   {"target_host":"host","target_port":443,"listen_port":25380,"name":"日本"}
type importRule struct {
	Name       string   `json:"name"`
	ListenPort int      `json:"listen_port"`
	Dest       []string `json:"dest"`
	TargetHost string   `json:"target_host"`
	TargetPort int      `json:"target_port"`
	Protocol   string   `json:"protocol"`
	Enabled    *bool    `json:"enabled"`
}

type importResult struct {
	Created int      `json:"created"`
	Skipped int      `json:"skipped"`
	Errors  []string `json:"errors"`
	Notes   []string `json:"notes"`
}

func splitHostPort(s string) (string, int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", 0, fmt.Errorf("空地址")
	}
	host, portStr, err := net.SplitHostPort(s)
	if err != nil {
		return "", 0, fmt.Errorf("地址格式应为 host:port，收到 %q", s)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return "", 0, fmt.Errorf("端口无效: %q", portStr)
	}
	return host, port, nil
}

// ImportRules 逐行解析并创建，单行失败不影响其他行
func (a *API) ImportRules(w http.ResponseWriter, r *http.Request) {
	gid := pathID(r, "id")
	if a.store.Group(gid) == nil {
		writeJSON(w, 404, map[string]string{"error": "分组不存在"})
		return
	}
	var in struct {
		Text string `json:"text"`
	}
	if err := readJSON(r, &in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "请求格式错误"})
		return
	}
	res := importResult{Errors: []string{}, Notes: []string{}}

	lines := strings.Split(in.Text, "\n")
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		var ir importRule
		if err := json.Unmarshal([]byte(line), &ir); err != nil {
			res.Skipped++
			res.Errors = append(res.Errors, fmt.Sprintf("第 %d 行 JSON 解析失败: %v", i+1, err))
			continue
		}

		host, port := "", 0
		switch {
		case len(ir.Dest) > 0:
			h, p, err := splitHostPort(ir.Dest[0])
			if err != nil {
				res.Skipped++
				res.Errors = append(res.Errors, fmt.Sprintf("第 %d 行: %v", i+1, err))
				continue
			}
			host, port = h, p
			if len(ir.Dest) > 1 {
				res.Notes = append(res.Notes,
					fmt.Sprintf("第 %d 行有 %d 个 dest，本面板只支持单目标，已取第一个", i+1, len(ir.Dest)))
			}
		case ir.TargetHost != "" && ir.TargetPort > 0:
			host, port = ir.TargetHost, ir.TargetPort
		default:
			res.Skipped++
			res.Errors = append(res.Errors, fmt.Sprintf("第 %d 行: 缺少 dest 或 target_host/target_port", i+1))
			continue
		}

		if ir.ListenPort <= 0 || ir.ListenPort > 65535 {
			res.Skipped++
			res.Errors = append(res.Errors, fmt.Sprintf("第 %d 行: listen_port 无效 (%d)", i+1, ir.ListenPort))
			continue
		}

		name := ir.Name
		if name == "" {
			name = fmt.Sprintf("%s:%d", host, port)
		}
		proto := ir.Protocol
		if proto == "" {
			proto = "tcp"
		}
		enabled := true
		if ir.Enabled != nil {
			enabled = *ir.Enabled
		}

		_, err := a.store.CreateRule(Rule{
			GroupID:    gid,
			Name:       name,
			ListenPort: ir.ListenPort,
			TargetHost: host,
			TargetPort: port,
			Protocol:   proto,
			Enabled:    enabled,
		})
		if err != nil {
			res.Skipped++
			res.Errors = append(res.Errors, fmt.Sprintf("第 %d 行 (%s): %v", i+1, name, err))
			continue
		}
		res.Created++
	}

	writeJSON(w, 200, res)
}

// ExportRules 导出成 Nyanpass 兼容的 JSONL
func (a *API) ExportRules(w http.ResponseWriter, r *http.Request) {
	gid := pathID(r, "id")
	rules := a.store.Rules(gid)
	var sb strings.Builder
	for _, rl := range rules {
		obj := map[string]any{
			"name":        rl.Name,
			"listen_port": rl.ListenPort,
			"dest":        []string{net.JoinHostPort(rl.TargetHost, strconv.Itoa(rl.TargetPort))},
		}
		if rl.Protocol != "" && rl.Protocol != "tcp" {
			obj["protocol"] = rl.Protocol
		}
		if !rl.Enabled {
			obj["enabled"] = false
		}
		b, err := json.Marshal(obj)
		if err != nil {
			continue
		}
		sb.Write(b)
		sb.WriteString("\n")
	}

	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="routes.jsonl"`)
	_, _ = io.WriteString(w, sb.String())
}

// ---------- 批量操作 ----------

type batchIn struct {
	IDs     []int64 `json:"ids"`
	Enabled *bool   `json:"enabled"`
}

func (a *API) batchIDs(r *http.Request) (batchIn, bool) {
	var in batchIn
	if err := readJSON(r, &in); err != nil {
		return in, false
	}
	return in, len(in.IDs) > 0
}

// DeleteRulesBatch 批量删除
func (a *API) DeleteRulesBatch(w http.ResponseWriter, r *http.Request) {
	in, ok := a.batchIDs(r)
	if !ok {
		writeJSON(w, 400, map[string]string{"error": "没有选中任何线路"})
		return
	}
	done := 0
	for _, id := range in.IDs {
		if a.store.Rule(id) == nil {
			continue
		}
		if err := a.store.DeleteRule(id); err == nil {
			done++
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true, "deleted": done})
}

// EnableRulesBatch 批量启用/停用
func (a *API) EnableRulesBatch(w http.ResponseWriter, r *http.Request) {
	in, ok := a.batchIDs(r)
	if !ok || in.Enabled == nil {
		writeJSON(w, 400, map[string]string{"error": "参数不完整"})
		return
	}
	done := 0
	for _, id := range in.IDs {
		rl := a.store.Rule(id)
		if rl == nil {
			continue
		}
		rl.Enabled = *in.Enabled
		if err := a.store.UpdateRule(*rl); err == nil {
			done++
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true, "updated": done})
}
