# CN60 internal-microphone debugging log

Live investigation of `user@live-caption-box.local`. This is an operational log of
changes made to the box while testing its internal microphone.

## Baseline — 2026-09-03

- Service: `livecaption.service` active, capturing `alsa:plughw:CARD=PCH,DEV=0`.
- Hardware: card `PCH`, capture device `ALC283 Analog` (the CN60 internal-mic
  path); no USB capture device is expected for this setup.
- `/audio.mp3` is valid 128 kb/s MP3 and capture frames advance with no restarts
  or xruns.
- A five-second `volumedetect` sample measured `mean_volume: -81.3 dB` and
  `max_volume: -67.4 dB`; it is effectively silence.
- `Capture` was enabled at 62% (12 dB). `Mic Boost` was 0/3.

## Test 1 — increase microphone boost

**Planned change:** set ALSA `Mic Boost` on card `PCH` from `0` to `3`, while
leaving the livecaption service running. Verify the setting and then sample the
existing `/audio.mp3` stream while someone speaks into the mic.

**Applied:** `amixer -c PCH set 'Mic Boost' 3`.

**Result:** both channels now report `Mic Boost: 3 [100%] [36.00dB]`.

### Measurement after Test 1

An eight-second MP3-stream sample taken after the speaking test measured
`mean_volume: -80.8 dB`, `max_volume: -67.4 dB`, effectively unchanged from the
baseline. The stream remains live and capture remains restart/xrun-free. The
Speechmatics connection was open after the test, but no settled segments were
received. Raising gain alone has not made the microphone audible in the stream.

## Test 2 — USB microphone capture

**Discovery:** a CUBILUX CB5 USB Audio device appeared as ALSA card `CB5`. Its
three capture devices map from the USB descriptors as follows:

| ALSA device | USB input terminal |
| --- | --- |
| `DEV=0` | Microphone |
| `DEV=1` | Line Connector |
| `DEV=2` | Personal Microphone |

The device's main Microphone input is enabled (`Mic`, control 0, 0 dB, on).

**Planned change:** change livecaption from the CN60 PCH input to
`plughw:CARD=CB5,DEV=0`, restart the service, and confirm the new source is
capturing before the speaking test.

**Result:** not applied. The SSH account can run non-privileged diagnostics but
passwordless `sudo` is unavailable, so it cannot edit `/etc/default/livecaption`
or restart the service. The configuration remains on `PCH`; no service change
was made.

### Test 2 applied and verified

The operator applied the configuration change and restarted the service. The
new session reports `alsa:plughw:CARD=CB5,DEV=0 (-> 16000 Hz mono)`; frames are
advancing, MP3 streaming is live, and CB5 `Mic`, control 0, remains enabled at
0 dB.

### Test 2 measurement

The USB microphone is audible but quiet. An eight-second MP3-stream sample
measured `mean_volume: -87.3 dB`, `max_volume: -74.7 dB`. CB5 microphone gain
is currently 23/39 (59%, 0 dB); the device permits up to 39/39. No further
setting has been changed yet.

## Potential future improvement — per-source input gain

The USB microphone is audible at a different level from the intended soundboard
monitor output. ALSA mixer controls can compensate per device, but that makes
an appliance's behavior depend on mutable system-wide mixer state.

Consider a persisted livecaption setting such as
`LIVECAPTION_INPUT_GAIN_DB=12` (`--input-gain-db` on the CLI). It should add a
fixed ffmpeg volume filter to **both** capture-derived outputs: the PCM stream
sent to the recognizer and the MP3 stream served at `/audio.mp3`. Applying it
only to the PCM output would make captions respond to boosted audio while
operator listeners still hear a quiet stream.

Keep this as a fixed per-source setting rather than enabling automatic gain
control by default. AGC can raise room/monitor noise during quiet passages and
makes the listener level unpredictable. Test the soundboard monitor output at
its normal operating level before choosing any default or deployment value.

## Test 3 — return to the CN60 internal microphone

**Planned change:** restore `LIVECAPTION_DEVICE` to
`plughw:CARD=PCH,DEV=0`, restart livecaption, and sample `/audio.mp3` while
speaking near the CN60 internal microphone.

### Test 3 applied and verified

The configuration was restored to `plughw:CARD=PCH,DEV=0` and livecaption was
restarted; the new session reports that source and capture frames are advancing.
The PCH driver reset `Mic Boost` to 0 when the device was reopened, so it was
reapplied with `amixer -c PCH set 'Mic Boost' 3`. Both channels now report
3/3 (36 dB). This confirms the mixer adjustment is not persistent across a
capture-service restart and should be handled explicitly if retained.

### Test 3 measurement

After the speaking test, an eight-second MP3-stream sample measured
`mean_volume: -80.8 dB`, `max_volume: -66.2 dB`. This is effectively the same
near-silence observed before the USB test, despite PCH Mic Boost at 3/3. Frames
continue without restarts/xruns; the recognizer connection opened but produced
no segments. The configured PCH capture device is therefore functioning as a
clocked audio source but is not receiving usable microphone signal.

### Test 3 hardware diagnosis

The PCH codec exposes only one analog capture PCM (`PCH`, `DEV=0`), so there is
no alternate internal ALSA capture device to select. Its codec describes the
input as `Mic at Ext Left`, and the read-only `Mic Jack` control reports `off`:
the external microphone jack is not detected as plugged in. This is hardware
state below livecaption, rather than a source-selection or ffmpeg issue.

After an unplug/replug test, `Mic Jack` still reported `off`; the codec did not
detect a connection-state change. A later cable insertion, despite an audible
pop, also left `Mic Jack` at `off`.

