#!/usr/bin/env bash
# Build one .deb for one architecture.
#
# Deliberately dpkg-deb over debhelper: this is a single static binary and a
# handful of text files, which does not justify the full Debian toolchain.
#
#   ./deploy/build-deb.sh amd64            # version from CHANGELOG.md, ~dev
#   ./deploy/build-deb.sh arm64 0.2.0+42   # explicit version (CI)
set -euo pipefail

ARCH=${1:?usage: build-deb.sh <amd64|arm64> [version]}
case "$ARCH" in
amd64 | arm64) ;;
*)
	echo "unsupported architecture: $ARCH (want amd64 or arm64)" >&2
	exit 1
	;;
esac

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"

VERSION=${2:-"$("$ROOT/deploy/version.sh")~dev"}
# `|| true` matters: git config exits non-zero when unset, which under set -e
# would abort the build with no message at all on a machine (or a CI runner)
# that has no git identity configured.
if [ -z "${MAINTAINER:-}" ]; then
	name=$(git config user.name || true)
	email=$(git config user.email || true)
	MAINTAINER="${name:-unknown} <${email:-nobody@invalid}>"
fi
REPO=${REPO:-bmayfi3ld/pi-live-captions}
OUT=${OUT:-"$ROOT/dist"}

STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT

install -d "$STAGE/DEBIAN" \
	"$STAGE/usr/bin" \
	"$STAGE/usr/lib/systemd/system" \
	"$STAGE/usr/share/livecaption" \
	"$STAGE/usr/share/doc/livecaption" \
	"$STAGE/etc/default"

# CGO_ENABLED=0 keeps this a pure cross-compile: no toolchain needed to build
# the arm64 package on an amd64 box. Debian's arch names match GOARCH for both
# targets, so ARCH is used directly.
echo "building livecaption $VERSION for $ARCH"
CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath \
	-ldflags "-s -w -X livecaption/internal/cli.Version=$VERSION" \
	-o "$STAGE/usr/bin/livecaption" ./cmd/livecaption

install -m 0644 deploy/livecaption.service "$STAGE/usr/lib/systemd/system/"
install -m 0644 deploy/default "$STAGE/etc/default/livecaption"
# Staged in /usr/share rather than /etc: postinst copies it to
# /etc/livecaption/secrets.env only if that does not already exist, so an
# upgrade never touches configured keys.
install -m 0644 deploy/secrets.env.example "$STAGE/usr/share/livecaption/"
install -m 0644 deploy/README.md "$STAGE/usr/share/doc/livecaption/"

# Debian expects a changelog in its own format at this exact name. Rather than
# translate CHANGELOG.md into it, ship one entry per release pointing at the
# real file, which is installed alongside and is what people actually read.
DOC="$STAGE/usr/share/doc/livecaption"
install -m 0644 CHANGELOG.md "$DOC/CHANGELOG.md"
cat >"$DOC/changelog" <<EOF
livecaption ($VERSION) stable; urgency=medium

  * See /usr/share/doc/livecaption/CHANGELOG.md for what changed.

 -- $MAINTAINER  $(date -R)
EOF
gzip -9n "$DOC/changelog"

# Policy requires a copyright file; the licence is the project's own.
{
	echo "Upstream-Name: livecaption"
	echo "Source: https://github.com/$REPO"
	echo
	cat LICENSE
} >"$DOC/copyright"
chmod 0644 "$DOC/copyright"

install -m 0644 deploy/debian/conffiles "$STAGE/DEBIAN/conffiles"
for script in postinst prerm postrm; do
	install -m 0755 "deploy/debian/$script" "$STAGE/DEBIAN/$script"
done

sed -e "s|@VERSION@|$VERSION|" \
	-e "s|@ARCH@|$ARCH|" \
	-e "s|@MAINTAINER@|$MAINTAINER|" \
	-e "s|@REPO@|$REPO|" \
	deploy/debian/control.in >"$STAGE/DEBIAN/control"

mkdir -p "$OUT"
DEB="$OUT/livecaption_${VERSION}_${ARCH}.deb"
# --root-owner-group so the package does not carry the builder's uid.
dpkg-deb --root-owner-group --build "$STAGE" "$DEB" >/dev/null
echo "$DEB"
