// language: Go 1.23, file: internal/panel/web.go
package panel

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

//go:embed web/*
var webFS embed.FS

func webHandler() http.Handler {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	return http.FileServer(http.FS(sub))
}

// installScript 返回节点一键安装脚本。
func (a *API) installScript(w http.ResponseWriter, r *http.Request) {
	base := strings.TrimRight(a.cnf.PublicURL, "/")
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	_, _ = io.WriteString(w, installTmpl(base))
}

// dlHandler 托管节点二进制。文件放在 panel 数据目录下的 dl/ 里。
func (a *API) dlHandler(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(r.PathValue("name")) // 防穿越
	if name == "" || name == "." || name == "/" {
		http.NotFound(w, r)
		return
	}
	dir := filepath.Join(filepath.Dir(a.cnf.DB), "dl")
	p := filepath.Join(dir, name)
	f, err := os.Open(p)
	if err != nil {
		http.Error(w, "not found", 404)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", "application/octet-stream")
	if st != nil {
		w.Header().Set("Content-Length", itoa(int(st.Size())))
	}
	_, _ = io.Copy(w, f)
}
