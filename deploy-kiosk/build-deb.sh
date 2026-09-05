#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"

VERSION=${1:-"$(deploy-kiosk/version.sh)~dev"}
if [ -z "${MAINTAINER:-}" ]; then
	name=$(git config user.name || true)
	email=$(git config user.email || true)
	MAINTAINER="${name:-unknown} <${email:-nobody@invalid}>"
fi
REPO=${REPO:-bmayfi3ld/pi-live-captions}
OUT=${OUT:-"$ROOT/dist"}
STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT

install -d "$STAGE/DEBIAN" "$STAGE/etc/default" \
	"$STAGE/etc/systemd/sleep.conf.d" "$STAGE/etc/systemd/logind.conf.d" \
	"$STAGE/etc/xdg/autostart" "$STAGE/etc/firefox/policies" \
	"$STAGE/usr/bin" "$STAGE/usr/share/doc/livecaption-kiosk"
install -m 0644 deploy-kiosk/default "$STAGE/etc/default/livecaption-kiosk"
install -m 0755 deploy-kiosk/files/livecaption-kiosk "$STAGE/usr/bin/"
install -m 0644 deploy-kiosk/files/livecaption-kiosk.desktop "$STAGE/etc/xdg/autostart/"
install -m 0644 deploy-kiosk/files/sleep.conf "$STAGE/etc/systemd/sleep.conf.d/livecaption-kiosk.conf"
install -m 0644 deploy-kiosk/files/logind.conf "$STAGE/etc/systemd/logind.conf.d/livecaption-kiosk.conf"
install -m 0644 deploy-kiosk/files/policies.json "$STAGE/etc/firefox/policies/policies.json"
install -m 0644 deploy-kiosk/README.md "$STAGE/usr/share/doc/livecaption-kiosk/README.md"
install -m 0644 deploy-kiosk/debian/conffiles "$STAGE/DEBIAN/conffiles"
install -m 0755 deploy-kiosk/debian/postinst "$STAGE/DEBIAN/"
sed -e "s|@VERSION@|$VERSION|" -e "s|@MAINTAINER@|$MAINTAINER|" -e "s|@REPO@|$REPO|" \
	deploy-kiosk/debian/control.in > "$STAGE/DEBIAN/control"
mkdir -p "$OUT"
DEB="$OUT/livecaption-kiosk_${VERSION}_all.deb"
dpkg-deb --root-owner-group --build "$STAGE" "$DEB" >/dev/null
echo "$DEB"
