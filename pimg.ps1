<#
.SYNOPSIS
    pimg 本地开发服务管理脚本。

.DESCRIPTION
    后端：本机默认使用本地存储，配置位于根目录 config.yaml，
          监听 127.0.0.1:8790（与 vite 代理一致），数据落在 data/。
    前端：vite dev server，端口 5174，/api 代理到后端；
          后端二进制本身也内嵌了前端构建产物，加 -NoFrontend 可只跑后端。

    进程为隐藏窗口后台运行，PID 记录在 .pimg-dev.pids，日志写在 logs/。
    停止时只终止可执行文件位于本项目目录内的进程，不会误杀其他项目占端口的进程。

.EXAMPLE
    .\pimg.ps1 start                    启动后端 + 前端
    .\pimg.ps1 start -NoFrontend        只启动后端（用内嵌前端）
    .\pimg.ps1 start -BackendPort 8791  临时换后端端口（注意需同步改 vite 代理）
    .\pimg.ps1 stop                     停止本项目进程
    .\pimg.ps1 restart                  重启
    .\pimg.ps1 status                   查看运行状态
    .\pimg.ps1 log backend              查看后端日志尾部（frontend 同理）
#>
param(
    [Parameter(Position = 0)][ValidateSet('start', 'stop', 'restart', 'status', 'log')][string]$Command = 'status',
    [Parameter(Position = 1)][string]$Target = '',
    [int]$BackendPort = 8790,
    [int]$FrontendPort = 5174,
    [switch]$NoFrontend,
    [switch]$MockStorage
)

$ErrorActionPreference = 'Stop'
$Root        = $PSScriptRoot

# 用户可见错误：红字提示后退出（避免 throw 打印堆栈噪音）
function Fail([string]$Message) {
    Write-Host $Message -ForegroundColor Red
    exit 1
}
$PidFile     = Join-Path $Root '.pimg-dev.pids'
$LogDir      = Join-Path $Root 'logs'
$BackendExe  = Join-Path $Root 'pimg-dev.exe'
$FrontendDir = Join-Path $Root 'frontend'
$DataDir     = Join-Path $Root 'data'

function Get-ListenerPid([int]$Port) {
    $conns = Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue
    if ($conns) { return @($conns | Select-Object -ExpandProperty OwningProcess -Unique) }
    return @()
}

# 取进程的可执行文件路径与命令行（用于判断归属）
function Get-ProcIdentity([int]$ProcId) {
    return Get-CimInstance Win32_Process -Filter "ProcessId=$ProcId" -ErrorAction SilentlyContinue
}

# 归属判断：可执行文件路径或命令行包含本项目目录，才认为是 pimg 自己的进程
function Test-OwnProcess([int]$ProcId) {
    $p = Get-ProcIdentity $ProcId
    if (-not $p) { return $false }
    $rootLower = $Root.ToLower()
    if ($p.ExecutablePath -and $p.ExecutablePath.ToLower().StartsWith($rootLower)) { return $true }
    if ($p.CommandLine -and $p.CommandLine.ToLower().Contains($rootLower)) { return $true }
    return $false
}

function Stop-PidTree([int]$ProcId) {
    if ($ProcId -le 0 -or $ProcId -eq $PID) { return }
    $proc = Get-Process -Id $ProcId -ErrorAction SilentlyContinue
    if ($proc) {
        Write-Host ("  停止 {0} (PID {1})" -f $proc.ProcessName, $ProcId)
        taskkill /PID $ProcId /T /F | Out-Null
    }
}

function Read-SavedPids {
    $saved = @()
    if (Test-Path $PidFile) {
        foreach ($line in Get-Content $PidFile) {
            $v = ($line -split '=', 2)[1]
            if ($v -match '^\d+$') { $saved += [int]$v }
        }
    }
    return $saved
}

# 端口被占时的提示：本项目残留 → 提示 stop；其他项目 → 指名道姓，绝不动手
function Assert-PortFree([int]$Port, [string]$Role) {
    $owners = Get-ListenerPid $Port
    if (-not $owners) { return }
    $p = Get-ProcIdentity $owners[0]
    $name = if ($p) { "$($p.Name) (PID $($owners[0]))" } else { "PID $($owners[0])" }
    Fail "端口 $Port（$Role）已被占用：$name`n若是本项目残留请先 .\pimg.ps1 stop；若是其他项目请先关掉它，或换端口后重试。"
}

