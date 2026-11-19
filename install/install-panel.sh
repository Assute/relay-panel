#!/bin/bash
# 中转面板 一键安装脚本
# 用法：
#   bash install-panel.sh                          # 默认装到 /opt/relay-panel
#   bash install-panel.sh --port 8080 --url http://1.2.3.4:8080
#   bash install-panel.sh --uninstall
set -euo pipefail

APP_DIR="/opt/relay-panel"
BIN="$APP_DIR/panel"
CONF="$APP_DIR/config.json"
SVC="/etc/systemd/system/relay-panel.service"
LISTEN="127.0.0.1:8080"
PUBLIC_URL=""
UNINSTALL=0

while [ $# -gt 0 ]; do
  case "$1" in
    --port)     LISTEN="127.0.0.1:$2"; shift 2 ;;
    --listen)   LISTEN="$2"; shift 2 ;;
    --url)      PUBLIC_URL="$2"; shift 2 ;;
    --dir)      APP_DIR="$2"; BIN="$APP_DIR/panel"; CONF="$APP_DIR/config.json"; shift 2 ;;
    --uninstall) UNINSTALL=1; shift ;;
    -h|--help)  sed -n '2,6p' "$0"; exit 0 ;;
    *) echo "未知参数: $1" >&2; exit 1 ;;
  esac
done

if [ "$(id -u)" != "0" ]; then
  echo "请用 root 运行" >&2
  exit 1
fi

if [ "$UNINSTALL" = "1" ]; then
  systemctl stop relay-panel 2>/dev/null || true
  systemctl disable relay-panel 2>/dev/null || true
  rm -f "$SVC"
  systemctl daemon-reload 2>/dev/null || true
  echo "服务已移除。数据仍保留在 $APP_DIR（手动删除该目录即可彻底清理）"
  exit 0
fi

# ---------- 1. 文件 ----------
echo "[1/5] 安装到 $APP_DIR"
mkdir -p "$APP_DIR/dl"
if [ ! -f "./panel" ]; then
  echo "当前目录找不到 panel 二进制，请把安装包解压后在本目录执行" >&2
  exit 1
fi
install -m 0755 ./panel "$BIN"
for f in ./node-linux-amd64 ./node-linux-arm64 ./node-linux-arm; do
  if [ -f "$f" ]; then
    install -m 0755 "$f" "$APP_DIR/dl/$(basename "$f")"
  fi
done

# ---------- 2. 配置 ----------
echo "[2/5] 写配置 $CONF"
if [ -f "$CONF" ]; then
  echo "      配置已存在，保留原文件"
  KEEP=1
else
  KEEP=0
  # 没给 --url 就猜：取默认路由的出口 IP
  if [ -z "$PUBLIC_URL" ]; then
    IP=$(ip route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src") {print $(i+1); exit}}' || true)
    [ -z "$IP" ] && IP=$(hostname -I 2>/dev/null | awk '{print $1}' || echo "127.0.0.1")
    PORTNO="${LISTEN##*:}"
    PUBLIC_URL="http://$IP:$PORTNO"
    echo "      未指定 --url，推测为 $PUBLIC_URL"
  fi
  cat > "$CONF" <<EOF
{
  "listen": "$LISTEN",
  "db": "$APP_DIR/data.json",
  "admin_user": "admin",
  "admin_pass_hash": "",
  "cloudflare_token": "",
  "public_url": "$PUBLIC_URL",
  "node_bin_url": "",
  "probe_no_port": false
}
EOF
  chmod 600 "$CONF"
fi

# ---------- 3. 初始密码 ----------
echo "[3/5] 设置管理员密码"
PASSWD=""
if [ "$KEEP" = "0" ]; then
  set +o pipefail
  PASSWD=$(head -c 32 /dev/urandom | base64 | tr -dc 'a-zA-Z0-9' | cut -c1-12)
  set -o pipefail
  "$BIN" -config "$CONF" -passwd "$PASSWD" >/dev/null 2>&1 || {
    echo "      设置密码失败，稍后用 journalctl -u relay-panel 查看" >&2
  }
fi

# ---------- 4. systemd ----------
echo "[4/5] 安装 systemd 服务"
cat > "$SVC" <<EOF
[Unit]
Description=Relay Panel
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$BIN -config $CONF
Restart=always
RestartSec=5
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable relay-panel >/dev/null 2>&1 || true
systemctl restart relay-panel

# ---------- 5. 结果 ----------
echo "[5/5] 启动"
sleep 2
if systemctl is-active --quiet relay-panel; then
  echo ""
  echo "================================================"
  echo "  中转面板已启动"
  echo "  地址   : http://<本机IP>:${LISTEN##*:}"
  echo "  账号   : admin"
  if [ -n "$PASSWD" ]; then
    echo "  密码   : $PASSWD"
  else
    echo "  密码   : （沿用原有配置，未改动）"
  fi
  echo "  数据   : $APP_DIR/data.json"
  echo "  程序   : $BIN"
  echo "================================================"
  echo ""
  echo "下一步："
  echo "  1. 浏览器打开面板，登录"
  echo "  2. 左侧「设置」填 Public URL（一键命令要用，必须填外网可达的地址）"
  echo "  3. 「设置」填 Cloudflare Token，点测试"
  echo "  4. 新建分组，填域名和 Zone"
  echo "  5. 添加节点，把生成的一键命令复制到节点机上执行"
  echo ""
  echo "常用："
  echo "  systemctl status relay-panel      查看状态"
  echo "  journalctl -u relay-panel -f      看日志"
  echo "  $BIN -config $CONF -passwd 新密码   改密码"
  echo "  bash install-panel.sh --uninstall  卸载服务"
else
  echo "启动失败，看日志：" >&2
  journalctl -u relay-panel -n 30 --no-pager || true
  exit 1
fi
