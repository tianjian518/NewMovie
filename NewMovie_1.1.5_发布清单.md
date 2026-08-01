# NewMovie 1.1.5 发布清单 — 配置/导入流程修复（存储源编辑去重、媒体库创建入口、扫描可用）

> 修复类型：功能性 Bug + 缺失功能。所有需要「绑定 OpenList 并导入网盘」的用户都需要升级。

---

## 一、问题现象（来自真实 OpenList 用户反馈）

1. **OpenList 连接**：填好信息点「测试连接」报错 `cannot unmarshal string into ... modified of type int64`。真实 139cas OpenList 把 `modified` 返回成**纳秒 RFC3339 串**（`2026-07-29T14:12:07.778495506Z`），`type` 返回成**数字**（`1`），而代码按 `int64`/`string` 硬解析。
2. **保存重复**：点一次「保存」下面出一条 OpenList 记录，点无数次出无数条 —— 表单只会「新建」，不会「编辑已有」，也没删除按钮。默认值 `name:"main"`、`rate_limit:2` 也造成困惑。
3. **无法导入**：OpenList 绑定好了，却**看不到任何按钮**能把网盘内容导进媒体库 —— 前端根本没有「创建媒体库」的入口，且扫描接口路由本来就是坏的（`POST /api/libraries/:id/scan` 因判断条件写错永远 404）。
4. **海报墙崩溃**：空媒体库列表接口返回 `null`（nil 切片），前端 `items.map` 直接抛 `Cannot read properties of null`。
5. **海报不显示**：用户 Strm 驱动里的剧集文件名只有集数（`第15集.mp4.strm`），没有剧名 —— 导致 12 集被拆成 12 个条目，TMDB 拿「第15集」去搜还会错配成别的剧。
6. **TMDB 连不上**：部分网络（含本测试环境）无法直连 `api.themoviedb.org`（连接超时），需要自动切备用域名或支持自建反代。

## 二、根因与修复

### 2.1 OpenList 字段类型兼容（1.1.4 仅 mock 验证，未覆盖真实服务器）
- `internal/openlist/client.go`：
  - 新增 `FlexInt64`：兼容 `modified`/`size` 为数字 / 字符串数字 / 浮点 / **ISO 纳秒串**（`RFC3339Nano`），解析不出回退 0。
  - 新增 `FlexString`：兼容 `type` 为字符串 / 数字（真实 OpenList 返回 `int`）。
  - `FsObj.Size`/`Modified`/`Type` 改用上述类型；`scanner.go` 落库处做 `int64(...)` 转换。

### 2.2 存储源：编辑 / 删除 / 去重
- 后端 `handleStorages`：
  - `POST` 时按 `BaseURL` 去重（同 URL 视为更新，不再越攒越多）。
  - 新增 `PUT /api/storages/:id`（编辑更新）与 `DELETE /api/storages/:id`（删除）；store 新增 `DeleteStorage` / `GetStorageByBaseURL`。
- 前端 `Settings.tsx` 存储面板：点列表项可「编辑」（按钮变「更新」）；每条可「删除」；默认值 `name` 改为空（消除「main」困惑），`rate_limit` 占位提示「默认 2」。
- `api.ts` 新增 `updateStorage` / `deleteStorage`。

### 2.3 媒体库：创建入口 + 自动扫描导入
- 前端 `Library.tsx`：右上角新增「创建媒体库」按钮，表单含名称 / 模式 / **存储源下拉** / 网盘内部路径；提交即建库并**自动发起扫描导入**，随后跳转到海报墙看进度。每条可删除。
- 前端 `App.tsx` `LibraryItems`：挂载时若后台已有进行中的扫描任务（如刚自动发起的），自动轮询进度；「扫描」按钮改名「扫描导入」。
- 后端扫描路由修复（关键）：原 `case len(parts)==4 && parts[2]=="scan"` 判断条件错误（lib id 在 `parts[2]`、字面量 `"scan"` 在 `parts[3]`），且误把 `parts[3]`（即 `"scan"`）当 lib id 传给 `startScan` —— 导致 `POST /api/libraries/:id/scan` 永远 404、「扫描」按钮完全失效。改为 `parts[3]=="scan"` 并传 `parts[2]`。新增 `GET /api/libraries/:id/scan` 返回该库最近一次扫描任务（供前端轮询）。store 新增 `GetLatestScanJob`。

