param(
    [string]$OutputDir,
    [string]$SourcePath,
    [string]$WebView2Version = '1.0.4022.49',
    [ValidateSet('x64', 'x86', 'arm64')]
    [string]$Platform = $(if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } elseif ([Environment]::Is64BitOperatingSystem) { 'x64' } else { 'x86' })
)

$ErrorActionPreference = 'Stop'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

function Write-Info([string]$message) { Write-Host "[INFO] $message" -ForegroundColor Cyan }
function Write-Success([string]$message) { Write-Host "[SUCCESS] $message" -ForegroundColor Green }
function Write-Err([string]$message) { Write-Host "[ERROR] $message" -ForegroundColor Red }

function Get-CscPath {
    $candidates = @(
        (Join-Path $env:WINDIR 'Microsoft.NET\Framework64\v4.0.30319\csc.exe'),
        (Join-Path $env:WINDIR 'Microsoft.NET\Framework\v4.0.30319\csc.exe')
    )

    foreach ($candidate in $candidates) {
        if (Test-Path -LiteralPath $candidate) {
            return $candidate
        }
    }

    throw 'csc.exe not found. Please install .NET Framework 4.x developer tools or run on a standard Windows system.'
}

function Expand-NuGetPackage([string]$PackagePath, [string]$Destination) {
    if (Test-Path -LiteralPath (Join-Path $Destination 'lib\net462\Microsoft.Web.WebView2.WinForms.dll')) {
        return
    }

    if (Test-Path -LiteralPath $Destination) {
        Remove-Item -LiteralPath $Destination -Recurse -Force
    }

    New-Item -ItemType Directory -Path $Destination -Force | Out-Null
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    [System.IO.Compression.ZipFile]::ExtractToDirectory($PackagePath, $Destination)
}

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Definition
if (-not $OutputDir) {
    $OutputDir = $ScriptDir
}

if (-not $SourcePath) {
    $SourcePath = Join-Path $ScriptDir 'desktop\AxonHubDesktop.cs'
}

if (-not (Test-Path -LiteralPath $SourcePath)) {
    Write-Err "Desktop source file not found: $SourcePath"
    exit 1
}

New-Item -ItemType Directory -Path $OutputDir -Force | Out-Null

$BuildRoot = Join-Path (Split-Path -Parent $ScriptDir) '.builds\desktop'
New-Item -ItemType Directory -Path $BuildRoot -Force | Out-Null

$PackageDir = Join-Path $BuildRoot "microsoft.web.webview2.$WebView2Version"
$PackagePath = Join-Path $BuildRoot "microsoft.web.webview2.$WebView2Version.nupkg"
$PackageUrl = "https://api.nuget.org/v3-flatcontainer/microsoft.web.webview2/$WebView2Version/microsoft.web.webview2.$WebView2Version.nupkg"

if (-not (Test-Path -LiteralPath $PackagePath)) {
    Write-Info "Downloading WebView2 SDK $WebView2Version..."
    Invoke-WebRequest -Uri $PackageUrl -OutFile $PackagePath -UseBasicParsing
}

Write-Info 'Preparing WebView2 SDK files...'
Expand-NuGetPackage -PackagePath $PackagePath -Destination $PackageDir

$LibDir = Join-Path $PackageDir 'lib\net462'
$NativeDir = Join-Path $PackageDir "runtimes\win-$Platform\native"
$CoreDll = Join-Path $LibDir 'Microsoft.Web.WebView2.Core.dll'
$WinFormsDll = Join-Path $LibDir 'Microsoft.Web.WebView2.WinForms.dll'
$LoaderDll = Join-Path $NativeDir 'WebView2Loader.dll'

foreach ($required in @($CoreDll, $WinFormsDll, $LoaderDll)) {
    if (-not (Test-Path -LiteralPath $required)) {
        Write-Err "Required WebView2 file not found: $required"
        exit 1
    }
}

$CscPath = Get-CscPath
$OutputExe = Join-Path $OutputDir 'AxonHubDesktop.exe'
$CompilerPlatform = if ($Platform -eq 'arm64') { 'anycpu' } else { $Platform }

Write-Info "Building AxonHubDesktop.exe..."
$CompilerArgs = @(
    '/nologo',
    '/target:winexe',
    # 使用 UTF-8 读取桌面 C# 源码，避免中文菜单和提示被系统代码页误解码。
    '/codepage:utf8',
    "/platform:$CompilerPlatform",
    '/optimize+',
    '/reference:System.dll',
    '/reference:System.Core.dll',
    '/reference:System.Drawing.dll',
    '/reference:System.Windows.Forms.dll',
    "/reference:$CoreDll",
    "/reference:$WinFormsDll",
    "/out:$OutputExe",
    $SourcePath
)

& $CscPath @CompilerArgs
if ($LASTEXITCODE -ne 0) {
    exit $LASTEXITCODE
}

# WebView2 managed DLLs and native loader must stay next to the desktop entry.
Copy-Item -LiteralPath $CoreDll -Destination (Join-Path $OutputDir 'Microsoft.Web.WebView2.Core.dll') -Force
Copy-Item -LiteralPath $WinFormsDll -Destination (Join-Path $OutputDir 'Microsoft.Web.WebView2.WinForms.dll') -Force
Copy-Item -LiteralPath $LoaderDll -Destination (Join-Path $OutputDir 'WebView2Loader.dll') -Force

Write-Success "Desktop app built: $OutputExe"
