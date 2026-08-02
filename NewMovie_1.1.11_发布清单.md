# NewMovie v1.1.11 — 响应式导航（手机底部标签栏，仿飞牛影视）

- 日期：2026-08-02
- 基线：v1.1.10（`17a35b0`）
- 主题：用户反馈整体布局不好——侧边栏太宽、手机完全不适应；要求把导航改成底部标签栏（媒体库 / 收藏 / 设置…），仿飞牛影视。

## 一、问题

旧 `App.tsx` 用一段固定 `w-48`（192px）的 `<aside>` 侧边栏，**在所有屏幕宽度都显示**：

- 手机上侧栏直接撑满，没有做任何响应式适配，主内容被挤到一旁，几乎不可用；
- 侧栏本身也偏宽（192px）浪费横向空间。

用户希望像**飞牛影视 / 网易爆米花**那样：导航收到底部，变成等宽的图标+文字标签，手机单手可操作。

## 二、改动

`web/src/App.tsx`：

- 抽出 `navItems`（首页 / 媒体库 / 收藏 / 设置）+ 4 个内联 SVG 图标（首页、网格、心形、齿轮），无第三方图标库依赖。
- 新增 `Sidebar`（桌面）：`hidden md:flex w-44`，较旧版更窄；用 `NavLink` 高亮当前项（`bg-brand/15 text-brand`）；含「退出登录」。
- 新增 `BottomNav`（手机）：`md:hidden fixed bottom-0 inset-x-0`，等宽分布的图标+文字标签，`pb-[env(safe-area-inset-bottom)]` 兼容刘海屏；同样用 `NavLink` 高亮。
- 主布局 `main` 加 `pb-20 md:pb-6` 底部内边距，避免手机端内容被底部栏遮挡。
- `<aside w-48>` 整段删除，改用 `Sidebar` + `BottomNav`。

响应式断点（Tailwind `md` = 768px）：
- `<768px`：侧栏 `hidden` 隐藏，底部标签栏显示；
- `≥768px`：侧栏 `md:flex` 显示，底部栏 `md:hidden` 隐藏。

## 三、验证

- `web` 端 `tsc -b && vite build` 通过。
- 编译后 CSS 确认存在响应式规则：
  `.hidden{display:none}` 与 `@media (min-width:768px){.md\:flex{display:flex}.md\:hidden{display:none}}`，
  即移动端隐藏侧栏、显示底部栏，桌面端反之，逻辑正确。

## 四、版本

`internal/api/handlers.go` 版本号 `1.1.10` → `1.1.11`。
