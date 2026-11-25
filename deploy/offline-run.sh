#!/bin/bash
set -euo pipefail

for tool in mktemp tail sha256sum dd mkdir chmod mv; do
    command -v "$tool" >/dev/null || { printf '缺少系统命令 %s\n' "$tool" >&2; exit 1; }
done

work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
archive=${BASH_SOURCE[0]}
header_size=$((10#@HEADER_BYTES@))
actual=$(tail -c "+$((header_size + 1))" -- "$archive" | sha256sum)
[[ ${actual%% *} == '@PAYLOAD_SHA256@' ]] ||
    { printf '安装包校验失败，请重新复制文件。\n' >&2; exit 1; }

package=$work/netpatrol-kylin-v10-x86_64
unpack() {
    local file=$package/$1
    mkdir -p -- "${file%/*}"
    dd if="$archive" of="$file" bs=64K iflag=skip_bytes,count_bytes \
        skip="$((header_size + $2))" count="$3" status=none
    chmod "$4" -- "$file"
}
@FILES@

if [[ ${1-} == --extract ]]; then
    [[ $# -eq 2 && ! -e $2 && ! -L $2 ]] ||
        { printf '用法  bash 安装包.run --extract 新目录\n' >&2; exit 1; }
    mv -T -- "$package" "$2"
else
    bash "$package/install.sh" "$@"
fi
exit
