# NewMovie v2.1.0 — 媒体库自动定时扫描（网盘新增剧集自动入库）

- 日期：2026-09-05
- 基线：v2.0
- 主题：补齐「媒体库监控」能力——网盘里新增剧集/电影后，不再需要手动点「扫描」，
  配置一个间隔即可自动增量入库。解决 OpenList strm 驱动「仅访问目录时生成、
  不支持定时/监听自动生成 → 新增剧集媒体库不更新」的根因性痛点。

## 一、能力说明

新增环境变量 **`VIDRIVE_SCAN_INTERVAL`**（Go duration，如 `30m` / `1h` / `6h`）：

- 未设置 / `0`：关闭，行为与 1.x / 2.0 完全一致（仅手动或 API 触发扫描）；
- 设置后：后台 goroutine 按该间隔遍历所有媒体库，逐个触发**增量扫描**。

## 二、设计要点

1. **增量而非全量**：复用 `scanner.Scan` 的 path + size + mtime 三元组 diff，
   未变更的目录整棵跳过，扫描开销与风控压力都小（默认 2 req/s + `refresh=false`
   复用 OpenList 缓存）。
2. **防抖 / 幂等**：与 HTTP 手动扫描共用 `tryLockScan` 单库并发锁——同一媒体库
   同一时刻只有一个扫描协程；上一轮没扫完的库，下个周期自动补齐，不会互相踩踏，
   也不会出现「连点两下起两个协程」的旧问题。
3. **不阻断启动**：定时器在后台 goroutine 里跑，HTTP 端口照常秒起；
   扫描失败只记日志，不拉垮服务（沿用全链路 panic 恢复）。
4. **配置安全**：`VIDRIVE_SCAN_INTERVAL` 解析失败（如拼错单位）时保持关闭并打警告，
   宁可不开也不因配置笔误把服务拖进疯狂扫描。

## 三、改动清单

| 文件 | 改动 |
| --- | --- |
| `internal/config/config.go` | 新增 `ScanInterval` 字段 + `VIDRIVE_SCAN_INTERVAL` 解析 |
| `internal/api/handlers.go` | 扫描核心逻辑抽为 `scanLibrary(libID)`（HTTP 与定时任务共用）；新增 `StartScheduledScanner()`；`startScan` 改为薄包装（错误语义不变：409 忙 / 400 预检失败） |
| `cmd/server/main.go` | 启动时调用 `StartScheduledScanner()` |
| `internal/config/config_test.go` | 新增 `TestLoad_ScanIntervalParsing`（合法值 / 未设置 / 非法值 / 负值） |
| `README.md` | 核心能力 + 配置表新增 `VIDRIVE_SCAN_INTERVAL` |
| `docker-compose.yml` | 环境变量示例注释 |

## 四、使用方式

```bash
# docker-compose.yml 的 environment 里加一行即可
- VIDRIVE_SCAN_INTERVAL=30m
```

重启容器后日志会出现 `[scan] 自动定时扫描已启用，间隔 30m`；
每个周期触发时打印 `[scan] 定时扫描已触发 "xxx"（新增内容自动入库）`。

## 五、验证

- `go build ./...` 通过；
- `go test ./...` 全量通过（14 个包）；
- 新增单测覆盖配置解析的四个分支；
- 手动/API 触发扫描（`POST /api/libraries/:id/scan`）行为不变（409/400 语义保留）。
