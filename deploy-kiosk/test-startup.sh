#!/bin/sh
# Exercise desktop configuration with mocks, never launch the kiosk or browser.
set -eu
cd "$(dirname "$0")"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
awk '/^# GNOME/{copy=1} /^# Xfce/{exit} copy' files/livecaption-kiosk >"$tmp/settings.sh"
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
# Check Xfce settings on both the first and subsequent startup.
awk '/^# Xfce/{copy=1} /^# X11/{exit} copy' files/livecaption-kiosk >"$tmp/settings.sh"
cat >"$tmp/xfconf-query" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"$CALLS"
EOF
chmod +x "$tmp/xfconf-query"
cat >"$tmp/expected" <<'EOF'
-c xfce4-power-manager -p /xfce4-power-manager/blank-on-ac --create -t uint -s 0
-c xfce4-power-manager -p /xfce4-power-manager/inactivity-on-ac --create -t uint -s 0
-c xfce4-power-manager -p /xfce4-power-manager/blank-on-battery --create -t uint -s 0
-c xfce4-power-manager -p /xfce4-power-manager/inactivity-on-battery --create -t uint -s 0
-c xfce4-power-manager -p /xfce4-power-manager/dpms-enabled --create -t bool -s false
-c xfce4-power-manager -p /xfce4-power-manager/lock-screen-suspend-hibernate --create -t bool -s false
EOF
for run in 1 2; do
	: >"$CALLS"
	sh -eu "$tmp/settings.sh"
	diff -u "$tmp/expected" "$CALLS"
done

# Exercise the actual per-user autostart override without root or a real user.
awk '/# Disable Light Locker/{copy=1} /display_manager=/{exit} copy' debian/postinst >"$tmp/locker.sh"
export KIOSK_TEST_HOME="$tmp/kiosk"
cat >"$tmp/getent" <<'EOF'
#!/bin/sh
printf 'kiosk:x:1001:1001::%s:/bin/sh\n' "$KIOSK_TEST_HOME"
EOF
cat >"$tmp/id" <<'EOF'
#!/bin/sh
printf 'kiosk\n'
EOF
cat >"$tmp/install" <<'EOF'
#!/bin/sh
# Remove only the ownership options; use real mkdir for the directories.
shift 5
mkdir -p "$@"
EOF
cat >"$tmp/chown" <<'EOF'
#!/bin/sh
exit 0
EOF
chmod +x "$tmp/getent" "$tmp/id" "$tmp/install" "$tmp/chown"
for run in 1 2; do
	sh -eu "$tmp/locker.sh"
	grep -qx 'Hidden=true' "$KIOSK_TEST_HOME/.config/autostart/light-locker.desktop"
done
printf 'Desktop startup checks passed.\n'
