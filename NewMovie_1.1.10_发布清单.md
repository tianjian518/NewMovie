# NewMovie v1.1.10 — 扫描断连健壮化 + 首页行式海报墙

- 日期：2026-08-02
- 基线：v1.1.9（`a60e86e`）
- 主题：修复 OpenList 扫描媒体库时偶发断连报错，并把首页改成爆米花/飞牛影视风格的行式海报墙。

## 一、问题 A：扫描媒体库偶发断连，报 `invalid character '<'`

扫描大目录时，OpenList 可能因网络抖动、反向代理（Nginx/Caddy）502/504、或 TLS 中途断开，间歇返回非 JSON 内容。
旧实现把响应体直接喂给 `json.Unmarshal`，于是用户看到一串看不懂的：

```
失败：{"error":"连接失败: invalid character '\u003c' looking for beginning of value"}
```

根因有两层：

- **未区分瞬时错误与持久错误**：连接重置、TLS 中断、代理抽风这类偶发抖动，本应重试自愈，
  旧代码却一抖就让整个媒体库扫描失败。
- **底层报错直接透传**：HTML 错误页 / 空响应被当成 JSON 解析，把 `invalid character '<'`
  这种内部解析报错原样丢给用户；而且 `handlers.go` 还硬拼了「连接失败: 」前缀，掩盖了真实原因。

## 二、改动 A：客户端健壮性

`internal/openlist/client.go`：

- `do()` 改为带重试的循环：`openlistMaxRetries = 3`、`openlistRetryBackoff = 400ms`
  （累计约 0.8~1.2s，既足够偶发抖动自愈，又不会在 OpenList 真挂掉时无限拖延）。
- 抽出 `tryOnce()`：单次请求 + 读响应，返回**可读错误**。
  - 网络层错误（connection reset / TLS 中断）→ 瞬时，提示「OpenList 连接失败…请检查地址是否可达、网络是否稳定」；
  - `resp.StatusCode >= 500` → 瞬时，提示「OpenList 返回 5xx，服务可能临时不可用或被反向代理拦截」；
  - 读响应失败 → 瞬时；
  - 响应体 `trimmed == ""` → 瞬时，提示「返回了空响应，可能是连接中途断开」；
  - 响应体以 `'<'` 开头（HTML 错误页 / 代理报错页）→ 瞬时，提示「返回的不是 JSON…请检查 OpenList 是否在线」。
- 新增 `olErr{msg, transient}` 与 `isTransient(err)`：持久错误（路径错、鉴权失败、业务码非 200）**不重试**，立即返回。

`internal/scanner/scanner.go`：

- `FriendlyErr` 新增匹配「连接失败」「返回的不是 json」「返回了空响应」「返回 5」，翻译成人话并提示
  「连不上 OpenList…请检查地址是否可达、容器网络、反向代理是否拦截」。

`internal/api/handlers.go`：

- `testStorage` 去掉误导性的「连接失败: 」前缀拼接，直接返回 `err.Error()`。

## 三、验证 A

`internal/openlist/client_test.go`（新增，3 条全 PASS）：

- `TestList_HTMLResponseIsFriendlyAndRetried`：HTML 错页 → 友好报错 + 重试 3 次；
- `TestList_RetryOnTransient5xx`：首次 502 后恢复 → 成功返回；
- `TestList_PersistentErrorNotRetried`：业务 code=500 → 不重试，立即返回。

`go test ./...` 全绿。

## 四、问题 B：首页布局不合理

原首页：顶部是「最近添加」大网格，下面每个媒体库只是一个**文字方块**，要点进去才看得到海报墙。
用户希望参考**网易爆米花 / 飞牛影视**：首页就是一排排**横滑的海报行**，每个媒体库在首页直接展示自己的海报墙。

## 五、改动 B：首页行式海报墙

`web/src/components/PosterRow.tsx`（新增）：

- 通用横滑海报行组件；空时显示 `empty` 文案；每项用 `<Link>` 进详情页。

`web/src/App.tsx`（`Dashboard` 重写）：

- 顶部「继续观看」（带进度条，单行横滑）；
- 「最近添加」单行横滑（`<PosterRow>`）；
- **每个媒体库一个 `<section>`，直接在首页铺自己的 `<PosterRow>`**：库名可点进详情，「查看全部 →」；
- 并发拉取各库最近条目 `api.search({ library: l.id, sort: "recent", limit: 12 })`，归集到 `libItems` 字典。
- 删除旧的「媒体库文字方块」网格。

## 六、验证 B

- `web` 端 `tsc -b --noEmit` + `vite build` 通过。
- Playwright 端到端：起 `:8100` 干净后端 + 假 OpenList `:5599`，建「电影库」「剧集库」两库扫描灌入条目，
  登录后断言首页出现「最近添加」单行横滑 + 两个媒体库各自的海报行 section（DOM 断言通过），
  布局与爆米花/飞牛影视一致。

## 七、版本

`internal/api/handlers.go` 版本号 `1.1.9` → `1.1.10`。
