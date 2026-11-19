// language: Go 1.23, file: internal/node/version.go
package node

// BuildStamp 编译时注入，启动时打一行，方便确认节点端是否真的更新了。
var BuildStamp = "dev"