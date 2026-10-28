#!/usr/bin/env bash
# mochi-oa-server-go 交叉编译（Linux/macOS 用；Windows 用同目录 build.ps1）。
#
# 产物（dist/）：
#   mochi-oa-server_<GOOS>_<GOARCH>[.exe]          服务端主程序
#   mochi-oa-database_update_<GOOS>_<GOARCH>[.exe] 独立数据库迁移工具
#
# 用法：./build.sh
set -euo pipefail
cd "$(dirname "$0")"

rm -rf ./dist
mkdir -p ./dist

# GOOS/GOARCH 目标
targets="windows/amd64 windows/arm64 linux/amd64 linux/arm64 darwin/amd64 darwin/arm64"
# 程序入口:输出名基
programs="cmd/server/main.go:mochi-oa-server cmd/tools/database_update/main.go:mochi-oa-database_update"

for prog in $programs; do
    entry="${prog%%:*}"
    base="${prog##*:}"
    for t in $targets; do
        GOOS="${t%/*}"
        GOARCH="${t##*/}"
        ext=""
        [ "$GOOS" = "windows" ] && ext=".exe"
        out="./dist/${base}_${GOOS}_${GOARCH}${ext}"
        echo "===== Building $GOOS/$GOARCH -> $out ====="
        CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" \
            go build -trimpath -ldflags "-s -w" -o "$out" "$entry"
    done
done

echo ""
echo "All builds done! Output in ./dist"
