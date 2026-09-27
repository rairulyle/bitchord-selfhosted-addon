<h1 align="center">bitchord-selfhosted-addon</h1>

<p align="center">
  Play your own Plex and Jellyfin music libraries inside BitChord.
</p>

<div align="center">
  <video src="https://github.com/user-attachments/assets/54e40931-afd8-49c3-ab2f-c6598f182081" width="320" controls></video>
</div>

## 🎵 What it is

A small self-hosted server that makes your Plex and Jellyfin music libraries
sources in BitChord (Android). BitChord searches your library through the
addon and streams the original files from it.

BitChord ranks user-added addons above its built-in sources, so when a queued
track also exists in your library, your own copy plays instead.

## ✨ How it works

- **Setup page.** One container, set up in the browser. Open `/setup`,
  choose a password, add your servers, and copy each one's URL into
  BitChord. No server details go into a file.
- **One source per server.** Every Plex or Jellyfin server you add gets its
  own URL and shows up in BitChord as its own source, so you order and toggle
  them there.
- **Fast search.** The addon keeps an in-memory index of every track in each
  library and refreshes it on a timer. Expect roughly 30 to 50 MB of memory
  for a library of 100,000 tracks.
- **Original quality.** Audio and artwork bytes are proxied from your server,
  with `Range` support for seeking. The original file is always served.
  Quality tiers are ignored.
- **Your token stays home.** Plex tokens and Jellyfin keys never leave the
  server. Clients only ever see the addon's own URLs.
- **Secret URL.** Every route sits under a secret path segment. A wrong secret
  gets an empty `404`, the same answer as a server that does not exist.

## 📋 Requirements

- Docker.
- A Plex server, or a Jellyfin server on 10.9 or newer, that the addon
  container can reach over the network.
- A reverse proxy that terminates HTTPS, such as Caddy, Traefik or nginx. The
  addon itself listens on plain HTTP. BitChord requires HTTPS. A
  [Cloudflare Tunnel](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/)
  pointed at `bitchord-selfhosted-addon:8080` works too, and reaches the addon
  from outside your network without opening router ports.
- For **Sign in with Plex**, the container must reach `plex.tv`. Pasting a
  token works without it.

## 🗺️ Roadmap

- **Emby support.** Emby and Jellyfin share most of their API, so this
  follows the Jellyfin adapter: one more kind on the setup page, shown as
  "Emby".
- **Navidrome support.** Through the Subsonic API, which also opens the door
  to other Subsonic-compatible servers.

## 🚀 Setup

1. **Add the service to your docker compose file**.

   ```yaml
   services:
     bitchord-selfhosted-addon:
       image: ghcr.io/rairulyle/bitchord-selfhosted-addon:latest
       ports:
         - "8080:8080"
       volumes:
         - </path/to/your/data>:/data
       restart: unless-stopped
       networks:
         - proxy

   networks:
     proxy:
       external: true
       name: replace-with-your-reverse-proxy-network
   ```

2. **Connect it to HTTPS.** In `compose.yml`, set the network name to the
   Docker network your reverse proxy uses, then point the proxy at
   `bitchord-selfhosted-addon:8080` for the host you will use, such as
   `music.example.com`.

3. **Start it.**

   ```bash
   docker compose up -d
   ```

4. **Open the setup page** at `https://music.example.com/setup` and choose a
   password. Do this right after starting: until a password exists, whoever
   opens the page first owns it.

5. **Set the public URL** to the HTTPS origin from step 2, then **add a
   server**: pick Plex or Jellyfin, sign in or paste a token, test the
   connection, choose a library if you want to limit it, and save.

