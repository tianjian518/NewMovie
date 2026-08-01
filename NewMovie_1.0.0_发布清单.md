# NewMovie 1.0.0 发布清单

自托管云盘媒体库 **NewMovie** 1.0 版本已构建、打标签并推送至 GitHub 与 Docker Hub。

## 交付地址

| 平台 | 地址 | 状态 |
|------|------|------|
| GitHub 仓库 | https://github.com/tianjian518/NewMovie | ✅ 已推送 `main` + `v1.0.0` |
| GitHub Release | https://github.com/tianjian518/NewMovie/releases/tag/v1.0.0 | ✅ 已发布 |
| Docker Hub 仓库 | https://hub.docker.com/r/tianjian518/newmovie | ✅ 标签已上线 |
| Docker 镜像 `1.0.0` | `tianjian518/newmovie:1.0.0`（18.6MB, alpine） | ✅ 已推送并冒烟验证 |
| Docker 镜像 `latest` | `tianjian518/newmovie:latest` | ✅ 已推送 |

## 镜像内容验证

- 本地拉起容器，`GET /api/health` 返回：
  ```json
  {"name":"NewMovie","ok":true,"version":"1.0.0"}
  ```
- digest：`sha256:b4611eb20d4726de5349c2e22b425b70b7fd98cb0e71c08bd4608e9cda16ec43`（两标签同 digest）

## 1.0 核心能力

- **云盘直连**：通过 OpenList / 139cas 挂载云盘，不落本地。
- **海报墙刮削**：NFO 优先、TMDB 兜底，自动填海报/背景/简介/评分/tmdb_id。
- **本地图代理**：同目录 `poster.jpg`/`fanart.jpg` 与 NFO 远程图统一走服务端代理，规避直链过期。
- **增量缓存**：已刮削条目跳过，重扫不再反复打 TMDB / 读 NFO。
- **用户自填 TMDB Key**：设置页填写，环境变量 `TMDB_API_KEY` 优先。
- **播放进度 / 收藏**：内置 ArtPlayer，断点续播。

## 快速开始

```bash
docker run -d -p 8096:8096 -v $(pwd)/data:/data tianjian518/newmovie:1.0.0
```

## 说明

- 凭据（GitHub PAT / Docker Hub Token）仅用于登录与推送，**未写入任何仓库文件**，仅存在于本机 `docker login` 配置中。
- 沙箱无外网出口，后端纯 Go 标准库实现；Docker 构建与推送在你已确认的网络环境中执行。

## 后续可选项（未启动，待你确认）

- `.vidrive.json` 手动锁定 TMDB-ID
- 剧集 `tvshow.nfo` 递归父目录查找
- 服务端图片缓存层
- `.cas` 资源（Phase 4）
