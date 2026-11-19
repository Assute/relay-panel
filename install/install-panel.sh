#!/bin/bash
# 中转面板 · 管理菜单
#
#   bash <(curl -fsSL https://raw.githubusercontent.com/Assute/relay-panel/main/install/install-panel.sh)
#
# 带参数则跳过菜单直接安装（用于自动化）：
#   bash install-panel.sh --port 3070 --url http://1.2.3.4:3070
#
# 参数：
#   --port 3070              监听端口（默认只绑 127.0.0.1）
#   --listen 0.0.0.0:3070    直接对外监听
#   --url  http://1.2.3.4:3070   一键命令的外网地址
#   --user 123456            管理员账号
#   --pass 123456            管理员密码
#   --dir  /opt/relay-panel  安装目录
set -uo pipefail

APP_DIR="/opt/relay-panel"
LISTEN="127.0.0.1:3070"
PUBLIC_URL=""
ADMIN_USER="123456"
ADMIN_PASS="123456"
REPO_BASES=(
  "https://cdn.jsdelivr.net/gh/Assute/relay-panel@main/dl"
  "https://raw.githubusercontent.com/Assute/relay-panel/main/dl"
)

BIN="$APP_DIR/panel"
CONF="$APP_DIR/config.json"
DATA="$APP_DIR/data.json"
SVC="/etc/systemd/system/relay-panel.service"

die()  { echo "  ✗ $*" >&2; }
note() { echo "  · $*"; }
okay() { echo "  ✓ $*"; }

need_root() {
  if [ "$(id -u)" != "0" ]; then
    die "需要 root 权限，请用 sudo 或 root 运行"
    exit 1
  fi
}

# ---------- 交互能力 ----------
# curl | bash 时 stdin 是管道，菜单读不到输入，改从终端读
setup_tty() {
  if [ -t 0 ]; then
    return 0
  fi
  if [ -e /dev/tty ] && [ -r /dev/tty ]; then
    exec < /dev/tty
    return 0
  fi
  return 1
}

pause() {
  printf "\n  按回车返回菜单…"
  read -r _ || true
}

ask() {  # ask 提示 默认值
  local prompt="$1" def="${2:-}" ans
  if [ -n "$def" ]; then
    printf "  %s [%s]: " "$prompt" "$def"
  else
    printf "  %s: " "$prompt"
  fi
  read -r ans || true
  [ -z "$ans" ] && ans="$def"
  echo "$ans"
}

confirm() {  # confirm 问题
  local ans
  printf "  %s [y/N]: " "$1"
  read -r ans || true
  case "$ans" in y|Y|yes|YES) return 0 ;; *) return 1 ;; esac
}

# ---------- 架构与下载 ----------
detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64)  echo amd64 ;;
    aarch64|arm64) echo arm64 ;;
    *) echo "" ;;
  esac
}

fetch() {
  local name="$1" dest="$2"
  for base in "${REPO_BASES[@]}"; do
    if curl -fsSL --connect-timeout 10 --max-time 300 "$base/$name" -o "$dest" 2>/dev/null; then
      if [ -s "$dest" ]; then return 0; fi
    fi
  done
  return 1
}

