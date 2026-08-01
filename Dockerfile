# syntax=docker/dockerfile:1

# ---- 阶段 1：前端构建 ----
# 固定在构建机原生平台执行（产物为 JS/CSS，与架构无关），避免在每个目标架构下重复用 QEMU 跑 node。
FROM --platform=$BUILDPLATFORM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json* ./
RUN npm install
COPY web/ ./
# 构建产物输出到 /src/cmd/server/dist（与 Go embed 路径一致）
RUN npm run build

# ---- 阶段 2：后端构建（多架构交叉编译，零依赖，CGO 关闭） ----
# 用 TARGETPLATFORM / TARGETARCH 让 go 直接交叉编译，无需 QEMU 模拟整条工具链。
FROM golang:1.23-alpine AS backend
WORKDIR /src
ARG TARGETARCH
ARG TARGETVARIANT
COPY go.mod ./
RUN go mod download || true
COPY . .
# 把前端构建产物带进来，供 Go embed 进二进制
COPY --from=web /src/cmd/server/dist ./cmd/server/dist
# arm/v7 需 GOARM=7，其余架构 GOARM 留空（被忽略）。
RUN GOARM=$( [ "$TARGETARCH" = "arm" ] && echo 7 || echo "" ) && \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} GOARM=${GOARM} \
    go build -ldflags="-s -w" -o /out/newmovie ./cmd/server

# ---- 阶段 3：运行镜像（多架构） ----
FROM alpine:3.20 AS run
RUN apk add --no-cache ca-certificates tzdata wget
WORKDIR /app
COPY --from=backend /out/newmovie /app/newmovie
# 预建数据目录：未挂载卷时也能启动，避免因目录缺失反复失败重启。
RUN mkdir -p /data
EXPOSE 8096
VOLUME ["/data"]
ENV VIDRIVE_DATA=/data \
    VIDRIVE_ADDR=:8096 \
    TZ=Asia/Shanghai
# 健康检查：让编排层能区分「启动中」与「真的挂了」，而不是盲目重启。
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8096/api/health >/dev/null 2>&1 || exit 1
ENTRYPOINT ["/app/newmovie"]
