# Maintaining the package and the apt repository

Operators do not need this file; [`README.md`](README.md) is the runbook for
setting up a box. This is the one-time maintainer setup and the mechanics
behind it.

## Building a package by hand

```sh
just deb amd64          # -> dist/livecaption_0.2.0+dev_amd64.deb
just deb arm64
```

Version comes from the first `## X.Y.Z` heading in `CHANGELOG.md`
(`deploy/version.sh`), with `+dev` appended for local builds. Pass an explicit
version as the second argument to override.

Cross-compiling needs no toolchain: the module is pure Go and builds with
`CGO_ENABLED=0`.

Inspect before trusting:

```sh
dpkg-deb -I dist/livecaption_*_amd64.deb    # control metadata
dpkg-deb -c  dist/livecaption_*_amd64.deb   # file list and permissions
lintian      dist/livecaption_*_amd64.deb   # policy check
```

## Releasing

Releases are cut by **editing `CHANGELOG.md`**. Add a `## X.Y.Z` heading above
the previous one, describe what changed, push to `main`. That is the whole
ceremony — there are no tags to remember creating.

CI compares that version against the newest `v*` tag it previously wrote:

- version moved forward → build both architectures, publish, tag `vX.Y.Z`
- version unchanged or lower → do nothing, and pass

The gate passes rather than fails when there is nothing to release, because
most pushes to `main` are not releases and a red X on every ordinary commit
teaches everyone to ignore CI. The comparison uses `dpkg --compare-versions`,
so a changelog edit that accidentally *lowers* the version is caught too —
that would otherwise publish a package apt refuses to install as a downgrade.

Published versions are `X.Y.Z+<build number>`, e.g. `0.2.0+42`. The build
number always increases, so re-running a failed publish produces a strictly
newer package instead of colliding with a half-published one. The commit sha
is in the GitHub release notes.

## One-time: the signing key

apt will not use an unsigned repository. Generate a dedicated key — not a
personal one:

```sh
gpg --quick-generate-key "livecaption archive <you@example.com>" rsa4096 sign never
gpg --list-secret-keys --keyid-format=long      # note the key id
```

Export the **public** key into the repo so the runbook's `curl` can fetch it:

```sh
gpg --armor --export <KEY_ID> > deploy/livecaption-archive-keyring.asc
```

Export the **private** key and load it into GitHub repository secrets
(Settings → Secrets and variables → Actions):

```sh
gpg --armor --export-secret-keys <KEY_ID>       # -> secret APT_GPG_KEY
```

| Secret | Value |
| --- | --- |
| `APT_GPG_KEY` | armoured private key |
| `APT_GPG_KEY_ID` | the key id |

The private key never leaves that secret. If it leaks, revoke it, generate a
new one, and every box needs its keyring replaced — so treat it accordingly.

## One-time: enable GitHub Pages

Settings → Pages → deploy from branch `gh-pages`, root. The workflow creates
the branch on its first successful release.

## How the repository is built

A flat repository — one directory of `.deb` files plus an index — which is all
apt needs and avoids the pool/dists layout that only matters at Debian's scale:

```sh
dpkg-scanpackages --multiversion . > Packages
gzip -k9 Packages
apt-ftparchive release . > Release
gpg --default-key "$KEY_ID" -abs   -o Release.gpg Release
gpg --default-key "$KEY_ID" --clearsign -o InRelease Release
```

`--multiversion` keeps older versions listed, which is what makes
`apt install livecaption=0.2.0+41` work for a rollback.

Two workflow details this depends on:

- `actions/checkout` needs `fetch-depth: 0`. A shallow clone has no tags, so
  the release gate would see no previous version and treat every push as a
  release.
- The job needs `permissions: contents: write` to push the tag and the
  `gh-pages` branch.

## What goes in the package

| Path | Notes |
| --- | --- |
| `/usr/bin/livecaption` | static binary |
| `/usr/lib/systemd/system/livecaption.service` | enabled, not started, by `postinst` |
| `/etc/default/livecaption` | conffile — apt preserves local edits |
| `/usr/share/livecaption/secrets.env.example` | copied to `/etc/livecaption/secrets.env` only if absent |
| `/usr/share/doc/livecaption/README.md` | the runbook |

`postrm` deletes `/var/lib/livecaption` on **purge only**. A plain remove — and
a version rollback, which is a remove plus an install — must never destroy the
transcripts of events that already happened.
