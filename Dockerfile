# syntax=docker/dockerfile:1
#
# NewMovie 2.0 —— 一个镜像，两个进程。
#
#   openlist(139cas)  127.0.0.1:5244   网盘挂载与直链后端（不对外暴露）
#   newmovie          0.0.0.0:8096     媒体库与播放器（唯一入口，反代 /openlist/*）
#
# 用户跑 `docker run -p 8096:8096` 就能得到完整的「网盘 + 刮削 + 播放」，
# 不用再单独部署 OpenList、不用复制 Token。

# 构建期镜像源（可选）：默认留空走官方源，行为与常规构建一致。
# 在访问 dl-cdn.alpinelinux.org / proxy.golang.org / registry.npmjs.org 受限的网络里，
# 用下面三个 ARG 换成国内镜像即可，例如：
#   docker build \
#     --build-arg APK_MIRROR=https://mirrors.tencent.com/alpine \
#     --build-arg GOPROXY=https://mirrors.tencent.com/go/,direct \
#     --build-arg NPM_REGISTRY=https://mirrors.tencent.com/npm/ .
ARG APK_MIRROR=
ARG GOPROXY=
ARG NPM_REGISTRY=

# ---- 阶段 1：NewMovie 前端构建 ----
# 固定在构建机原生平台执行（产物为 JS/CSS，与架构无关），避免在每个目标架构下重复用 QEMU 跑 node。
FROM --platform=$BUILDPLATFORM node:22-alpine AS web
ARG NPM_REGISTRY
RUN [ -n "$NPM_REGISTRY" ] && npm config set registry "$NPM_REGISTRY" || true
WORKDIR /src/web
COPY web/package.json web/package-lock.json* ./
RUN npm install
COPY web/ ./
# 构建产物输出到 /src/cmd/server/dist（与 Go embed 路径一致）
RUN npm run build

# ---- 阶段 2：内置 OpenList（139cas）构建 ----
# 它的前端是预编译产物，构建时从上游 release 下载（build.sh 的行为），
# 这里单独下载以便利用镜像层缓存。
FROM --platform=$BUILDPLATFORM alpine:3.20 AS olweb
ARG OPENLIST_WEB_VERSION=v4.1.3
ARG APK_MIRROR
RUN if [ -n "$APK_MIRROR" ]; then \
      printf '%s/v3.20/main\n%s/v3.20/community\n' "$APK_MIRROR" "$APK_MIRROR" > /etc/apk/repositories; \
    fi; \
    apk add --no-cache curl tar
WORKDIR /dist
RUN set -eux; \
    url="https://github.com/OpenListTeam/OpenList-Frontend/releases/download/${OPENLIST_WEB_VERSION}/openlist-frontend-dist-${OPENLIST_WEB_VERSION}.tar.gz"; \
    for base in "" "https://ghproxy.net/" "https://gh-proxy.com/"; do \
      if curl -fsSL --connect-timeout 20 -o dist.tar.gz "${base}${url}"; then break; fi; \
    done; \
    tar -xzf dist.tar.gz && rm dist.tar.gz && ls -la

FROM golang:1.24-alpine AS openlist
WORKDIR /src
ARG TARGETARCH
ARG TARGETVARIANT
ARG APK_MIRROR
ARG GOPROXY
RUN if [ -n "$APK_MIRROR" ]; then \
      V=$(cut -d. -f1,2 /etc/alpine-release); \
      printf '%s/v%s/main\n%s/v%s/community\n' "$APK_MIRROR" "$V" "$APK_MIRROR" "$V" > /etc/apk/repositories; \
    fi; \
    apk add --no-cache git
COPY openlist/go.mod openlist/go.sum ./
RUN go mod download
COPY openlist/ ./
COPY --from=olweb /dist ./public/dist
# 139cas 用 glebarez/sqlite（纯 Go 驱动），因此可以 CGO_ENABLED=0 直接交叉编译，
# 无需 musl-cross 工具链，多架构构建快得多。
RUN GOARM=$( [ "$TARGETARCH" = "arm" ] && echo 7 || echo "" ) && \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} GOARM=${GOARM} \
    go build -tags=jsoniter -ldflags="-s -w" -o /out/openlist .

# ---- 阶段 3：NewMovie 后端构建（多架构交叉编译，零依赖，CGO 关闭） ----
FROM golang:1.23-alpine AS backend
WORKDIR /src
ARG TARGETARCH
ARG TARGETVARIANT
ARG GOPROXY
COPY go.mod ./
RUN go mod download || true
# openlist/ 是独立 Go 模块，不参与主模块构建，排除掉可以少传几 MB 上下文。
COPY cmd/ ./cmd/
COPY internal/ ./internal/
# 把前端构建产物带进来，供 Go embed 进二进制
COPY --from=web /src/cmd/server/dist ./cmd/server/dist
# arm/v7 需 GOARM=7，其余架构 GOARM 留空（被忽略）。
RUN GOARM=$( [ "$TARGETARCH" = "arm" ] && echo 7 || echo "" ) && \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} GOARM=${GOARM} \
    go build -ldflags="-s -w" -o /out/newmovie ./cmd/server

# ---- 阶段 4：运行镜像（多架构，双进程） ----
FROM alpine:3.20 AS run
# ffmpeg 用于 /api/play/remux 实时重封装（MKV 等容器转 MP4），让页内播放不再依赖外部播放器。
# supervisor 负责在同一容器里管住 openlist 与 newmovie 两个进程。
# python3 仅用于 entrypoint 里安全地改写 OpenList 的 config.json（sed 改 JSON 太脆）。
ARG APK_MIRROR
RUN if [ -n "$APK_MIRROR" ]; then \
      printf '%s/v3.20/main\n%s/v3.20/community\n' "$APK_MIRROR" "$APK_MIRROR" > /etc/apk/repositories; \
    fi; \
    apk add --no-cache ca-certificates tzdata wget ffmpeg supervisor python3
WORKDIR /app
COPY --from=backend /out/newmovie /app/newmovie
COPY --from=openlist /out/openlist /app/openlist/openlist
COPY docker/ /app/docker/
RUN chmod +x /app/docker/entrypoint.sh /app/docker/quit-on-fail.sh && \
    mkdir -p /data/openlist /run
EXPOSE 8096
VOLUME ["/data"]
ENV VIDRIVE_DATA=/data \
    VIDRIVE_ADDR=:8096 \
    NEWMOVIE_BUNDLED=1 \
    NEWMOVIE_BUNDLED_URL=http://127.0.0.1:5244 \
    TZ=Asia/Shanghai
# 健康检查只看 NewMovie：内置后端的状态由 /api/health 的 bundled 字段体现，
# 后端暂时没起来不代表容器该被杀掉（NewMovie 自己会重试接管）。
HEALTHCHECK --interval=30s --timeout=5s --start-period=40s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8096/api/health >/dev/null 2>&1 || exit 1
ENTRYPOINT ["/app/docker/entrypoint.sh"]
