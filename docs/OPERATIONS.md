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

The Docker build includes reader commit `3297cd3d69ea6a3d86193f003ab88bf0b1119d63`. `QUIRE_READER_REF` is the build argument for selecting another reviewed revision.

The container runs as an unprivileged user, with a read-only root filesystem and a named data volume. Compose binds the HTTP port to the host loopback interface. Put your HTTPS reverse proxy in front of it and set `QUIRE_PUBLIC_URL` to the external origin before starting. Do not expose the unencrypted container port directly to the internet. Configure per-client login rate limits at the proxy as well; Quire ignores forwarded client-IP headers and throttles its immediate peer.

Docker includes CA certificates for outbound HTTPS. Compose provides a bounded temporary filesystem for backup staging while keeping the image read-only.

## Reader connection and file transfer

In the installed reader, open **Settings > Server**, enter `alice@books.example.com`, find the server, confirm its displayed address, and sign in. Use Advanced server address for custom ports. Native Windows/iOS sessions use OS credential storage. Standalone browser sessions stay in memory; the hosted WebUI uses 30-day HttpOnly, SameSite=Strict cookies (Secure on HTTPS) to preserve sign-in across refreshes and browser restarts; explicitly configure `-allowed-origins http://localhost:1420` (or `QUIRE_ALLOWED_ORIGINS`) to permit a browser client. No wildcard origins are accepted. Native apps do not need CORS configuration.

Books, progress and passages sync automatically; appearance settings stay device-local. EPUB transfer is explicit in **Book details > Server copy**. Removing a server upload leaves reading data and existing device downloads intact. Identical watched and uploaded copies remain independent sources.

- `GET /v1/files`: this account's available file identities and sources.
- `PUT /v1/books/{sha256}/file`: raw EPUB, maximum 128 MiB, hash verified before registration.
- `GET /v1/books/{sha256}/file`: owned EPUB download.
- `DELETE /v1/books/{sha256}/file`: remove uploaded source only; watched-only files return 403.

Interrupted uploads never become available. Downloads are checked against the identity again by the reader. Server snapshots live beneath the private data directory; source folders are never written to. Unreferenced snapshots from failed scans may remain on disk in this initial version; automatic garbage collection is not implemented.

## Read-only watched folders

```sh
bin/quire-server watch-add -username alice -path /library/alice
bin/quire-server scan
bin/quire-server watch-list
bin/quire-server watch-remove -id WATCH_ID
```

Serving scans registered folders at startup and every five minutes. Change this with `-scan-interval 10m`; `0` disables background scans. Scans create private snapshots and seed metadata from the EPUB. Existing manual metadata and deletion records are preserved. A successful scan reconciles removed source files while retaining reading data. Missing roots, changed root filesystem identity, read errors, or files changing during the scan leave the prior availability list intact. Symlinks are not followed. To change a mounted folder's identity, remove the old watch and register the intended folder again. Removing a watch keeps original files and reading metadata.

For Docker, add a read-only bind mount such as `/your/library:/library:ro`, then run `docker compose exec quire /quire-server watch-add -username alice -path /library`. Keep the `/data` named volume writable. Folder paths and account creation are owner commands; clients cannot select arbitrary server filesystem paths.

### Library previews

Watched EPUBs publish their embedded title, author, and series metadata during scanning. Existing user edits are preserved. Authenticated `GET /v1/books/{id}/metadata` returns embedded metadata and a small JPEG cover preview (up to 320 x 480 pixels), without transferring the EPUB. Cover extraction accepts bounded raster images only; missing or unsupported covers use the reader fallback. No publisher scripts or external URLs are loaded.

Readers cache previews automatically and fetch the EPUB through the existing authenticated file endpoint when a user opens a book. Downloads are hash-verified and stored for offline reading; notes and user metadata are retained.

Shared libraries can be renamed or deleted from Administration. Deleting a library removes its grants, watch registrations, and managed server files, but preserves watched originals and members' own reading records and downloaded copies. Scan history reports newly imported and existing distinct EPUBs plus skipped non-EPUB files/symlinks; failed scans keep the previous catalog.
