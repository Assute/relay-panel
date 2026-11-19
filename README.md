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

## 一键安装

面板装在一台能连外网的机器上，1 核 1G 够用。只要 root，不用装任何依赖。

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/Assute/relay-panel/main/install/install-panel.sh)
```

指定端口和外网地址（推荐，省得装完再进面板改）：

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/Assute/relay-panel/main/install/install-panel.sh) \
  --port 8080 --url http://你的服务器IP:8080
```

脚本会：识别 CPU 架构 → 下载对应二进制 → 写配置 → 生成随机密码 → 装 systemd → 启动。

装完终端直接打印：

```
================================================
  中转面板已启动
  地址   : http://<本机IP>:8080
  账号   : admin
  密码   : xxxxxxxxxxxx
================================================
```

**密码只显示这一次。**

> 国内机器访问 `raw.githubusercontent.com` 常常超时。脚本内置 jsDelivr 备用源，会自动切换。两个源都不通的话，可以直接拉包：
>
> ```bash
> curl -fL -o panel https://cdn.jsdelivr.net/gh/Assute/relay-panel@main/dl/panel-linux-amd64
> curl -fL -o install-panel.sh https://cdn.jsdelivr.net/gh/Assute/relay-panel@main/install/install-panel.sh
> chmod +x panel install-panel.sh && bash install-panel.sh
> ```

### 常用参数

| 参数 | 说明 | 默认 |
|---|---|---|
| `--port 8080` | 监听端口，只绑 127.0.0.1 | 8080 |
| `--listen 0.0.0.0:8080` | 直接对外监听 | — |
| `--url http://1.2.3.4:8080` | 一键命令里的外网地址 | 自动推测 |
| `--dir /opt/relay-panel` | 安装目录 | /opt/relay-panel |
| `--uninstall` | 卸载服务，保留数据 | — |

### 装完检查

```bash
systemctl status relay-panel
journalctl -u relay-panel -f
curl -s http://127.0.0.1:8080/ | head -c 80
```

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
