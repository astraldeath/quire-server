# Quire Server

The optional self-hosted service for Quire. The service provides private accounts, revocable device sessions, and durable reading-data sync over a versioned HTTP API. Progress and notes can sync using the EPUB SHA-256 identity without uploading the book.

The reader connects through Settings > Server. EPUB uploads/downloads and owner-configured read-only watched folders are supported. No public server repository or hosted service is required.

## Run locally

Requires Go 1.27.1 or newer. From this folder:

```sh
go build -o bin/quire-server ./cmd/quire-server
# On Windows use bin/quire-server.exe for the executable below.
bin/quire-server user-add -username alice
bin/quire-server serve
```

The account command prompts for a password twice without echoing it. Use at least 12 characters. Accounts use lowercase names, not email addresses. The future reader will split `alice@your-server` into the username and server address.

Defaults: `http://localhost:8080`, SQLite at `./data/quire.db`. `GET /healthz` checks database readiness; `GET /.well-known/quire` exposes the server name, API URL and supported capabilities. No accounts are created automatically.

All commands accept `-data PATH`. Serving also accepts `-listen HOST:PORT`, `-public-url https://books.example.com`, and `-name NAME`. Equivalent environment variables: `QUIRE_DATA`, `QUIRE_LISTEN`, `QUIRE_PUBLIC_URL`, `QUIRE_NAME`. Only loopback public URLs may use HTTP. Remote use requires a TLS reverse proxy; the advertised origin must not have a subpath.

## Owner administration

```sh
bin/quire-server user-add -username bob
bin/quire-server password-reset -username alice
```

Password reset revokes **all** sessions for that account. Individual devices can revoke sessions through the authenticated API. Sessions expire after 30 days and are limited to 32 active sessions per account; sign in again after expiry. There is no self-registration or email password-reset endpoint.

For automation, account commands accept `-password-file PATH`. Protect this file and remove it when finished. Passwords are never command-line values, environment variables, or logged. The database stores salted Argon2id hashes; bearer tokens are stored only as SHA-256 hashes.

## Docker

```sh
docker compose up -d --build
docker compose exec quire /quire-server user-add -username alice
```

The container runs as an unprivileged user, with a read-only root filesystem and a named data volume. Compose binds the HTTP port to the host loopback interface. Put your HTTPS reverse proxy in front of it and set `QUIRE_PUBLIC_URL` to the external origin before starting. Do not expose the unencrypted container port directly to the internet. Configure per-client login rate limits at the proxy as well; Quire ignores forwarded client-IP headers and throttles its immediate peer.

Docker packaging is included; it has not yet been run on this development machine because Docker Desktop’s daemon was unavailable. Native Windows tests and a Linux cross-build are the local verification paths.

## API and sync semantics

The reusable contract is [api/openapi.json](api/openapi.json), licensed separately under MIT. Server implementation is AGPL-3.0-only; see [LICENSE](LICENSE). Contract code can be generated for the MIT reader without copying server implementation.

- `POST /v1/sessions`: username, password, deviceName; returns a bearer token once.
- `GET /v1/sessions`, `DELETE /v1/sessions/{id}`: list/revoke your devices.
- `POST /v1/sync`: submit up to 50 operations and read up to 100 changes.

Each operation contains a stable `id`, SHA-256 `bookId`, `kind` (`book`, `position`, `annotation`), `recordId`, `baseRevision`, `deleted`, and a typed `value`. Book and position record IDs are `default`; annotation IDs remain stable across devices. Device timestamps, file bytes and cover data are excluded from these records. API strings are plain data, never trusted HTML.

Example request after authentication:

```json
{
  "cursor": 0,
  "operations": [{
    "id": "device-a-operation-1",
    "bookId": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "kind": "position",
    "recordId": "default",
    "baseRevision": 0,
    "deleted": false,
    "value": {"cfi": "epubcfi(/6/2)", "fraction": 0.1, "section": "Chapter 1"}
  }]
}
```

The account comes from the bearer session, never from a supplied user ID. Identical retries return the original operation result without duplicating changes. Reusing an operation ID for different content, a future revision/cursor, or exceeding 16 unresolved candidates returns 409 and rolls back the batch. Malformed data also rolls back the batch.

Each record has a server revision and one or more candidates. Stale edits append candidates, preserving concurrent notes and reading positions. A write based on the latest revision explicitly resolves all candidates. Deletions are retained as tombstone candidates; stale writes cannot erase them. The client must display conflicts and allow a choice rather than silently selecting the furthest reading position.

Pull with `operations: []` and the last stored `cursor`. Apply returned changes and advance the cursor atomically on the client. Repeat while `hasMore` is true. A change cursor is independent of a record revision. Persist local changes and outbox operations in one client transaction; remove acknowledged operations only after handling the server response. The reader implements this durable outbox, foreground/reconnect sync, and explicit conflict choices.

Request bodies are limited to 2 MiB, individual values to 32 KiB. Histories and tombstones have no expiry in v1, allowing old devices to reconnect safely; monitor data-volume growth. File transfer uses separate endpoints and never deletes reading data.

## Persistence and backups

SQLite uses WAL, full synchronous writes, transactions and a schema version. Keep the complete persistent data directory private. To back up this initial server, stop it, copy the entire data directory (or Docker volume), then restart it. Restore with the service stopped. A raw server backup contains password hashes and hashed sessions and needs protection. Reader `.quire-backup` files are a separate format and are not imported by this service.

## Verify

```sh
go test ./...
go vet ./...
go build ./cmd/quire-server
```

Tests use isolated temporary SQLite databases. They cover authentication, expiry/revocation/reset, account isolation, persistent notes, atomic rollback, retry IDs, concurrent-edit candidates, tombstones, cursor paging, malformed input and login throttling. No real books or credentials are included.


## Reader connection and file transfer

In the installed reader, open **Settings > Server**, enter `alice@books.example.com`, find the server, confirm its displayed address, and sign in. Use Advanced server address for custom ports. Native Windows/iOS sessions use OS credential storage. Browser sessions stay in memory until the tab closes; explicitly configure `-allowed-origins http://localhost:1420` (or `QUIRE_ALLOWED_ORIGINS`) to permit a browser client. No wildcard origins are accepted. Native apps do not need CORS configuration.

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

Serving scans registered folders at startup and every five minutes. Change this with `-scan-interval 10m`; `0` disables background scans. Scans create private snapshots and seed new book metadata from filenames. Existing manual metadata and deletion records are preserved. A successful scan reconciles removed source files while retaining reading data. Missing roots, changed root filesystem identity, read errors, or files changing during the scan leave the prior availability list intact. Symlinks are not followed. To change a mounted folder's identity, remove the old watch and register the intended folder again. Removing a watch keeps original files and reading metadata.

For Docker, add a read-only bind mount such as `/your/library:/library:ro`, then run `docker compose exec quire /quire-server watch-add -username alice -path /library`. Keep the `/data` named volume writable. Folder paths and account creation are owner commands; clients cannot select arbitrary server filesystem paths.

### Library previews

Watched EPUBs publish their embedded title, author, and series metadata during scanning. Existing user edits are preserved. Authenticated `GET /v1/books/{id}/metadata` returns embedded metadata and a small JPEG cover preview (up to 320 x 480 pixels), without transferring the EPUB. Cover extraction accepts bounded raster images only; missing or unsupported covers use the reader fallback. No publisher scripts or external URLs are loaded.

Readers cache previews automatically and fetch the EPUB through the existing authenticated file endpoint when a user opens a book. Downloads are hash-verified and stored for offline reading; notes and user metadata are retained.