### 2.4 空列表返回 `null` 崩溃
- `internal/store/store.go`：`ListMediaItems` / `ListMediaFiles` / `ListContinue`(PlayRecord) / `ListFavorites` 原为 nil 切片（空时 JSON 序列化成 `null`），前端 `.map` 崩溃。统一改为返回非 nil 空切片 `[]`。

### 2.5 剧集聚合与海报显示
- `internal/parser/parser.go`：
  - 新增 `ParseInDir(fileName, dirs)`：文件名只有集数（如 `第15集.mp4.strm`）时，向上跳过 `Season 1`/`第一季` 等季目录，从父目录（如 `将夜 (2026)`）取剧名和年份。
  - 标题清理现在会剔除残留的集数标记（`第12集` / `S01E05` 等），避免拿「庆余年 第12集」去搜 TMDB。
- `internal/scanner/scanner.go`：
  - 扫描器把当前目录链传给 `ParseInDir`。
  - `MediaItem` 的 ID 改为按 **库+剧名+年份** 生成：同一部剧的多个 strm 自动合并成 1 个条目，每集作为 `MediaFile` 归拢。
  - `MediaFile` 的 ID 改按完整路径生成：不同剧集里同名的「第1集.mp4.strm」不再互相覆盖。
  - 新增 `resolveItem`：优先按 ID 取回 upsert 后的条目，修复刮削改标题后再次扫描产生重复条目的 bug。
- `web/src/pages/Detail.tsx`：分集列表按 `season_no / episode_no` 排序，并显示「第 N 集」/ `SxEx` 标签。

### 2.6 TMDB 稳定性：自动备用域名 + 匹配打分 + 测试连接
- `internal/tmdb/tmdb.go`：
  - `Client` 内置主域名 `api.themoviedb.org/3` 和备用域名 `api.tmdb.org/3`；主域名传输层失败（DNS/连接超时）时自动降级，HTTP 错误（如 401）则保留错误原因不重试。
  - 支持 `TMDB_API_BASE` 环境变量 / 前端设置 `tmdb_api_base` 自建反代覆盖。
  - 修复 `ByID`：/movie/{id}、/tv/{id} 返回单个对象而非列表，旧实现永远解析为空。
  - `Search` 增加**标题匹配打分**（完全相同 > 前缀/包含 > 年份吻合），不再盲取 `results[0]`；真实案例「将夜」曾因 TMDB 排序把《昨夜将至》放在首条而错配海报。
  - 剧集搜索带年份无结果时，自动去掉 `first_air_date_year` 再搜一次（目录年份常是资源发布年，与首播年不一致）。
- `internal/api/handlers.go`：新增 `POST /api/settings/tmdb/test`，用当前 Key/地址真实搜索一次，返回实际命中的 API 地址和示例结果。
- `web/src/pages/Settings.tsx`：TMDB 面板增加「API 地址」输入框与「测试连接」按钮。

## 三、验证

- `go build` / `go vet` / `go test ./...` 全绿（含 openlist FlexInt64/FlexString 多项用例）。
- **真实 OpenList 端到端（Playwright + 无头 Chromium）**：
  - 测试连接：返回 `ok:true`、20 个网盘列出、`modified` 从纳秒串正确解析（`1785334327`）、`type` 数字→`"1"`。
  - 存储源：保存两次仅 1 条（去重）；点列表项改名后「更新」仍 1 条；「删除」后归零。
  - 媒体库：点「创建媒体库」→ 选存储源 + 填路径 →「创建并扫描导入」→ 跳转海报墙；`GET /api/libraries/:id/scan` 返回 200（路由修复生效）。
  - **控制台 0 错误**（空列表 null 崩溃已消除）。
  - **海报测试**：导入用户 Strm 驱动 `/strm/国漫/将夜 (2026)`（12 个 `.cas`/`.strm` 文件），结果：
    - 条目数 **1**（剧名「将夜」），不是 12 个「第N集」。
    - TMDB ID = **282136**（正确那部《将夜》），没有错配成《昨夜将至》。
    - 12 个分集自动归拢到同一条目。
    - 海报墙正确展示海报，详情页正确展示简介、评分、分集列表。

## 四、升级

```bash
docker compose pull && docker compose up -d
```

标准导入流程：设置 → 绑定 OpenList（测试连接通过）→ 媒体库 → 创建媒体库（选存储源、填网盘内部路径如 `/115_open/Video`）→ 自动扫描导入 → 海报墙查看。

## 五、镜像

```
tianjian518/newmovie:1.1.5
tianjian518/newmovie:latest
```

架构：`linux/amd64` · `linux/arm64` · `linux/arm/v7`
