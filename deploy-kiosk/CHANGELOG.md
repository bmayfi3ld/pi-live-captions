# Kiosk package changelog

The topmost version heading is the source of truth for the kiosk package
version. Cutting a kiosk release means adding a new `## X.Y.Z` heading here.

## 0.1.2

- Disable Light Locker autostart for the kiosk user on installation and upgrade.
- Disable Xfce idle blanking, DPMS, inactivity sleep and locking on suspend on AC and battery.
- Reboot after upgrading to apply the settings to the desktop session.

## 0.1.1

- Fix kiosk startup on Xfce by only applying GNOME settings when their schemas are installed.

## 0.1.0

- Create a dedicated unprivileged kiosk user.
- Configure automatic login with GDM or LightDM.
- Start Firefox in kiosk mode and keep the display awake.
