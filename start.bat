@echo off
chcp 65001 >nul 2>&1
cd /d "%~dp0"
if not exist "%~dp0umi_upload.exe" (
    echo [ERROR] umi_upload.exe not found in %~dp0
    pause
    exit /b 1
)
"%~dp0umi_upload.exe" %*
