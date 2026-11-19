// language: Go 1.23, file: internal/panel/version.go
package panel

// BuildStamp 编译时通过 -ldflags 注入，页面上用来确认跑的是哪个版本。
var BuildStamp = "dev"