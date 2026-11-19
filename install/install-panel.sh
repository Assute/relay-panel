#!/bin/bash
# 中转面板 · 一键安装
#
#   bash <(curl -fsSL https://raw.githubusercontent.com/Assute/relay-panel/main/install/install-panel.sh)
#
# 可选参数：
#   --port 3070                  监听端口（默认只绑 127.0.0.1）
#   --listen 0.0.0.0:3070        直接对外监听
#   --url  http://1.2.3.4:3070   一键命令里的外网地址（默认自动探测公网 IP）
#   --user 123456                管理员账号
#   --pass 123456                管理员密码
#   --dir  /opt/relay-panel      安装目录
#   --uninstall                  卸载服务（保留数据）
set -euo pipefail

APP_DIR="/opt/relay-panel"
LISTEN="127.0.0.1:3070"
PUBLIC_URL=""
ADMIN_USER="123456"
ADMIN_PASS="123456"
UNINSTALL=0
REPO_BASES=(
  "https://cdn.jsdelivr.net/gh/Assute/relay-panel@main/dl"
  "https://raw.githubusercontent.com/Assute/relay-panel/main/dl"
)

while [ $# -gt 0 ]; do
  case "$1" in
    --port)      LISTEN="127.0.0.1:$2"; shift 2 ;;
    --listen)    LISTEN="$2"; shift 2 ;;
    --url)       PUBLIC_URL="$2"; shift 2 ;;
    --user)      ADMIN_USER="$2"; shift 2 ;;
    --pass)      ADMIN_PASS="$2"; shift 2 ;;
    --dir)       APP_DIR="$2"; shift 2 ;;
    --uninstall) UNINSTALL=1; shift ;;
    -h|--help)   sed -n '2,15p' "$0"; exit 0 ;;
    *) echo "未知参数: $1" >&2; exit 1 ;;
  esac
done

BIN="$APP_DIR/panel"
CONF="$APP_DIR/config.json"
SVC="/etc/systemd/system/relay-panel.service"
PORT="${LISTEN##*:}"

die() { echo "错误: $*" >&2; exit 1; }

[ "$(id -u)" = "0" ] || die "请用 root 运行"

# ---------- 卸载 ----------
if [ "$UNINSTALL" = "1" ]; then
  systemctl stop relay-panel 2>/dev/null || true
  systemctl disable relay-panel 2>/dev/null || true
  rm -f "$SVC"
  systemctl daemon-reload 2>/dev/null || true
  echo "服务已移除。数据保留在 $APP_DIR"
  exit 0
fi

# ---------- 架构 ----------
ARCH=$(uname -m)
case "$ARCH" in
  x86_64|amd64)   A=amd64 ;;
  aarch64|arm64)  A=arm64 ;;
  *) die "不支持的面板架构: $ARCH（只提供 amd64 / arm64）" ;;
esac

command -v curl >/dev/null 2>&1 || die "缺少 curl，先装：apt install -y curl"

# 多源下载：jsDelivr 优先（国内可达），失败退回 raw.githubusercontent
fetch() {
  local name="$1" dest="$2"
  for base in "${REPO_BASES[@]}"; do
    if curl -fsSL --connect-timeout 10 --max-time 300 "$base/$name" -o "$dest" 2>/dev/null; then
      if [ -s "$dest" ]; then return 0; fi
    fi
  done
  return 1
}

