# NewMovie 1.1.3 发布清单 — 修复登录按钮「点了没反应」

> 修复类型：功能性 Bug。所有通过浏览器访问的用户都需要升级。

---

## 一、问题现象

1.1.2 修复白屏后，浏览器能正常打开登录页，账号密码都是 `admin`/`admin`，但**点「登录」毫无反应**——页面纹丝不动，URL 仍为 `/`，必须手动刷新才能进主界面。

## 二、根因

`web/src/App.tsx` 的根组件用 `useState` 初始化登录态：

```ts
const [token] = useState(getToken());   // 只在组件挂载时读一次 localStorage
if (!token) return <Login />;
```

`useState` 的初值**仅在首次挂载时计算一次**。登录接口其实已经成功返回 token 并写进了 `localStorage`，但 React 不会因为 `localStorage` 变化而重渲染。于是 `token` 状态永远是空字符串，组件一直卡在 `<Login />`，表现就是「点了没反应」。手动刷新之所以能进主界面，是因为刷新等于重新挂载，此时 `getToken()` 才读到了刚写进去的 token。

> 这种陷阱在「状态来自 localStorage / 外部存储」时极易踩中：只写存储、不更新 React 状态，视图不会动。

## 三、修复

- **`web/src/App.tsx`**：
  - 根组件改为 `const [token, setTok] = useState(getToken())`，登录成功由 `Login` 的 `onLogin` 回调调用 `setToken(t); setTok(t)` 触发重渲染，真正进入主界面（无需刷新）。
  - `Login` 增加 `busy` 状态（防止重复提交）与 `err` 状态（展示后端错误信息）。
  - 新增 `useEffect` 监听 `newmovie:unauthorized` 全局事件：token 失效时自动 `clearToken()` + `setTok("")` 退回登录页。
  - 新增侧边栏「退出登录」按钮，调用 `logout()` 清 token 并退回登录页。
- **`web/src/api.ts`**：
  - 新增 `clearToken()`（删除 `localStorage` 中的 token）。
  - `req()` 在收到 `HTTP 401` 时派发 `window.dispatchEvent(new Event("newmovie:unauthorized"))`，与 `App` 的监听联动——顺手解决了「token 失效后主界面所有请求报错、整站卡死」的隐患。
- **`internal/api/handlers.go`**：`Version` 升到 `1.1.3`。
- **`README.md`**：补充 1.1.3 变更说明。

## 四、验证

- `npm run build` 成功（41 模块，JS 321.8KB / CSS 10.82KB）。
- `go build ./...` / `go test ./...` 全绿（沿用 1.1.1/1.1.2 的回归用例）。
- 无头浏览器（Playwright + 系统 Chromium）实测：
  - 登录页正常渲染；填入 `admin`/`admin` 后**点击登录，无需刷新即进入主界面**（出现「媒体库」导航，不再停留登录页）；
  - `/api/login` 返回 HTTP 200，token 写入 `localStorage`；
  - 控制台 0 错误 / 0 警告；
  - 反向验证：派发 `newmovie:unauthorized` 事件后，页面自动退回登录页且 `localStorage` token 已清除（401 联动链路生效）。

## 五、升级

```bash
docker compose pull && docker compose up -d
```

升级后打开 `http://<你的IP>:8096`，用 `admin`/`admin` 登录应**直接**进入媒体库。首次进入务必在「设置」里改默认密码。

## 六、镜像

```
tianjian518/newmovie:1.1.3
tianjian518/newmovie:latest
```

架构：`linux/amd64` · `linux/arm64` · `linux/arm/v7`
