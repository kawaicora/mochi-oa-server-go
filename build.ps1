# 清理旧产物
Remove-Item -Recurse -Force ./dist -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force ./dist | Out-Null

# 定义编译目标数组
$targets = @(
    # GOOS, GOARCH, 输出文件名后缀
    @("windows", "amd64", ".exe"),
    @("windows", "arm64", ".exe"),
    @("linux", "amd64", ""),
    @("linux", "arm64", ""),
    @("darwin", "amd64", ""),
    @("darwin", "arm64", "")
)

foreach ($t in $targets) {
    $GOOS = $t[0]
    $GOARCH = $t[1]
    $ext = $t[2]
    $outFile = "./dist/mochi-oa-server_$GOOS`_$GOARCH$ext"

    Write-Host "`n===== Building $GOOS/$GOARCH -> $outFile ====="
    $env:CGO_ENABLED="0"
    $env:GOOS=$GOOS
    $env:GOARCH=$GOARCH

    go build -o $outFile .\cmd\server\main.go

    if ($LASTEXITCODE -ne 0) {
        Write-Error "Build failed for $GOOS/$GOARCH"
        exit $LASTEXITCODE
    }
}

Write-Host "`n✅ All builds done! Output in ./dist"
