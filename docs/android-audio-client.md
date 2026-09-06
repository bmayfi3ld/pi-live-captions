# Setting up an Android audio client

A runbook for turning a dedicated Pixel phone into a room-audio receiver. It uses
Termux and its command-line `mpv` package because a shell loop can retry forever;
Android media-player frontends commonly return to their menu after a long outage.

**Target:** a supported Pixel running current stock Android, connected to power and
the same trusted network as the livecaption server.

## 1. Check the stream

Open <http://livecaptions.local/audio.mp3> in a browser once to confirm the
phone can reach the server over mDNS.

## 2. Install Termux

Install [Termux](https://f-droid.org/packages/com.termux/) from F-Droid. The
Google Play build is an experimental branch with missing functionality.

Open Termux and install `mpv`:

```sh
pkg update
pkg install mpv
command -v mpv
mpv --version
```

`command -v mpv` must print a path under
`/data/data/com.termux/files/usr/bin/`.

## 3. Install the reconnect loop

Paste this whole block into Termux:

```sh
cat > ~/livecaption-audio <<'EOF'
#!/bin/bash

URL=http://livecaptions.local/audio.mp3

termux-wake-lock
trap 'termux-wake-unlock' EXIT
trap 'exit 0' INT TERM

while true; do
    mpv --no-video \
        --stream-lavf-o=reconnect=1,reconnect_at_eof=1,reconnect_streamed=1,reconnect_on_network_error=1,reconnect_delay_max=5 \
        "$URL"
    echo "Stream unavailable; retrying in 5 seconds"
    sleep 5
done
EOF
```

The outer loop is intentional. The FFmpeg options handle interruptions while
`mpv` is running; the loop also reopens the URL after a complete server outage
or an initial connection failure.

Start it:

```sh
bash ~/livecaption-audio
```

Press `Ctrl+C` to stop it.

## 4. Make Android leave it running

On the Pixel:

1. Allow notifications for Termux.
2. In **Settings → Apps → Termux → App battery usage**, select **Unrestricted**.
   Menu wording varies slightly by Android release.
3. Keep the phone powered and do not swipe Termux away from the recent-apps
   screen.

The script takes a wake lock while running. After rebooting the phone, open
Termux and start the script again with `bash ~/livecaption-audio`.

## 5. Verify before deployment

1. Start the livecaption server and confirm audio plays.
2. Stop the server for at least two minutes. The client should remain in its
   retry loop rather than returning to a menu.
3. Start the server and confirm audio resumes without touching the phone.
4. Turn Wi-Fi off for two minutes, restore it, and confirm playback resumes.
5. Lock the screen for at least ten minutes and confirm playback continues.
6. Disconnect and restore power; confirm the phone remains connected and does
   not enter battery-saver mode.

## Troubleshooting

Check that the endpoint is reachable without starting playback:

```sh
curl -I http://livecaptions.local/audio.mp3
```

A working endpoint returns `HTTP/1.1 200 OK` and `Content-Type: audio/mpeg`.
If it does not, check the Wi-Fi network, mDNS resolution, and whether the
server's audio stream is enabled.

If a command reports `No such file or directory`, confirm the player and script
exist:

```sh
command -v bash mpv
ls -l ~/livecaption-audio
```

Inspect a permanently powered phone periodically for heat or battery swelling.