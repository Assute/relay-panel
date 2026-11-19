# 中转面板 · 部署文档

---

## 最省事：管理菜单

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/Assute/relay-panel/main/install/install-panel.sh)
```

```
  ┌────────────────────────────────────────┐
  │          中转面板 · 管理菜单           │
  └────────────────────────────────────────┘
  未安装

    1) 安装 / 重装      4) 查看日志
    2) 修改账号密码      5) 重启面板
    3) 查看状态         6) 卸载
    0) 退出
```

选 1 开始安装，脚本会问外网地址（节点靠它下载，填错节点装不上），然后全自动装完。

装好后同一个命令还能改密码、看日志、重启、卸载，不用记别的东西。

**不想用菜单**，带参数直接装：

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/Assute/relay-panel/main/install/install-panel.sh) \
  --port 3070 --url http://你的服务器IP:3070
```

默认端口 3070，默认账号密码都是 `123456`。

国内机器访问 `raw.githubusercontent.com` 超时会自动切 jsDelivr CDN。

装完直接从下面「二、配面板」开始。

下面「一、装面板」是手动安装的完整步骤，两种方式选一种即可。

---
## 一、装面板

面板要装在一台**能连外网**的机器上，1 核 1G 足够。

```bash
tar xzf relay-panel-v1.0-linux-amd64.tar.gz
cd relay-panel-v1.0
chmod +x install-panel.sh
bash install-panel.sh --port 3070 --url http://你的服务器IP:3070
```

安装脚本会输出：

```
================================================
  中转面板已启动
  地址   : http://<本机IP>:3070
  账号   : 123456
  密码   : 123456
================================================
```

**默认账号密码都是 `123456`。**

### 参数说明

| 参数 | 说明 | 默认 |
|---|---|---|
| `--port 3070` | 监听端口，只绑 127.0.0.1 | 3070 |
| `--listen 0.0.0.0:3070` | 直接对外监听（不建议，建议走反代） | — |
| `--url http://1.2.3.4:3070` | **一键命令里的地址，必填外网可达** | 自动推测 |
| `--dir /opt/relay-panel` | 安装目录 | /opt/relay-panel |
| `--uninstall` | 卸载服务（保留数据） | — |

### 装完检查

```bash
systemctl status relay-panel
curl -s http://127.0.0.1:3070/ | head -c 100
```

---

## 二、配面板

浏览器打开 `http://服务器IP:3070`，用 `123456` / `123456` 登录。

### 1. 设置页（必须先做）

**Public URL** —— 一键命令里的地址。填错的后果是节点装不上。

要填节点能访问到的地址。如果面板在 NAT 后面或者走了反代，填反代后的域名：

```
https://panel.example.com
```

**Cloudflare Token** —— 切 DNS 用。

获取步骤：

1. Cloudflare 后台 → 右上角头像 → **My Profile** → **API Tokens**
2. **Create Token** → 选 **Edit zone DNS** 模板
3. 权限确认：`Zone` · `DNS` · `Edit`
4. Zone Resources：`Include` · `Specific zone` · 选你要用的域名
5. Continue → Create → **复制那串 token**

填进面板，点「测试 Token」，出现 ✅ 说明通了。

> 别用 Global API Key，权限过大。
> Token 权限只需要 DNS 编辑，别给全 Zone 权限。

### 2. 新建分组

一个分组 = 一组转发节点 + 一套转发规则。

| 字段 | 说明 |
|---|---|
| 分组名 | 随便填，比如「日本线路」 |
| Cloudflare Zone | 填 `example.com` 就行，不用找 Zone ID |
| 域名（完整） | `relay.example.com`，这条 A 记录会被切来切去 |
| TTL | 60（Cloudflare 免费版最小值） |
| 峰值带宽 | 用来算切换余量，填你的实际带宽（Mbps） |
| 探活端口 | 切流前探测节点用，填 `22` 或填 `0` 关闭 |
| 重置日 | 每月几号清零流量，1–28 |

保存后点分组编辑里的「测试这条 DNS 配置」：

| 结果 | 含义 |
|---|---|
| ✅ 记录存在，指向 x.x.x.x | 全对 |
| ⓘ Zone 正确，记录还不存在 | 正常，第一次切流时自动建 |
| ❌ 找不到 zone | 域名拼错或 token 区域没覆盖 |

### 3. 添加节点

节点页 → 添加节点，填：

| 字段 | 说明 |
|---|---|
| 名称 | 日本-A |
| 公网 IP | 这台机器的公网 IP，域名会指到它 |
| 网卡名 | 一般 `eth0` 或 `ens3`；OpenVZ 老机器是 `venet0` |
| 计费口径 | `both` 双向 / `tx` 只算出向 / `rx` 只算入向 |
| 本机流量配额 | 商家后台给的数，比如 200（GB） |
| 备用节点 | 勾了以后不参与正常轮换，只在主用全挂/全用尽时兜底 |

**计费口径怎么选**：去商家后台看一眼「已用流量」，和自己机器上算出来的对一下。选错了配额直接差一倍。

保存后会弹出一条命令：

```bash
curl -fsSL http://你的面板/i.sh | bash -s -- --panel=http://你的面板 --token=xxxxx
```

**这条命令只显示一次**，复制下来。

### 4. 添加转发线路

转发线路页 → 添加线路：

| 字段 | 说明 |
|---|---|
| 名称 | 新加坡 |
| 监听端口 | 客户连的那个端口，比如 25310 |
| 转发目标地址 | 流量要去的地方 |
| 目标端口 | 目标端口 |

**支持批量导入**：点「批量导入」，一行一个 JSON，兼容 Nyanpass 格式：

```json
{"dest":["18.141.35.11:25310"],"listen_port":25310,"name":"新加坡"}
{"dest":["156.246.89.218:25320"],"listen_port":25320,"name":"日本"}
```

