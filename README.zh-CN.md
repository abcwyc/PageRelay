# PageRelay

中文 | [English](./README.md)

> 一个面向团队与内网的轻量级文件管理与公开分享服务。单个 Go 二进制同时提供管理端和公开分享端，元数据保存在 SQLite，用户文件保存在本地磁盘。

PageRelay 把“上传文件 → 生成链接 → 对外分享”这条最常见的工作流压缩到一个二进制里：内部同事用它管理资料、协作分发，外部用户通过独立的公开域名安全访问被分享的内容。无需对象存储、无需数据库运维、无需前端工程化，部署即用。

项目以 [MIT 协议开源](#license)，源码完整公开，方便部署者学习、审阅、二次开发和自定义修改。

## 在线 Demo

如果你想先体验再决定是否部署，可以使用官方在线 Demo：

- **Demo 地址**：<https://pagerelay.348580.xyz/>
- **账号**：`demo`
- **密码**：`1234567890`

> Demo 环境是一个体验账号，数据会被定期重置。请勿上传任何真实或敏感文件。体验流程建议：
> 1. 用上面的账号密码登录管理端，浏览文件工作台。
> 2. 试着上传一个小文件、新建一个 Markdown，体验预览与编辑。
> 3. 打开某个文件的分享开关，复制公开链接在隐身窗口访问，体验内外双端隔离。
> 4. 关闭分享，再次访问旧链接，确认立即失效。

---

## 目录

- [在线 Demo](#在线-demo)
- [产品价值](#产品价值)
- [设计背景](#设计背景)
- [核心特性](#核心特性)
- [架构概览](#架构概览)
- [快速开始](#快速开始)
- [部署指南](#部署指南)
- [使用指南](#使用指南)
- [配置参数](#配置参数)
- [安全模型](#安全模型)
- [测试与构建](#测试与构建)
- [历史兼容](#历史兼容)
- [License](#license)

---

## 产品价值

团队内部每天都在产生需要“对内管理、对外分享”的文件：产品资料、销售报价、客户交付物、活动素材、演示页面、Markdown 文档。常见的解法要么是把文件丢进聊天工具（无法沉淀、无法回收），要么是接一个公有云网盘（数据外流、合规风险），要么是自建一套对象存储 + 网关 + 前端的三件套（运维成本高、迭代慢）。

PageRelay 试图在“够用”和“可控”之间找到一个平衡点：

- **一条命令上线**：一个 Go 二进制 + 一个 SQLite 文件 + 一个磁盘目录，复制到任意 Linux 机器即可跑起来。没有外部依赖，没有容器编排负担。
- **内外双端隔离**：管理端面向登录用户，公开分享端面向匿名访客。两者监听不同端口、不同域名，分享内容受独立沙箱约束，公开域不暴露任何管理接口。
- **分享即一个开关**：每个文件/文件夹的分享只有“开启 / 关闭”两种状态。关闭后旧链接立刻失效，再次开启会生成新链接——既简单，又满足“分享可回收”的合规直觉。
- **所见即所得的预览**：图片、PDF、视频、音频、文本、代码、Markdown、HTML、SVG 都能在线预览；HTML/SVG 在独立分享域的沙箱中运行，Markdown 使用安全模式渲染，避免 XSS 风险。
- **多人多账号**：管理员可以创建账号、禁用账号、重置密码、分配存储配额；每个账号的数据目录相互隔离，逻辑路径以账号为根（如 `/demo/产品资料/index.html`）。
- **审计可追溯**：登录、登出、登录失败、密码变更等关键操作写入审计日志，管理员可在后台查看。
- **零前端工程化**：界面用原生 HTML + 一份 `app.js` 实现，无构建步骤、无 npm 依赖，便于内网环境审阅和二次定制。

简单来说，PageRelay 适合：**需要在内网或小范围公网快速搭一个“文件管理 + 公开分享”服务的团队**，尤其是对数据出境、外部存储合规有顾虑的场景。

---

## 设计背景

PageRelay 的前身是一个公司内网的文件中转站（`company-filehub`），最初只解决“同事之间互传大文件”的问题。随着使用范围扩大，出现了几个反复出现的需求：

1. **要把资料发给外部**，但又不希望把文件上传到公有云。
2. **要能随时撤回**已经发出去的链接，而不是任由它永久有效。
3. **要能在线看**，避免对方下载一个不知道打不开的文件。
4. **要能多人共用一套服务**，但彼此的数据互不串扰。
5. **要足够轻**，不要让运维再装一套数据库和对象存储。

PageRelay 2.0 就是在这些诉求上重构的版本：引入了哈啰租车品牌风格的视觉、双栏文件工作台、拖拽上传、目录树懒加载、文件多选与批量操作、命令面板（`⌘/Ctrl + K`）、沙箱化 HTML 预览、账号配额与审计日志等能力，同时把分享模型简化为“开关式”，去掉了原本容易让人困惑的“有效期”概念。

它不追求取代网盘或对象存储，而是覆盖“80% 的日常文件分发场景”里那些最磨人的细节。

---

## 核心特性

### 文件工作台

- 哈啰租车品牌 Logo 与紧凑、克制的双栏文件工作台
- 可拖动侧栏分隔线，浏览器会保存设备本地的宽度偏好
- 左侧固定拖拽上传入口和懒加载文件目录树
- 右侧目录表格、排序、搜索、文件详情与快捷预览
- 文件多选、批量移动、批量删除和 ZIP 下载
- 将文件或多选项目拖到列表文件夹、左侧目录树或根目录即可移动
- `⌘/Ctrl + K` 文件搜索与命令面板

### 预览与编辑

- 图片、PDF、视频、音频、文本和常见代码文件在线预览
- HTML、SVG 在独立分享域的沙箱中运行；Markdown 使用安全模式渲染
- 新建并编辑 Markdown、HTML 和纯文本文件

### 分享

- 分享只有开启和关闭两种状态，不再限制有效期
- 新上传和新建的文件默认开启分享；关闭后旧链接立即失效，再开启会生成新链接
- 文件逻辑路径以登录账号为根，例如 `/admin/产品资料/index.html`

### 账号与管理

- 管理员创建、禁用账号，重置密码并分配存储配额
- 账号数据目录隔离、路径穿越防护、CSRF 防护、Argon2id 密码哈希和操作审计

---

## 架构概览

```
                      ┌────────────────────────────────────────────┐
                      │              单个 Go 二进制                 │
                      │                                            │
   登录用户 ──HTTP──▶  │  管理端 (management listener :8080)         │
                      │   ├── 登录 / 登出 / 改密                    │
                      │   ├── 文件工作台 / 上传 / 预览 / 编辑       │
                      │   ├── 分享记录 / 账号管理 / 审计日志        │
                      │   └── JSON API (list/move/delete/...)      │
                      │                                            │
   匿名访客 ──HTTP──▶  │  公开分享端 (share listener :8081)         │
                      │   ├── /s/<token>   公开分享入口             │
                      │   └── /p/<token>   沙箱化预览               │
                      │                                            │
                      │  元数据: SQLite (filehub.db)                │
                      │  文件  : 本地磁盘 (data/<user-id>/...)      │
                      └────────────────────────────────────────────┘
```

- **管理端**与**公开分享端**是同一进程内的两个 HTTP listener，分别监听不同端口/域名。
- 元数据全部存 SQLite，开启 WAL 模式以支持并发读。
- 用户文件按账号 ID 隔离存放，对外只暴露逻辑路径，物理路径不可穿越。
- 前端资源通过 `//go:embed web/*` 嵌入二进制，部署时无需携带 `web/` 目录。

---

## 快速开始

需要 Go 1.26 或更高版本：

```sh
git clone https://github.com/abcwyc/PageRelay.git
cd PageRelay
go run . \
  -listen 127.0.0.1:8080 \
  -share-listen 127.0.0.1:8081 \
  -data ./data \
  -db ./filehub.db
```

首次启动时，如果没有指定管理员密码，会自动创建默认管理员 `admin`，密码为 `admin`，并在日志中提示你必须在首次登录后修改：

```
BOOTSTRAP ADMIN: default credentials admin/admin — you must set a new password on first login
```

更安全的做法是用 `-admin-password-file` 指定初始密码文件，管理员写入数据库后可删除该明文文件：

```sh
echo -n 'my-strong-password' > /tmp/admin.secret
go run . -admin-password-file /tmp/admin.secret
# 登录并改密成功后，可删除 /tmp/admin.secret
rm /tmp/admin.secret
```

打开 `http://127.0.0.1:8080` 即可登录；公开分享端默认在 `http://127.0.0.1:8081`。

---

## 部署指南

### 1. 构建二进制

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags='-s -w' -o dist/filehub-linux-amd64 .
```

`CGO_ENABLED=0` 配合纯 Go 的 `modernc.org/sqlite` 驱动，可以静态编译，目标机器无需 libc 依赖。

### 2. 上传到服务器

```sh
scp dist/filehub-linux-amd64 user@server:/usr/local/bin/filehub
ssh user@server 'chmod +x /usr/local/bin/filehub'
```

### 3. 准备目录

```sh
sudo mkdir -p /var/lib/filehub /srv/filehub/users
sudo chown -R filehub:filehub /var/lib/filehub /srv/filehub
```

### 4. 配置 systemd

创建 `/etc/systemd/system/filehub.service`：

```ini
[Unit]
Description=PageRelay file management and sharing service
After=network.target

[Service]
Type=simple
User=filehub
Group=filehub
ExecStart=/usr/local/bin/filehub \
  -listen 0.0.0.0:8080 \
  -share-listen 0.0.0.0:8081 \
  -public-base http://share.example.com \
  -db /var/lib/filehub/filehub.db \
  -data /srv/filehub/users
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
```

启动并设置开机自启：

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now filehub
sudo systemctl status filehub
```

### 5. 配置反向代理与 TLS（推荐）

生产环境建议用 Nginx/Caddy 反向代理，把管理端与公开分享端分别绑定到不同域名，并启用 TLS：

```nginx
# 管理端
server {
  listen 443 ssl http2;
  server_name relay.example.com;
  ssl_certificate     /etc/ssl/relay.example.com.crt;
  ssl_certificate_key /etc/ssl/relay.example.com.key;

  client_max_body_size 2g;          # 与 maxUploadBytes 对齐
  location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto https;
  }
}

# 公开分享端
server {
  listen 443 ssl http2;
  server_name share.example.com;
  ssl_certificate     /etc/ssl/share.example.com.crt;
  ssl_certificate_key /etc/ssl/share.example.com.key;

  location / {
    proxy_pass http://127.0.0.1:8081;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
  }
}
```

启用 TLS 后，记得在 `filehub.service` 的启动参数里加上 `-public-base https://share.example.com` 与 `-cookie-secure`，让公开链接使用 https、会话 cookie 带 Secure 标记。

### 当前参考部署（内网 HTTP）

- 管理端：`http://10.192.66.28:8080`
- 公开分享端：`http://10.192.66.28:8081`
- systemd 服务：`filehub.service`
- 程序：`/usr/local/bin/filehub`
- 数据库：`/var/lib/filehub/filehub.db`
- 用户文件：`/srv/filehub/users/<user-id>`

> 当前部署使用公司内网 HTTP。正式推广前建议申请内网域名和 TLS 证书，然后开启 `-cookie-secure`；否则登录密码和会话在传输层没有加密。

---

## 使用指南

### 登录与改密

1. 浏览器打开管理端地址（如 `https://relay.example.com`）。
2. 输入用户名和密码登录。
3. 首次登录或被重置密码后，会进入“修改密码”页面，设置新密码后才能继续使用。

### 文件管理

- **上传**：把文件拖到工作台主区域的“拖拽上传”浮层，或拖到左侧目录树某个文件夹上，即可上传到该目录。
- **浏览**：左侧目录树懒加载，点击展开；右侧表格显示当前目录内容，支持按名称、大小、修改时间排序和搜索。
- **预览**：单击文件即可在右侧预览图片、PDF、视频、音频、文本、代码、Markdown、HTML、SVG。
- **新建/编辑**：支持新建并编辑 Markdown、HTML 和纯文本文件，保存即生效。
- **多选与批量**：用复选框或 `Shift` 多选文件，可批量移动、批量删除，或打包成 ZIP 下载。
- **移动**：把文件（或多选项）直接拖到目标文件夹、目录树节点或根目录即可移动。
- **命令面板**：按 `⌘/Ctrl + K` 打开文件搜索与命令面板，键盘快速跳转。

### 分享

- 在文件详情面板里，每个文件/文件夹都有一个“分享开关”。
- 开启后立刻生成一条公开链接，复制即可发给外部。
- 关闭后旧链接立刻失效（访客会看到 404/已下线）；再次开启会生成一条全新的链接。
- 新上传或新建的文件默认开启分享，方便“传完即发”。
- 在左侧导航的“分享记录”页面可以查看、管理自己所有分享。

### 账号管理（管理员）

- 在“系统管理 → 账号管理”可以创建、禁用账号，重置密码，分配存储配额。
- 每个账号有独立的数据目录，逻辑路径以账号为根（如 `/demo/产品资料/index.html`）。
- 在“系统管理 → 审计日志”可以查看登录、登出、登录失败、密码变更等操作记录。

---

## 配置参数

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-listen` | `127.0.0.1:8080` | 管理端监听地址 |
| `-share-listen` | `127.0.0.1:8081` | 公开分享端监听地址 |
| `-public-base` | 空 | 公开分享端对外基址，如 `https://share.example.com`，用于拼接分享链接 |
| `-db` | `./filehub.db` | SQLite 数据库路径 |
| `-data` | `./data` | 用户文件存储根目录 |
| `-admin-user` | `admin` | 引导管理员用户名 |
| `-admin-password` | 空 | 引导管理员密码（明文，不推荐） |
| `-admin-password-file` | 空 | 从文件读取引导管理员密码，写入数据库后可删除该文件 |
| `-cookie-secure` | `false` | 启用 Secure 会话 cookie，建议在 HTTPS 部署时打开 |

---

## 安全模型

- **账号隔离**：用户文件按账号 ID 物理隔离，对外只暴露逻辑路径，服务端做路径穿越校验。
- **密码哈希**：使用 Argon2id 存储密码哈希，不明文落库。
- **CSRF 防护**：所有写操作校验 CSRF 令牌。
- **沙箱预览**：HTML、SVG 在独立分享域运行，与管理端 cookie 域隔离；Markdown 使用安全模式渲染，剥离危险节点。
- **分享可回收**：分享开关关闭即失效，再次开启生成新链接。
- **操作审计**：关键管理操作写入审计日志，可追溯。
- **配额限制**：每个账号有存储配额，覆盖上传会被拒绝。
- **传输加密**：建议配合反向代理启用 TLS 并打开 `-cookie-secure`，避免密码与会话明文传输。

---

## 测试与构建

```sh
# 前端语法检查（无构建步骤）
node --check web/app.js

# Go 测试与静态检查
go test -race ./...
go vet ./...

# 静态二进制构建
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags='-s -w' -o dist/filehub-linux-amd64 .
```

自动化测试覆盖登录、CSRF、账号隔离、覆盖上传配额、默认分享、分享关闭和重新生成、安全 HTML/Markdown 预览、历史链接迁移及 Logo 静态资源。

---

## 历史兼容

升级 PageRelay 2.0 时，历史链接按以下规则迁移：

- 升级前有效的限时链接会转为永久有效；
- 已经撤销的链接保持无效；
- 没有分享记录的历史文件不会被自动公开。

这样既保留了对历史外发链接的承诺，又把新的分享模型统一为“开关式”。

---

## License

本项目基于 [MIT License](./LICENSE) 开源，源码完整公开，欢迎自由学习、审阅、使用和修改。鼓励部署者根据自身团队的需求进行二次开发和定制，也欢迎以 Issue / Pull Request 的方式反馈改进。
