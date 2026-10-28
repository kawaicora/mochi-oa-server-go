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

# 需要编译的程序：入口文件 -> 输出名基
$programs = @(
    @(".\cmd\server\main.go", "mochi-oa-server"),
    @(".\cmd\tools\database_update\main.go", "mochi-oa-database_update")
)

foreach ($p in $programs) {
    $entry = $p[0]
    $outBase = $p[1]
    foreach ($t in $targets) {
        $GOOS = $t[0]
        $GOARCH = $t[1]
        $ext = $t[2]
        $outFile = "./dist/${outBase}_$GOOS`_$GOARCH$ext"

        Write-Host "`n===== Building $GOOS/$GOARCH -> $outFile ====="
        $env:CGO_ENABLED="0"
        $env:GOOS=$GOOS
        $env:GOARCH=$GOARCH

        go build -trimpath -ldflags "-s -w" -o $outFile $entry

        if ($LASTEXITCODE -ne 0) {
            Write-Error "Build failed for $GOOS/$GOARCH ($entry)"
            exit $LASTEXITCODE
        }
    }
}

Write-Host "`n✅ All builds done! Output in ./dist"
