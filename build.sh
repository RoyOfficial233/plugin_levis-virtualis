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

# Python's standard ZIP writer works in Git Bash too and explicitly records
# executable permissions required by the Levis installer on Linux/macOS.
if command -v python3 >/dev/null 2>&1; then
  PYTHON=python3
elif command -v python >/dev/null 2>&1; then
  PYTHON=python
else
  printf 'ERROR: Python 3.8+ is required for release packaging.\n' >&2
  exit 1
fi

OUT_DIR="${PLUGIN_DIST_DIR:-dist}"
[[ "$OUT_DIR" != / && "$OUT_DIR" != . && "$OUT_DIR" != .. ]] || exit 1
mkdir -p "$OUT_DIR/.build"
"$PYTHON" .github/scripts/plugin_inputs.py "$OUT_DIR/build-inputs.json"
if [[ -n "${PLUGIN_PLATFORMS:-}" ]]; then read -r -a PLATFORMS <<< "$PLUGIN_PLATFORMS"; fi

for platform in "${PLATFORMS[@]}"; do
  os="${platform%%/*}"
  arch="${platform##*/}"

  echo "构建 ${os}/${arch} ..."
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -mod=readonly -trimpath -o "$OUT_DIR/.build/plugin" -ldflags='-s -w' .

  stage="$OUT_DIR/.build/${ID}"
  rm -rf "$stage"
  mkdir -p "$stage/frontend"
  cp "$OUT_DIR/.build/plugin" "$stage/plugin"
  cp frontend/index.html "$stage/frontend/index.html"

  zip="$OUT_DIR/${ID}-${os}-${arch}.zip"
  "$PYTHON" scripts/package.py "$stage" "$zip"
  rm -rf "$stage" "$OUT_DIR/.build/plugin"
  echo "  -> $zip"
done

"$PYTHON" .github/scripts/plugin_inputs.py "$OUT_DIR/build-inputs.json" --verify
rm -rf "$OUT_DIR/.build"
printf '完成：dist/virtualis-<os>-<arch>.zip\n'
