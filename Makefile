# Vidrive Makefile
IMAGE ?= tianjian518/newmovie
TAG   ?= latest

.PHONY: build web build-go test run docker clean

## 前端构建
web:
	cd web && npm install && npm run build

## 后端构建（无需前端时也可单独跑，使用占位页）
build-go:
	cd cmd/server && go build -o ../../bin/vidrive ./cmd/server

## 全量构建二进制（先前端后后端）
build: web build-go

test:
	go test ./...

run:
	VIDRIVE_DATA=./data VIDRIVE_ADDR=:8096 go run ./cmd/server

## 构建并推送多架构镜像到 Docker Hub
docker:
	docker buildx build --platform linux/amd64,linux/arm64 \
		-t $(IMAGE):$(TAG) --push .

clean:
	rm -rf bin web/dist cmd/server/dist data
