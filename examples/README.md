# Demo audio

[`demo.mp3`](demo.mp3) is a 65-second edited compilation: 10 seconds of instrumental
music, followed by 55 seconds of clean speech. The music does not overlap the narration.

Use `examples/demo.mp3` as the file argument to `livecaption replay`. Mock mode produces
canned captions, not the words in the recording. Use Speechmatics to try music detection;
that behaviour has not yet been verified with this sample.

## Sources and permissions

### Music: “Menu Music” by mrpoly

- Source: https://opengameart.org/content/menu-music
- Audio: https://opengameart.org/sites/default/files/awesomeness.wav
- Source excerpt: 00:00–00:10.
- The upload is marked [CC0](https://creativecommons.org/publicdomain/zero/1.0/).
- Provenance caveat: the author says the composition combines several GarageBand/Apple
  Loops, rather than redistributing a single loop. Apple permits distributing compositions
  made with its loops, but not redistributing the loops themselves as standalone samples.
  The uploader's CC0 designation does not remove underlying third-party rights. See the
  source-page discussion and [Apple's usage guidance](https://support.apple.com/en-us/102034).

### Speech: *Moby-Dick*, Chapter 1, by Herman Melville

- Read by **Stewart Wills** for LibriVox.
- Source: https://librivox.org/moby-dick-by-herman-melville
- Audio: https://archive.org/download/moby_dick_librivox/mobydick_001_002_melville.mp3
- Source excerpt: 00:28.900–01:23.900, from the combined Chapters 1–2 recording.
- Starts “Call me Ishmael”; ends “…as soon as I can.” The LibriVox introduction,
  book/chapter announcements, and separate Etymology and Extracts section are omitted.
- LibriVox identifies its recordings as public domain in the USA; check local copyright
  rules for use elsewhere. See [LibriVox's public-domain guidance](https://librivox.org/pages/public-domain/).

## Edits

Both excerpts were converted to mono and independently loudness-normalized to a target
of −18 LUFS, with a −2 dBTP ceiling. The music fades out over its final second; short
edge fades avoid clicks. They were concatenated without overlap and encoded as a
44.1 kHz, 96 kbps MP3. Source timestamps were selected using offline speech recognition;
the finished clip still needs a listening check.

The source media retain their own rights/status; the repository's code licence does not
relicense them.
