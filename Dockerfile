# 运行环境镜像：为「宿主机交叉编译、再挂载进容器的 Linux 二进制」提供带 CA 根证书的基础。
#
# 为什么需要它（根源）：
#   ubuntu:24.04 与 debian:bookworm 官方镜像【都不含 ca-certificates】——
#   debian 有 tzdata 但没有 CA，容器里连 /etc/ssl 目录都不存在。
#   于是 Go 的 crypto/x509 找不到任何根证书，所有出站 HTTPS 一律失败：
#     x509: certificate signed by unknown authority
#   受影响的不只是 Cloudflare ICE，还有爱发电 HTTPS、SMTP over TLS 等。
#   （换 debian/ubuntu 基础镜像解决不了，必须真的把 CA 装进去。）
#
# 为什么放在构建期而不是容器启动时 apt-get：
#   启动时装需要每次联网 + 数秒到数十秒延迟，且镜像可离线运行、启动行为确定。
#
# 用法：二进制不 COPY 进来，仍由 docker-compose 的 volume（./dist:/app）挂载。
FROM debian:bookworm

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tzdata \
 && rm -rf /var/lib/apt/lists/* \
 # 构建期自检：CA 包必须真的落地，否则直接让构建失败（避免又跑出一个报 x509 的镜像）
 && test -s /etc/ssl/certs/ca-certificates.crt

WORKDIR /app

# compose 里也显式写了同样的 command；这里留一份让镜像可独立运行。
CMD ["/app/mochi-oa-server_linux_amd64"]
