#!/bin/bash
# 构建 Virtualis 对接插件：全平台 × 全架构，打包成 Levis 支持的 ZIP 格式。
#
# 包结构（Levis install.go 的要求）：
#   virtualis/plugin            主程序二进制（各平台统一叫 plugin）
#   virtualis/frontend/index.html 插件管理页内嵌页面
#
# 产物：dist/virtualis-<os>-<arch>.zip
set -euo pipefail

cd "$(dirname "$0")"

ID="virtualis"
PLATFORMS=(
  linux/amd64
  linux/arm64
  darwin/amd64
  darwin/arm64
)

# go.mod 用 replace 指向 ../levis，跨机器构建时若没有同级 levis 目录会失败。
if [ ! -d "../levis" ]; then
  echo "错误：需要同级目录存在 levis 仓库（go.mod 的 replace 指向 ../levis）" >&2
  exit 1
fi

rm -rf dist
mkdir -p dist

for platform in "${PLATFORMS[@]}"; do
  os="${platform%%/*}"
  arch="${platform##*/}"

  echo "构建 ${os}/${arch} ..."
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -o "dist/.build/plugin" -ldflags='-s -w' .

  stage="dist/.build/${ID}"
  rm -rf "$stage"
  mkdir -p "$stage/frontend"
  cp "dist/.build/plugin" "$stage/plugin"
  cp frontend/index.html "$stage/frontend/index.html"

  zip="dist/${ID}-${os}-${arch}.zip"
  (cd "dist/.build" && zip -qr "../$(basename "$zip")" "$ID")
  rm -rf "$stage" "dist/.build/plugin"
  echo "  -> $zip"
done

rm -rf dist/.build
ls -lh dist/
echo "完成。"
