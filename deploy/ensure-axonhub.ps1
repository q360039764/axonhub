$ErrorActionPreference = 'Stop'

$BaseDir = Split-Path -Parent $PSCommandPath
$BinaryPath = Join-Path $BaseDir 'axonhub.exe'
$PidFile = Join-Path $BaseDir 'axonhub.pid'
$LogDir = Join-Path $BaseDir 'logs'
$StdoutFile = Join-Path $LogDir 'stdout.log'
$StderrFile = Join-Path $LogDir 'stderr.log'

function Get-AxonHubProcess {
    param(
        [string]$ExpectedBinaryPath
    )

    if (Test-Path -LiteralPath $PidFile) {
        $ExistingPid = Get-Content -LiteralPath $PidFile -ErrorAction SilentlyContinue
        if ($ExistingPid) {
            $ProcessFromPid = Get-Process -Id $ExistingPid -ErrorAction SilentlyContinue
            if ($ProcessFromPid) {
                return $ProcessFromPid
            }
        }
    }

    $ExpectedFullPath = [System.IO.Path]::GetFullPath($ExpectedBinaryPath)
    return Get-Process -Name 'axonhub' -ErrorAction SilentlyContinue |
        Where-Object {
            try {
                $_.Path -and ([System.IO.Path]::GetFullPath($_.Path) -ieq $ExpectedFullPath)
            } catch {
                $false
            }
        } |
        Select-Object -First 1
}

if (-not (Test-Path -LiteralPath $BinaryPath)) {
    throw "axonhub.exe not found: $BinaryPath"
}

New-Item -ItemType Directory -Path $LogDir -Force | Out-Null

$RunningProcess = Get-AxonHubProcess -ExpectedBinaryPath $BinaryPath
if ($RunningProcess) {
    $RunningProcess.Id | Set-Content -LiteralPath $PidFile -Encoding ASCII
    exit 0
}

Remove-Item -LiteralPath $PidFile -Force -ErrorAction SilentlyContinue

$Process = Start-Process `
    -FilePath $BinaryPath `
    -WorkingDirectory $BaseDir `
    -RedirectStandardOutput $StdoutFile `
    -RedirectStandardError $StderrFile `
    -PassThru `
    -WindowStyle Hidden

Start-Sleep -Seconds 2

if (-not (Get-Process -Id $Process.Id -ErrorAction SilentlyContinue)) {
    throw "AxonHub failed to start. Check $StdoutFile and $StderrFile"
}

$Process.Id | Set-Content -LiteralPath $PidFile -Encoding ASCII
