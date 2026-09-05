# Setting up a livecaption kiosk

**Target:** Debian Trixie or Raspberry Pi OS Trixie with a graphical desktop.
The current kiosk is a Chromebook running Debian; the package is architecture
independent so it can later be installed on a Raspberry Pi desktop image.

The package opens a terminal after graphical login while it waits for the site
to respond, then starts Firefox in kiosk mode, restarts it if it exits, disables
screen locking and sleep, and ignores the laptop lid. The physical power button retains its normal behavior.

## Install

Add the livecaption apt repository if it is not already configured:

```sh
sudo install -d -m 0755 /usr/share/keyrings
sudo apt install -y curl
curl -fsSL https://bmayfi3ld.github.io/pi-live-captions/livecaption-archive-keyring.asc | sudo tee /usr/share/keyrings/livecaption.asc >/dev/null

sudo tee /etc/apt/sources.list.d/livecaption.sources >/dev/null <<'EOF'
Types: deb
URIs: https://bmayfi3ld.github.io/pi-live-captions
Suites: ./
Components:
Signed-By: /usr/share/keyrings/livecaption.asc
EOF

sudo apt update
sudo apt install livecaption-kiosk
```

Set the page to display if the default is not suitable:

```sh
sudoedit /etc/default/livecaption-kiosk
```

Quote URLs containing shell characters such as `&`.

## Automatic login

The package creates an unprivileged `kiosk` desktop user without a password and
configures it to log in automatically. It detects the active display manager
from `/etc/X11/default-display-manager` and supports:

- **GDM**, used by Debian's default GNOME desktop.
- **LightDM**, normally used by Raspberry Pi OS and commonly used with Xfce.

The desktop environment and display manager are separate, so an Xfce install
can use a different display manager. Check the selected one with:

```sh
cat /etc/X11/default-display-manager
```

If installation reports an unsupported display manager, configure that manager
to log in as `kiosk` automatically. Otherwise, reboot after installation.

## Verify before leaving the device unattended

1. Reboot and confirm the user logs in without input.
2. With the caption service unavailable, confirm the terminal says it is waiting.
3. Confirm Firefox opens `http://livecaptions.local?wake=0` in kiosk mode without a **Tap to start** prompt once the service is available.
4. Close Firefox and confirm it returns after about two seconds.
5. Leave the machine idle and confirm the display remains on.
6. Close and reopen the lid and confirm the kiosk remains available.
7. Disconnect and restore the network and confirm the page reconnects.
8. Pull and restore power and confirm the kiosk returns automatically.

The package deliberately does not disable the power button or install a
desktop environment. Firefox receives security updates through apt.

## Removal

```sh
sudo apt remove livecaption-kiosk
```

The Firefox profile in `~/.mozilla/livecaption-kiosk` belongs to the kiosk user
and is left in place.
