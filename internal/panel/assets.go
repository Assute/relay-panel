// language: Go 1.23, file: internal/panel/assets.go
// 把节点二进制嵌进面板，安装时直接推给目标机。
//
// 为什么不用现成的 /dl/ 目录：面板换成新版了，/dl/ 里躺的可能还是半年前
// 那份 node，装出来的节点是旧版——心跳能上来，但速率、负载、连通性全是空的，
// 面板上显示「节点端 未知（旧版二进制）」，很难一眼看出是安装包过期。
//
// 嵌进来以后：换面板 = 换安装包，装出去的节点必然跟面板同版本。
// 代价是面板大几 MB，换来「不会装错版本」。
package panel

import (
	"embed"
)

//go:embed assets
var assetsFS embed.FS

// nodeBinary 取出对应架构的节点二进制。
// 仓库里只提交 .gitkeep 占位，真正的二进制由 make assets 在构建前放进来。
// 没放（或者放的是空占位）就返回 nil，调用方回退到让目标机自己下载。
func nodeBinary(arch string) []byte {
	if arch == "" {
		return nil
	}
	b, err := assetsFS.ReadFile("assets/node-linux-" + arch)
	if err != nil || len(b) < 1<<20 { // 小于 1 MiB 肯定是占位，不是真二进制
		return nil
	}
	return b
}
