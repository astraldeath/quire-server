# Server operation

For a source build, use Go 1.27.1 or newer. First build the browser reader with `npm ci` and `npm run build:web` in the sibling `quire-reader` repository. Then run these commands from the server repository root:

```sh
go build -o bin/quire-server ./cmd/quire-server
# On Windows use bin/quire-server.exe for the executable below.
bin/quire-server serve -web-dir ../quire-reader/dist-web
```

The `-web-dir` / `QUIRE_WEB_DIR` directory must contain only the trusted reader build. Keep the private data directory outside it. API responses and UI documents use separate content-security policies; hosted chapters load in sanitized srcdoc documents. Native rendering is unchanged.

Defaults: `http://localhost:8080`, SQLite at `./data/quire.db`. `GET /healthz` checks database readiness; `GET /.well-known/quire` exposes the server name, API URL and supported capabilities.

On a new installation, open the server URL and enter the one-time setup code printed in the private service logs. Choose the first administrator username and password. The code changes on restart, and setup closes once an administrator exists.

All commands accept `-data PATH`. Serving also accepts `-listen HOST:PORT`, `-public-url https://books.example.com`, and `-name NAME`. Equivalent environment variables: `QUIRE_DATA`, `QUIRE_LISTEN`, `QUIRE_PUBLIC_URL`, `QUIRE_NAME`. Only loopback public URLs may use HTTP. Remote use requires a TLS reverse proxy; the advertised origin must not have a subpath.

## Accounts and administration

Accounts use lowercase usernames, not email addresses. Passwords require at least 12 characters. The reader splits `alice@your-server` into the username and server address.

For an existing installation, promote an account locally with `bin/quire-server admin-promote -username NAME`. Existing accounts, books and reading data are preserved.

The browser administration panel provides:

- **Overview:** account count, active book copies and active book storage. Storage excludes orphaned snapshots and other disk use.
- **Accounts:** change roles, disable or enable accounts, and revoke devices. The last active administrator cannot be demoted or disabled.
- **Invitations:** issue single-use codes or links with a seven-day expiry, preassign libraries, and revoke unused invitations. New accounts are members. Secrets are shown once and stored as hashes.
- **Libraries:** manage shared collections, membership, uploads, common metadata, and uploaded-copy removal. Shared storage is separate from personal accounts. Administrators have no API endpoint for reading members' annotations or positions.
- **Watched folders:** register personal or shared-library paths, scan on demand, inspect errors, and remove watches without changing source files.
- **Settings:** change the server name and scan interval, including manual-only scans.

Local account commands prompt twice for a password without echoing it:

```sh
bin/quire-server user-add -username bob
bin/quire-server password-reset -username alice
```

Password reset revokes all sessions for that account. Users can also change their own passwords in the account menu, which signs out all sessions. Individual devices can revoke sessions through the authenticated API. Sessions expire after 30 days and are limited to 32 active sessions per account; sign in again after expiry. Registration requires an invitation; there is no email password-reset endpoint.

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

The Docker build includes reader commit `0610e9ada1e3eb0c7420127dff6d8f2d5026034f`. `QUIRE_READER_REF` is the build argument for selecting another reviewed revision.

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

## Reader connections

In the installed reader, open **Settings > Sync**, enter `alice@books.example.com`, find the server, confirm its displayed address, and sign in. Use Advanced server address for custom ports.

Native Windows, Linux and iOS sessions use OS credential storage. Standalone browser sessions stay in memory; the hosted WebUI uses 30-day HttpOnly, SameSite=Strict cookies (Secure on HTTPS) to preserve sign-in across refreshes and browser restarts.

For a separate browser client, explicitly configure `-allowed-origins http://localhost:1420` (or `QUIRE_ALLOWED_ORIGINS`) to permit a browser client. No wildcard origins are accepted. Native apps do not need CORS configuration.

Hosted browser caches are account-specific. Browser imports upload to the personal library; covers and metadata arrive automatically, and opening a book downloads its bytes. Revoking server access does not erase cached device files.

Book metadata, progress and passages sync automatically; appearance settings stay device-local. Upload a local book with **Book details > Upload book file**. To upload existing and new books automatically while Quire is open, enable **Auto-upload books** in **Settings > Library**. Removing a server upload leaves reading data and existing device downloads intact. Identical watched and uploaded copies remain independent sources.

