#!/usr/bin/env bash
# chatglm-cli 独立构建脚本：不依赖工作区根脚本，克隆本仓后 `bash build.sh` 即可产出二进制。
# 产物：本仓 dist/chatglm-cli.exe（ww3 渠道插件 glm）+ dist/chatglm-cli（Linux，便利命令用）
# 用法：bash build.sh [--clean] [--debug] [--zip]
#   --clean  只清本仓 dist/ 里的产物（不动运行数据目录）
#   --debug  保留符号表/DWARF 与本机路径，便于本地排障
#   --zip    再打一个插件包 dist/<id>-<version>.zip（包根 = exe + manifest.json）
set -euo pipefail
cd "$(dirname "$0")"

EXE="chatglm-cli.exe"     # ww3 交付物名（历史名 glm2cli.exe 已废，见 README）
PKG="./cmd/glm2cli"
OUT="$PWD/dist"
BUILD_FLAGS=(-trimpath "-ldflags=-s -w")   # -trimpath：不把本机路径烧进公开产物
DO_ZIP=0

for a in "$@"; do
  case "$a" in
    --clean)
      for f in "$OUT"/*.exe "$OUT"/*.zip "$OUT"/*.sha256 "$OUT"/manifest.json "$OUT"/chatglm-cli; do
        if [ -e "$f" ]; then
          rm -f "$f" 2>/dev/null || echo "[clean] 进程占用，跳过：$(basename "$f")"
        fi
      done
      rm -rf "$OUT/.data" ;;
    --debug) BUILD_FLAGS=() ;;
    --zip)   DO_ZIP=1 ;;
    *) echo "未知参数: $a（可用 --clean / --debug / --zip）" >&2; exit 2 ;;
  esac
done

mkdir -p "$OUT"
echo "[build] chatglm-cli -> dist/$EXE  flags=${BUILD_FLAGS[*]:-none}"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build "${BUILD_FLAGS[@]}" -o "$OUT/$EXE" "$PKG"
# Linux 产物供 CLI 直用（插件树只取 .exe，故不参与 layout）
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build "${BUILD_FLAGS[@]}" -o "$OUT/chatglm-cli" "$PKG"

# manifest 由刚产出的 exe 自己吐：保证包内清单与二进制同源，不手抄、不在仓里另存一份
if [ "$DO_ZIP" = 1 ]; then
  # 临时数据根：插件 `--data` 缺省是 exe 所在目录，不掺会把 logs/ 落在 dist/ 里
  WW3_DATA="$OUT/.data" "$OUT/$EXE" manifest --out "$OUT/manifest.json"
  rm -rf "$OUT/.data"
  id=$(sed -n 's/.*"id":[[:space:]]*"\([^"]*\)".*/\1/p' "$OUT/manifest.json" | head -1)
  ver=$(sed -n 's/.*"version":[[:space:]]*"\([^"]*\)".*/\1/p' "$OUT/manifest.json" | head -1)
  if [ -z "$id" ] || [ -z "$ver" ]; then
    echo "[zip] manifest 缺 id/version" >&2; exit 1
  fi
  pkgname="$id-$ver.zip"
  ZIPPER=$(command -v zip || true)
  [ -n "$ZIPPER" ] || ZIPPER=$(command -v bsdtar || true)
  if [ -z "$ZIPPER" ]; then
    for t in /c/Windows/System32/tar.exe "$SYSTEMROOT/System32/tar.exe"; do
      [ -f "$t" ] && ZIPPER="$t" && break
    done
  fi
  if [ -z "$ZIPPER" ]; then
    echo "[zip] 找不到 zip / bsdtar / tar.exe，无法打插件包" >&2; exit 1
  fi
  rm -f "$OUT/$pkgname"
  case "$ZIPPER" in
    */zip) ( cd "$OUT" && "$ZIPPER" -q "$pkgname" "$EXE" manifest.json ) ;;
    *)     ( cd "$OUT" && "$ZIPPER" -a -cf "$pkgname" "$EXE" manifest.json ) ;;
  esac
  if command -v sha256sum >/dev/null; then
    ( cd "$OUT" && sha256sum "$pkgname" | tee "$pkgname.sha256" )
  fi
  echo "[zip] dist/$pkgname"
fi

ls -l --block-size=1 "$OUT" | awk 'NR>1 && $NF ~ /\.(exe|zip)$/ {printf "  %-34s %8.2f MB\n", $NF, $5/1048576}'
