# NewMovie 1.1.1 发布清单 — 根治 ARM 设备无限重启

> 修复类型：**稳定性关键修复（Critical）**。建议所有用户升级，ARM 设备（NAS / 树莓派）用户**必须**升级。

---

## 一、问题现象

ARM 版本容器启动后陷入**无限重启循环**，日志无明显报错，容器状态反复在 `Restarting` 之间跳转。

## 二、排查过程（结论：不是 ARM 镜像的锅）

先排除了最容易被怀疑的几项，逐条证伪：

| 排查项 | 手段 | 结论 |
|---|---|---|
| ARM 二进制损坏 / 架构错配 | `file` 校验 ELF 头 | 正常：`ELF 64-bit LSB, ARM aarch64, statically linked` |
| 多架构 manifest 缺失 | 直接查询 registry manifest | 正常：`amd64` / `arm64` / `arm/v7` 三份俱全（`unknown/unknown` 是 buildx attestation，非异常） |
| arm64 与 armv7 产物相同 | `sha256sum` 比对 | 正常：哈希各异，确为独立编译 |
| ARM 二进制无法启动 | 提取 qemu-aarch64 用户态模拟直接运行 | **正常启动**，`/api/health` 返回 200 |

既然镜像和二进制都健康，那问题必然出在**运行时被杀死后被 restart 策略反复拉起**。

## 三、根因（已稳定复现）

**主因 —— 扫描器递归无深度限制、无环检测：**

`scanner.Scan` 的 `walk` 是裸递归。当网盘目录成环（软链、循环 rclone/webdav 映射、异常挂载）时会无限下钻。构造自引用目录的复现测试直接把进程跑到 `signal: killed`（内存耗尽被 OOM Killer 干掉）。

于是形成闭环：

```
目录成环 → 无限递归 → 内存耗尽 → OOM Killer 杀进程
   ↑                                      ↓
   └──── 前端再次触发扫描 ← 容器 restart 拉起 ←┘
```

ARM 设备内存小、CPU 弱，比 x86 更早触发，所以「只有 ARM 版重启」。

**帮凶 1 —— 全库零 `recover()`：** Go 中任何 goroutine 的 panic 都会终止**整个进程**。后台扫描协程一旦 panic（坏 NFO、异常字段等），服务直接崩溃。已用最小样例验证：后台协程 panic → `exit status 2`。

**帮凶 2 —— 存储层 O(n²) 落盘：** 每入库一个条目就把整个数据库全量序列化重写。实测 3000 条目累计分配 **4.9GB**、耗时 11s；且用的是非原子的 `os.WriteFile` —— 进程被杀时留下半截 JSON，下次启动解析失败，进一步加剧重启。

## 四、修复内容

| 模块 | 修复 |
|---|---|
| `internal/scanner` | 新增 `MaxScanDepth=24` 深度上限 + 已访问目录集合（环检测）；跳过 `.` / `..` 自引用条目 |
| `internal/api` | 后台扫描协程加 `recover`（失败仅标记该任务 failed）；新增 `recoverMW` HTTP 中间件，坏请求返回 500 而非打挂服务（`ErrAbortHandler` 按约定透传） |
| `internal/store` | 落盘改为 400ms **防抖合并写**；改为**临时文件 + rename 原子写入**；新增 `Close()` 确保退出前落盘 |
| `cmd/server` | 监听失败**退避重试 5 次**（不再因端口占用直接 Fatal）；`SIGTERM` 优雅关闭；数据目录创建失败降级为告警 |
| `Dockerfile` | 预建 `/data`；新增 `HEALTHCHECK`；消除冗余 platform 警告 |
| `docker-compose.yml` | 增加 `healthcheck` 与 `mem_limit: 512m` 护栏 |

## 五、性能改善

| 指标 | 1.1.0 | 1.1.1 | 提升 |
|---|---|---|---|
| 写入 3000 条目 | 11.08s | 0.03s | **约 370x** |
| 累计内存分配（3000 条） | 4889 MB | 显著降低 | — |
| 目录成环时行为 | 进程被 OOM 杀死 | 0.00s 内收敛并告警 | — |

## 六、验证结果

- `go build` / `go vet` / `go test ./...` **全部通过**
- 新增 **8 个回归用例**：目录环终止、自引用跳过、panic 恢复 ×3、防抖落盘、Close 持久化、原子写入
- arm64 交叉编译产物经 **QEMU 用户态实测**启动正常，健康检查返回 `1.1.1`
- 容器实测状态 **`Up (healthy)`**；`docker stop` 触发优雅退出，数据完整落盘，**无残留 `.tmp` 文件**

## 七、升级方式

```bash
docker compose pull && docker compose up -d
```

若此前已陷入重启循环，建议先停容器再升级：

```bash
docker compose down
docker compose pull
docker compose up -d
docker compose logs -f      # 观察启动日志
```

升级后如日志出现 `[scan] 检测到目录环` 或 `[scan] 目录深度超过上限`，说明防护已生效并成功拦截了问题目录 —— 服务会跳过它继续正常扫描，不再崩溃。

## 八、镜像

```
tianjian518/newmovie:1.1.1
tianjian518/newmovie:latest
```

架构：`linux/amd64` · `linux/arm64` · `linux/arm/v7`
