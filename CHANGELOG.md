# Changelog

All notable changes to this project are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions follow [SemVer](https://semver.org/).

Entries are written **one line per paragraph and bullet** (no hard-wrapping) so that a section pasted into GitHub release notes reflows to full width instead of breaking at the source wrap points.

## [Unreleased]

### Added

- Navidrome and Subsonic server support via the Subsonic API (compatible with Navidrome, Gonic, Lyrion Music Server, Airsonic, Funkwhale, and Ampache).
- Fast flat-library synchronization using the OpenSubsonic `search3` endpoint, with automatic fallback to concurrent album scanning for standard Subsonic servers.
- Full high-resolution audio metadata extraction (`bitDepth`, `samplingRate`) and extended audio codec support for Navidrome streams.
- Playback progress and scrobble reporting for Navidrome: tracks register as "Now Playing" upon stream start and submit completed scrobbles once the listening threshold is reached.
- Configurable scrobble settings with an enable toggle (defaulting to disabled upon creation) and threshold slider (10%–90%), alongside live scrobble status indicators on server overview cards.
- Segmented button selector for server types (`[ Plex ] [ Jellyfin ] [ Navidrome / Subsonic ]`) with solid active styling and keyboard accessibility.
- Support for BitChord 1.9.1 Addon Auth protocol v1 (`bitchordAuth: true` in `manifest.json`, offline JWKS token and ECDSA P-256 device proof verification via `X-BitChord-Token` and `X-BitChord-Proof`).
- Enforced security by default via `BITCHORD_AUTH_MODE=enforce` (with configurable `log` and `disabled` rollout options) and dynamic allowed host binding.
- Short-lived HMAC-SHA256 signed media playback URLs (`/file/{id}?exp=...&sig=...`) with 30-minute validity and cryptographic tamper protection.
- On-demand library synchronization via the setup UI and dedicated webhook endpoints (`/{slug}/{secret}/sync`) with copyable curl helper commands.
- Per-server sync interval configuration (5m, 15m, 30m, 1h, 6h, 24h, or manual only) with automatic deduplication against the default schedule.
- In-browser search diagnostic tester on the setup overview with `[ All ] [ Songs ] [ Albums ]` category tabs, live debounced querying, cover art thumbnails, and album grid cards.
- Search index ranking enhancements: prefix matching with pre-sorted vocabulary binary search, title relevance weighting, and automatic noise token stripping.
- Strict entity search isolation: albums and artists only match against their own names (`score > 0`), preventing unrelated entities from leaking into result lists.
- Multi-artist parsing and index consolidation: composite artist tags separated by `;`, `•`, `|`, or ` / ` are parsed into individual artist entries, eliminating fragmented combo profiles and correctly attributing collaborative tracks.
- Standardized multi-artist display formatting across song rows and album cards using ` • `.
- Category support in `/search` responses: returns distinct matching `albums` alongside `tracks`.
- Detailed library statistics on server cards showing track, artist, and album counts alongside last scan duration.
- Real-time server connectivity health badges (`Online`, `Unreachable`, `Disabled`) with immediate status reporting.
- Recent Activity event history feed tracking sync completions and playback scrobbles across all active servers with auto-refresh.
- Streamlined, responsive two-column layout for server configuration with balanced desktop space usage and mobile stacking.
- Minimalist, accessible focus rings and neutral copy feedback matching shadcn design patterns across all dashboard components.
- Automatic root redirect (`/` to `/setup`) and standard web security headers (`X-Content-Type-Options`, `X-Frame-Options`, `Referrer-Policy`).
- Improved accessibility and high-contrast color palette (WCAG AAA/AA) across light and dark modes.
- Embedded static asset caching and compression with SHA-256 ETags, precomputed gzip compression (`gzip.BestCompression`), HTTP 304 conditional validation, and automatic cache-busting.
- Filterable recent activity feed with `All`, `Syncs`, and `Playback` category toggles preserving the active filter during background polling.
- Password-protected AES-256-GCM encrypted configuration backups and restores with PBKDF2-HMAC-SHA256 key derivation, legacy plaintext fallback, and admin credential overwrite protection.
- Global search tester keyboard shortcut (`/` to open and focus, `Escape` to clear and dismiss) with inline `<kbd>` shortcut indicators.
- Manual theme switcher in the navigation bar cycling between System, Dark, and Light themes with zero-flicker local storage persistence.
- Concurrent multi-server synchronization via the "Sync all" action in the setup header.
- Detailed connection error diagnostics banner on server cards and tooltip on unreachable badges indicating the exact failure reason.
- Independent ring buffer tracking for sync (max 10) and playback (max 30) events with monotonic sequence IDs, preventing playback activity from evicting sync history.
- Manual activity log clearing via the setup interface and `POST /setup/activity/clear`, supporting category-specific clearing (`sync`, `playback`) and full activity clearing with client-side optimistic UI updates.
- Optional activity category filtering query parameter (`?filter=sync`, `?filter=playback`) on the `GET /setup/activity` JSON endpoint.
- Enforced password requirement for all configuration backup exports, guaranteeing that downloaded snapshots are always encrypted with AES-256-GCM.
- In-memory thread-safe LRU artwork cache with SHA-256 ETag generation, HTTP 304 Not Modified validation, and capacity-based pruning for Plex, Jellyfin, and Navidrome cover art (`/art/{id}`).
- Typo and fuzzy search tolerance powered by Damerau-Levenshtein distance calculation, handling character omissions, insertions, substitutions, and adjacent transpositions across tracks, albums, and artists.
- Bidirectional synonym and abbreviation expansion in the search index for common audio metadata terms (`ft` / `feat` / `featuring`, `vol` / `volume`, `pt` / `part`, `ost` / `soundtrack`, `vs` / `versus`).

### Fixed

- Support integer music folder IDs in Subsonic and Navidrome `getMusicFolders` responses.
- Gracefully handle single-object responses for music folders, albums, and songs from XML-to-JSON Subsonic gateways.
- Serialize concurrent library sync requests per server to prevent memory spikes from rapid webhook triggers.
- Block cloud instance metadata IP addresses in server URLs as SSRF defense-in-depth.
- Properly mask secrets and `navidrome-*` slugs in debug logs and UI copy pills.
- Suppress duplicate sync interval options matching the default schedule.
- Prevent `JSON.parse` syntax errors in the search tester by registering both `/setup/servers/{slug}/test-search` and `/setup/servers/{slug}/search` routes and enforcing HTTP response validation.
- Fix prefix search truncation where exact word matches terminated prefix candidate expansion early (e.g. searching `lose` finding both `Lose My Mind` and `Loser`).
- Search tester scrolling in Songs and Albums tabs, and removed truncation limits in the "All" tab so all matching tracks and albums render completely.
- Backup restore error handling by routing restore failures to dedicated error alert banners, auto-detecting encrypted backup files client-side to enforce password entry, and keeping the backup accordion open on failure.

### Security

- Add standard web security headers to the `/setup` admin interface: `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, and `Referrer-Policy: same-origin`.
- Add advisory hint in the Navidrome setup form recommending the use of a dedicated non-admin user account for Subsonic API authentication.
- Prevent admin credential overwrite during backup restoration by unconditionally preserving the active instance's admin credentials regardless of uploaded payload contents.

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
- Searching and streaming with a token read directly from a server segment are removed.
- In-memory indices are keyed on track paths or server ratings keys instead of artificial URLs.

### Removed

- Environment variables configuring servers: `PLEX_URL`, `PLEX_TOKEN`, `PLEX_SECTION`, `JELLYFIN_URL`, `JELLYFIN_API_KEY`, `JELLYFIN_USER_ID`. Each server is configured once at `/setup`.
- The per-server endpoints `/plex/...` and `/jellyfin/...`. Each server has its own URL at `/<server id>/<secret>/...`, shown on the setup page.
- Direct-token requests without a secret segment.
- `ADDON_SECRET`. The secret is generated on first start and stored in `/data/addon.json`; regenerate it on the setup page if needed.

## [0.4.0] - 2026-09-24

### Added

- Jellyfin support. Set `JELLYFIN_URL`, `JELLYFIN_API_KEY` and optional `JELLYFIN_USER_ID` in `.env` to index audio libraries from Jellyfin alongside Plex.
- Multi-source manifest at `/manifest.json`: BitChord lists both Plex and Jellyfin when both are configured.
- Dedicated routes per backend: `/plex/search`, `/plex/stream/...`, `/jellyfin/search`, `/jellyfin/stream/...`. The un-prefixed `/search` and `/stream/...` routes continue to point to Plex for backwards compatibility.

### Fixed

- Plex library refresh no longer stalls when Plex returns non-UTF-8 characters in track or artist names.
- Empty search queries return all indexed tracks instead of an empty list, allowing initial library browsing.

## [0.3.0] - 2026-09-23

### Added

- Support for reverse proxies with subpaths: `ROOT_PATH` or `SCRIPT_NAME` can be passed to host the addon behind path-prefix proxies (e.g., `example.com/bitchord/`).

### Fixed

- Preserve original audio stream codecs and containers during streaming, preventing unwanted transcode handoffs for FLAC, ALAC, and AAC tracks.

## [0.2.0] - 2026-09-22

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
