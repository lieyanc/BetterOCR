#!/usr/bin/env bash
# Verify a staged release directory before publishing.
#
# 期望的资产清单直接从 cross-compile.yml 的构建矩阵推导,避免"矩阵改了、
# 校验清单没改"这类漂移;资产命名协议本身由 updater 包的
# TestReleaseAssetNamesMatchCIMatrix 与 targetName() 对表。
#
# 用法: .github/scripts/verify-release-assets.sh <release-dir>
set -euo pipefail

release_dir="${1:-release}"
workflow="$(dirname "$0")/../workflows/cross-compile.yml"

if [ ! -d "$release_dir" ]; then
  echo "release directory ${release_dir} does not exist" >&2
  exit 1
fi
if [ ! -f "$workflow" ]; then
  echo "cannot read build matrix from ${workflow}" >&2
  exit 1
fi

# 每个矩阵条目产出 "target" 与紧随其后的 "ext",按出现顺序配对成资产后缀。
mapfile -t suffixes < <(awk '
  /^[[:space:]]*-[[:space:]]*target:[[:space:]]*/ { target = $NF; next }
  /^[[:space:]]*ext:[[:space:]]*/ {
    if (target != "") { ext = $NF; gsub(/"/, "", ext); print target ext; target = "" }
  }
' "$workflow")

if [ "${#suffixes[@]}" -eq 0 ]; then
  echo "no build targets parsed from ${workflow}" >&2
  exit 1
fi

echo "expecting ${#suffixes[@]} platform binaries from the build matrix"
missing=0
for suffix in "${suffixes[@]}"; do
  binary="betterocr-${suffix}"
  for file in "${binary}" "${binary}.sha256"; do
    if [ ! -f "${release_dir}/${file}" ]; then
      echo "missing ${release_dir}/${file}" >&2
      missing=1
    fi
  done
  # 校验和必须真的对得上:被截断的 artifact 绝不能发出去。
  if [ -f "${release_dir}/${binary}" ] && [ -f "${release_dir}/${binary}.sha256" ]; then
    if ! (cd "${release_dir}" && sha256sum --status -c "${binary}.sha256"); then
      echo "sha256 mismatch for ${release_dir}/${binary}" >&2
      missing=1
    fi
  fi
done

# 两个通道的发现入口都是 version.json,漏了它客户端永远看不到新版本。
if [ ! -f "${release_dir}/version.json" ]; then
  echo "missing ${release_dir}/version.json" >&2
  missing=1
fi

if [ "$missing" -ne 0 ]; then
  echo "release asset verification failed" >&2
  exit 1
fi
echo "release assets verified"
