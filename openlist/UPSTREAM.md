# 内置后端：139cas

本目录是 NewMovie 2.0 的**内置存储后端**，源自：

- 上游仓库：<https://github.com/tianjian518/139cas>
- 分支：`main`
- 引入时间：2026-08-02
- Go module：`github.com/OpenListTeam/OpenList/v4`（独立于主模块 `newmovie`，两者互不干扰）

## 它是什么

139cas 是 OpenList v4 的魔改版，相对上游 OpenList 主要增加：

| 增强 | 位置 |
| --- | --- |
| `.cas` 秒传元数据（记录 md5/sliceMd5/sha1/preID，供跨盘秒传与真实文件预览） | `internal/casmeta/` |
| `.cas` 文件按视频/音频类型直出（`cas_video`） | `server/handles/down.go` |
| 广雅盘 driver | `drivers/guangyapan/` |
| 139/189/115 的 `.cas` 还原与恢复逻辑 | `drivers/139/`、`drivers/189pc/`、`drivers/115/` |
| 端口自适应（`SERVER_PORT`/`PORT` → `OPENLIST_HTTP_PORT`）、单端口模式 | `entrypoint.sh` |

## 与主项目的关系

NewMovie 是**播放器与媒体库外壳**，139cas 是**网盘挂载与直链后端**。2.0 把两者打进同一个容器：

```
容器
├─ openlist (139cas)   监听 127.0.0.1:5244   ← 不对外暴露
└─ newmovie            监听 0.0.0.0:8096     ← 用户唯一入口
   ├─ 启动时自动登录内置 OpenList 并注册为默认存储
   └─ /openlist/* 反代到内置 OpenList 管理界面
```

## 同步上游

```bash
# 拉取上游最新代码覆盖本目录（保留 UPSTREAM.md）
scripts/sync-openlist.sh
```

## 注意

- 本目录**不做源码级修改**。所有整合逻辑都写在 NewMovie 侧（`internal/api/bootstrap.go`、`internal/openlistproxy/`），
  这样上游更新时可以整目录覆盖，零冲突。
- `public/dist/`（前端产物）不入库，由 `build.sh` 在构建时下载。
