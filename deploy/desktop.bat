@echo off
setlocal

REM AxonHub desktop window launcher.

set "SCRIPT_DIR=%~dp0"
set "DESKTOP_EXE=%SCRIPT_DIR%AxonHubDesktop.exe"
set "DESKTOP_BUILD_SCRIPT=%SCRIPT_DIR%build-desktop.ps1"

if not exist "%DESKTOP_EXE%" (
  if not exist "%DESKTOP_BUILD_SCRIPT%" (
    echo [ERROR] AxonHubDesktop.exe and build-desktop.ps1 were not found.
    exit /b 1
  )

  powershell -NoProfile -ExecutionPolicy Bypass -File "%DESKTOP_BUILD_SCRIPT%" -OutputDir "%SCRIPT_DIR%"
  if %ERRORLEVEL% NEQ 0 exit /b %ERRORLEVEL%
)

start "" "%DESKTOP_EXE%" %*
exit /b 0
