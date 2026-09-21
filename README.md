<h1 align="center">bitchord-selfhosted-addon</h1>

<p align="center">
  Play your own Plex music library inside BitChord.
</p>

<div align="center">
  <video src="https://github.com/user-attachments/assets/54e40931-afd8-49c3-ab2f-c6598f182081" width="320" controls></video>
</div>

## 🎵 What it is

A small self-hosted server that makes your Plex music library a source in
BitChord (Android). It speaks the
[Eclipse Music addon protocol](https://eclipsemusic.app/docs), so any other
client that speaks the protocol can search the library and stream the original
files too: Eclipse Music (iOS, iPadOS, web), Sonido (Linux) and others.

BitChord ranks user-added addons above its built-in sources, so when a queued
track also exists in your Plex library, the Plex copy plays instead.

## ✨ How it works

- **Fast search.** The addon keeps an in-memory index of every track in your
  Plex music sections and refreshes it on a timer. Expect roughly 30 to 50 MB
  of memory for a library of 100,000 tracks.
- **Original quality.** Audio and artwork bytes are proxied from Plex, with
  `Range` support for seeking. The original file is always served. Quality
  tiers are ignored.
- **Your token stays home.** Your Plex token never leaves the server. Clients
  only ever see the addon's own URLs.
- **Secret URL.** Every route sits under a secret path segment. A wrong secret
  gets an empty `404`, the same answer as a server that does not exist.

## 📋 Requirements

- Docker.
- A Plex server the addon container can reach over the network.
- A reverse proxy that terminates HTTPS, such as Caddy, Traefik or nginx. The
  addon itself listens on plain HTTP. Eclipse clients require HTTPS. A
  [Cloudflare Tunnel](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/)
  pointed at `bitchord-selfhosted-addon:8080` works too, and reaches the addon
  from outside your network without opening router ports.

## 🚀 Setup

1. **Copy the example files.**

   ```bash
   cp .env.example .env
   cp compose.example.yml compose.yml
   ```

2. **Fill in `.env`.** Generate the secret with `openssl rand -hex 24`. See
   [Configuration](#%EF%B8%8F-configuration) for every setting and for where
   to find your Plex token.

3. **Connect it to HTTPS.** In `compose.yml`, set the network name to the
   Docker network your reverse proxy uses, then point the proxy at
   `bitchord-selfhosted-addon:8080` for the host in `PUBLIC_URL`.

4. **Start it.**

   ```bash
   docker compose up -d
   ```

5. **Check it.** The first command prints the manifest. The second prints
   `200` once the first index load has finished, and `503` before that.

   ```bash
   curl https://music.example.com/<ADDON_SECRET>/manifest.json
   curl -o /dev/null -w '%{http_code}\n' https://music.example.com/health
   ```

6. **Add it to your app.** See [Adding it to a client](#-adding-it-to-a-client).

### Image tags and updates

- The image is `ghcr.io/rairulyle/bitchord-selfhosted-addon:latest`, built for
  `linux/amd64` and `linux/arm64`.
- **Pin a version** with a tag such as `:0.2` or `:0.2.0`.
- **Update** with `docker compose pull`, then `docker compose up -d`.
- **Build from source** by replacing the `image:` line in `compose.yml` with
  `build: .` and running `docker compose up -d --build`.

## ⚙️ Configuration

| Variable | Required | Default | Meaning |
|---|---|---|---|
| `PLEX_URL` | yes | | Base URL the container uses to reach Plex, such as `http://plex:32400` |
| `PLEX_TOKEN` | yes | | Plex authentication token |
| `ADDON_SECRET` | yes | | Path segment guarding every route. At least 16 characters of letters, digits, `-` or `_` |
| `PUBLIC_URL` | yes | | The HTTPS origin clients use, such as `https://music.example.com`. Must start with `https://` unless the host is `localhost` |
| `PLEX_SECTION` | no | all music sections | Section id or title to limit the index to |
| `REFRESH_INTERVAL` | no | `15m` | Index refresh period, such as `5m` or `1h` |
| `ADDON_NAME` | no | `Plex` | Display name in the client's source list |
| `PORT` | no | `8080` | Listen port |
| `LOG_LEVEL` | no | `info` | `debug`, `info`, `warn` or `error` |
| `LOG_FORMAT` | no | `text` | `text` for reading in `docker logs`, `json` for a log shipper |

A bad configuration prints every problem at once and exits.

### Finding your Plex token

In Plex Web, open any item, choose **Get Info**, then **View XML**. The
address bar of the new tab ends with `X-Plex-Token=...`. That value is the
token. Plex documents this under "Finding an authentication token".

## 📱 Adding it to a client

The addon URL is your public URL followed by the secret:

```
https://music.example.com/<ADDON_SECRET>
```

- **BitChord:** open **Sources**, add an addon, and paste the URL.
- **Eclipse Music:** open the addons screen, add an addon, and paste the URL.

If a client asks for a manifest URL instead, append `/manifest.json`.

> [!WARNING]
> Treat the URL like a password. Anyone who has it can stream your library.
> To revoke it, change `ADDON_SECRET`, restart the container, and add the new
> URL to your clients.

## 🔀 Reverse proxy notes

Turn off response buffering for the addon, so seeking stays fast and a
skipped track stops downloading from Plex at once.

- **Caddy** and **Traefik** stream responses by default. No change needed.
- **nginx:**

  ```nginx
  location / {
      proxy_pass http://bitchord-selfhosted-addon:8080;
      proxy_buffering off;
      proxy_request_buffering off;
      proxy_http_version 1.1;
      proxy_read_timeout 1h;
  }
  ```

Do not publish the container's port on the host. Only the reverse proxy
should reach it.

## 📜 Logs

At the default `info` level the addon logs what a client asked for and what
came of it:

```
msg=search q="timebomb all time low" strict=1 fallback=0 returned=1 top="Time‐Bomb — All Time Low"
msg="search miss" q="some song that is not there" strict=0 fallback=0 returned=0
msg=stream id=5820 track="Time‐Bomb — All Time Low" quality="lossless 16-bit 44.1kHz" format=flac
msg=play id=5820 track="Time‐Bomb — All Time Low" range="bytes=0-" status=206 bytes=26779352 ended=complete
msg="library indexed" tracks=8697 added=12 removed=0 skipped=0
```

| Line | Meaning |
|---|---|
| `search` | How many tracks matched every word (`strict`), how many matched on the title alone (`fallback`), and the first row returned |
| `search miss` | A query that returned nothing |
| `stream` | The client accepted one of the rows and is about to play it |
| `play` | One line per file request. `ended` is `complete`, `client left` (a skip, or the player closing the connection) or `upstream error` |
| `library indexed` | An index refresh finished |

A `search` that returned rows with no `stream` after it means the client
turned them down. BitChord does that when the title, the version (live,
acoustic, remix), the artist or the runtime disagree with the track it wanted.

BitChord cannot match a track whose title has no Latin letters or digits at
all, such as `夜に駆ける`. It builds no search for those, so they never reach
the addon and leave no `search miss` behind.

**Privacy.** Search text is written to the log, so the log records what was
listened to. It stays on your server. The secret and the Plex token are never
logged. `LOG_LEVEL=debug` adds one line per HTTP request and per `HEAD` probe.

## 🛠️ Troubleshooting

| Symptom | Cause |
|---|---|
| `/health` stays `503` | The first index load has not succeeded. Check the logs for `first library load failed` |
| Log says `plex rejected the token` | `PLEX_TOKEN` is wrong or has been revoked |
| Log says `no music section matches` | `PLEX_SECTION` does not name a music library. Use its exact title or its numeric id |
| Search works but playback fails | The reverse proxy buffers or times out long responses. See the reverse proxy notes |
| A track in Plex plays from another source | Look for its `search` line. With rows returned and no `stream` after it, the client turned them down: compare the title, version words and runtime in Plex with the service's. With no `search` line at all, the client never asked, which is what BitChord does for titles with no Latin letters |
| A new album does not show up | The index refreshes every `REFRESH_INTERVAL`. Restart the container to refresh now |
| Playback stops when the container is redeployed | A restart lets active streams run for 10 seconds, then closes them. The player resumes with a Range request once the addon is back |

## 🗺️ Roadmap

- **Jellyfin support.** Serve a Jellyfin music library the same way as a Plex
  one. Each running addon will talk to one server, picked by which settings
  are filled in: `PLEX_URL` and `PLEX_TOKEN`, or `JELLYFIN_URL` and
  `JELLYFIN_API_KEY`. The source shows up in the client as "Plex" or
  "Jellyfin" unless `ADDON_NAME` overrides it. To use both servers, run two
  containers and add both addon URLs.

## 🧑‍💻 Development

```bash
go test -race ./...
gofmt -l . && go vet ./...
```

Tests run against an in-process fake Plex server in `internal/plextest`. No
real Plex server is needed.

### Releasing

`CHANGELOG.md` is the source of truth for release notes.

1. Move the entries under `## [Unreleased]` into a new `## [X.Y.Z] - YYYY-MM-DD`
   section and commit.
2. Tag and push:

   ```bash
   git tag vX.Y.Z
   git push origin vX.Y.Z
   ```

The workflow checks that the changelog has a section for that version, runs
the tests, pushes a multi-arch image to GHCR tagged `latest`, `X.Y.Z` and
`X.Y`, then creates the GitHub release with that section as its notes. The
version is stamped into the binary and shows in the manifest. Publishing a
release from the GitHub UI triggers the same workflow, and its notes are
replaced by the changelog section. Preview the notes locally with
`scripts/release-notes.sh vX.Y.Z`.
