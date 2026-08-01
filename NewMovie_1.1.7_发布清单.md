# NewMovie 1.1.7 发布清单 — 目录树选择器 + 扫描 Bug 修复 + 诊断信息

> 修复类型：扫描器 Bug + 重大易用性改进。解决「OpenList 连接成功但扫描扫不出内容」的典型问题。

---

## 一、用户遇到的问题

1. **创建媒体库要手填网盘内部路径**
   - 用户原文：「选择 OpenList 目录，还要我手动填写吗？这是太落后了吧？难道不应该是点击按钮，让我选择某一个目录里面的某一个文件夹吗？」
   - 路径全凭手打，少一个 `/`、多一个空格、带个域名，整库就扫不出来。

2. **手填目录后点击扫描，扫不出内容，也看不到原因**
   - 用户原文：「而且就算我手动填写目录，点击扫描，也是扫描不出内容来，哪里有 bug 吗？」
   - 扫描失败只返回 `status: "failed"`，没有任何错误说明。
   - 单个坏目录会把整次扫描全部中断，导致正常文件也丢失。
   - `.strm` 目录建错了模式，扫描成功但入库 0 条，用户完全无从得知原因。

---

## 二、修复内容

### 2.1 网盘目录树点击选择（新增）

- **后端**
  - 新增 `GET /api/storages/:id/browse?path=/xxx`：调用 OpenList `/api/fs/list`，只返回子目录；同时统计当前层的视频数 / strm 数，并按内容推荐建库模式（native / strm / mixed）。
  - `internal/openlist/client.go` 新增 `NormalizePath()`：自动处理「缺前导斜杠」「尾斜杠」「前后空格」「零宽字符」「重复斜杠」「反斜杠」等手填错误。
  - 路径规范化同时应用于 `List()`、`GetLink()`、建库保存、扫描入口。

- **前端**
  - 新增组件 `web/src/components/DirPicker.tsx`：弹窗式目录树，逐级懒加载，面包屑导航，实时显示「本层有几个视频 / strm / 建议模式」。
  - `web/src/pages/Library.tsx`：创建媒体库表单把「网盘内部路径」输入框替换为「浏览…」按钮 + 可手动输入兜底；选择目录后自动回填路径、名称和模式。

### 2.2 扫描器容错加固

- **致命 vs 非致命分离**
  - 只有根目录读不到才视为扫描失败。
  - 子目录读不到、单个文件处理失败 → 记为 warning 并继续扫描，不再因为一条坏数据搞丢整个库。

- **错误原因记录与展示**
  - `model.ScanJob` 新增字段：`error`、`warnings`、`skipped`、`skip_hint`、`dirs`。
  - 扫描失败时把 OpenList 的原始错误翻译成人话（找不到路径 / Token 失效 / 连不上 / 被中断）。
  - 扫描成功但 0 条入库时，给出「你是不是把 .strm 目录建成了 native 模式？」等提示。
  - 海报墙新增 `ScanDiagnostics` 组件，轮询到失败/跳过时直接展开错误、提示和警告列表。

- **扫描预检**
  - `POST /api/libraries/:id/scan` 在启动后台扫描前，先同步探测根目录。
  - 路径不存在立即返回 `400`，并写失败原因到 ScanJob，前端不用在空白页面上等半天。

### 2.3 测试覆盖

- Go 单元测试：
  - `internal/scanner/scanner_repro_test.go`：复现并保护 4 个 bug（脏路径、坏目录中断、strm 模式错配、错误原因未记录）。
  - `internal/api/handlers_browse_test.go`：覆盖 browse API、根目录默认、脏路径、推荐模式、错误可读、扫描预检、建库路径规范化。
- UI 端到端：
  - 用 Playwright + 系统 Chromium 实际驱动登录、打开目录树、逐级选择 `/115_open/电影`、建库扫描，验证「盗梦空间」「沙丘」入库；并验证错路径库显示人话错误。

---

## 三、变更文件

- `internal/model/models.go` — `ScanJob` 新增诊断字段。
- `internal/openlist/client.go` — `NormalizePath()`、路径规范化应用于 `List`/`GetLink`。
- `internal/scanner/scanner.go` — 扫描容错、warning、跳过统计、错误提示。
- `internal/api/handlers.go` — `browseStorage`、扫描预检、建库路径规范化、版本号 `1.1.7`。
- `internal/api/handlers_browse_test.go` — 新测试。
- `internal/scanner/scanner_repro_test.go` — 新测试。
- `web/src/types.ts` — 新增 `ScanJob` 字段与 `BrowseResp`。
- `web/src/api.ts` — 新增 `api.browse()`。
- `web/src/components/DirPicker.tsx` — 新增目录树组件。
- `web/src/pages/Library.tsx` — 接入目录树选择器与模式自动推荐。
- `web/src/App.tsx` — 海报墙展示扫描错误/提示/警告。
- `NewMovie_1.1.7_发布清单.md` — 本文件。

---

## 四、验证命令

```bash
cd /workspace
go test -race ./...
go vet ./...
cd web && npx tsc --noEmit && npm run build
```

---

## 五、使用建议

1. 升级后打开「媒体库 → 创建媒体库」，选好存储源，点「浏览…」逐层进入存放影片的目录。
2. 目录树底部会显示本层视频数和建议模式；确认后点击「选择此目录」。
3. 如果之前已有库扫不出内容，进入对应海报墙，点击「扫描导入」，新版会直接提示是路径问题还是模式问题。
