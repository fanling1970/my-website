# LAN-CMS · 局域网轻量内容管理系统

在路由器上跑的「网站建设模式」系统：左侧菜单 + 右侧富文本编辑器，写完保存、一键预览。
前台展示与后台编辑一体，单二进制、JSON 文件存储、无数据库。

## 功能

- 管理后台 `/admin`：左侧页面列表（新建/删除），右侧标题 + 富文本编辑器
- 编辑器工具栏：插入图片（上传 / 粘贴 / 网络图片）、超链接、标题、列表、表格、代码块等
- 图片上传自动保存到 `uploads/`，接口 `/api/upload`（仅限 jpg/png/gif/webp，最大 20MB）
- 保存后点「预览」新标签打开前台页面；前台 `/` 列表 + `/?p=id` 详情
- 前台无任何外部依赖，断网也能看；管理后台编辑器使用 CDN（需要外网）

## 技术栈

Go（标准库，零第三方依赖）· JSON 文件存储 · wangEditor 5 富文本 · Docker 多阶段构建

## 一、推送 GitHub 自动构建镜像

1. 在 GitHub 新建仓库（如 `lan-cms`），把本目录代码推上去（`main` 分支）：

```bash
git init
git add .
git commit -m "init lan-cms"
git branch -M main
git remote add origin https://github.com/<你的用户名>/<仓库名>.git
git push -u origin main
```

2. 推送后 Actions 自动执行 `.github/workflows/build.yml`，构建 `linux/amd64 + linux/arm64` 双架构镜像并推送到 GHCR：
   `ghcr.io/<你的用户名>/<仓库名>:latest`

3. 首次构建需把包设为公开（否则路由器拉取要登录）：
   GitHub → 你的头像 → Settings → Packages → 选中 lan-cms → **Change visibility → Public**；
   或仓库 Settings → Packages 里设置。

## 二、路由器部署

```bash
mkdir -p /opt/lan-cms && cd /opt/lan-cms
```

把 `docker-compose.yml` 拷入，修改 `image:` 行为 `ghcr.io/<你的用户名>/<仓库名>:latest`，然后：

```bash
docker compose up -d
```

- 前台：`http://路由器IP:8090`
- 管理后台：`http://路由器IP:8090/admin`

数据都在 `./lan-cms-data/`（`pages.json` + `uploads/`），备份 = 复制这个目录。

## 三、本地开发调试（可选）

```bash
go run .            # 默认 :8090，数据存 /data
# Windows 本地改数据目录：
$env:DATA_DIR=".\data"; go run .
```

## 注意事项

- 目前未做登录鉴权：局域网内谁能访问 8090 谁就能改内容。家庭内网够用；
  如需保护，后续可加简单密码（或依赖路由器防火墙限制来源 IP）。
- 管理后台的编辑器 JS 从 jsDelivr CDN 加载，编辑时需要外网；前台展示不依赖外网。
- 图片仅允许 jpg/png/gif/webp（拒绝 svg 防 XSS）。
