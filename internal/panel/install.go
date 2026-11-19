// language: Go 1.23, file: internal/panel/install.go
package panel

import "strings"

// installTmpl 生成节点一键安装脚本。
// 脚本自己探测架构、下载二进制、写 systemd unit、启动。
func installTmpl(base string) string {
	t := `#!/bin/bash
set -euo pipefail

PANEL=""
TOKEN=""
for arg in "$@"; do
  case "$arg" in
    --panel=*) PANEL="${arg#*=}" ;;
    --token=*) TOKEN="${arg#*=}" ;;
  esac
done

if [ -z "$PANEL" ] && [ -n "${PANEL_URL:-}" ]; then PANEL="$PANEL_URL"; fi
if [ -z "$TOKEN" ] && [ -n "${PANEL_TOKEN:-}" ]; then TOKEN="$PANEL_TOKEN"; fi

if [ -z "$PANEL" ]; then PANEL="__PANEL__"; fi
if [ -z "$TOKEN" ]; then
  echo "缺少 --token，请从面板重新复制命令" >&2
  exit 1
fi

if [ "$(id -u)" != "0" ]; then
  echo "请用 root 运行" >&2
  exit 1
fi

ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64)  A=amd64 ;;
  aarch64|arm64) A=arm64 ;;
  armv7l|armv7)  A=arm  ;;
  *) echo "不支持的架构: $ARCH" >&2; exit 1 ;;
esac

BIN="/usr/local/bin/relay-node"
echo "[1/4] 下载 relay-node ($A)"
curl -fsSL "${PANEL}/dl/node-linux-${A}" -o "${BIN}.new"
chmod +x "${BIN}.new"
mv -f "${BIN}.new" "${BIN}"

echo "[2/4] 写配置"
install -d -m 0755 /etc/relay
cat > /etc/relay/config.json <<EOF
{
  "panel": "${PANEL}",
  "token": "${TOKEN}"
}
EOF
chmod 600 /etc/relay/config.json

echo "[3/4] 安装 systemd 单元"
cat > /etc/systemd/system/relay-node.service <<'EOF'
[Unit]
Description=Relay Node Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/relay-node -config /etc/relay/config.json
Restart=always
RestartSec=5
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
EOF

echo "[4/4] 启动"
systemctl daemon-reload
systemctl enable relay-node >/dev/null 2>&1 || true
systemctl restart relay-node

sleep 2
if systemctl is-active --quiet relay-node; then
  echo "安装完成。节点已上线，回到面板刷新即可看到状态。"
  journalctl -u relay-node -n 5 --no-pager || true
else
  echo "启动失败，看日志：" >&2
  journalctl -u relay-node -n 30 --no-pager || true
  exit 1
fi
`
	return strings.ReplaceAll(t, "__PANEL__", base)
}