Updated clients also sync the private-library passcode verifier and hidden/locked book settings per account. Update all clients to enforce these restrictions. Face ID and unlocked sessions remain device-local. Server backups include privacy settings; book files, metadata and backups are not encrypted, and these settings do not restrict administrator access to server files.

## Files and upload limits

Supported formats are EPUB (including fixed layout), PDF, CBZ, CBR (RAR4/RAR5), CB7 (7-Zip), FB2/FBZ, and DRM-free MOBI/AZW3. Downloads and backups preserve the original format.

Uploads and watched-file ingestion default to 2 GiB per book. Set `QUIRE_MAX_UPLOAD_BYTES` or `-max-upload-bytes` to an integer from 1 through 8589934592 (8 GiB). The flag overrides the environment; invalid values fail startup. For Docker, add `QUIRE_MAX_UPLOAD_BYTES` to the service's `environment` section; the default Compose file does not forward it. Set reverse-proxy request limits and timeouts to allow the intended upload size.

Downloads and backup/restore have an independent 8 GiB per-book ceiling. Lowering the upload limit does not prevent downloading or backing up existing books. See the [file transfer API](API.md#file-transfer) for streaming uploads and discovery limits.

Interrupted uploads never become available. Readers verify downloads against the book's SHA-256 identity. Server snapshots live in the private data directory; watched source folders are never written to. Unreferenced snapshots from failed scans may remain on disk; automatic garbage collection is not implemented.

### Archive validation

Archives have finite entry counts, safe paths, bounded metadata and image extraction, and at most 512 MiB of expansion beyond the stored archive size. EPUB has a fixed 512 MiB expanded ceiling. Comic images are limited to 32 MiB and 100 million pixels.

CBR and CB7 use pure-Go decoders without server-side archive executables. ComicInfo.xml supplies title, writer and series; the first decodable image by filename supplies the cover. Encrypted, multipart, linked, unsafe-path and invalid-image archives are rejected. RAR and 7-Zip dictionaries are capped at 64 MiB. CB7 supports Copy, LZMA/LZMA2, Deflate, Bzip2 and associated filters; PPMd, Zstandard, Brotli and LZ4 are unsupported. Encoded 7-Zip headers are limited to 8 MiB. Members are inspected in memory and never extracted onto the server filesystem.

## Read-only watched folders

```sh
bin/quire-server watch-add -username alice -path /library/alice
bin/quire-server scan
bin/quire-server watch-list
bin/quire-server watch-remove -id WATCH_ID
```

Serving scans registered folders at startup and every five minutes. Change this with `-scan-interval 10m`; `0` disables background scans. Scans create private snapshots and seed metadata from the book. Nested directories seed library folders; later user organization, manual metadata and deletion records are preserved. Folder assignments sync with book metadata. A successful scan reconciles removed source files while retaining reading data. Missing roots, changed root filesystem identity, read errors, or files changing during the scan leave the prior availability list intact. Symlinks are not followed. To change a mounted folder's identity, remove the old watch and register the intended folder again. Removing a watch keeps original files and reading metadata.

For Docker, add a read-only bind mount such as `/your/library:/library:ro`, then run `docker compose exec quire /quire-server watch-add -username alice -path /library`. Keep the `/data` named volume writable. Folder paths and account creation are owner commands; clients cannot select arbitrary server filesystem paths.

### Library previews

Watched books publish their embedded title, author, and series metadata during scanning. Existing user edits are preserved. Authenticated `GET /v1/books/{id}/metadata` returns embedded metadata and a small JPEG cover preview (up to 320 x 480 pixels), without transferring the book. Cover extraction accepts bounded raster images only; missing or unsupported covers use the reader fallback. No publisher scripts or external URLs are loaded.

Readers cache previews automatically and fetch the book through the existing authenticated file endpoint when a user opens a book. Downloads are hash-verified and stored for offline reading; notes and user metadata are retained.

Shared libraries can be renamed or deleted from Administration. Deleting a library removes its grants, watch registrations, and managed server files, but preserves watched originals and members' own reading records and downloaded copies. Scan history reports newly imported and existing distinct books plus skipped unsupported files/symlinks; failed scans keep the previous catalog.
