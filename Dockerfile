# mochi-oa-server-go 多阶段构建
#   build 阶段：golang:1.27-alpine 编译出静态二进制（管理后台静态资源/templates 已 go:embed 进二进制，无需额外拷贝）
#   运行阶段：精简 alpine（ca-certificates + tzdata）
# 注意：/api/transcode 需要容器内有 ffmpeg；默认镜像未装，转码接口会返回 500（其余功能不受影响）。

FROM golang:1.27-alpine AS build
WORKDIR /src

# 国内直连 proxy.golang.org 被墙：build 阶段默认走 goproxy.cn（ARG 可外部 --build-arg GOPROXY=... 覆盖）。
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}

# 先拷依赖清单，利用层缓存
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO_ENABLED=0 → 纯静态二进制，可直接扔进精简镜像
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /out/mochioa-server ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 10001 app
WORKDIR /app
COPY --from=build /out/mochioa-server /app/mochioa-server
RUN chown -R app:app /app
USER app

# 文件上传/分片临时目录（FILE_DIR 默认 ./data，容器内即 /app/data）
VOLUME ["/app/data"]
EXPOSE 3001

ENTRYPOINT ["/app/mochioa-server"]
