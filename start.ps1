$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot
Set-Location $root


 Remove-Item Env:HOST -ErrorAction SilentlyContinue
 Remove-Item Env:PORT -ErrorAction SilentlyContinue
# 封装调用外部命令，返回退出码，同时输出stdout/stderr
function Invoke-Native([string]$cmdline) {
    Write-Host "> $cmdline"
    & cmd /c "$cmdline 2>&1"
    return $LASTEXITCODE
}

Write-Host ''
Write-Host '===== [0/4] clear env =====' -ForegroundColor Cyan

Remove-Item Env:HOST -ErrorAction SilentlyContinue
Remove-Item Env:PORT -ErrorAction SilentlyContinue
Remove-Item Env:SERVER_PORT -ErrorAction SilentlyContinue
Remove-Item Env:TRANSPORTS -ErrorAction SilentlyContinue
Remove-Item Env:TRUST_PROXY -ErrorAction SilentlyContinue
Remove-Item Env:CORS_ORIGIN -ErrorAction SilentlyContinue

Remove-Item Env:SCOKET_PATH -ErrorAction SilentlyContinue
Remove-Item Env:STORAGE -ErrorAction SilentlyContinue

Remove-Item Env:MYSQL_HOST -ErrorAction SilentlyContinue
Remove-Item Env:MYSQL_PORT -ErrorAction SilentlyContinue

Remove-Item Env:MYSQL_USER -ErrorAction SilentlyContinue
Remove-Item Env:MYSQL_PASSWORD -ErrorAction SilentlyContinue

Remove-Item Env:MYSQL_DATABASE -ErrorAction SilentlyContinue
Remove-Item Env:AUTO_CREATE_DATABASE -ErrorAction SilentlyContinue

Remove-Item Env:AUTO_MIGRATE -ErrorAction SilentlyContinue
Remove-Item Env:JWT_SECRET -ErrorAction SilentlyContinue
Remove-Item Env:JWT_EXPIRES_IN -ErrorAction SilentlyContinue
Remove-Item Env:ADMIN_USER -ErrorAction SilentlyContinue
Remove-Item Env:ADMIN_PASSWORD -ErrorAction SilentlyContinue
Remove-Item Env:SERVER_ADMIN_USERS -ErrorAction SilentlyContinue


Write-Host ''
Write-Host '===== [1/4] build.ps1 build dist =====' -ForegroundColor Cyan
& (Join-Path $root 'build.ps1')
if ($LASTEXITCODE -ne 0) {
    Write-Host '[ERR] build.ps1 fail' -ForegroundColor Red
    exit $LASTEXITCODE
}

$bin = Join-Path $root 'dist\mochi-oa-server_linux_amd64'
if (-not (Test-Path $bin)) {
    Write-Host "[ERR] can't find $bin" -ForegroundColor Red
    exit 1
}
$fileSizeMB = (Get-Item $bin).Length / 1MB
Write-Host ("  ok: {0} ({1:N1} MB)" -f $bin, $fileSizeMB) -ForegroundColor Green

Write-Host ''
Write-Host '===== [2/4] stop current container =====' -ForegroundColor Cyan
# 捕获返回码，失败直接退出，而不是[void]丢弃
$ret = Invoke-Native 'docker-compose stop'
if ($ret -ne 0) {
    Write-Host "[WARN] docker compose stop return $ret, continue clean" -ForegroundColor Yellow
}
$ret = Invoke-Native 'docker-compose rm -f'
if ($ret -ne 0) {
    Write-Host "[WARN] docker compose rm -f return $ret, continue clean" -ForegroundColor Yellow
}

# ✅ 改用 compose 批量清理本项目容器，不再硬编码容器名！
# 只清理当前compose项目下的容器，不会误删其他项目
$ret = Invoke-Native 'docker container rm -f $(docker compose ps -aq) 2>nul'
# 忽略这个命令的错误：没有容器时 docker rm 会报错，属于正常场景

Write-Host ''
Write-Host '===== [3/4] docker compose up -d --build =====' -ForegroundColor Cyan
docker-compose up -d --build
$code = $LASTEXITCODE
if ($code -ne 0) {
    Write-Host '[ERR] docker compose up fail' -ForegroundColor Red
    exit $code
}
Write-Host '[OK] docker compose started' -ForegroundColor Green

Write-Host ''
Write-Host '===== container status =====' -ForegroundColor Cyan
Invoke-Native 'docker compose ps' | Out-Null

Write-Host ''
Write-Host '===== [4/4] logs Ctrl+C exit =====' -ForegroundColor Cyan
# 不需要套 cmd /c，powershell原生直接执行logs -f
docker-compose logs -f mochi-oa-server-go-app-c54f8e4