导入完，面板会把规则同步给分组里所有节点（15 秒内）。

---

## 三、装节点

在每台节点机上，用 root 执行面板里复制的那条命令：

```bash
curl -fsSL http://你的面板/i.sh | bash -s -- --panel=http://你的面板 --token=xxxxx
```

脚本会：下载对应架构的二进制 → 写配置 → 装 systemd → 启动 → 注册。

**装完回面板刷新，节点应该变绿。**

### 节点上做了什么

```
/usr/local/bin/relay-node          二进制
/etc/relay/config.json             面板地址 + token
/etc/relay/...                     注册后换的 key
/etc/systemd/system/relay-node.service
```

### 检查节点

```bash
systemctl status relay-node
journalctl -u relay-node -f
```

正常会看到：

```
注册成功，已保存凭证
开始转发 :25310 -> 1.2.3.4:25310
配置 v3 已应用: 8 条规则
```

---

## 四、它是怎么工作的

```
域名 relay.example.com
   │
   ├─▶ 节点A（200G）  ─┐
   ├─▶ 节点B（200G）  ─┼─ 同一套转发规则
   └─▶ 节点C（备用）  ─┘
```

- **所有节点预置同一套规则**，所以切节点只改一条 DNS 记录，不下发配置
- 当前节点用量到阈值（默认 95%）→ 自动切下一个
- 当前节点失联（心跳 75 秒没动静）→ **立刻切**，不等用量
- 主用节点全部用尽或失联 → 切到备用节点
- 全部不可用 → 只有告警，域名停在原处（不会乱切到死节点）

### 流量怎么算

面板读节点上的 `/proc/net/dev`，也就是**商家后台用的同一个计数器**。不会出现「面板说还有 50G，商家停了」这种事。

切换阈值公式：

```
余量 = max(配额 × 5%, 峰值带宽 × 10分钟)
阈值 = 1 - 余量 / 配额
```

200G 配额 + 200Mbps 峰值 → 约 92.5%。留出的是「切换后旧节点上的长连接还剩多少要跑完」。

### 服务重启会怎样

- **面板挂了**：节点按缓存继续转发，恢复后自动重连
- **节点挂了**：面板 75 秒内发现，自动切走
- **节点重装**：旧 token 失效，面板里删掉重新加

---

## 五、反代（可选，推荐）

面板默认 `0.0.0.0:3070` 对外监听。想改成只绑本机 + nginx 反代（推荐，能上 HTTPS）：

```nginx
server {
    listen 443 ssl http2;
    server_name panel.example.com;

    ssl_certificate     /path/fullchain.pem;
    ssl_certificate_key /path/privkey.pem;

    client_max_body_size 100m;

    location / {
        proxy_pass http://127.0.0.1:3070;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

反代后记得把面板「设置」里的 **Public URL** 改成 `https://panel.example.com`，否则一键命令还是 http 的。

> 本面板走 HTTP 轮询，不用 WebSocket，nginx 不需要配 `Upgrade` 头。

---

## 六、常见问题

**节点一直显示「未接入」**

1. `journalctl -u relay-node -n 30` 看报错
2. `token_invalid` → token 已用过，面板里删掉重新添加
3. 连不上面板 → 检查 Public URL 是否外网可达，`curl http://面板/data`  试试
4. `fingerprint_mismatch` → 换了机器，面板里删掉重新加

**节点在线但流量不涨**

网卡名填错了。在节点机上跑：

```bash
cat /proc/net/dev     # 看实际网卡名
```

把正确的名字填回面板节点编辑里，或者点「删除」重建。

**切流没生效**

1. 设置页的 Cloudflare Token 是否配了、测试是否通过
2. 分组里的 Zone / 域名是否填对
3. `journalctl -u relay-panel -n 30` 看有没有 DNS 报错
4. 切完后用 `nslookup 你的域名` 确认解析变了（注意本地缓存）

**流量数字和商家后台对不上**

1. 计费口径选错了（both / tx / rx）
2. 云厂商读的是 hypervisor 层，和机器内差 1–3% 属正常
3. 节点上跑了别的服务（系统更新、备份），那部分也算商家的量

**能不能只绑一个端口转发多条线路**

可以，每条线路一个监听端口，互不影响。

**密码忘了**

```bash
/opt/relay-panel/panel -config /opt/relay-panel/config.json -passwd 新密码
systemctl restart relay-panel
```

---

## 七、备份

整个状态就是一个文件：

```
/opt/relay-panel/data.json
```

```bash
cp /opt/relay-panel/data.json /root/relay-panel-$(date +%F).json
```

配置在 `/opt/relay-panel/config.json`（含 Cloudflare Token，注意别外传）。

---

## 八、已知限制

| 限制 | 说明 |
|---|---|
| 只支持 TCP | UDP 转发还没做 |
| 串行切流 | 同一时刻只有一台节点在出力，5 台 200G 是 5 倍容量但一次用一份 |
| 切换有窗口 | 靠 5% 余量兜住，TTL 60 秒时约 1–2 分钟 |
| 单管理员 | 没有多用户 / 套餐 / 计费 |

---

## 附：文件位置速查

| 东西 | 位置 |
|---|---|
| 面板程序 | `/opt/relay-panel/panel` |
| 面板配置 | `/opt/relay-panel/config.json` |
| 面板数据 | `/opt/relay-panel/data.json` |
| 面板服务 | `/etc/systemd/system/relay-panel.service` |
| 节点下载 | `/opt/relay-panel/dl/node-linux-*` |
| 节点程序 | `/usr/local/bin/relay-node` |
| 节点配置 | `/etc/relay/config.json` |
| 节点服务 | `/etc/systemd/system/relay-node.service` |
