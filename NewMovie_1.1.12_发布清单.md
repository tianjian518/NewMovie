# NewMovie v1.1.12 — 4K HEVC+DTS/TrueHD 页内播放（音轨转 AAC，不再甩外部）

- 日期：2026-08-02
- 基线：v1.1.11（`6f6177e`）
- 主题：用户反馈「解析播放源后仍弹『已为你选择外部播放器』」。根因是 4K 蓝光原盘的
  音轨是 DTS-HD / TrueHD / Atmos，此前被判定为无法重封装 → 直接 L4 外部播放器。

## 一、问题

v1.1.9 让 HEVC 走 L2 重封装页内播放，但 `remuxAudio` 只含 `aac/mp3/opus/ac3/eac3`，
**不含 DTS / TrueHD / Atmos**。而 4K 电影几乎都是「HEVC 视频 + DTS-HD/TrueHD 音轨」。
选择器里：

```go
remuxable := remuxVideo[vc] && remuxAudio[ac] && remuxContainers[c]
```

DTS 音轨使 `remuxable = false`，加上 `handlers.go` 里 `TranscodeEnabled` 硬编码为 `false`，
于是 HEVC+DTS 直接落到 **L4 外部播放器**——于是用户看到那句提示，点击就唤起外部播放器
（没外部播放器则变成浏览器下载）。v1.1.9 实际只修了 HEVC+AAC/AC3，没覆盖 DTS 音轨。

## 二、改动

`internal/playback/selector.go`：

- `Decision` 新增 `NeedsAudioTranscode` 字段。
- 重封装判定改为两段：
  - 视频可重封装（`remuxVideo`）且容器可重封装（`remuxContainers`）时即走 **L2**；
  - 音轨是 `aac/ac3/eac3/mp3/opus` → 纯 `-c copy` 重封装；
  - 音轨是 `dts/truehd/atmos/flac` 等装不进 MP4 的 → **视频保持拷贝、仅把音轨实时转成 AAC**
    （`NeedsAudioTranscode = true`），同样页内可播。
- 结果：HEVC + DTS/TrueHD/Atmos 现在走 L2 轻量音轨转码，原画质零损失、服务端几乎零开销。

`internal/api/handlers.go`：

- `handleRemux`：新增 `aac=1` 参数 → ffmpeg 用 `-c:v copy -c:a aac -b:a 320k`
  （视频不重编码，只转音轨）；否则维持原 `-c copy`。
- 构造播放决策时，若 `dec.NeedsAudioTranscode`，给 remux URL 追加 `&aac=1`。

`internal/playback/selector_test.go`：

- 原 `TestL4ExternalWhenTranscodeOff`（HEVC+DTS→L4）、`TestL3TranscodeWhenEnabled`（HEVC+DTS→L3）
  改为用「不支持重封装的视频编码（wmv）+ 未开转码」触发，分别对应 L4 / L3；
- 新增 `TestL2RemuxHEVC_MKV_DTS`、`TestL2RemuxHEVC_MKV_TrueHD`：确认 HEVC+DTS/TrueHD → L2 且 `NeedsAudioTranscode`。

## 三、验证

- `go test ./...` 全绿（含 playback / api）。
- 功能验证：用 `libopenh264 + dca` 造「视频 + DTS 音轨」的 MKV，跑 `aac=1` 那条 ffmpeg 管线，
  产物为 `视频流原样保留 + 音轨变为 AAC` 的合法 MP4（容器 `mov,mp4,...`），即页内可播格式。

## 四、部署注意（务必读）

- **本次是后端改动，必须重新构建/部署后才会生效**：拉取新镜像或 `go build` 重启服务。
  若仍跑着旧容器，会继续使用旧的 L4 逻辑、照弹外部播放器提示。
- 前提：运行环境已装 `ffmpeg`（`handleRemux` 会在缺失时明确报错）。
- 边界：本修复让「视频编码浏览器能解」的内容页内播。若浏览器**本身不解码 HEVC**
  （如 Firefox / 部分 Linux Chrome），MP4-HEVC 仍放不出——此时需开启转码（L3）把视频也转成 H.264。
  默认 `TranscodeEnabled=false`，如确需可在设置/配置里打开。

## 五、版本

`internal/api/handlers.go` 版本号 `1.1.11` → `1.1.12`。

## 六、重新部署与页内播放验证

> v1.1.12 已推送 GitHub（`main` + `v1.1.12` tag），GitHub Actions 会自动重建并推到 Docker Hub。

### 方案 A：Docker（标准路径，等镜像上 Docker Hub 后拉取）

1. 等 GitHub Actions 把 v1.1.12 镜像推到 Docker Hub（通常几分钟内完成）。
2. 部署机执行：
   ```bash
   docker compose pull
   docker compose up -d
   ```
3. 确认版本：
   ```bash
   curl -s localhost:8096/api/health
   # 期望返回 {"ok":true,"version":"1.1.12","name":"NewMovie"}
   ```

### 方案 B：本地源码构建（不等 Docker Hub，立刻验证）

前置：Go 1.22+ 、Node 22+ ，运行环境带 `ffmpeg`。

```bash
git pull                 # 或 git checkout v1.1.12
make build               # Makefile 的 build-go 路径 bug 已修，现可一次跑通
# 直接运行：
VIDRIVE_DATA=./data VIDRIVE_ADDR=:8096 ./bin/vidrive
```

或本地打镜像（不依赖 Docker Hub）：

```bash
docker build -t newmovie:local .
docker run -p 8096:8096 -v "$PWD/data:/data" newmovie:local
```

### 页内播放验证（核心）

1. 打开一部 **4K HEVC + DTS / TrueHD / Atmos** 的电影详情页，点「播放」。
2. 预期：直接页内播放，**不再弹「已为你选择外部播放器」**。
3. 抓包看播放请求 URL：应带 `&aac=1`（表示视频拷贝 + 音轨实时转 AAC 的 L2 重封装）。
4. 若仍弹外部播放器：说明该文件「视频编码」不在可重封装列表（如 wmv / rm / avi 老编码），
   属预期的 L3 / L4 降级，并非本修复失效。
5. 边界：Firefox / 部分 Linux Chrome 本身不解码 HEVC，MP4-HEVC 仍放不出，
   需开 `TranscodeEnabled`（视频也转 H.264）才能页内播。

### Makefile 已修的坑

原 `build-go` 写 `cd cmd/server && go build ... ./cmd/server`，`cd` 之后 `./cmd/server`
路径不存在会报错。已改为 `go build -o bin/vidrive ./cmd/server`，`make build` 现可一次跑通。
另把 `/bin` 加入 `.gitignore`，避免构建产物污染提交。
