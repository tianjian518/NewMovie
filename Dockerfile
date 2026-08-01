# ---- 阶段 1：前端构建 ----
FROM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json* ./
RUN npm install
COPY web/ ./
# 构建产物输出到 /src/cmd/server/dist（与 Go embed 路径一致）
RUN npm run build

# ---- 阶段 2：后端构建（零依赖，CGO 关闭，多架构） ----
FROM golang:1.23-alpine AS backend
WORKDIR /src
COPY go.mod ./
RUN go mod download || true
COPY . .
# 把前端构建产物带进来，供 Go embed 进二进制
COPY --from=web /src/cmd/server/dist ./cmd/server/dist
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/vidrive ./cmd/server

# ---- 阶段 3：运行镜像 ----
FROM alpine:3.20 AS run
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=backend /out/vidrive /app/vidrive
EXPOSE 8096
VOLUME ["/data"]
ENV VIDRIVE_DATA=/data \
    VIDRIVE_ADDR=:8096 \
    TZ=Asia/Shanghai
ENTRYPOINT ["/app/vidrive"]
