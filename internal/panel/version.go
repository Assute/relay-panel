// language: Go 1.23, file: internal/panel/version.go
package panel

// BuildStamp 编译时通过 -ldflags 注入，页面上用来确认跑的是哪个版本。
var BuildStamp = "dev"

// NodeStamp 面板里嵌着的节点二进制的构建号。
// 构建脚本用同一个 STAMP 注入，所以「节点上报的 agent_stamp == NodeStamp」
// 就等于「这台节点已经是最新版」，节点页据此决定要不要亮「更新」按钮。
var NodeStamp = "dev"