guess_ip() {
  local ip="" ext=""
  ip=$(ip route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src"){print $(i+1);exit}}')
  [ -z "$ip" ] && ip=$(hostname -I 2>/dev/null | awk '{print $1}')
  case "$ip" in
    10.*|192.168.*|172.1[6-9].*|172.2[0-9].*|172.3[01].*|127.*|"")
      ext=$(curl -fsS --connect-timeout 5 --max-time 8 https://api.ipify.org 2>/dev/null || true)
      if [ -z "$ext" ]; then
        ext=$(curl -fsS --connect-timeout 5 --max-time 8 https://ipv4.icanhazip.com 2>/dev/null | tr -d '\r\n' || true)
      fi
      [ -n "$ext" ] && ip="$ext"
      ;;
  esac
  echo "$ip"
}

# ---------- 状态辅助 ----------
is_installed() { [ -x "$BIN" ] && [ -f "$CONF" ]; }
is_running()   { systemctl is-active --quiet relay-panel 2>/dev/null; }

get_conf() {  # get_conf 键名
  [ -f "$CONF" ] || return 0
  sed -n "s/.*\"$1\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" "$CONF" | head -1
}

human_size() {
  local b="${1:-0}"
  if [ "$b" -ge 1048576 ]; then
    echo "$((b / 1048576)) MB"
  elif [ "$b" -ge 1024 ]; then
    echo "$((b / 1024)) KB"
  else
    echo "$b B"
  fi
}

# ---------- 3 状态 ----------
do_status() {
  echo ""
  if ! is_installed; then
    note "面板还没安装"
    note "选 1 开始安装"
    return
  fi

  local port listen url user
  listen=$(get_conf listen); [ -z "$listen" ] && listen="127.0.0.1:3070"
  port="${listen##*:}"
  url=$(get_conf public_url)
  user=$(get_conf admin_user); [ -z "$user" ] && user="123456"

  echo "  ┌─────────────── 面板状态 ───────────────"
  if is_running; then
    echo "  │ 服务    运行中"
  else
    echo "  │ 服务    已停止"
  fi
  echo "  │ 监听    $listen"
  [ -n "$url" ] && echo "  │ 地址    $url"
  echo "  │ 账号    $user"

  if [ -f "$DATA" ]; then
    echo "  │ 数据    $(human_size "$(stat -c%s "$DATA")")  $DATA"
  else
    echo "  │ 数据    尚未生成"
  fi

  if command -v ss >/dev/null 2>&1; then
    if ss -tln 2>/dev/null | grep -q ":$port "; then
      echo "  │ 端口    $port 已监听"
    else
      echo "  │ 端口    $port 未监听"
    fi
  fi

  if is_running; then
    local since start now dif d h m
    since=$(systemctl show -p ActiveEnterTimestamp --value relay-panel 2>/dev/null)
    if [ -n "$since" ]; then
      start=$(date -d "$since" +%s 2>/dev/null || echo 0)
      now=$(date +%s)
      if [ "$start" -gt 0 ]; then
        dif=$((now - start))
        d=$((dif / 86400)); h=$((dif % 86400 / 3600)); m=$((dif % 3600 / 60))
        if [ "$d" -gt 0 ]; then
          echo "  │ 运行    ${d}天${h}小时"
        elif [ "$h" -gt 0 ]; then
          echo "  │ 运行    ${h}小时${m}分"
        else
          echo "  │ 运行    ${m}分钟"
        fi
      fi
    fi
  fi
  echo "  └────────────────────────────────────────"
  echo ""
  echo "  最近日志："
  journalctl -u relay-panel -n 3 --no-pager 2>/dev/null | sed 's/^/    /'
}

# ---------- 1 安装 ----------
do_install() {
  echo ""
  need_root

  local arch
  arch=$(detect_arch)
  if [ -z "$arch" ]; then
    die "不支持的架构: $(uname -m)（只提供 amd64 / arm64）"
    return
  fi

  command -v curl >/dev/null 2>&1 || { die "缺少 curl，先装：apt install -y curl"; return; }

  local reinst=false
  if is_installed; then
    reinst=true
    note "检测到已安装，将覆盖程序并重启"
    note "数据（$DATA）不会动"
    if ! confirm "继续？"; then
      note "已取消"
      return
    fi
  fi

  if [ -z "$PUBLIC_URL" ] && [ "$reinst" = "true" ]; then
    PUBLIC_URL=$(get_conf public_url)
  fi
  if [ -z "$PUBLIC_URL" ]; then
    local guess
    guess="http://$(guess_ip):${LISTEN##*:}"
    echo ""
    note "外网地址用于生成节点一键接入命令"
    note "节点必须能访问它，填错节点装不上"
    PUBLIC_URL=$(ask "外网地址" "$guess")
  fi

  if [ "$reinst" = "true" ]; then
    local cu
    cu=$(get_conf admin_user); [ -z "$cu" ] && cu="123456"
    note "当前账号: $cu"
    if ! confirm "同时重置账号密码为 $ADMIN_USER / $ADMIN_PASS ？"; then
      ADMIN_PASS=""
      ADMIN_USER="$cu"
    fi
  fi

  echo ""
  mkdir -p "$APP_DIR/dl"

  echo "[1/5] 安装到 $APP_DIR"
  if [ -f "./panel" ] && [ "$(stat -c%s ./panel 2>/dev/null || echo 0)" -gt 1000000 ]; then
    install -m 0755 ./panel "$BIN" || { die "复制 panel 失败"; return; }
    note "使用当前目录的 panel"
  else
    local tmp
    tmp=$(mktemp)
    note "下载面板程序 ($arch)"
    if ! fetch "panel-linux-$arch" "$tmp"; then
      rm -f "$tmp"
      die "下载失败，检查网络后重试"
      return
    fi
    install -m 0755 "$tmp" "$BIN" || { rm -f "$tmp"; die "安装失败"; return; }
    rm -f "$tmp"
  fi

  local a
  for a in amd64 arm64 arm; do
    [ -f "$APP_DIR/dl/node-linux-$a" ] && continue
    if [ -f "./node-linux-$a" ]; then
      install -m 0755 "./node-linux-$a" "$APP_DIR/dl/node-linux-$a"
    elif fetch "node-linux-$a" "$APP_DIR/dl/node-linux-$a.tmp"; then
      chmod +x "$APP_DIR/dl/node-linux-$a.tmp"
      mv -f "$APP_DIR/dl/node-linux-$a.tmp" "$APP_DIR/dl/node-linux-$a"
    else
      rm -f "$APP_DIR/dl/node-linux-$a.tmp"
      note "警告: node-linux-$a 下载失败，该架构节点无法一键安装"
    fi
  done
  note "节点端: $(ls "$APP_DIR/dl" 2>/dev/null | tr '\n' ' ')"

  echo "[2/5] 写配置 $CONF"
  if [ -f "$CONF" ]; then
    note "配置已存在，保留原有设置"
    sed -i "s/\"admin_user\"[[:space:]]*:[[:space:]]*\"[^\"]*\"/\"admin_user\": \"$ADMIN_USER\"/" "$CONF" 2>/dev/null || true
  else
    cat > "$CONF" <<EOF
{
  "listen": "$LISTEN",
  "db": "$DATA",
  "admin_user": "$ADMIN_USER",
  "admin_pass_hash": "",
  "cloudflare_token": "",
  "public_url": "$PUBLIC_URL",
  "node_bin_url": "",
  "probe_no_port": false
}
EOF
    chmod 600 "$CONF"
    note "外网地址 $PUBLIC_URL"
  fi

  echo "[3/5] 设置管理员账号密码"
  if [ -n "$ADMIN_PASS" ]; then
    if "$BIN" -config "$CONF" -passwd "$ADMIN_PASS" >/dev/null 2>&1; then
      note "账号 $ADMIN_USER / 密码 $ADMIN_PASS"
    else
      note "设置密码失败，可从菜单选 2 重试"
    fi
  else
    note "密码保持不变"
  fi

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

  echo "[5/5] 启动"
  sleep 2
  if ! is_running; then
    die "启动失败"
    echo ""
    journalctl -u relay-panel -n 20 --no-pager 2>/dev/null | sed 's/^/    /'
    return
  fi

  echo ""
  echo "  ╔══════════════════════════════════════════"
  echo "  ║  安装完成"
  echo "  ╠══════════════════════════════════════════"
  echo "  ║  地址   ${PUBLIC_URL}"
  case "$LISTEN" in
    127.0.0.1:*|localhost:*) echo "  ║  监听   $LISTEN（本机，外部需反代）" ;;
    *)                       echo "  ║  监听   $LISTEN" ;;
  esac
  echo "  ║  账号   $ADMIN_USER"
  if [ -n "$ADMIN_PASS" ]; then
    echo "  ║  密码   $ADMIN_PASS"
  else
    echo "  ║  密码   （未改动）"
  fi
  echo "  ╚══════════════════════════════════════════"
  echo ""
  echo "  下一步："
  echo "    1. 浏览器打开 $PUBLIC_URL"
  echo "    2. 设置 → 确认 Public URL（节点靠它下载）"
  echo "    3. 设置 → 填 Cloudflare Token → 点测试"
  echo "    4. 新建分组 → 填域名和 Zone → 点测试"
  echo "    5. 添加节点 → 复制命令到节点机执行"
}