# 公网 IP：优先本地路由，拿到内网地址再问外部服务
guess_ip() {
  local ip=""
  ip=$(ip route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src"){print $(i+1);exit}}')
  [ -z "$ip" ] && ip=$(hostname -I 2>/dev/null | awk '{print $1}')
  case "$ip" in
    10.*|192.168.*|172.1[6-9].*|172.2[0-9].*|172.3[01].*|127.*|"")
      local ext=""
      ext=$(curl -fsS --connect-timeout 5 --max-time 8 https://api.ipify.org 2>/dev/null || true)
      if [ -z "$ext" ]; then
        ext=$(curl -fsS --connect-timeout 5 --max-time 8 https://ipv4.icanhazip.com 2>/dev/null | tr -d '\r\n' || true)
      fi
      [ -n "$ext" ] && ip="$ext"
      ;;
  esac
  echo "$ip"
}

# ---------- 1. 主程序 ----------
echo "[1/5] 安装到 $APP_DIR"
mkdir -p "$APP_DIR/dl"

if [ -f "./panel" ] && [ "$(stat -c%s ./panel 2>/dev/null || echo 0)" -gt 1000000 ]; then
  echo "      使用当前目录的 panel"
  install -m 0755 ./panel "$BIN"
else
  echo "      下载面板程序 ($A)"
  TMP="$(mktemp)"
  fetch "panel-linux-$A" "$TMP" || die "下载失败，检查网络，或手动下载 panel-linux-$A 到当前目录后重试"
  install -m 0755 "$TMP" "$BIN"
  rm -f "$TMP"
fi

# 节点端二进制（面板要托管给节点机下载）
for a in amd64 arm64 arm; do
  DEST="$APP_DIR/dl/node-linux-$a"
  if [ -f "$DEST" ]; then continue; fi
  if [ -f "./node-linux-$a" ]; then
    install -m 0755 "./node-linux-$a" "$DEST"
  else
    if fetch "node-linux-$a" "$DEST.tmp"; then
      chmod +x "$DEST.tmp"
      mv -f "$DEST.tmp" "$DEST"
    else
      echo "      警告: node-linux-$a 下载失败，该架构节点将无法一键安装" >&2
      rm -f "$DEST.tmp"
    fi
  fi
done
echo "      已就绪: $(ls "$APP_DIR/dl" 2>/dev/null | tr '\n' ' ')"

# ---------- 2. 配置 ----------
echo "[2/5] 写配置 $CONF"
KEEP=0
if [ -f "$CONF" ]; then
  echo "      配置已存在，保留原有设置"
  KEEP=1
  # 重装也同步账号名
  sed -i "s/\"admin_user\"[[:space:]]*:[[:space:]]*\"[^\"]*\"/\"admin_user\": \"$ADMIN_USER\"/" "$CONF"
  # 沿用配置里的外网地址
  if [ -z "$PUBLIC_URL" ]; then
    PUBLIC_URL=$(sed -n 's/.*"public_url"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$CONF" | head -1)
  fi
else
  if [ -z "$PUBLIC_URL" ]; then
    IP=$(guess_ip)
    PUBLIC_URL="http://$IP:$PORT"
    echo "      未指定 --url，探测到公网地址 $PUBLIC_URL"
  fi
  cat > "$CONF" <<EOF
{
  "listen": "$LISTEN",
  "db": "$APP_DIR/data.json",
  "admin_user": "$ADMIN_USER",
  "admin_pass_hash": "",
  "cloudflare_token": "",
  "public_url": "$PUBLIC_URL",
  "node_bin_url": "",
  "probe_no_port": false
}
EOF
  chmod 600 "$CONF"
fi

# ---------- 3. 账号密码 ----------
echo "[3/5] 设置管理员账号密码"
if ! "$BIN" -config "$CONF" -passwd "$ADMIN_PASS" >/dev/null 2>&1; then
  echo "      设置密码失败，稍后用 journalctl -u relay-panel 查看初始密码" >&2
  ADMIN_PASS=""
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
if ! systemctl is-active --quiet relay-panel; then
  echo "启动失败，日志：" >&2
  journalctl -u relay-panel -n 30 --no-pager || true
  exit 1
fi

echo ""
echo "================================================"
echo "  中转面板已启动"
echo "  地址   : ${PUBLIC_URL:-http://<服务器公网IP>:$PORT}"
case "$LISTEN" in
  127.0.0.1:*|localhost:*)
    echo "  监听   : $LISTEN（只绑本机，外部访问需 nginx 反代）" ;;
  *)
    echo "  监听   : $LISTEN" ;;
esac
echo "  账号   : $ADMIN_USER"
if [ -n "$ADMIN_PASS" ]; then
  echo "  密码   : $ADMIN_PASS"
else
  echo "  密码   : （沿用原有配置）"
fi
echo "  数据   : $APP_DIR/data.json"
echo "================================================"
echo ""
echo "下一步："
echo "  1. 浏览器打开 $PUBLIC_URL 登录"
echo "  2. 「设置」确认 Public URL 是节点能访问到的地址（一键命令靠它）"
echo "  3. 「设置」填 Cloudflare Token，点测试"
echo "  4. 新建分组，填域名和 Zone，点测试"
echo "  5. 添加节点，复制命令到节点机执行"
echo ""
echo "常用："
echo "  systemctl status relay-panel       状态"
echo "  journalctl -u relay-panel -f       日志"
echo "  $BIN -config $CONF -passwd 新密码    改密码"
echo "  bash install-panel.sh --uninstall    卸载"
