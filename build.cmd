@echo off
rem Build Linux/macOS amd64/arm64 Levis installer ZIPs.
rem Requires Go, Python 3.8+, and the sibling ..\levis SDK checkout.
setlocal
cd /d "%~dp0"
if not exist "..\levis\go.mod" (
  echo ERROR: sibling levis checkout is required.
  exit /b 1
)
python --version >nul 2>&1
if errorlevel 1 (
  echo ERROR: Python 3.8+ is required for portable ZIP permissions.
  exit /b 1
)
if exist dist rmdir /s /q dist
mkdir dist
if errorlevel 1 exit /b 1
set "STAGE=dist\.stage\virtualis"
for %%P in (linux-amd64 linux-arm64 darwin-amd64 darwin-arm64) do (
  for /f "tokens=1,2 delims=-" %%A in ("%%P") do (
    call :build %%A %%B
    if errorlevel 1 goto :failed
  )
)
rmdir /s /q dist\.stage
echo Build complete: dist\virtualis-OS-ARCH.zip
endlocal
exit /b 0

:build
set "GOOS=%1"
set "GOARCH=%2"
set "CGO_ENABLED=0"
echo Building %1/%2 ...
if exist "%STAGE%" rmdir /s /q "%STAGE%"
mkdir "%STAGE%\frontend"
if errorlevel 1 exit /b 1
call go build -trimpath -ldflags "-s -w" -o "%STAGE%\plugin" .
if errorlevel 1 exit /b 1
copy /y "frontend\index.html" "%STAGE%\frontend\index.html" >nul
if errorlevel 1 exit /b 1
python scripts\package.py "%STAGE%" "dist\virtualis-%1-%2.zip"
if errorlevel 1 exit /b 1
echo   -^> dist\virtualis-%1-%2.zip
exit /b 0

:failed
echo ERROR: build failed; no success is reported for incomplete packages.
if exist dist\.stage rmdir /s /q dist\.stage
endlocal
exit /b 1
