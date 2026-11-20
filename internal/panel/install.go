// language: Go 1.23, file: internal/panel/install.go
package panel

import "strings"

// installTmpl 生成节点一键安装脚本。
// 脚本自己探测架构、下载二进制、写 systemd unit、启动。
func installTmpl(base string) string {
	t := `#!/bin/bash
set -euo pipefail

# 任何一步失败都打出具体是哪一行、哪条命令，不然远端只回一句
# "Process exited with status 1"，在面板上看不出卡在哪。
trap 'echo "[错误] 第 $LINENO 行失败: $BASH_COMMAND" >&2' ERR

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

# 覆盖正在运行的可执行文件会报 Text file busy（跨分区的 mv 是复制+截断，
# 一样会失败）。先停掉再装，反正是重装。
if systemctl is-active --quiet relay-node 2>/dev/null; then
  echo "[0/4] 停止已在运行的 relay-node"
  systemctl stop relay-node || true
fi

AVAIL=$(df -P /usr/local | awk 'NR==2{print $4}')
if [ -n "$AVAIL" ] && [ "$AVAIL" -lt 30720 ]; then
  echo "[错误] /usr/local 所在分区只剩 ${AVAIL}KB，至少要 30MB" >&2
  exit 1
fi

BIN="/usr/local/bin/relay-node"
if [ -f /tmp/relay-node.bin ] && [ -s /tmp/relay-node.bin ]; then
  SZ=$(wc -c < /tmp/relay-node.bin)
  echo "[1/4] 使用面板推送的 relay-node（$A，${SZ} 字节）"
  mv -f /tmp/relay-node.bin "${BIN}"
else
  echo "[1/4] 下载 relay-node（$A）"
  curl -fsSL "${PANEL}/dl/node-linux-${A}" -o "${BIN}.new"
  chmod +x "${BIN}.new"
  mv -f "${BIN}.new" "${BIN}"
fi
chmod +x "${BIN}"

echo "[2/4] 写配置"
install -d -m 0755 /etc/relay

# 重装必须清掉旧的 session_key：节点启动时优先用 /var/lib/relay/key 里的凭证，
# 那是上一次注册留下的，配新 token 也是白搭，会一直报 token_invalid 然后被
# systemd 反复拉起。
rm -f /var/lib/relay/key

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
# 退出码 2 = 凭证被面板拒绝（token_invalid / 换机器了）。
# 这种情况重启一万次也没用，只会刷日志，直接停住等人处理。
RestartPreventExitStatus=2
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
