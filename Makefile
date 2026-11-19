.PHONY: all panel node linux clean test

# 构建标识：面板和节点端都会在日志/界面上打出来，
# 用来确认「我换的二进制到底生效了没」。想固定版本就 make STAMP=v1.0 all
STAMP ?= $(shell date +%Y%m%d-%H%M%S 2>/dev/null || echo dev)

PANEL_LD := -s -w -X relay/internal/panel.BuildStamp=$(STAMP)
NODE_LD  := -s -w -X relay/internal/node.BuildStamp=$(STAMP)

# 默认：本机面板 + 三架构 Linux 节点
all: panel linux

panel:
	go build -trimpath -ldflags="$(PANEL_LD)" -o bin/panel ./cmd/panel

# 本机调试用的节点（Windows/macOS）
node:
	go build -trimpath -ldflags="$(PANEL_LD) -X relay/internal/node.BuildStamp=$(STAMP)" -o bin/node ./cmd/node

# 节点二进制，面板会托管在 /dl/ 下给一键脚本下载
linux:
	GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$(NODE_LD)" -o bin/node-linux-amd64 ./cmd/node
	GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="$(NODE_LD)" -o bin/node-linux-arm64 ./cmd/node
	GOOS=linux GOARCH=arm   go build -trimpath -ldflags="$(NODE_LD)" -o bin/node-linux-arm   ./cmd/node

clean:
	rm -rf bin

test:
	go vet ./...
	go build ./...
