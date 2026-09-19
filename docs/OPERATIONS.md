# Server operation

Requires Go 1.27.1 or newer. From this folder:

```sh
go build -o bin/quire-server ./cmd/quire-server
# On Windows use bin/quire-server.exe for the executable below.
bin/quire-server user-add -username alice
bin/quire-server serve
```

The account command prompts for a password twice without echoing it. Use at least 12 characters. Accounts use lowercase names, not email addresses. The reader splits `alice@your-server` into the username and server address.

Defaults: `http://localhost:8080`, SQLite at `./data/quire.db`. `GET /healthz` checks database readiness; `GET /.well-known/quire` exposes the server name, API URL and supported capabilities. The first administrator is created through browser setup.

All commands accept `-data PATH`. Serving also accepts `-listen HOST:PORT`, `-public-url https://books.example.com`, and `-name NAME`. Equivalent environment variables: `QUIRE_DATA`, `QUIRE_LISTEN`, `QUIRE_PUBLIC_URL`, `QUIRE_NAME`. Only loopback public URLs may use HTTP. Remote use requires a TLS reverse proxy; the advertised origin must not have a subpath.

## Owner administration

```sh
bin/quire-server user-add -username bob
bin/quire-server password-reset -username alice
```

Password reset revokes **all** sessions for that account. Individual devices can revoke sessions through the authenticated API. Sessions expire after 30 days and are limited to 32 active sessions per account; sign in again after expiry. Registration requires an invitation; there is no email password-reset endpoint.

For automation, account commands accept `-password-file PATH`. Protect this file and remove it when finished. Passwords are never command-line values, environment variables, or logged. The database stores salted Argon2id hashes; bearer tokens are stored only as SHA-256 hashes.

## Docker

```sh
docker compose pull
docker compose up -d
docker compose logs quire
# Open the server URL and enter the setup code from the logs.
```

The default Compose file pulls `ghcr.io/astraldeath/quire-server:latest`, including the WebUI. For a source build use `docker compose -f compose.yaml -f compose.build.yaml up -d --build`.

For cloudflared running on the host, set `QUIRE_PORT=8770` and `QUIRE_PUBLIC_URL=https://books.example.com` in `.env`, and point the tunnel at `http://127.0.0.1:8770`. No Caddy container is needed. If you retain a separate `docker-compose.yml`, use `-f docker-compose.yml` consistently; Docker prefers `compose.yaml` when both exist. Replace its `build:` section with `image: ghcr.io/astraldeath/quire-server:latest`, preserving ports and volumes.

The Docker build includes reader commit `2d4c11b5145c2fae0cac26b48e88e589c7eeddc4`. `QUIRE_READER_REF` is the build argument for selecting another reviewed revision.

The container runs as an unprivileged user, with a read-only root filesystem and a named data volume. Compose binds the HTTP port to the host loopback interface. Put your HTTPS reverse proxy in front of it and set `QUIRE_PUBLIC_URL` to the external origin before starting. Do not expose the unencrypted container port directly to the internet. Configure per-client login rate limits at the proxy as well; Quire ignores forwarded client-IP headers and throttles its immediate peer.

Docker includes CA certificates for outbound HTTPS. Compose provides a bounded temporary filesystem for backup staging while keeping the image read-only.

## Server update checks and installation

Settings shows the installed server build and [published changes](../CHANGELOG.md). Every signed-in account can read this information; only administrators see the management instructions. The server never replaces its own binary or container.

The server checks the fixed public GitHub `server-latest/update.json` release asset at most once per six hours, on demand. The request sends no account information or credentials, times out after eight seconds, and accepts at most 64 KiB. GitHub outages do not affect reading or sync. On failure, the last successful metadata and its check time remain visible alongside an error. Restarting clears the in-memory cache.

To install an available update, first review its changes and create a backup using the [backup procedure](BACKUPS.md). From the directory containing your deployment's Compose file, run:

```sh
docker compose pull quire
docker compose up -d quire
docker compose logs --tail=100 quire
```

Use the same `-f` options as your deployment. A source-built deployment instead uses `docker compose -f compose.yaml -f compose.build.yaml up -d --build quire`. Preserve the existing data volume. After startup, reload Settings and verify the installed revision. The hosted reader changes with the image; reload its browser page to load the new reader.

