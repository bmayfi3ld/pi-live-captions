# Setting up a livecaption box

A start-to-finish runbook for turning a bare machine into a caption appliance:
plugged into the soundboard, running headless, serving
<http://livecaptions.local> to every phone and screen in the room.

Everything here is a command you run yourself. There is no setup script on
purpose — you should be able to read what is about to happen to the box. The
fiddly parts (creating the service user, installing the unit, permissions) live
in the Debian package instead, and step 5 says exactly what it does.

**Target:** Debian Trixie or Raspberry Pi OS Trixie, 64-bit.
The primary box is a **Chromebox CN60** (amd64). A **Raspberry Pi 4/5** (arm64)
is supported by the package but **has never been run in production** — see
[Known unknowns](#known-unknowns).

About fifteen commands, twenty minutes plus OS install time.

---

## 1. Install the OS

### Chromebox CN60 (primary)

The CN60 ships with ChromeOS and firmware write protection on. Getting Debian
onto it is a one-time job:

1. Power off, open the case, remove the **write-protect screw** from the
   mainboard, reassemble.
2. Enter Developer Mode, then enable legacy/UEFI boot in the firmware.
3. Install **Debian Trixie** from the netinst image. At the tasksel prompt
   deselect the desktop environment and select **SSH server** and **standard
   system utilities** only. A desktop wastes disk on a 16 GB M.2 and pulls in a
   display manager the box will never use.

If the box is already running Debian, skip all of this.

### Raspberry Pi 4/5 (unproven)

Flash **Raspberry Pi OS Trixie, 64-bit Lite** with Raspberry Pi Imager. In the
advanced options set the hostname, enable SSH, and create your user before
writing the card — that avoids ever needing a keyboard and monitor on the Pi.

### Then, on either

```sh
ssh <user>@<hostname>.local
sudo apt update && sudo apt full-upgrade
```

## 2. Wifi that a stranger can reconfigure — comitup

Skip this entirely if the box will be on wired ethernet. **The CN60 has no
built-in wifi**, so it also needs a USB wifi adapter whose driver supports AP
mode.

comitup solves the venue problem: the box arrives somewhere new, cannot see any
network it knows, and there is no keyboard. So it raises its own access point.
You join that AP from a phone, a captive portal lists the venue's networks, you
pick one and enter the password, and the box joins it and remembers it.

> **This step can drop the SSH session you are running it over.** comitup takes
> over network management. Do it over ethernet, or at a directly attached
> keyboard and monitor.

```sh
# comitup is not in Debian; it ships its own repository.
sudo apt install -y curl
curl -fsSL https://davesteele.github.io/key-arch.pub \
  | sudo gpg --dearmor -o /usr/share/keyrings/davesteele.gpg
echo "deb [signed-by=/usr/share/keyrings/davesteele.gpg] https://davesteele.github.io/comitup/repo comitup main" \
  | sudo tee /etc/apt/sources.list.d/comitup.list
sudo apt update
sudo apt install -y comitup
```

Set the AP name so the operator knows which network is the box:

```sh
sudoedit /etc/comitup.conf     # ap_name: livecaptions-<nnn>
sudo reboot
```

comitup requires NetworkManager and conflicts with `dhcpcd` and
`wpa_supplicant` managing the same interface. Its installer usually sorts this
out; if wifi misbehaves afterwards, check `systemctl status comitup` and follow
[comitup's own documentation](https://davesteele.github.io/comitup/) — that is
its problem to solve, not this project's.

## 3. Add the livecaption package repository

```sh
sudo install -d -m 0755 /usr/share/keyrings
curl -fsSL https://<owner>.github.io/pi-caption-stream/livecaption-archive-keyring.asc \
  | sudo tee /usr/share/keyrings/livecaption.asc >/dev/null

sudo tee /etc/apt/sources.list.d/livecaption.sources >/dev/null <<'EOF'
Types: deb
URIs: https://<owner>.github.io/pi-caption-stream
Suites: stable
Components: main
Signed-By: /usr/share/keyrings/livecaption.asc
EOF

sudo apt update
```

Replace `<owner>` with the GitHub account hosting the repo. The `.sources`
(deb822) format is Trixie's default; the `Signed-By` line means this key can
only ever validate this one repository.

## 4. Install

```sh
sudo apt install livecaption
```

That pulls in `ffmpeg` (audio capture and encoding) and `avahi-daemon` plus
`avahi-utils` (the `.local` name), then:

- creates a `livecaption` system user, in the `audio` group so it can read
  `/dev/snd`;
- installs `/usr/lib/systemd/system/livecaption.service` and **enables** it, so
  the box starts captioning on boot with no login;
- installs `/etc/default/livecaption` (settings) and `/etc/livecaption/secrets.env`
  (API key, admin password, mode `0640`);
- **does not start the service** — it cannot work until you do steps 5 and 6.

## 5. Find the audio device

```sh
sudo -u livecaption livecaption devices
```

Run it as the service user, not as yourself: if this cannot see the device,
neither can the service.

Pick the entry for the soundboard's USB output. **Prefer the
`plughw:CARD=<name>,DEV=0` form over `hw:1,0`** — USB card numbers change
between boots depending on enumeration order, and a box that captions silence
after a power cycle because the card became `hw:2` is a bad night. Card names
are stable. `arecord -l` shows the card names if the listing is ambiguous.

## 6. Configure

```sh
sudoedit /etc/default/livecaption
```

Every setting is a command-line flag, uppercased with a `LIVECAPTION_` prefix
(`LIVECAPTION_ADDR` is `--addr`), so `livecaption live --help` is the complete
reference. At minimum set `LIVECAPTION_DEVICE` to what step 5 found.

```sh
sudoedit /etc/livecaption/secrets.env
```

Set `DEEPGRAM_API_KEY`. These are in a separate, mode-`0640` file because they
are secrets, and they have no command-line flags at all — a key passed as an
argument shows up in `ps` and shell history for every user on the box.

`ADMIN_PASSWORD` is optional. Set it to enable the "Clear screen" control on
`/admin` and require basic auth (username `admin`) to reach that page. Left
unset, the control stays greyed out and `POST /api/clear` returns 503 — a valid
choice for a box nobody needs to touch mid-event, not a fault.

### Optional: a logo

Shown in the viewer's top-right corner.

```sh
scp logo.png <user>@<hostname>.local:/tmp/
sudo install -o livecaption -g livecaption -m 0644 /tmp/logo.png /var/lib/livecaption/
```

Then uncomment `LIVECAPTION_LOGO` in `/etc/default/livecaption`. PNG, JPEG,
WebP, GIF or SVG, **2 MiB maximum**. It is read once at startup, so replacing
the file needs a `systemctl restart livecaption`. Viewers who would rather not
see it can add `?logo=0` to the URL.

### Optional: keyterms

Proper nouns to bias recognition toward — names, places, anything the engine
would otherwise mangle.

```sh
scp keyterms-esv.txt <user>@<hostname>.local:/tmp/
sudo install -m 0644 /tmp/keyterms-esv.txt /etc/livecaption/keyterms.txt
```

Then uncomment `LIVECAPTION_KEYTERM_FILE`. One term per line; blank lines and
`#` comments ignored. A list longer than the engine accepts is **cut from the
end**, so put the terms most likely to be spoken first.

> Both of these settings point at a file that must exist. A path that is wrong
> stops the service from starting.

## 7. Start it

```sh
sudo systemctl start livecaption
systemctl status livecaption
journalctl -u livecaption -f
```

## 8. Verify

- `curl -f http://localhost/healthz` → `ok`
- Open `http://livecaptions.local` from a phone on the same network. Talk into
  the soundboard; captions appear.
- `http://livecaptions.local/audio.mp3` plays the source audio in VLC or mpv.
- `http://livecaptions.local/admin` shows metrics.
- Transcripts accumulate under `/var/lib/livecaption/transcripts/<timestamp>/`.
- **Pull the power, plug it back in, and confirm captions return with no
  login.** This is the one that matters; it is how the box gets used.

Viewers can tune their own screen with query parameters: `?lines=N`, `?size=N`,
`?theme=light`, `?logo=0`, `?wake=0`.

## 9. Appliance housekeeping

The box runs for hours unattended and loses power without warning. Two settings
worth making before it goes to a venue:

```sh
# The service restarts forever and logs as JSON to the journal. Uncapped, that
# is a slow disk-fill on the CN60's ~10 GB of free space.
sudo sed -i 's/^#\?SystemMaxUse=.*/SystemMaxUse=200M/' /etc/systemd/journald.conf
sudo systemctl restart systemd-journald

# The apt cache is the other thing that quietly grows.
sudo apt clean
```

Transcripts themselves are not the risk — roughly 100 KB per hour.

**Known gaps to be aware of, not yet fixed** (see
`specs/2026-09-01_hardening_transcript_durability.md`): transcript writes are
flushed but not `fsync`ed, so pulling the power can cost up to ~30 seconds of
the tail; and if the disk fills, the health indicator returns to normal after a
minute even though every line is still being dropped.

## 10. Upgrading

```sh
sudo apt update && sudo apt upgrade
```

The service restarts itself. `/etc/default/livecaption` is a conffile, so your
edits survive; if a release changes the shipped defaults, apt will ask which
version to keep and show you the difference. `/etc/livecaption/secrets.env` is
never touched by an upgrade.

To pin or roll back:

```sh
apt list -a livecaption                  # available versions
sudo apt install livecaption=0.2.0+41    # a specific one
```

Versions look like `0.2.0+42`: the release from `CHANGELOG.md` plus the build
number, which always increases.

Removing the package leaves transcripts and secrets in place. `sudo apt purge
livecaption` deletes them along with the service user.

## Troubleshooting

| Symptom | Cause and fix |
| --- | --- |
| `permission denied` binding `:80` | The unit grants `AmbientCapabilities=CAP_NET_BIND_SERVICE`; check it survived an override with `systemctl cat livecaption`. **Do not** run `setcap` on `/usr/bin/livecaption` — apt replaces the binary on every upgrade and the capability silently disappears. (The `setcap` advice in the project's `justfile` is for running a locally built binary by hand, where there is no systemd to grant anything.) |
| `device not found` after a reboot | The USB card was renumbered. Use the `plughw:CARD=<name>,DEV=0` form (step 5). |
| A setting seems to do nothing | A misspelled `LIVECAPTION_*` name is silently ignored — nothing validates env var names. Check it against `livecaption live --help`, then confirm what actually took effect in the startup banner: `journalctl -u livecaption -b`. |
| `livecaptions.local` does not resolve | Is `avahi-daemon` running? Is the viewer on the same subnet — mDNS does not cross most VLAN or guest-network boundaries? Some Android versions still resolve `.local` poorly; hand out the IP address instead. |
| No audio at `/audio.mp3` | The installed ffmpeg has no `libmp3lame`. The service logs `audio disabled` at startup and carries on serving captions. |
| Service starts, captions never appear | Wrong capture device (step 5), or silence: `--auto-pause` disconnects the recognizer after 60 seconds of quiet and reconnects on sound, which is normal and shows in the log. Confirm audio is actually arriving with `LIVECAPTION_LOG_LEVEL=debug`. |
| Won't start after editing config | Usually `LIVECAPTION_LOGO` or `LIVECAPTION_KEYTERM_FILE` pointing at a file that is not there. `journalctl -u livecaption -n 20` names it. |

## Known unknowns

Untested on real venue hardware. Tick these off as they are confirmed, and
correct this document when reality disagrees:

- [ ] Boot-start after a cold power cycle, and after a *pulled-power* cycle
- [ ] USB capture from the soundboard through ALSA, over a multi-hour service
- [ ] `avahi-publish` running as the non-root `livecaption` user
- [ ] `.local` resolution from phones on venue wifi
- [ ] comitup's AP appearing when no known network is present, without
      fighting avahi
- [ ] CPU and memory headroom on the real box — run `scripts/pi-headroom.sh 3h`
      (its per-model factors are Pi-specific; on the CN60 the numbers it
      reports are a direct measurement rather than a projection)
- [ ] `apt upgrade` restarting cleanly and preserving config edits
- [ ] Anything at all on a Raspberry Pi

## For maintainers

Building the package and running the apt repository: [`apt-repo.md`](apt-repo.md).