# ---------- 2 改账号密码 ----------
do_passwd() {
  echo ""
  need_root
  if ! is_installed; then
    note "面板还没安装"
    return
  fi

  local cu nu np
  cu=$(get_conf admin_user); [ -z "$cu" ] && cu="123456"
  note "当前账号: $cu"
  echo ""

  nu=$(ask "新账号" "$cu")
  [ -z "$nu" ] && { die "账号不能为空"; return; }

  printf "  新密码（留空只改账号）: "
  read -r np || true

  if [ -n "$np" ]; then
    if ! "$BIN" -config "$CONF" -passwd "$np" >/dev/null 2>&1; then
      die "设置密码失败"
      return
    fi
  fi

  if [ "$nu" != "$cu" ]; then
    sed -i "s/\"admin_user\"[[:space:]]*:[[:space:]]*\"[^\"]*\"/\"admin_user\": \"$nu\"/" "$CONF" || {
      die "写入账号失败"
      return
    }
  fi

  systemctl restart relay-panel
  sleep 1
  echo ""
  okay "账号: $nu"
  if [ -n "$np" ]; then okay "密码: $np"; else note "密码未改动"; fi
  if is_running; then
    okay "服务已重启"
  else
    die "重启后服务未运行"
    journalctl -u relay-panel -n 15 --no-pager 2>/dev/null | sed 's/^/    /'
  fi
}

