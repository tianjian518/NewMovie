# NewMovie v1.1.8 — 测试报告对照维护

- 日期：2026-08-01
- 基线：v1.1.7（`277ebeb`）
- 主题：对照外部《NewMovie 项目 Bug 测试报告》（测试对象 v1.1.6，标注"手工代码审查 + 逻辑推演"）逐条核验并维护。

## 一、报告总体结论

报告引用的 6 个源文件路径（如 `internal/provider/openlist/client.go`、`internal/handler/playback.go`、`internal/handler/favorite.go` 等）**在本项目均不存在**，项目实际布局为 `internal/openlist`、`internal/api`、`internal/scanner`、`internal/store` 等。报告属"逻辑推演"，多数结论与本项目实现不符。

8 个 Bug 中 **7 个不成立（已有防护）**，**1 个真实（Bug-04）已修复**。

## 二、逐条对照

| 编号 | 报告声称 | 对照结论 | 处理 |
|------|----------|----------|------|
| Bug-01 | 用 `/webdav/` 调 OpenList，应改 `/dav/` | 本项目**从未用 WebDAV**，走 REST `/api/fs/list`、`/api/fs/get` | 不成立，加 `TestReport_Bug01_UsesRestApiNotWebDAV` 锁死 |
| Bug-02 | STRM 缺 token，需补签名 | 本项目是**消费** strm 而非生成，`GetLink`/`SignedDURL` 播放时带鉴权（`TestResolveSignedDURL` 已覆盖） | 不成立 |
| Bug-03 | 并发续播记录互相覆盖 | `SavePlayRecord` 持 `d.mu` 且以 `(UserID, FileID)` 复合键，不同用户各成行 | 不成立，加 `TestReport_Bug03_ConcurrentPlayRecordsDoNotOverwrite` 锁死 |
| Bug-04 | 海报加载失败无降级，显示破图 | **真实**：`PosterCard` 仅空 URL 才占位，URL 存在但加载失败会破图 | **已修复**：`<img onError>` 回退首字占位 |
| Bug-05 | 搜索不支持模糊匹配 | `searchItems` 已用 `strings.Contains` 子串匹配标题+简介 | 不成立，加 `TestReport_Bug05_SearchSupportsPartialMatch` 锁死 |
| Bug-06 | 重复收藏插入多条 | `SaveFavorite` 插入前已按 `(UserID, ItemID, Kind)` 查重 | 不成立，加 `TestReport_Bug06_FavoriteIsIdempotent` 锁死 |
| Bug-07 | 全屏黑屏约 0.5s | 无具体代码位置；播放器容器已为 `bg-black`（规避白闪），属浏览器全屏重绘/解码固有现象 | 非代码缺陷，低优先级，留待真机复现 |
| Bug-08 | 分页参数负值/0 未校验 | 仅 `limit` 参数且已守卫 `n>0`，无 page/pageSize | 不成立，加 `TestReport_Bug08_LimitParamBoundaries` 锁死 |

## 三、代码变更

- `web/src/components/PosterCard.tsx`：`<img>` 增加 `onError`，加载失败回退到标题首字占位；`poster_url` 变化时重置失败态避免复用残留。
- `internal/api/handlers_report_test.go`：新增 5 条可执行断言（Bug-01/03/05/06/08），把"伪 Bug"的正确行为钉死，防止日后误改。
- `internal/api/handlers.go`：版本号 `1.1.7` → `1.1.8`。

## 四、验证

- Go 全量测试：`go test ./...` 全绿（含 5 条报告验证测试）。
- 前端：`npx tsc -b --noEmit` 与 `vite build` 通过。
- Bug-04 端到端：用独立 React 沙盒 + Playwright（系统 Chromium）渲染 `PosterCard`，
  传入必定 404 的 `poster_url`，验证卡片**不再渲染 `<img>`** 且回退到首字占位（破图卡片 `hasImg:false`，文本含"盗"）。

## 五、未做项

- Bug-07：非代码缺陷，未改动；如需进一步排查全屏观感，建议在真实设备复现并采集时间线。