6. **Copy the server's URL into BitChord.** Each server card shows its URL
   with a copy button. In BitChord, open **Sources**, add an addon, and paste
   it. See [Adding it to a client](#-adding-it-to-a-client).

### Upgrading from v0.4

Servers, the public URL and the secret moved from `.env` to the setup page,
and the addon now keeps them in `/data`. A v0.4 `compose.yml` has no volume, so
edit it before pulling the latest image:

1. **Add the data volume.** Give the service a `volumes:` entry and declare
   the volume at the top level, as `compose.example.yml` does:

   ```yaml
   services:
     bitchord-selfhosted-addon:
       volumes:
         - </path/to/your/data>:/data:/data
   ```

   Without it, `docker compose down` loses the password, the secret and every
   server, and the setup page is open again to whoever reaches it first.

2. **Remove `env_file: .env`** from the service, then delete `.env`. Or keep
   both, with `.env` holding only the settings listed below; Compose refuses
   to start while `env_file:` names a file that does not exist.
3. **Set it up.** Pull latest image, start the container, open `/setup`, set a password
   and the public URL, add your server, and replace the URL in BitChord with
   the new one from the server card. The old variables are ignored; the log
   names any that are still set.

## ⚙️ Configuration

State lives in one file, `/data/addon.json`, on the volume `compose.example.yml`
mounts at `/data`. It holds the password hash, the secret, the public URL and
every server with its token, so keep the volume private. With a bind mount,
`chown 65532` the directory so the container's `nonroot` user can write it.

The environment holds process settings only. None is required.

| Variable           | Default | Meaning                                                       |
| ------------------ | ------- | ------------------------------------------------------------- |
| `REFRESH_INTERVAL` | `15m`   | Index refresh period for every server, such as `5m` or `1h`   |
| `PORT`             | `8080`  | Listen port                                                   |
| `LOG_LEVEL`        | `info`  | `debug`, `info`, `warn` or `error`                            |
| `LOG_FORMAT`       | `text`  | `text` for reading in `docker logs`, `json` for a log shipper |

## 📱 Adding it to a client

A server's URL is your public URL, the server's id and the secret:

```
https://music.example.com/plex-1/<secret>
```

In BitChord, open **Sources**, add an addon, and paste the URL. Repeat for
each server. If a client asks for a manifest URL instead, append
`/manifest.json`.

### Using it with other sources

BitChord tries your sources from top to bottom for every track. With your
servers first, the order looks like this:

1. **Your servers**, in the order you put them. If the track is in one of
   your libraries, your own file plays.
2. **Your other addons**, in the order listed.
3. **Built-in sources**, ending with YouTube Music, which is always on.

If a source does not have the track or cannot be reached, BitChord moves on
to the next one, so music outside your library still plays.

To change the order, open **Sources** and drag a source by the handle on its
right. Use the toggle to switch a source off without removing it.

> [!WARNING]
> Treat each URL like a password. Anyone who has it can stream that library.
> To revoke every URL at once, regenerate the secret on the setup page and
> add the new URLs to your clients.

## 🔀 Reverse proxy notes

Turn off response buffering for the addon, so seeking stays fast and a
skipped track stops downloading from your server at once. The setup page at
`/setup` goes through the same proxy.

- **Caddy** and **Traefik** stream responses by default. No change needed.
- **nginx:**

  ```nginx
  location / {
      proxy_pass http://bitchord-selfhosted-addon:8080;
      proxy_buffering off;
      proxy_request_buffering off;
      proxy_http_version 1.1;
      proxy_read_timeout 1h;
      proxy_set_header X-Forwarded-Proto $scheme;
      proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
  }
  ```

The setup page marks its session cookie `Secure` when the proxy sends
`X-Forwarded-Proto: https`. Caddy and Traefik set it by default.

Login lockout is keyed on the TCP peer address, never `X-Forwarded-For`, so
behind a reverse proxy every client shares one key: five wrong passwords in a
minute pause logins for everyone for that minute.

Do not publish the container's port on the host. Only the reverse proxy
should reach it.

## 📜 Logs

At the default `info` level the addon logs what a client asked for and what
came of it:

```
msg=search server=plex-1 source=plex q="timebomb all time low" strict=1 fallback=0 returned=1 top="Time‐Bomb — All Time Low"
msg="search miss" server=plex-1 source=plex q="some song that is not there" strict=0 fallback=0 returned=0
msg=stream server=plex-1 source=plex id=5820 track="Time‐Bomb — All Time Low" quality="lossless 16-bit 44.1kHz" format=flac
msg=play server=plex-1 source=plex id=5820 track="Time‐Bomb — All Time Low" range="bytes=0-" status=206 bytes=26779352 ended=complete
msg="library indexed" server=plex-1 source=plex tracks=8697 added=12 removed=0 skipped=0
msg="server added" server=jellyfin-1 source=jellyfin host=jellyfin:8096 auth=jellyfin-signin
```

Every line about a server names it under `server` and its kind under
`source`. Setup page actions are logged too: servers added, changed and
removed, the public URL, a regenerated secret, and failed logins with the
client address.

| Line                                | Meaning                                                                                                                            |
| ----------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------- |
| `search`                            | How many tracks matched every word (`strict`), how many matched on the title alone (`fallback`), and the first row returned        |
| `search miss`                       | A query that returned nothing                                                                                                      |
| `stream`                            | The client accepted one of the rows and is about to play it                                                                        |
| `play`                              | One line per file request. `ended` is `complete`, `client left` (a skip, or the player closing the connection) or `upstream error` |
| `library indexed`                   | An index refresh finished                                                                                                          |
| `server started` / `server stopped` | A server's refresher started or stopped, on startup and after a change on the setup page                                           |

A `search` that returned rows with no `stream` after it means the client
turned them down. BitChord does that when the title, the version (live,
acoustic, remix), the artist or the runtime disagree with the track it wanted.

BitChord cannot match a track whose title has no Latin letters or digits at
all, such as `夜に駆ける`. It builds no search for those, so they never reach
the addon and leave no `search miss` behind.

**Privacy.** Search text is written to the log, so the log records what was
listened to. It stays on your server. The secret, tokens, API keys and
passwords are never logged. `LOG_LEVEL=debug` adds one line per HTTP
request and per `HEAD` probe.

## 🛠️ Troubleshooting

| Symptom                                           | Cause                                                                                                                                                                                                                                                                                                    |
| ------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| The setup page asks for a password I never set    | Someone else reached it first, or the volume was reused. Delete the `admin` entry from `/data/addon.json` and restart                                                                                                                                                                                    |
| `/health` stays `503`                             | A server has not loaded its index yet. Its card on the setup page shows the last error, and the logs have `first library load failed`                                                                                                                                                                    |
| The container came up with no servers             | State lives on the `/data` volume. Check `compose.yml` mounts it, and that the volume is the same one as before                                                                                                                                                                                          |
| Log says `cannot open the data file`              | `/data` is not writable by the container's `nonroot` user, or `addon.json` is not valid JSON. Fix the mount or the file and restart                                                                                                                                                                      |
| Log says `plex rejected the Plex token`           | The token is wrong, or the Plex account signed out of all devices. Open the server on the setup page and sign in again or paste a new token                                                                                                                                                              |
| Log says `jellyfin rejected the Jellyfin API key` | The key was deleted in the dashboard, or the user signed out of all devices. Sign in again or paste a new key                                                                                                                                                                                            |
| `Sign in with Plex` says it cannot reach plex.tv  | The container has no route to `plex.tv`. Paste a token instead                                                                                                                                                                                                                                           |
| No server on the account is reachable             | The addon must reach the server over the network. Type an address the container can use, such as a Docker service name                                                                                                                                                                                   |
| The server has no music library with that name    | The library filter must be one the connection test listed. Pick it from the list after testing                                                                                                                                                                                                           |
| Search works but playback fails                   | The reverse proxy buffers or times out long responses. See the reverse proxy notes                                                                                                                                                                                                                       |
| A track in your library plays from another source | Look for its `search` line. With rows returned and no `stream` after it, the client turned them down: compare the title, version words and runtime in your server with the service's. With no `search` line at all, the client never asked, which is what BitChord does for titles with no Latin letters |
| A new album does not show up                      | The index refreshes every `REFRESH_INTERVAL`. Disable and enable the server on the setup page to refresh now                                                                                                                                                                                             |
| Playback stops when the container is redeployed   | A restart lets active streams run for 10 seconds, then closes them. The player resumes with a Range request once the addon is back                                                                                                                                                                       |

## 🧑‍💻 Development

```bash
go test -race ./...
gofmt -l . && go vet ./...
```

Tests run against in-process fake Plex, Jellyfin and plex.tv servers in
`internal/fakes`. No real server is needed.

Each media server is an adapter behind the `Backend` interface in
`internal/media`. `internal/store` owns the state file, `internal/registry`
runs one index per server and picks the adapter, `internal/server` serves
the BitChord routes, and `internal/setup` is the setup page. Only the
registry names the adapters.

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
