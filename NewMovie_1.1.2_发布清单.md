# NewMovie 1.1.2 发布清单 — 修复前端白屏 / 左上角 dist 链接

> 修复类型：功能性 Bug。所有通过浏览器访问的用户都需要升级。

---

## 一、问题现象

容器稳定后，浏览器打开首页，只在左上角看到一个 **`dist/` 链接**；点击后跳转到一个**完全空白的页面**。

## 二、根因

Go 的 `embed.FS` 把 `dist` 目录本身当作了文件系统根。

代码原写法：

```go
//go:embed dist
var dist embed.FS
mux.Handle("/", http.FileServer(http.FS(dist)))
```

这意味着 HTTP 根路径 `/` 对应的是 `dist` 的**父目录**，而不是 `dist` 里面的 `index.html`。于是浏览器看到的是：

```html
<pre>
<a href="dist/">dist/</a>
</pre>
```

点击 `dist/` 后虽然能看到 `index.html`，但 HTML 里引用的资源路径是 `/assets/index-xxx.js`，而真实文件在 `/dist/assets/index-xxx.js`。结果 **JS/CSS 全部 404**，React 无法挂载到 `<div id="root">`，页面完全空白。

| 请求 | 旧行为 | 期望 |
|---|---|---|
| `GET /` | 200 目录列表 | 200 `index.html` |
| `GET /assets/index-xxx.js` | 404 | 200 JS 文件 |
| `GET /dist/assets/...` | 200 | 不应暴露 |
| 刷新 `/settings` | 404 | 200 `index.html` |

这个 Bug 一直存在，只是之前容器在无限重启，没人能打开页面，所以没暴露。

## 三、修复

- **`cmd/server/main.go`**：新增 `spaHandler`
  - 用 `fs.Sub(dist, "dist")` 剥离前缀，让 `/assets/...` 正确命中 embed 资源；
  - 真实存在的文件走 `http.FileServer`，并设置合理的 `Cache-Control`：
    - `assets/*` → `public, max-age=31536000, immutable`（带哈希的构建产物可强缓存）；
    - `index.html` 等 → `no-cache`（避免前端更新后用户仍用旧版本）。
  - 未命中的非 `/api/` 路径统一回落 `index.html`，支持 `react-router` 的前端路由刷新；
  - `/api/unknown` 这类接口 404 不会被吞成 HTML。
- **`web/`**：页面标题与几处 UI 文案从 `Vidrive` 改为 `NewMovie`。

## 四、验证

- `go build` / `go vet` / `go test ./...` 全绿；新增 5 个 `cmd/server` SPA 回归用例。
- 容器内实测：
  - `GET /` 返回真正的 `index.html`（标题 `NewMovie · 云盘媒体库`）；
  - `/assets/index-xxx.js` 200，大小 322KB；
  - `/assets/index-xxx.css` 200，大小 10.6KB；
  - `/settings`、`/library/abc` 等前端路由刷新均返回 200 HTML；
  - `/api/health` 正常 JSON；
- 无头浏览器截图验证登录页完整渲染（NewMovie 标题 + 用户名/密码输入框 + 登录按钮）。

## 五、升级

```bash
docker compose pull && docker compose up -d
```

升级后打开 `http://<你的IP>:8096` 应直接显示登录页，而不是 `dist/` 目录链接。

## 六、镜像

```
tianjian518/newmovie:1.1.2
tianjian518/newmovie:latest
```

架构：`linux/amd64` · `linux/arm64` · `linux/arm/v7`
