# LAN-CMS · 局域网轻量内容管理系统

在路由器上跑的「网站建设模式」系统：左侧菜单 + 右侧富文本编辑器，写完保存、一键预览。
前台展示与后台编辑一体，单二进制、JSON 文件存储、无数据库。

## 功能

- **树形内容**：文件夹 / 页面 / 文件 三级结构；左侧可新建页面、新建文件夹、上传附件
- **管理后台 `/admin`**：左侧树形菜单（展开/折叠/删除），右侧富文本编辑器
- **编辑器工具栏**：
  - 插入链接：支持手动填地址，或从左侧下拉直接选站内页面自动建链（可新标签打开）
  - 插入图片：上传 / 粘贴 / 网络图片；**裁剪图片**（拖选区裁剪，原图替换）
  - **边框**：选中图片或表格可设置边框粗细（无 / 1px / 2px / 4px / 自定义）
  - 标题、加粗、颜色、列表、引用、代码块、表格等
- **附件与软件**：任何文件夹下可上传附件（最大 200MB），自动生成下载链接，前台可直接下载
- 前台 `/`：树形浏览（面包屑导航），`/?f=文件夹ID` 进入文件夹，`/?p=页面ID` 查看页面
- 图片自动存入 `uploads/`，附件存入 `files/`；旧版 v1 数据自动迁移，不丢失

## 技术栈

Go（标准库，零第三方依赖）· JSON 文件存储 · wangEditor 5 富文本 · Cropper.js 图片裁剪 · Docker 多阶段构建

## 一、推送 GitHub 自动构建镜像

1. 已有仓库则直接推送；新仓库先创建再推送（`main` 分支）：

```bash
git init && git add . && git commit -m "init lan-cms"
git branch -M main
git remote add origin https://github.com/<你的用户名>/<仓库名>.git
git push -u origin main
```

2. 推送后 Actions 自动执行 `.github/workflows/build.yml`，构建 `linux/amd64 + linux/arm64` 双架构镜像并推送到 GHCR：
   `ghcr.io/<你的用户名>/<仓库名>:latest`

3. 首次构建后把包设为公开（否则路由器拉取要登录）：
   GitHub → 你的头像 → Settings → Packages → 选中包 → **Change visibility → Public**。

## 二、路由器部署

```bash
mkdir -p /opt/lan-cms && cd /opt/lan-cms
```

把 `docker-compose.yml` 拷入，修改 `image:` 行为 `ghcr.io/<你的用户名>/<仓库名>:latest`，然后：

```bash
docker compose pull && docker compose up -d
```

- 前台：`http://路由器IP:8090`
- 管理后台：`http://路由器IP:8090/admin`

数据都在 `./lan-cms-data/`（`nodes.json` + `uploads/` + `files/`），备份 = 复制这个目录。
升级版本：`docker compose pull && docker compose up -d`，数据自动保留。

## 三、本地开发调试（可选）

```bash
go run .            # 默认 :8090，数据存 /data
# Windows 本地改数据目录：
$env:DATA_DIR=".\data"; go run .
```

## 注意事项

- 目前未做登录鉴权：局域网内谁能访问 8090 谁就能改内容。家庭内网够用；
  如需保护，可依赖路由器防火墙限制来源 IP。
- 管理后台的编辑器 JS 从 jsDelivr CDN 加载，编辑时需要外网；前台展示不依赖外网。
- 图片仅允许 jpg/png/gif/webp（拒绝 svg 防 XSS）；附件不限类型，单文件上限 200MB。
- 删除文件夹会级联删除其下所有子内容；已上传的物理文件不会被删除（避免误删导致图片失效）。
