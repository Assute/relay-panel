# relay-panel · 中转面板

多台 VPS 拼流量池，一个面板统一管：分组、配额、一键接入、用满自动切 DNS。

自己用的小型转发面板。单二进制，零外部依赖（不需要 MySQL / Redis / nginx）。

![主页](docs/ui-home.png)

## 它解决什么问题

手里一堆 200G 流量的小鸡，想当成一条线路用：

```
域名 relay.example.com
   │
   ├─▶ 节点A（200G）  ─┐
   ├─▶ 节点B（200G）  ─┼─ 同一套转发规则
   └─▶ 节点C（备用）  ─┘
```

- 所有节点预置同一套转发规则，域名指哪台哪台干活
- 哪台用满了，自动改 DNS 切到下一台
- 哪台挂了（心跳 75 秒没动静），**立刻切走**，不等用量
- 主用全用完或全失联，切到备用节点兜底
- 全部不可用，只告警，不会乱切到死节点

## 特性

| 能力 | 说明 |
|---|---|
| 分组管理 | 一组节点 + 一套规则 + 一条域名 |
| 两级配额 | 节点各自 200G，按网卡实际用量算 |
| 一键接入 | 面板生成命令，节点上 curl 一下就跑起来 |
| 规则同步 | 规则全量下发，版本号对不上才推 |
| 自动切流 | 用量到阈值 / 节点失联 两种触发 |
| 备用节点 | 主用全挂时兜底 |
| 延迟探测 | 面板到节点的 TCP 握手 RTT，两分钟一轮 |
| Cloudflare | 自动解析 Zone、自动建记录、切 A 记录 |
| 批量导入导出 | JSONL，兼容 Nyanpass 格式 |
| 手机端 | 底部导航、表格横滚 |
| 深色模式 | 记住选择 |

**不支持**：UDP 转发、多用户、套餐计费。

## 流量口径

面板读节点上的 `/proc/net/dev`——**商家后台用的同一个计数器**。

不会出现「面板说还剩 50G，商家已经把机器停了」这种事。计费口径可选双向 / 只算出向 / 只算入向，按商家后台对一遍就知道该选哪个。

## 编译

需要 Go 1.23+。零第三方依赖，不用 `go mod download`。

```bash
make panel    # 当前平台面板
make linux    # 三架构节点端（amd64 / arm64 / arm）
make node     # 当前平台节点端（本地调试用）
```

或手动：

```bash
go build -o bin/panel ./cmd/panel

GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o bin/node-linux-amd64 ./cmd/node
GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o bin/node-linux-arm64 ./cmd/node
GOOS=linux GOARCH=arm   go build -trimpath -ldflags="-s -w" -o bin/node-linux-arm   ./cmd/node
```

## 一键安装 / 管理

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/Assute/relay-panel/main/install/install-panel.sh)
```

打开管理菜单：

```
  ┌────────────────────────────────────────┐
  │          中转面板 · 管理菜单           │
  └────────────────────────────────────────┘
  未安装

    1) 安装 / 重装
    2) 修改账号密码
    3) 查看状态
    4) 查看日志
    5) 重启面板
    6) 卸载
    0) 退出

  请输入序号:
```

| 序号 | 做什么 |
|---|---|
| 1 | 识别架构、下载二进制、写配置、装 systemd、启动。重装会保留数据 |
| 2 | 改账号、改密码，改完自动重启 |
| 3 | 服务状态、监听地址、端口是否在听、数据大小、运行时长、最近日志 |
| 4 | 最近 50 行日志，可选实时跟踪 |
| 5 | 重启 |
| 6 | 卸载，二次确认，可选是否连数据一起删 |

装完打印：

```
  ╔══════════════════════════════════════════
  ║  安装完成
  ╠══════════════════════════════════════════
  ║  地址   http://23.106.44.26:3070
  ║  监听   127.0.0.1:3070（本机，外部需反代）
  ║  账号   123456
  ║  密码   123456
  ╚══════════════════════════════════════════
```

### 不想用菜单

带参数就会跳过菜单直接装，适合写进部署脚本：

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/Assute/relay-panel/main/install/install-panel.sh) \
  --port 3070 --url http://你的服务器IP:3070
```

| 参数 | 说明 | 默认 |
|---|---|---|
| `--port 3070` | 监听端口，只绑 127.0.0.1 | 3070 |
| `--listen 0.0.0.0:3070` | 直接对外监听 | — |
| `--url http://1.2.3.4:3070` | 一键命令里的外网地址，不填会自动探测 | 自动 |
| `--user 123456` | 管理员账号 | 123456 |
| `--pass 123456` | 管理员密码 | 123456 |
| `--dir /opt/relay-panel` | 安装目录 | /opt/relay-panel |
| `--uninstall` | 直接卸载，不进菜单 | — |

### 网络不通时

国内机器访问 `raw.githubusercontent.com` 常超时。脚本内置 jsDelivr 备用源会自动切换。两个都不通时手动拉：

```bash
curl -fL -o panel https://cdn.jsdelivr.net/gh/Assute/relay-panel@main/dl/panel-linux-amd64
curl -fL -o install-panel.sh https://cdn.jsdelivr.net/gh/Assute/relay-panel@main/install/install-panel.sh
chmod +x panel && bash install-panel.sh
```

本地已有 `panel` 文件时脚本会直接用，不再下载。
## 节点接入

面板里「添加节点」后会生成一条命令，复制到节点机上用 root 执行：

```bash
curl -fsSL http://你的面板/i.sh | bash -s -- --panel=http://你的面板 --token=xxxxx
```

节点脚本会：下载对应架构二进制 → 写配置 → 装 systemd → 启动 → 注册。

装完回面板刷新，节点变绿即成功。

```bash
systemctl status relay-node
journalctl -u relay-node -f
```

## 手动部署

不想用脚本的话：

```bash
# 面板机
mkdir -p /opt/relay-panel/dl
install -m755 panel /opt/relay-panel/panel
cp node-linux-* /opt/relay-panel/dl/
/opt/relay-panel/panel -config /opt/relay-panel/config.json   # 首次运行生成配置和密码
```

systemd 单元见 [install/panel.service](install/panel.service)。

完整部署文档（Cloudflare 配置、反代、排错）见 [install/DEPLOY.md](install/DEPLOY.md)。

## 目录

```
cmd/panel/          面板入口
cmd/node/           节点入口
internal/panel/     存储、API、调度、DNS、Web UI
internal/node/      Agent、TCP 转发器、网卡采样
install/            systemd 单元 + 安装脚本
docs/               界面截图
```

## 数据

整个状态就是一个 JSON 文件：

```
/opt/relay-panel/data.json
```

备份直接 `cp`。配置 `config.json` 含 Cloudflare Token，别外传。

## 已知限制

| 限制 | 说明 |
|---|---|
| 只支持 TCP | UDP 还没做 |
| 串行切流 | 同一时刻只有一台节点在出力 |
| 切换有窗口 | TTL 60 秒时约 1–2 分钟，靠 5% 余量兜住 |
| 单管理员 | 没有多用户和计费 |
