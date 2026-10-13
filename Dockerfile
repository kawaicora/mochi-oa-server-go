# 运行环境镜像：为「宿主机交叉编译、再挂载进容器的 Linux 二进制」提供带 CA 根证书的基础。
#
# 为什么需要它（根源）：
#   ubuntu:24.04 与 debian:bookworm 官方镜像【都不含 ca-certificates】——
#   debian 有 tzdata 但没有 CA，容器里连 /etc/ssl 目录都不存在。
#   于是 Go 的 crypto/x509 找不到任何根证书，所有出站 HTTPS 一律失败：
#     x509: certificate signed by unknown authority
#   受影响的不只是 Cloudflare ICE，还有爱发电 HTTPS、SMTP over TLS 等。
#   （只把基础镜像从 ubuntu 换成 debian 解决不了，必须真的把 CA 装进去。）
#
# 为什么放在构建期而不是容器启动时 apt-get：
#   启动时装需要每次联网 + 数秒延迟；构建期装一次，镜像可离线运行、启动行为确定。
#
# 国内网络：apt 源已切到阿里云（http，避免「还没有 CA 却要先走 https」的鸡生蛋问题）。
#   如要换源：把下面的 URI 换成 mirrors.tuna.tsinghua.edu.cn / mirrors.ustc.edu.cn /
#   repo.huaweicloud.com 同构路径即可（本包在任何 Debian 镜像上都有，不必换源也能装）。
#
# 用法：二进制不 COPY 进来，仍由 docker-compose 的 volume（./dist:/app）挂载。
FROM debian:bookworm

# debian:bookworm 用的是 deb822 格式（/etc/apt/sources.list.d/debian.sources），
# 没有 /etc/apt/sources.list；这里按同样结构整份重写为阿里云源。
# Components 由 main 扩到 main contrib non-free non-free-firmware，避免以后装别的包时缺组件。
RUN set -eux; \
    rm -f /etc/apt/sources.list; \
    printf '%s\n' \
        'Types: deb' \
        'URIs: http://mirrors.aliyun.com/debian/' \
        'Suites: bookworm bookworm-updates' \
        'Components: main contrib non-free non-free-firmware' \
        'Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg' \
        '' \
        'Types: deb' \
        'URIs: http://mirrors.aliyun.com/debian-security/' \
        'Suites: bookworm-security' \
        'Components: main contrib non-free non-free-firmware' \
        'Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg' \
        > /etc/apt/sources.list.d/debian.sources; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates tzdata; \
    rm -rf /var/lib/apt/lists/*; \
    # 构建期自检：CA 包必须真的落地，否则直接让构建失败（避免又产出一个报 x509 的镜像）
    test -s /etc/ssl/certs/ca-certificates.crt

WORKDIR /app

# compose 里也显式写了同样的 command；这里留一份让镜像可独立运行。
CMD ["/app/mochi-oa-server_linux_amd64"]
