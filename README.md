# stumpzlib

A small web app that searches book catalogs and drops the results straight into
your [Stump](https://www.stumpapp.dev) library — search, click **Add to Stump**,
and the file lands in your library directory and Stump rescans.

Single Go binary, no dependencies, UI embedded.

<img width="889" height="621" alt="Screenshot 2026-08-29 at 09 43 08" src="https://github.com/user-attachments/assets/d567f69b-98ad-4568-9ef5-c8ba23ac348a" />

## What it searches

- **Z-library**, using the same eAPI as the [KOReader plugin](https://github.com/ZlibraryKO/zlibrary.koplugin): search, language/format filters, most popular, recommended, and authenticated download. Mirrors can be auto-discovered.
- **Project Gutenberg**, via the [Gutendex](https://gutendex.com) API — ~75,000 public-domain books, EPUB preferred with plain-text fallback.

**Disclaimer:** Z-library access is for educational purposes and for works you have the right to download. Respect copyright law. This project is not affiliated with Z-library.

## How it works

Stump builds its library by scanning a directory on disk, so that is the
integration point:

1. You search a catalog; results are normalized into a common shape.
2. On **Add**, the server re-resolves the download URL from the catalog API
   (the browser never supplies it), streams the file to a `.part` temp file in
   your library directory, and renames it into place — so Stump's scanner never
   sees a half-written book.
3. It then runs the `scanLibrary` GraphQL mutation so the book shows up
   without waiting for the next scheduled scan.

### Which Stump API this speaks

Stump 0.1.x serves its data over **GraphQL at `/api/graphql`**. There is no
`/api/v1` — REST under `/api/v2` is limited to auth, thumbnails and a few
per-file endpoints, and any unrouted path returns Stump's web UI with HTTP 200
rather than a 404.

Verified against Stump `0.1.6`:

| Need | Operation |
| --- | --- |
| List libraries | `query { libraries { nodes { id name path } } }` |
| Rescan | `mutation($id: ID!) { scanLibrary(id: $id) }` |
| Log in | `POST /api/v2/auth/login` with `{username, password}` |
| API key | `Authorization: Bearer <key>` |

Because unrouted paths return the web UI, a wrong `STUMP_URL` looks like a
success at the HTTP level. The client detects an HTML body and says so instead
of reporting a parse error.

The rescan is an accelerator, not a requirement. If the Stump API call fails,
the file is still on disk and Stump will pick it up on its next scan; the UI
reports the failure but still counts the add as a success.

## Setup

```sh
go build -o stumpzlib .

LIBRARY_PATH=/path/to/your/stump/library \
STUMP_URL=http://localhost:10801 \
STUMP_API_KEY=your-api-key \
STUMP_LIBRARY_ID=your-library-id \
./stumpzlib
```

Then open <http://localhost:8080>.

`LIBRARY_PATH` must be the directory **as this process sees it**. If Stump runs
in Docker, that is the host path bind-mounted into the container, not the
container path.

Don't know your library id? Start it with just `LIBRARY_PATH` and open
**Settings** (`/settings.html`) in the UI — enter your Stump URL and API key
and it lists every library with its id to pick from.

### Configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `LIBRARY_PATH` | *(required)* | Directory Stump scans; downloads are written here |
| `LISTEN` | `:8080` | Address to listen on |
| `STUMP_URL` | `http://localhost:10801` | Base URL of your Stump server — initial value only, see [Settings page](#settings-page) |
| `STUMP_API_KEY` | — | API key, sent as a bearer token — initial value only |
| `STUMP_USERNAME` / `STUMP_PASSWORD` | — | Used instead of an API key; exchanged for a session cookie — initial value only |
| `STUMP_LIBRARY_ID` | — | Library to rescan after each add; omit to skip rescans — initial value only |
| `AUTH_USERNAME` / `AUTH_PASSWORD` | — | Require a login to use the app; must be set together, or both left blank |
| `GUTENDEX_URL` | `https://gutendex.com` | Point at your own Gutendex instance if you self-host one |
| `ZLIBRARY_URL` | — | Z-library mirror origin (no path), e.g. `https://z-lib.example` — initial value only |
| `ZLIBRARY_EMAIL` / `ZLIBRARY_PASSWORD` | — | Z-library account — initial value only; required for downloads and Recommended, not for search |
| `MAX_DOWNLOAD_MB` | `200` | Per-file download ceiling |

`-listen`, `-library-path`, `-stump-url` and `-stump-library-id` are also
available as flags, which take precedence.

### Settings page

`/settings.html` edits the Stump URL, API key, username/password and library
id, plus the Z-library base URL, account and search filters, at runtime
without a redeploy. The `STUMP_*` and `ZLIBRARY_*` env vars above only seed
these on first run; the moment you save on the Settings page, the saved
values take over permanently — env vars are no longer read after that; edit
them from the Settings page instead (or delete the settings file to fall back
to env vars again).

If you don't know a working Z-library URL, use **Auto-discover** on the
Settings page. It probes known mirrors (and skips ones that answer with a
bot-check page instead of the API) and stores the one it picks. Search works
without an account; **Add to Stump** and **Recommended** need email and
password. **Test login** stores a session and shows today's download quota.

Changes are saved to `.stumpzlib-settings.json`, a hidden file inside
`LIBRARY_PATH`, mode `0600`. Reusing `LIBRARY_PATH` means no extra volume to
mount in Coolify; Stump ignores dotfiles when scanning, and stumpzlib's own
filename sanitizer never produces one, so it won't collide with a book. The
Stump API key and password, and the Z-library password and session cookies,
are stored in that file in plaintext, same as they would be in an env var —
anyone with shell access to the container or the mounted volume can read them.

The Settings page is behind the same login as the rest of the app (see
[Login](#login)) — set `AUTH_USERNAME`/`AUTH_PASSWORD` before exposing this
anywhere reachable, since without them anyone who reaches it can point your
library at a different Stump server or read out your API key's presence.

## Deploying to Coolify

One thing decides the whole deployment: **stumpzlib and Stump must see the same
directory on disk.** stumpzlib writes a book into it, Stump scans it. Get this
wrong and downloads report success while nothing ever appears in Stump.

Two other container gotchas:

- `STUMP_URL` must be the Stump **container/service name**, e.g.
  `http://stump:10801`. `localhost` inside the container is stumpzlib itself.
- `LIBRARY_PATH` is the path as **stumpzlib's container** sees it, and must be
  a library that already exists in Stump. The app exits at startup if the
  directory is missing, rather than downloading into a path nothing scans.

### If Stump already runs (the usual case)

Use `docker-compose.yaml`, which deploys **only** stumpzlib and mounts your
existing library.

Do not deploy a second Stump. It would come up with its own empty library,
stumpzlib would write into that, and your real library would stay empty — a
deploy that looks green and does nothing.

1. **Edit two literal values in `docker-compose.yaml`** (both marked `EDIT ME`):
   the bind mount source, and the published port. They cannot be environment
   variables — see "Why those two are hardcoded" below.
2. Coolify → **+ New** → **Docker Compose**, pointing at this repo.
3. Set the environment variables below in Coolify's UI. Everything under
   `environment:` is a `${VARIABLE}`, so it appears there as an editable field.
4. Deploy.

Finding the two literal values:

```sh
# 1. The library's host path — the bind mount whose target is /data.
#    If Stump binds /opt/media -> /data and its library is /data/books,
#    the host path you want is /opt/media/books.
docker inspect <stump-container> \
  --format '{{range .Mounts}}{{.Source}} -> {{.Destination}}{{"\n"}}{{end}}'

# 2. A free host port. 8080 is very often taken (Coolify itself, among others),
#    which is why this defaults to 8081.
ss -ltn
```

Set in Coolify's UI:

| Variable | Example | Notes |
| --- | --- | --- |
| `STUMP_URL` | `http://host.docker.internal:10801` | Default; works when Stump publishes 10801 on the host |
| `STUMP_API_KEY` | | Or `STUMP_USERNAME` + `STUMP_PASSWORD` |
| `STUMP_LIBRARY_ID` | | Blank just skips rescans |
| `MAX_DOWNLOAD_MB` | `200` | |
| `ZLIBRARY_URL` | | Optional; or use Auto-discover in Settings |
| `ZLIBRARY_EMAIL` / `ZLIBRARY_PASSWORD` | | Needed for Z-library downloads |

### Why those two are hardcoded

Coolify rejects a compose file that uses variable substitution in a volume
source, to block command injection through a mount path:

```
Invalid volume source: contains forbidden character '${'
```

So the bind mount must be a literal path. The published port is literal for the
same reason — to keep the fields Coolify parses structurally free of
substitution. Credentials and URLs are unaffected: they live under
`environment:`, which is exactly where Coolify's variable UI applies.

If you'd rather manage the mount through Coolify's UI, deploy as an
**Application** (build pack: Dockerfile, port 8080) instead of a Docker
Compose resource, and add the bind mount under **Storages**. You'll then need
to add `--add-host=host.docker.internal:host-gateway` under custom Docker
options, or attach the app to Stump's network, so it can reach Stump.

### If Stump is not deployed anywhere yet

Use `docker-compose.with-stump.yaml`, which brings up both services sharing one
`books` volume. In Coolify, set the compose file path on the resource's
**General** tab.

Either way, Stump must be on the **same host** — the integration is a shared
filesystem. Across machines you'd need the library on an NFS/SMB share mounted
by both.

### Joining Stump's network instead of the host gateway

The default `STUMP_URL` goes through `host.docker.internal`, which works
without knowing Stump's network name. To attach directly instead, add to the
`stumpzlib` service:

```yaml
    networks:
      - stump
networks:
  stump:
    external: true
    name: <stump's network name>
```

and set `STUMP_URL=http://<stump-container-name>:10801`.

### Settings that matter

| Coolify setting | Value |
| --- | --- |
| Port | container `8080`, published on host `8081` |
| Health check path | `/healthz` |
| Storage | the literal bind mount in the compose file |

`/healthz` returns 503 when the library directory isn't writable, so a volume
that silently failed to mount shows up as an unhealthy container instead of an
app that accepts searches and fails every download.

The image runs as root by default; downloads are written `0644`, so Stump can
read them whatever UID it runs as. To have books *owned* by Stump's user,
run stumpzlib with the same PUID/PGID (`user: "1000:1000"` in compose, or
Coolify's custom Docker options).

### First deploy

Ordering, since two of the settings don't exist until Stump is up:

1. Deploy with just `LIBRARY_PATH` and `STUMP_URL`. It will start and search;
   rescans are simply skipped.
2. In Stump, create your library if you haven't, and mint an API key.
3. Open `/settings.html`, paste the API key in, and pick your library from the
   list it shows — no redeploy needed, it takes effect immediately.

### Do not give it a public domain

Set `AUTH_USERNAME`/`AUTH_PASSWORD` (see [Login](#login) below) before you
expose this anywhere reachable — without them, anyone who reaches it can write
files into your library. Even with a login configured, prefer keeping it on
Coolify's internal network and reaching it over a tunnel/VPN, or putting
Coolify's basic auth (or Cloudflare Access) in front of it as a second layer.
Don't attach a public FQDN and leave it open.

### Login

Setting `AUTH_USERNAME` and `AUTH_PASSWORD` (both, or neither) puts every
route behind a login page at `/login`. A correct login sets an `HttpOnly`,
`SameSite=Lax` session cookie good for 30 days; **Log out** in the header
clears it.

Sessions are held in memory, not a database — restarting the container logs
everyone out, and this only really works for one shared login, not per-user
accounts. That's the deliberate tradeoff for staying a dependency-free single
binary; if you need real multi-user accounts, put a real auth provider (e.g.
Cloudflare Access) in front instead.

## Adding a source

Implement `Source` in `sources.go` and register it in `newSources`:

```go
type Source interface {
    Name() string
    Label() string
    DownloadHosts() []string
    Search(ctx context.Context, q SearchQuery) ([]Book, error)
    Resolve(ctx context.Context, id string) (*Book, error)
}
```

`SearchQuery.Text` is the user's search string. `Languages`, `Extensions`,
`Order` and `List` (`popular` / `recommended`) are for catalogs that support
them; Gutenberg ignores the extra fields.

`DownloadHosts` is enforced: the downloader refuses to fetch a file from any
host the source didn't declare, so a compromised or spoofed catalog API can't
turn this into an open proxy. Cleartext HTTP is only accepted for loopback.

The UI picks up new sources automatically from `/api/status`.

## Security notes

The catalog is a remote service, so its responses are treated as untrusted:

- Filenames are built from an allowlist of characters — path separators, `..`,
  NUL and control characters cannot survive, and the result is checked to land
  directly inside `LIBRARY_PATH`.
- Downloads are capped by `MAX_DOWNLOAD_MB` and streamed to a temp file that is
  removed if anything fails.
- The browser sends a source name and a book id, never a URL.

Without `AUTH_USERNAME`/`AUTH_PASSWORD` set, the app has no authentication of
its own. Bind it to localhost, or put it behind something that does, rather
than exposing it to a network.

## Tests

```sh
go test ./...
```

Covers filename sanitization and path-escape rejection, the download host
allowlist, format selection, the search and add handlers end to end against a
fake Gutendex and a fake Stump, duplicate handling, the size limit, the "save
succeeded but rescan failed" path, the login/session/logout flow (including
that requests are let through untouched when no login is configured), the
Settings page's API (secrets never round-trip to the browser, a blank secret
field on save keeps the stored value, `clearApiKey`/`clearPassword` remove
one explicitly, and settings persist across a store reload), and the
Z-library client against a fake eAPI (search, popular/recommended, login,
quota, session retry, download, auto-discover, bot-challenge handling, and
that Z-library secrets never round-trip).
