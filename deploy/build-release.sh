#!/usr/bin/env bash
# Build release assets on the native Linux runner for this architecture.
set -euo pipefail
export LC_ALL=C
version=${1:?expected vMAJOR.MINOR.PATCH}
date=${2:?expected YYYYMMDD}
[[ $version =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]
arch=$(dpkg --print-architecture)
case $arch in amd64|arm64) ;; *) exit 2 ;; esac
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
out=${3:?expected a new output directory}
mkdir -- "$out"
out=$(realpath "$out")
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
args=(--version "$version" --release-date "$date")
suffix="$date-$version"
archive="$out/netpatrol-linux-$arch-$suffix.tar.gz"
python3 deploy/package.py "${args[@]}" --arch "$arch" --output "$archive"
mkdir "$work/payload"
tar -xzf "$archive" -C "$work/payload" --strip-components=1
install -m755 "$work/payload/netpatrol" "$out/netpatrol-linux-$arch-$suffix"
"$work/payload/netpatrol" -h >/dev/null 2>&1

cp -R deploy/debian "$work/debian"
chmod 0755 "$work/debian/rules" "$work/debian/postinst"
cat >"$work/debian/changelog" <<EOF
netpatrol (${version#v}-1) trixie; urgency=medium

  * Package NetPatrol $version for Debian 13.

 -- Cyuniq <37724939+Cyuniq@users.noreply.github.com>  $(date -R)
EOF
deb="netpatrol-debian13-$arch-$suffix.deb"
docker run --rm -v "$work:/build" -w /build debian:13 bash -euc '
    apt-get update -qq
    apt-get install -qy --no-install-recommends debhelper build-essential >/dev/null
    mkdir -p /tmp/pkg/source
    cp -R /build/debian /build/payload /tmp/pkg/source/
    cd /tmp/pkg/source
    DEB_BUILD_OPTIONS=nostrip dpkg-buildpackage -b -us -uc
    cd /tmp/pkg
    dpkg -i ./netpatrol_*.deb
    test "$(dpkg-query -W -f="\${Architecture}" netpatrol)" = "$1"
    test "$(stat -c %a /etc/netpatrol/netpatrol.env)" = 600
    test -f /usr/lib/systemd/system/netpatrol.service
    getent passwd netpatrol >/dev/null
    /usr/bin/netpatrol -h >/dev/null 2>&1
    dpkg -r netpatrol
    test -f /etc/netpatrol/netpatrol.env
    mv ./netpatrol_*.deb /build/package.deb
' _ "$arch"
cp "$work/package.deb" "$out/$deb"

if [[ $arch == amd64 ]]; then
    kylin="$out/netpatrol-kylin-v10-x86_64-$suffix.run"
    windows="$out/netpatrol-windows-x64-$suffix.zip"
    python3 deploy/package.py "${args[@]}" --kylin-offline --output "$kylin"
    python3 deploy/package.py "${args[@]}" --windows-offline --output "$windows"
    bash "$kylin" --extract "$work/kylin"
    "$work/kylin/netpatrol" -h >/dev/null 2>&1
    unzip -tq "$windows"
fi
cd "$out"
for file in *.deb *.tar.gz "netpatrol-linux-$arch-$suffix"; do
    sha256sum "$file" >"$file.sha256"
done
sha256sum -c -- *.sha256
