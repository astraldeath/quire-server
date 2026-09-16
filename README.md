# Quire Server

**Quire** is pronounced **“kwire”** (/kwaɪər/), rhyming with **choir**.

Quire Server includes the Quire browser reader and an administration panel. Users have private progress, notes and highlights, with personal uploads and optional shared libraries.

## Browser setup

Build the reader alongside this repository:

```sh
cd ../quire-reader
npm ci
npm run build:web
cd ../quire-server
go build -o bin/quire-server ./cmd/quire-server
bin/quire-server serve -web-dir ../quire-reader/dist-web
```

Open `http://localhost:8080`. On a new installation the server prints a one-time setup code to its console. Enter that code and choose the initial administrator username and password. The code changes when the server restarts; setup closes permanently once an admin exists. For an existing installation, run `bin/quire-server admin-promote -username NAME` locally to explicitly promote an existing account instead. Existing accounts, books and reading data are preserved.

The browser uses the same reader code as the installed apps. Hosted builds have same-server sign-in and account-specific IndexedDB caches. Hosted browser sessions use 30-day HttpOnly cookies and survive refresh and browser restarts. EPUB imports upload to the user's personal library. Covers and metadata arrive automatically; EPUB bytes download on opening. Revoking server access does not remotely erase cached files.

## Administration

- **Overview:** account count, active book copies and active EPUB storage (not total disk usage or orphaned snapshots).
- **Accounts:** roles, disable/enable and revoke devices. The last active admin is protected. Password recovery remains available through the local `password-reset` command. Users can change their own passwords in the account menu; this signs out all sessions.
- **Invitations:** single-use codes/links, seven-day expiry, revoke unused invitations, and preassign shared libraries. New accounts are always members. Invite secrets are hashed in storage and shown only when issued.
- **Libraries:** shared collections, membership, EPUB uploads, common metadata editing and uploaded-copy removal. Shared library storage is separate from personal accounts. Administration has no endpoint for reading members' annotations or positions.
- **Watched folders:** register server paths against a personal or shared library; scan automatically when added or on demand, review last scan errors, remove watches without modifying source files.
- **Settings:** server name and scan interval, including manual-only scans.

Data is migrated on opening. Back up the full data directory with the service stopped before upgrading. Keep it out of the public web directory. The `-web-dir` / `QUIRE_WEB_DIR` directory must contain only the trusted reader build; API responses and UI documents have separate content-security policies. Hosted chapter loading uses sanitized srcdoc documents; native rendering is unchanged.

## Deployment and reference

Requires Go 1.27.1 or newer for a source build. Docker Compose builds the reader and server together:

```sh
docker compose up -d --build
docker compose logs quire
```

The image pins reader commit `3297cd3d69ea6a3d86193f003ab88bf0b1119d63`. Use `QUIRE_READER_REF` to build another reviewed revision.

- [Production deployment and upgrades](docs/PRODUCTION.md)
- [Server commands, Docker, reader connections, and watched folders](docs/OPERATIONS.md)
- [Backup and restore](docs/BACKUPS.md)
- [MangaBaka OAuth and tracking](docs/tracking.md)
- [API and synchronization semantics](docs/API.md)
- [OpenAPI contract](api/openapi.json)

## Development

```sh
go test ./...
go vet ./...
go build ./cmd/quire-server
```

Format Go changes with `gofmt`. Tests use isolated temporary databases. Use incremental Conventional Commits; never commit runtime data, books, credentials, or generated binaries.

The server is licensed under [AGPL-3.0-only](LICENSE). The API contract is separately licensed under MIT, allowing the [MIT reader](https://github.com/astraldeath/quire) to generate client code without copying server implementation.
