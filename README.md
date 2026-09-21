# bitchord-selfhosted-addon

A small self-hosted server that makes your Plex music library a source in
BitChord (Android). BitChord searches your library through the addon and
streams the original files from it.

BitChord ranks user-added addons above its built-in sources, so when a queued
track also exists in your Plex library, the Plex copy plays instead.

## How it works

- The addon keeps an in-memory index of every track in your Plex music
  sections and refreshes it on a timer.
- Search answers come from that index. Audio and artwork bytes are proxied
  from Plex, with `Range` support for seeking.
- Your Plex token never leaves the server. Clients only ever see the addon's
  own URLs.
- Every route sits under a secret path segment. A wrong secret gets an empty
  `404`, the same answer as a server that does not exist.
- The original file is always served. Quality tiers are ignored.

## Requirements

- Docker.
- A reverse proxy that terminates HTTPS, such as Caddy, Traefik or nginx. The
  addon itself listens on plain HTTP. BitChord requires HTTPS.
- Network access from the addon container to your Plex server.

## Setup

1. Copy the example files:

   ```bash
   cp .env.example .env
   cp compose.example.yml compose.yml
   ```

2. Fill in `.env`. Generate the secret with `openssl rand -hex 24`.
3. In `compose.yml`, set the network name to the Docker network your reverse
   proxy uses.
4. Point your reverse proxy at `bitchord-selfhosted-addon:8080` for the host in
   `PUBLIC_URL`.
5. Start it:

   ```bash
   docker compose up -d --build
   ```

6. Check it. The first command prints the manifest. The second prints `200`
   once the first index load has finished, and `503` before that.

   ```bash
   curl https://music.example.com/<ADDON_SECRET>/manifest.json
   curl -o /dev/null -w '%{http_code}\n' https://music.example.com/healthz
   ```

## Configuration

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

A bad configuration prints every problem at once and exits.

### Finding your Plex token

In Plex Web, open any item, choose **Get Info**, then **View XML**. The
address bar of the new tab ends with `X-Plex-Token=...`. That value is the
token. Plex documents this under "Finding an authentication token".

## Adding it to a client

The addon URL is your public URL followed by the secret:

```
https://music.example.com/<ADDON_SECRET>
```

In BitChord, open **Sources**, add an addon, and paste the URL.

If a client asks for a manifest URL instead, append `/manifest.json`.

Treat the URL like a password. Anyone who has it can stream your library. To
revoke it, change `ADDON_SECRET`, restart the container, and add the new URL
to your clients.

## Reverse proxy notes

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

## Memory

The whole index lives in memory. Expect roughly 30 to 50 MB for a library of
100,000 tracks.

## Troubleshooting

| Symptom | Cause |
|---|---|
| `/healthz` stays `503` | The first index load has not succeeded. Check the logs for `first library load failed` |
| Log says `plex rejected the token` | `PLEX_TOKEN` is wrong or has been revoked |
| Log says `no music section matches` | `PLEX_SECTION` does not name a music library. Use its exact title or its numeric id |
| Search works but playback fails | The reverse proxy buffers or times out long responses. See the notes above |
| A new album does not show up | The index refreshes every `REFRESH_INTERVAL`. Restart the container to refresh now |
| Playback stops when the container is redeployed | A restart lets active streams run for 10 seconds, then closes them. The player resumes with a Range request once the addon is back |

## Development

```bash
go test -race ./...
gofmt -l . && go vet ./...
```

Tests run against an in-process fake Plex server in `internal/plextest`. No
real Plex server is needed.
