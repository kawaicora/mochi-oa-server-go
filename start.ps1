# mochi-oa-server-go 一键启动（Windows 宿主机 / Docker Desktop）
# 用法：仓库根目录执行  ./start.ps1
# 流程：1) build.ps1 交叉编译 -> 2) 停止现有容器 -> 3) docker compose up -d -> 4) 跟随日志
$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot
Set-Location $root

function Invoke-Native([string]$cmdline) {
  # docker 会把进度写到 stderr，PowerShell 5.1 会把 native stderr 当致命错误中断脚本。
  # 用 cmd /c 合并 stderr 到 stdout，再用 $LASTEXITCODE 判断真实失败。
  & cmd /c "$cmdline 2>&1"
  return $LASTEXITCODE
}

Write-Host ''
Write-Host '===== [1/4] build.ps1 （交叉编译，产出 dist\mochi-oa-server_linux_amd64） ====='
& (Join-Path $root 'build.ps1')
if ($LASTEXITCODE -ne 0) { Write-Host '[ERR] build.ps1 失败'; exit $LASTEXITCODE }

$bin = Join-Path $root 'dist\mochi-oa-server_linux_amd64'
if (-not (Test-Path $bin)) { Write-Host "[ERR] 找不到 $bin"; exit 1 }
Write-Host ("  ok: {0} ({1:N1} MB)" -f $bin, ((Get-Item $bin).Length / 1MB))

Write-Host ''
Write-Host '===== [2/4] 停止现有容器 ====='
# 本 compose 的容器
[void](Invoke-Native 'docker compose stop')
[void](Invoke-Native 'docker compose rm -f')
# 旧版命名容器（c54f8e4 后缀那套，以及老 Node 服务）
foreach ($c in @('mochi-oa-server-go-app-c54f8e4', 'mochi-oa-server-go-mysql-c54f8e4', 'mochi-oa-server-2f8e9c')) {
  & cmd /c "docker rm -f $c 2>&1" | Out-Null
}

Write-Host ''
Write-Host '===== [3/4] docker compose up -d ====='
$code = Invoke-Native 'docker compose up -d'
if ($code -ne 0) { Write-Host '[ERR] docker compose up 失败'; exit $code }

Write-Host ''
Write-Host '===== 容器状态 ====='
[void](Invoke-Native 'docker compose ps')

Write-Host ''
Write-Host '===== [4/4] 跟随日志（Ctrl+C 退出，不影响容器） ====='
& cmd /c "docker compose logs -f app 2>&1"