Official main images have version `main.YYYYMMDD.<12-character revision>`. The image embeds the full server revision, the source commit's timestamp, and the exact reader revision actually checked out. CI publishes the matching rolling prerelease manifest only after the image push succeeds. Tag images use the tag as their version; the update channel tracks main. An update is offered only for a different revision with a strictly newer source timestamp, so an older main image is not offered to a newer installed build.

Plain `go build` and Docker builds without identity arguments report `dev` / `unknown` and cannot determine whether they are current. Custom distributors can embed `QUIRE_VERSION`, `QUIRE_REVISION` (full commit SHA), and `QUIRE_BUILD_TIME` (the source commit's RFC3339 timestamp) as Docker build arguments. The Go equivalents are `-ldflags '-X quire.local/server/internal/server.BuildVersion=... -X quire.local/server/internal/server.BuildRevision=... -X quire.local/server/internal/server.BuildTime=... -X quire.local/server/internal/server.ReaderRevision=...'`. Use identity values for the sources actually built.

## Reader connection and file transfer

In the installed reader, open **Settings > Server**, enter `alice@books.example.com`, find the server, confirm its displayed address, and sign in. Use Advanced server address for custom ports. Native Windows/iOS sessions use OS credential storage. Standalone browser sessions stay in memory; the hosted WebUI uses 30-day HttpOnly, SameSite=Strict cookies (Secure on HTTPS) to preserve sign-in across refreshes and browser restarts; explicitly configure `-allowed-origins http://localhost:1420` (or `QUIRE_ALLOWED_ORIGINS`) to permit a browser client. No wildcard origins are accepted. Native apps do not need CORS configuration.

Books, progress and passages sync automatically; appearance settings stay device-local. book transfer is explicit in **Book details > Server copy**. Removing a server upload leaves reading data and existing device downloads intact. Identical watched and uploaded copies remain independent sources.

Updated clients also sync the private-library passcode verifier and hidden/locked book settings per account. Update all clients to enforce these restrictions. Face ID and unlocked sessions remain device-local. Server backups include privacy settings; book files, metadata and backups are not encrypted, and these settings do not restrict administrator access to server files.

- `GET /v1/files`: this account's available file identities and sources.
- `PUT /v1/books/{sha256}/file`: raw book, maximum 128 MiB, hash verified before registration.
- `GET /v1/books/{sha256}/file`: owned book download.
- `DELETE /v1/books/{sha256}/file`: remove uploaded source only; watched-only files return 403.

Interrupted uploads never become available. Downloads are checked against the identity again by the reader. Server snapshots live beneath the private data directory; source folders are never written to. Unreferenced snapshots from failed scans may remain on disk in this initial version; automatic garbage collection is not implemented.

## Read-only watched folders

```sh
bin/quire-server watch-add -username alice -path /library/alice
bin/quire-server scan
bin/quire-server watch-list
bin/quire-server watch-remove -id WATCH_ID
```

Serving scans registered folders at startup and every five minutes. Change this with `-scan-interval 10m`; `0` disables background scans. Scans create private snapshots and seed metadata from the book. Existing manual metadata and deletion records are preserved. A successful scan reconciles removed source files while retaining reading data. Missing roots, changed root filesystem identity, read errors, or files changing during the scan leave the prior availability list intact. Symlinks are not followed. To change a mounted folder's identity, remove the old watch and register the intended folder again. Removing a watch keeps original files and reading metadata.

For Docker, add a read-only bind mount such as `/your/library:/library:ro`, then run `docker compose exec quire /quire-server watch-add -username alice -path /library`. Keep the `/data` named volume writable. Folder paths and account creation are owner commands; clients cannot select arbitrary server filesystem paths.

### Library previews

Watched books publish their embedded title, author, and series metadata during scanning. Existing user edits are preserved. Authenticated `GET /v1/books/{id}/metadata` returns embedded metadata and a small JPEG cover preview (up to 320 x 480 pixels), without transferring the book. Cover extraction accepts bounded raster images only; missing or unsupported covers use the reader fallback. No publisher scripts or external URLs are loaded.

Readers cache previews automatically and fetch the book through the existing authenticated file endpoint when a user opens a book. Downloads are hash-verified and stored for offline reading; notes and user metadata are retained.

Shared libraries can be renamed or deleted from Administration. Deleting a library removes its grants, watch registrations, and managed server files, but preserves watched originals and members' own reading records and downloaded copies. Scan history reports newly imported and existing distinct books plus skipped unsupported files/symlinks; failed scans keep the previous catalog.
