#!/usr/bin/env bash
# 兼容垫片：构建脚本已上移到仓根 build.sh（W3-41 要求每仓 `bash build.sh` 即可独立构建）。
# 保留本文件只为不打断既有调用习惯；参数原样透传。
set -euo pipefail
cd "$(dirname "$0")/.."
exec bash ./build.sh "$@"
