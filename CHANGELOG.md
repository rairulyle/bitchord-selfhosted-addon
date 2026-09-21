# Changelog

All notable changes to this project are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions follow [SemVer](https://semver.org/).

Entries are written **one line per paragraph and bullet** (no hard-wrapping) so that a section pasted into GitHub release notes reflows to full width instead of breaking at the source wrap points.

## [Unreleased]

### Changed

- Renamed the project to `bitchord-selfhosted-addon`. The image is now `ghcr.io/rairulyle/bitchord-selfhosted-addon`; the old image name gets no further updates, so change the `image:` line in `compose.yml` and point your reverse proxy at the new container name `bitchord-selfhosted-addon`.
- The manifest id is now `app.bitchord-selfhosted-addon.plex`. A client that keys addons by id may list it as a new source, in which case remove the old entry.

## [0.1.0] - 2026-09-21

### Added

- BitChord addon server for a Plex music library: `manifest.json`, `search` and `stream` endpoints.
- In-memory index of every track in the Plex music sections, refreshed every `REFRESH_INTERVAL` and swapped atomically, so searches never wait on Plex. `PLEX_SECTION` limits it to one library.
- Two-tier search built for BitChord's query shapes: every query word must match across title, track artist, album artist and album, with a fallback on the title alone for when the streaming service credits a different artist than the file's tags. Accents, apostrophes and punctuation are ignored, and non-Latin titles match whole.
- Audio and artwork are proxied from Plex with `Range`, `If-Range` and `HEAD` support, so seeking works and the Plex token never leaves the server. The original file is always served, with its real format, bit depth and sample rate reported.
- Every route sits under a secret path segment (`ADDON_SECRET`). A wrong secret, an unknown route and a malformed path all get the same empty `404`, and the secret and the Plex token never appear in logs.
- `/healthz` reports readiness once the first index load has finished, and the image probes it with its own `-healthcheck` flag.
- Multi-arch Docker image (`linux/amd64`, `linux/arm64`) on a distroless, non-root base, published to `ghcr.io/rairulyle/bitchord-selfhosted-addon`.
