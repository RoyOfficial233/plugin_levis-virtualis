@echo off
rem 构建 Virtualis 对接插件：全平台 x 全架构，打包成 Levis 支持的 ZIP 格式。
rem
rem 包结构（Levis install.go 的要求）：
rem   virtualis\plugin                 主程序二进制（linux/darwin 目标统一叫 plugin）
rem   virtualis\frontend\index.html    插件管理页内嵌页面
rem
rem 产物：dist\virtualis-<os>-<arch>.zip
rem
rem go.mod 用 replace 指向 ..\levis，构建需要同级目录存在 levis 仓库。
setlocal enabledelayedexpansion
cd /d "%~dp0"

set ID=virtualis

if not exist "..\levis" (
  echo 错误：需要同级目录存在 levis 仓库（go.mod 的 replace 指向 ..\levis）
  exit /b 1
)

if exist dist rmdir /s /q dist
mkdir dist
set STAGE=dist\.stage

for %%P in (linux-amd64 linux-arm64 darwin-amd64 darwin-arm64) do (
  for /f "tokens=1,2 delims=-" %%A in ("%%P") do (
    set OS=%%A
    set ARCH=%%B
    echo 构建 %%A/%%B ...

    if exist "%STAGE%" rmdir /s /q "%STAGE%"
    mkdir "%STAGE%\%ID%\frontend"

    set CGO_ENABLED=0
    set GOOS=%%A
    set GOARCH=%%B
    go build -trimpath -ldflags "-s -w" -o "%STAGE%\%ID%\plugin" .

    copy /y "frontend\index.html" "%STAGE%\%ID%\frontend\index.html" >nul

    tar -a -cf "dist\%ID%-!OS!-!ARCH!.zip" -C "%STAGE%" "%ID%"
    if errorlevel 1 (
      echo tar 打包失败，回退 PowerShell Compress-Archive ...
      powershell -NoProfile -Command "Compress-Archive -Path '%STAGE%\%ID%' -DestinationPath 'dist\%ID%-!OS!-!ARCH!.zip' -Force"
    )
    echo   -^> dist\%ID%-!OS!-!ARCH!.zip
  )
)

rmdir /s /q "%STAGE%"
dir dist
echo 完成。
endlocal