# ---------- 4 日志 ----------
do_logs() {
  echo ""
  if ! is_installed; then
    note "面板还没安装"
    return
  fi
  echo "  最近 50 行："
  echo "  ────────────────────────────────────────"
  journalctl -u relay-panel -n 50 --no-pager 2>/dev/null | sed 's/^/  /'
  echo "  ────────────────────────────────────────"
  if confirm "实时跟踪？（Ctrl+C 退出）"; then
    echo ""
    journalctl -u relay-panel -f -n 0 || true
  fi
}

# ---------- 5 重启 ----------
do_restart() {
  echo ""
  need_root
  if ! is_installed; then
    note "面板还没安装"
    return
  fi
  systemctl restart relay-panel
  sleep 1
  if is_running; then
    okay "已重启"
  else
    die "重启失败"
    journalctl -u relay-panel -n 15 --no-pager 2>/dev/null | sed 's/^/    /'
  fi
}

# ---------- 6 卸载 ----------
do_uninstall() {
  echo ""
  need_root
  if ! is_installed; then
    note "面板还没安装"
    return
  fi

  echo "  将停止并删除服务：$SVC"
  echo "  程序目录：$APP_DIR"
  echo ""
  if ! confirm "确认卸载？"; then
    note "已取消"
    return
  fi

  systemctl stop relay-panel 2>/dev/null || true
  systemctl disable relay-panel 2>/dev/null || true
  rm -f "$SVC"
  systemctl daemon-reload 2>/dev/null || true

  local deldata=0
  if [ -f "$DATA" ]; then
    echo ""
    note "数据还留着：$DATA"
    note "里面是分组、节点、规则、流量记录"
    if confirm "连数据一起删除？"; then
      deldata=1
    fi
  fi

  if [ "$deldata" = "1" ]; then
    rm -rf "$APP_DIR"
    okay "服务、程序、数据已全部删除"
  else
    rm -f "$BIN"
    rm -rf "$APP_DIR/dl"
    okay "服务已移除，程序已删除"
    note "数据保留在 $DATA"
    note "彻底清理：rm -rf $APP_DIR"
  fi
}

# ---------- 菜单 ----------
show_banner() {
  echo ""
  echo "  ┌────────────────────────────────────────┐"
  echo "  │          中转面板 · 管理菜单           │"
  echo "  └────────────────────────────────────────┘"
  if is_installed; then
    if is_running; then
      echo "  运行中 · $(get_conf public_url)"
    else
      echo "  已停止"
    fi
  else
    echo "  未安装"
  fi
  echo ""
}

menu() {
  while true; do
    show_banner
    echo "    1) 安装 / 重装"
    echo "    2) 修改账号密码"
    echo "    3) 查看状态"
    echo "    4) 查看日志"
    echo "    5) 重启面板"
    echo "    6) 卸载"
    echo "    0) 退出"
    echo ""
    printf "  请输入序号: "
    local choice
    read -r choice || exit 0

    case "$choice" in
      1) do_install;   pause ;;
      2) do_passwd;    pause ;;
      3) do_status;    pause ;;
      4) do_logs;      pause ;;
      5) do_restart;   pause ;;
      6) do_uninstall; pause ;;
      0|q|Q) echo ""; echo "  再见"; echo ""; exit 0 ;;
      "") ;;
      *) echo ""; die "无效序号: $choice"; sleep 1 ;;
    esac
  done
}

# ---------- 入口 ----------
HAS_ARGS=0
while [ $# -gt 0 ]; do
  HAS_ARGS=1
  case "$1" in
    --port)      LISTEN="127.0.0.1:$2"; shift 2 ;;
    --listen)    LISTEN="$2"; shift 2 ;;
    --url)       PUBLIC_URL="$2"; shift 2 ;;
    --user)      ADMIN_USER="$2"; shift 2 ;;
    --pass)      ADMIN_PASS="$2"; shift 2 ;;
    --dir)       APP_DIR="$2"; BIN="$APP_DIR/panel"; CONF="$APP_DIR/config.json"; DATA="$APP_DIR/data.json"; shift 2 ;;
    --uninstall) need_root; do_uninstall; exit 0 ;;
    -h|--help)   sed -n '2,18p' "$0"; exit 0 ;;
    *) echo "未知参数: $1" >&2; exit 1 ;;
  esac
done

if [ "$HAS_ARGS" = "1" ]; then
  need_root
  do_install
  exit 0
fi

need_root
if ! setup_tty; then
  die "当前环境没有终端，无法显示菜单"
  echo ""
  echo "  改用带参数安装："
  echo "    bash $0 --port 3070 --url http://1.2.3.4:3070"
  exit 1
fi
menu