function Start-Pimg {
    Assert-PortFree $BackendPort '后端'
    if (-not $NoFrontend) { Assert-PortFree $FrontendPort '前端' }
    New-Item -ItemType Directory -Force -Path $LogDir, $DataDir | Out-Null

    # --- 后端：优先编译最新代码 ---
    $exe = $BackendExe
    $go = Get-Command go -ErrorAction SilentlyContinue
    if ($go) {
        Write-Host '编译后端（串行编译包、限制构建内存）...'
        Push-Location $Root
        $savedBuildEnvironment = @{}
        foreach ($buildVariable in @('GOMAXPROCS', 'GOMEMLIMIT', 'GOEXPERIMENT')) {
            $savedBuildEnvironment[$buildVariable] = [Environment]::GetEnvironmentVariable($buildVariable, 'Process')
        }
        try {
            # Keep compiler/linker commit peaks bounded on Windows. These
            # values apply only to build children, not the running server.
            $env:GOMAXPROCS = '2'
            $env:GOMEMLIMIT = '512MiB'
            # Green Tea's span queue can dereference a failed OS allocation.
            # Use Go's supported opt-out without changing the installed SDK.
            # Explicit operator experiment selections remain authoritative.
            $experiments = @($env:GOEXPERIMENT -split ',' | Where-Object { $_ })
            if ($experiments -notcontains 'greenteagc' -and $experiments -notcontains 'nogreenteagc') {
                $experiments += 'nogreenteagc'
            }
            $env:GOEXPERIMENT = $experiments -join ','
            go build -p 1 -o $exe ./cmd/pimg
            if ($LASTEXITCODE -ne 0) { Fail 'go build 失败' }
        } finally {
            foreach ($buildVariable in $savedBuildEnvironment.Keys) {
                if ($null -eq $savedBuildEnvironment[$buildVariable]) {
                    # PowerShell may convert null to an empty string in the
                    # .NET call; remove the entry to restore an unset value.
                    Remove-Item -LiteralPath "Env:$buildVariable" -ErrorAction SilentlyContinue
                } else {
                    [Environment]::SetEnvironmentVariable($buildVariable, $savedBuildEnvironment[$buildVariable], 'Process')
                }
            }
            Pop-Location
        }
    } elseif (Test-Path (Join-Path $Root 'pimg.exe')) {
        $exe = Join-Path $Root 'pimg.exe'
        Write-Warning '未找到 go 工具链，改用现有 pimg.exe（可能不是最新代码）'
    } else {
        Fail '未找到 go 工具链，也没有可用的 pimg.exe'
    }

    $env:PIMG_LISTEN   = "127.0.0.1:$BackendPort"
    # Child processes run from the project root; keep configured paths portable.
    $env:PIMG_DATA_DIR = './data'
    $env:PIMG_RUNTIME_DIR = '.'
    $env:PIMG_DEV_MOCK = if ($MockStorage) { '1' } else { '0' }
    if ($MockStorage) {
        $env:PIMG_COLD_STORAGE = 'telegram'
        $env:PIMG_HOT_STORAGE = 'r2'
    }
    # Otherwise use config.yaml/defaults, retaining explicit environment overrides.
    $env:PIMG_PUBLIC_BASE_URL = "http://127.0.0.1:$BackendPort"
    if (-not $env:PIMG_ADMIN_PASSWORD) { $env:PIMG_ADMIN_PASSWORD = 'admin123' }

    $backend = Start-Process -FilePath $exe -WorkingDirectory $Root -WindowStyle Hidden -PassThru `
        -RedirectStandardOutput (Join-Path $LogDir 'backend.out.log') `
        -RedirectStandardError  (Join-Path $LogDir 'backend.err.log')
    Write-Host ("后端已启动 (PID {0})，等待 healthz ..." -f $backend.Id)

    $healthy = $false
    foreach ($i in 1..60) {
        if ($backend.HasExited) {
            Fail ("后端进程异常退出（码 {0}），详见 logs\backend.err.log" -f $backend.ExitCode)
        }
        try {
            Invoke-WebRequest "http://127.0.0.1:$BackendPort/healthz" -UseBasicParsing -TimeoutSec 2 | Out-Null
            $healthy = $true; break
        } catch { Start-Sleep -Milliseconds 500 }
    }
    if (-not $healthy) { Stop-PidTree $backend.Id; Fail "后端 30 秒内未就绪（$BackendPort），详见 logs\backend.err.log" }
    Write-Host "后端就绪 http://127.0.0.1:$BackendPort/healthz"

    $frontend = $null
    if (-not $NoFrontend) {
        # --- 前端 ---
        $npm = Get-Command npm.cmd -ErrorAction SilentlyContinue
        if (-not $npm) { $npm = Get-Command npm -ErrorAction SilentlyContinue }
        if (-not $npm) { Stop-PidTree $backend.Id; Fail '未找到 npm，无法启动前端（或用 -NoFrontend 只跑后端）' }

        if (-not (Test-Path (Join-Path $FrontendDir 'node_modules'))) {
            Write-Host 'frontend/node_modules 不存在，先 npm install ...'
            Push-Location $FrontendDir
            try {
                npm install
                if ($LASTEXITCODE -ne 0) { Fail 'npm install 失败' }
            } finally { Pop-Location }
        }

        $frontend = Start-Process -FilePath $npm.Source -ArgumentList 'run', 'dev' `
            -WorkingDirectory $FrontendDir -WindowStyle Hidden -PassThru `
            -RedirectStandardOutput (Join-Path $LogDir 'frontend.out.log') `
            -RedirectStandardError  (Join-Path $LogDir 'frontend.err.log')
        Write-Host ("前端已启动 (PID {0})，等待 {1} 端口监听 ..." -f $frontend.Id, $FrontendPort)

        $frontOk = $false
        foreach ($i in 1..60) {
            if ($frontend.HasExited) {
                Fail ("前端进程异常退出（码 {0}），详见 logs\frontend.err.log" -f $frontend.ExitCode)
            }
            if (Get-ListenerPid $FrontendPort) { $frontOk = $true; break }
            Start-Sleep -Milliseconds 500
        }
        if (-not $frontOk) {
            Stop-PidTree $backend.Id; Stop-PidTree $frontend.Id
            Fail "前端 30 秒内未监听 $FrontendPort，详见 logs\frontend.out.log"
        }
    }

    if ($frontend) {
        Set-Content -Path $PidFile -Value ("backend={0}`nfrontend={1}" -f $backend.Id, $frontend.Id)
    } else {
        Set-Content -Path $PidFile -Value ("backend={0}" -f $backend.Id)
    }

    Write-Host ''
    Write-Host '全部就绪：' -ForegroundColor Green
    if ($frontend) { Write-Host "  前端     http://127.0.0.1:$FrontendPort  (vite 热更新)" }
    $storageMode = if ($MockStorage) { '远端内存模拟' } else { '使用本机配置，默认本地存储' }
    Write-Host "  后端     http://127.0.0.1:$BackendPort  ($storageMode，内嵌前端)"
    Write-Host '  日志     ./logs'
    Write-Host "  停止     .\pimg.ps1 stop"
}

