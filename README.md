# PageRelay

PageRelay 是一个面向公司内网的轻量文件管理与分享服务。单个 Go 二进制同时提供管理端和公开分享端，元数据保存在 SQLite，用户文件保存在本地磁盘。

## PageRelay 2.0

- 哈啰租车品牌 Logo 与紧凑、克制的双栏文件工作台
- 可拖动侧栏分隔线，浏览器会保存设备本地的宽度偏好
- 左侧固定拖拽上传入口和懒加载文件目录树
- 右侧目录表格、排序、搜索、文件详情与快捷预览
- 文件多选、批量移动、批量删除和 ZIP 下载
- 将文件或多选项目拖到列表文件夹、左侧目录树或根目录即可移动
- `⌘/Ctrl + K` 文件搜索与命令面板
- HTML、SVG 在独立分享域的沙箱中运行；Markdown 使用安全模式渲染
- 图片、PDF、视频、音频、文本和常见代码文件在线预览
- 新建并编辑 Markdown、HTML 和纯文本文件
- 分享只有开启和关闭两种状态，不再限制有效期
- 新上传和新建的文件默认开启分享；关闭后旧链接立即失效，再开启会生成新链接
- 文件逻辑路径以登录账号为根，例如 `/admin/产品资料/index.html`
- 管理员创建、禁用账号，重置密码并分配存储配额
- 账号数据目录隔离、路径穿越防护、CSRF 防护、Argon2id 密码哈希和操作审计

历史兼容规则：升级前有效的限时链接会转为永久有效；已经撤销的链接保持无效；没有分享记录的历史文件不会被自动公开。

## 本地开发

需要 Go 1.26 或更高版本：

```sh
go run . \
  -listen 127.0.0.1:8080 \
  -share-listen 127.0.0.1:8081 \
  -data ./data \
  -db ./filehub.db
```

首次启动可使用 `-admin-password-file /path/to/secret` 指定管理员初始密码。管理员写入数据库后可删除该明文文件；后续启动不会再读取它。

## 测试与构建

```sh
node --check web/app.js
go test -race ./...
go vet ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags='-s -w' -o dist/filehub-linux-amd64 .
```

自动化测试覆盖登录、CSRF、账号隔离、覆盖上传配额、默认分享、分享关闭和重新生成、安全 HTML/Markdown 预览、历史链接迁移及 Logo 静态资源。

## 当前服务器部署

- 管理端：`http://10.192.66.28:8080`
- 公开分享端：`http://10.192.66.28:8081`
- systemd 服务：`filehub.service`
- 程序：`/usr/local/bin/filehub`
- 数据库：`/var/lib/filehub/filehub.db`
- 用户文件：`/srv/filehub/users/<user-id>`

当前部署使用公司内网 HTTP。正式推广前建议申请内网域名和 TLS 证书，然后开启 `-cookie-secure`；否则登录密码和会话在传输层没有加密。
