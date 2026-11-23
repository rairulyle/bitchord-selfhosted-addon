# Changelog

All notable changes to this project are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions follow [SemVer](https://semver.org/).

Entries are written **one line per paragraph and bullet** (no hard-wrapping) so that a section pasted into GitHub release notes reflows to full width instead of breaking at the source wrap points.

## [Unreleased]

## [1.2.0] - 2026-11-23

### Added

- Navidrome and other Subsonic-compatible servers, signed in with a username and password. Their password is stored in `/data/addon.json` because Subsonic token auth needs it, so use a non-admin account. Plays report "now playing" to Navidrome and scrobble once a set share of the track has played. `NAVIDROME_*` environment variables are not read; the log says so when one is set (thanks @1ofx9, #10).
- A sync interval per server, including manual only, and a **Sync now** button on each server card plus **Sync all**.
- A sync webhook per server, `POST https://<public url>/<server id>/<secret>/sync`, shown on its card. A call while a sync is already running answers `202` and starts nothing.
- A recent activity log on the setup page with syncs, sync errors, now playing and scrobbles, filters for syncs and playback, and **Clear**.
- Cover art is cached in memory and revalidated with an `ETag`.
- Search forgives a typo in words of five letters or more, finishes the last word when nothing matches it exactly, treats ft./feat., pt./part, vol./volume, OST/soundtrack and vs./versus as the same word, and drops words such as "live", "remastered" or a year when the full query finds nothing (thanks @1ofx9, #10).
- A search tester on each server card with All, Songs and Albums tabs and cover thumbnails. It shows the words a query becomes, how many tracks matched strictly and loosely, and the matching tracks, albums and artists, with several artists joined by ` • `. Press `/` to jump to it and Escape to clear and close it. The same answer is at `GET /setup/servers/<server id>/test-search?q=<query>`, or `/search?q=<query>`. BitChord's own search still answers tracks only.
- Backup and restore on the setup page. A backup holds the public URL, secret, servers and app authentication settings, encrypted with a password you choose, and never the setup password. Restoring replaces every server and the secret, signs out Jellyfin sessions it replaces, and keeps your setup password. The panel asks for the password when the chosen file is encrypted (thanks @1ofx9, #10).
- A redesigned setup page with light and dark themes and a System, Dark or Light switcher, and server cards with an Online or Unreachable badge and the last connection error (thanks @1ofx9, #10).
- App authentication for BitChord Addon Auth v1, off by default and set on the setup page. In log or enforce mode the addon checks the token and device proof BitChord sends with each search and stream against signing keys fetched from the issuer, and signs each play link for six hours with a key that never leaves the server. Log only reports what it would refuse; enforce refuses with `401` or `403`, so it needs a BitChord version that sends Addon Auth headers. The default issuer is a third-party domain not confirmed as official; see the README before turning it on (thanks @1ofx9, #10).
- `/` redirects to `/setup`, and the addon serves a favicon.

### Changed

- WavPack, APE and DSD tracks count as lossless on every server.
- Search ranks a track whose title matches the whole query above one whose title only starts with it, and both above artist and album matches. The in-memory index for 100,000 tracks grows from about 26 MB to about 40 MB.
- The setup stylesheet is served gzip-compressed with an `ETag`, and its versioned URL is cached for a year.
- Setup pages and their stylesheet send `X-Content-Type-Options`, `X-Frame-Options` and `Referrer-Policy` headers.
- A server URL entered on the setup page can no longer be a link-local or cloud metadata address such as `169.254.169.254`. Servers already saved with one still load.

### Fixed

- The setup page shows a notice or error only when the addon wrote it, never text taken from a crafted link.

## [1.1.0] - 2026-09-27

### Changed

- The public URL is saved automatically when you create the setup password over HTTPS on a public hostname, and the setup page explains what it is. You can still change or clear it. Behind nginx, add `proxy_set_header Host $host;` so the addon sees your hostname.

## [1.0.0] - 2026-09-27

**Breaking:** servers are now set up on a password-protected page at `/setup` instead of in `.env`, state needs a `/data` volume, and every BitChord URL changes. Follow the upgrade steps under Removed, or "Upgrading from v0.4" in the README.

### Added

- Several servers in one container. Each Plex or Jellyfin server is its own BitChord source with its own URL, `https://<public url>/<server id>/<secret>`, so sources are ordered and toggled in BitChord. Server ids are `plex-1`, `plex-2`, `jellyfin-1` and so on, and the name in BitChord is the kind plus a label you choose, such as `Plex - Home`.
- A setup page at `/setup`. Choose a password on first visit, then set the public URL, add servers, test each connection, pick a library, copy each server's URL, and regenerate the secret. Changes apply at once; nothing needs a restart.
- Sign in with Plex on the setup page, through plex.tv. An empty server URL is filled with the first server on the account the addon can reach, and the connection test and library list run on their own after any sign-in. Pasting a token still works.
- Sign in to Jellyfin with a username and password on the setup page. Only the access token Jellyfin returns is kept; the password is never stored. Pasting an API key still works.
- State lives in `/data/addon.json`, on a folder mounted at `/data`.

### Changed

- Every log line about a server names it under `server`, next to `source`.
- `/health` answers `200` with no servers configured, so a fresh container passes its healthcheck before it is set up.
- A malformed or unknown server URL segment answers the same empty `404` as a wrong secret.

### Removed

- `ADDON_SECRET`, `PUBLIC_URL`, `ADDON_NAME`, `PLEX_URL`, `PLEX_TOKEN`, `PLEX_SECTION`, `JELLYFIN_URL`, `JELLYFIN_API_KEY` and `JELLYFIN_LIBRARY` are no longer read. The log names any that are still set. **Upgrading from 0.4:** mount a folder at `/data` (`volumes: - </path/to/your/data>:/data`, owned by user `65532`) as in `compose.example.yml`, remove `env_file: .env` (or keep a `.env` with only `PORT`, `LOG_LEVEL`, `LOG_FORMAT` and `REFRESH_INTERVAL`), then start the container, open `/setup`, set a password and the public URL, add your server, and replace the URL in BitChord with the one on its card. The old `/<secret>/manifest.json` URL no longer answers.

## [0.4.0] - 2026-09-27

### Added

- Jellyfin support. Set `JELLYFIN_URL` and `JELLYFIN_API_KEY` instead of the Plex variables, and optionally `JELLYFIN_LIBRARY`, to serve a Jellyfin music library the same way. Each container talks to one server. The source shows up in the client as "Jellyfin", the manifest id is `app.bitchord-selfhosted-addon.jellyfin`, and the API key travels only in the `Authorization` header. Jellyfin 10.9 or newer is required.
- `source=plex` or `source=jellyfin` on every log line.

### Changed

- `ADDON_NAME` now defaults to the server kind, `Plex` or `Jellyfin`, instead of always `Plex`.
- Starting with neither server configured, or with both, now prints one message naming the variables to set instead of listing every Plex variable as missing.
- A rejected token or key is logged as `upstream request failed` with an error naming the variable to check, such as `plex rejected PLEX_TOKEN`, instead of a message of its own.
- The stream descriptor route answers from the in-memory index and asks the server only for a track the index does not know, which saves one upstream request per play.
- The `starting` log line reports the server host under `server` and the filter under `library`, for both server kinds.

## [0.3.1] - 2026-09-21

### Fixed

- Tracks that Plex never finished analysing (no media bitrate) now play. Plex answers `500` to a direct-play request for those parts but serves the same file as a download, so a `500` on a file request is retried once with `download=1`. Before this, such tracks got a `502` and the client fell through to the next source.

## [0.3.0] - 2026-09-21

### Changed

- The readiness endpoint is now `/health`. `/healthz` is gone and answers the same empty `404` as any unknown path. The image's own healthcheck uses the binary's `-healthcheck` flag, so containers need no change; update anything external that probes the old path, such as an uptime monitor or a dashboard link.

## [0.2.0] - 2026-09-21

### Added

- `search`, `search miss`, `stream` and `play` log lines at `info`, showing each query with its strict and fallback counts and top row, the track a client went on to play, and how each file transfer ended (`complete`, `client left` or `upstream error`). Search text is now written to the log; the secret and the Plex token still never are.
- `LOG_FORMAT` setting: `text` (the new default, readable in `docker logs`) or `json`.
- A `starting` line with the version and the non-secret settings, and `added` / `removed` counts on every library refresh.
- A "Logs" section in the README explaining how to read a miss, and a Roadmap section announcing Jellyfin support.

### Changed

- Logs are plain text by default. Set `LOG_FORMAT=json` to keep the previous format.
- The per-request `request` line moved from `info` to `debug`.
- Renamed the project to `bitchord-selfhosted-addon`. The image is now `ghcr.io/rairulyle/bitchord-selfhosted-addon`; the old image name gets no further updates, so change the `image:` line in `compose.yml` and point your reverse proxy at the new container name `bitchord-selfhosted-addon`.
- The manifest id is now `app.bitchord-selfhosted-addon.plex`. A client that keys addons by id may list it as a new source, in which case remove the old entry.

### Fixed

- Tracks whose titles have punctuation inside a word, accented letters, or mixed scripts are now found by BitChord. BitChord joins `Time‐Bomb` into `timebomb`, reads `11:11 PM` as `1111 pm`, deletes accented letters so `Naïve` becomes `nave`, and keeps only the Latin words of a title such as `ワンテンポ遅れたMonster ain't dead`. Those spellings are now indexed alongside the normal ones. Replaying BitChord's queries for an 8,697-track library took unreturned tracks from 175 to 0.

## [0.1.0] - 2026-09-21

### Added

- BitChord addon server for a Plex music library: `manifest.json`, `search` and `stream` endpoints.
- In-memory index of every track in the Plex music sections, refreshed every `REFRESH_INTERVAL` and swapped atomically, so searches never wait on Plex. `PLEX_SECTION` limits it to one library.
- Two-tier search built for BitChord's query shapes: every query word must match across title, track artist, album artist and album, with a fallback on the title alone for when the streaming service credits a different artist than the file's tags. Accents, apostrophes and punctuation are ignored, and non-Latin titles match whole.
- Audio and artwork are proxied from Plex with `Range`, `If-Range` and `HEAD` support, so seeking works and the Plex token never leaves the server. The original file is always served, with its real format, bit depth and sample rate reported.
- Every route sits under a secret path segment (`ADDON_SECRET`). A wrong secret, an unknown route and a malformed path all get the same empty `404`, and the secret and the Plex token never appear in logs.
- `/healthz` reports readiness once the first index load has finished, and the image probes it with its own `-healthcheck` flag.
- Multi-arch Docker image (`linux/amd64`, `linux/arm64`) on a distroless, non-root base, published to `ghcr.io/rairulyle/bitchord-selfhosted-addon`.
