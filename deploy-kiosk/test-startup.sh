#!/bin/sh
# Exercise only GNOME configuration, never launch the kiosk or browser.
set -eu
cd "$(dirname "$0")"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
awk '/^# GNOME/{copy=1} /^# X11/{exit} copy' files/livecaption-kiosk >"$tmp/settings.sh"
cat >"$tmp/gsettings" <<'EOF'
#!/bin/sh
set -eu
if [ "$1" = list-schemas ]; then
	printf '%s\n' "$SCHEMAS"
else
	printf '%s\n' "$SCHEMAS" | grep -qx "$2"
	printf '%s\n' "$*" >>"$CALLS"
fi
EOF
chmod +x "$tmp/gsettings"
export PATH="$tmp:$PATH" CALLS="$tmp/calls" SCHEMAS
for desktop in empty xfce gnome; do
	SCHEMAS=''
	if [ "$desktop" != empty ]; then
		SCHEMAS='org.gnome.desktop.session
org.gnome.desktop.screensaver'
	fi
	if [ "$desktop" = gnome ]; then
		SCHEMAS="$SCHEMAS
org.gnome.settings-daemon.plugins.power"
	fi
	: >"$CALLS"
	sh -eu "$tmp/settings.sh"
	case "$desktop" in
		empty) expected=0 ;;
		xfce) expected=2 ;;
		gnome) expected=4 ;;
	esac
	test "$(wc -l <"$CALLS")" -eq "$expected"
done
printf 'Startup schema checks passed.\n'