function Stop-Pimg {
    $killed = $false
    foreach ($procId in @(Read-SavedPids)) {
        Stop-PidTree $procId
        $killed = $true
    }
    # 兜底：本项目残留进程仍占着端口（如 PID 文件丢失）才清；其他项目的进程只提示、不杀
    foreach ($port in @($BackendPort, $FrontendPort)) {
        foreach ($procId in (Get-ListenerPid $port)) {
            if (Test-OwnProcess $procId) {
                Stop-PidTree $procId
                $killed = $true
            } else {
                $p = Get-ProcIdentity $procId
                Write-Warning "端口 $port 被 $($p.Name) (PID $procId) 占用，但不是本项目进程，未处理。"
            }
        }
    }
    Remove-Item $PidFile -Force -ErrorAction SilentlyContinue

    foreach ($i in 1..30) {
        $busy = @()
        foreach ($port in @($BackendPort, $FrontendPort)) { $busy += Get-ListenerPid $port }
        if (-not ($busy | Where-Object { Test-OwnProcess $_ })) { break }
        Start-Sleep -Milliseconds 300
    }
    if ($killed) { Write-Host '已停止。' -ForegroundColor Green }
    else { Write-Host '本项目未在运行。' }
}

function Show-Status {
    foreach ($item in @(
            @{ Name = 'backend';  Port = $BackendPort;  Url = "http://127.0.0.1:$BackendPort" },
            @{ Name = 'frontend'; Port = $FrontendPort; Url = "http://127.0.0.1:$FrontendPort" }
        )) {
        $owners = Get-ListenerPid $item.Port
        if ($owners) {
            $proc = Get-Process -Id $owners[0] -ErrorAction SilentlyContinue
            $tag = ''
            if (-not (Test-OwnProcess $owners[0])) { $tag = '  [非本项目进程]' }
            Write-Host ("{0,-9} 端口占用  PID {1}  {2}  ({3}){4}" -f $item.Name, $owners[0], $item.Url, $proc.ProcessName, $tag) -ForegroundColor Yellow
        } else {
            Write-Host ("{0,-9} 已停止    {1}" -f $item.Name, $item.Url) -ForegroundColor DarkGray
        }
    }
    # 补充：按 PID 文件报告上次 start 的进程（换过端口时端口行看不到）
    $saved = Read-SavedPids
    if ($saved) {
        foreach ($line in Get-Content $PidFile) {
            $parts = $line -split '=', 2
            $p = Get-Process -Id $parts[1] -ErrorAction SilentlyContinue
            $state = if ($p) { "运行中 (PID $($p.Id), $($p.ProcessName))" } else { '已退出' }
            Write-Host ("pidfile   {0,-9} {1}" -f $parts[0], $state)
        }
    }
}

function Show-Log([string]$Which) {
    if ($Which -ne 'backend' -and $Which -ne 'frontend') {
        Fail '用法: .\pimg.ps1 log backend|frontend'
    }
    $found = $false
    foreach ($f in @((Join-Path $LogDir "$Which.out.log"), (Join-Path $LogDir "$Which.err.log"))) {
        if (Test-Path $f) {
            $found = $true
            $displayLog = Join-Path 'logs' (Split-Path $f -Leaf)
            Write-Host "`n===== $displayLog (尾部 80 行) =====" -ForegroundColor Cyan
            Get-Content $f -Tail 80
        }
    }
    if (-not $found) { Write-Host "暂无 $Which 日志（尚未启动过？）" }
}

switch ($Command) {
    'start'   { Start-Pimg }
    'stop'    { Stop-Pimg }
    'restart' { Stop-Pimg; Start-Pimg }
    'status'  { Show-Status }
    'log'     { Show-Log $Target }
}
