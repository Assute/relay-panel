.PHONY: all panel node linux clean

# 默认：本机面板 + 三架构 Linux 节点
all: panel linux

panel:
	go build -o bin/panel ./cmd/panel

# 本机调试用的节点（Windows/macOS）
node:
	go build -o bin/node ./cmd/node

# 节点二进制，面板会托管在 /dl/ 下给一键脚本下载
linux:
	GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o bin/node-linux-amd64 ./cmd/node
	GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o bin/node-linux-arm64 ./cmd/node
	GOOS=linux GOARCH=arm   go build -trimpath -ldflags="-s -w" -o bin/node-linux-arm   ./cmd/node

clean:
	rm -rf bin

test:
	go vet ./...
	go build ./...
