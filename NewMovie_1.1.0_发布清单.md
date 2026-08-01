# NewMovie 1.1.0 发布清单

自托管云盘媒体库 **NewMovie** 1.1.0 已构建、打标签并推送至 GitHub 与 Docker Hub（多架构）。

## 交付地址

| 平台 | 地址 | 状态 |
|------|------|------|
| GitHub 仓库 | https://github.com/tianjian518/NewMovie | ✅ `main` + `v1.1.0` |
| GitHub Release | https://github.com/tianjian518/NewMovie/releases/tag/v1.1.0 | ✅ 已发布 |
| Docker Hub | https://hub.docker.com/r/tianjian518/newmovie | ✅ `1.1.0` / `latest` 均多架构 |
| 镜像 `1.1.0`（多架构） | `tianjian518/newmovie:1.1.0` | ✅ 已推送并冒烟验证 |
| 镜像 `latest`（多架构） | `tianjian518/newmovie:latest` | ✅ 已推送 |

## 多架构验证（注册表直查）

`1.1.0` 与 `latest` 的 OCI manifest index 均包含：

- `linux/amd64`
- `linux/arm64`
- `linux/arm/v7`（树莓派等 32 位 ARM）

本地（amd64）拉取运行：`GET /api/health` → `{"name":"NewMovie","ok":true,"version":"1.1.0"}`。

## 1.1.0 新增能力

| 能力 | 说明 |
|------|------|
| **`.vidrive.json` 手动锁定** | 同目录或父目录放 `{"tmdb_id":27205,"type":"movie","title":"...","year":2010}`，手动锁定的 TMDB-ID/标题/类型优先级最高（高于 NFO 与 TMDB）。 |
| **剧集 `tvshow.nfo` 递归父目录** | 系列级 `tvshow.nfo` / `poster.jpg` / `fanart.jpg` 在剧集根目录时，沿父目录向上查找（优先级高于单集 nfo）。 |
| **服务端图片缓存层** | 本地图与 NFO 远程图统一走服务端代理，落磁盘缓存（`data/cache/images`），命中直出、支持 Range，规避直链过期、减少回源。 |
| **`.cas` 资源支持** | 139cas（OpenList rebrand）的 `.cas` 指针文件按 strm 同类处理并归一化解析。 |
| **多架构镜像** | Dockerfile 用 `TARGETPLATFORM`/`TARGETARCH`/`GOARM` 真正交叉编译；前端阶段固定 `BUILDPLATFORM`（产物与架构无关），不再依赖 QEMU 模拟。CI 经 `buildx + QEMU` 产出 amd64/arm64/arm/v7。 |

## 测试覆盖

- `scraper`：`ManualMeta` 锁定覆盖 NFO/TMDB、强制类型（movie↔tv）。
- `scanner`：`.vidrive.json` 锁定、`tvshow.nfo` 递归父目录、`.cas` 解析、NFO/本地图、增量缓存。
- `api`：图片代理 + 缓存层（回源→落盘→关源后仍可命中）、Range 分段。
- `go build` / `go vet` / `go test ./...` 全绿。

## 快速开始

```bash
docker run -d -p 8096:8096 -v $(pwd)/data:/data tianjian518/newmovie:1.1.0
```

ARM 设备（如树莓派 4/5）直接拉同一镜像标签即可，Docker 会自动按架构选取。

## 说明

- 凭据（GitHub PAT / Docker Hub Token）仅用于登录与推送，未写入任何仓库文件；Docker Hub 登录信息存于本机 `docker login` 配置。
- 早期误推的 `v1.1.0`（带 v 前缀）Docker 标签为过渡产物；规范标签为 `1.1.0`（无 v，与 `1.0.0` 约定一致）。
- 多架构构建在沙箱内无法用 QEMU 模拟（受限于 `binfmt_misc` 挂载权限），故由 GitHub Actions（完整特权 + QEMU）完成，构建与推送均已验证成功。
