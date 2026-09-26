# Quire Server

Host your books, sync reading progress, and share libraries. Quire Server includes the browser reader and an administration panel, with separate accounts for personal books, notes, and highlights.

**[Docker image](https://github.com/astraldeath/quire-server/pkgs/container/quire-server)** · [Download the reader](https://github.com/astraldeath/quire/releases/latest) · [Deployment guide](docs/PRODUCTION.md) · [API reference](docs/API.md)

### iOS sideloading

<p>
  <a href="https://intradeus.github.io/http-protocol-redirector?r=altstore://source?url=https://raw.githubusercontent.com/astraldeath/quire/refs/heads/main/repo/source.json"><img alt="AltStore Source" src="https://img.shields.io/badge/open_in_app-_?style=for-the-badge&amp;label=AltStore&amp;labelColor=black&amp;color=728EAE"></a>
  <a href="https://intradeus.github.io/http-protocol-redirector?r=feather://source/https://raw.githubusercontent.com/astraldeath/quire/refs/heads/main/repo/source.json"><img alt="Feather Source" src="https://img.shields.io/badge/open_in_app-_?style=for-the-badge&amp;label=Feather&amp;labelColor=black&amp;color=728EAE"></a>
  <a href="https://intradeus.github.io/http-protocol-redirector?r=sidestore://source?url=https://raw.githubusercontent.com/astraldeath/quire/refs/heads/main/repo/source.json"><img alt="SideStore Source" src="https://img.shields.io/badge/open_in_app-_?style=for-the-badge&amp;label=SideStore&amp;labelColor=black&amp;color=728EAE"></a>
</p>

[Direct source URL](https://raw.githubusercontent.com/astraldeath/quire/refs/heads/main/repo/source.json). The reader is also available for Windows, Linux, and Android. iOS IPAs need signing in your sideloading app.

## Quick start

The prebuilt Docker image includes the server and WebUI for AMD64 and ARM64.

```sh
mkdir quire-server
cd quire-server
curl -fsSLO https://raw.githubusercontent.com/astraldeath/quire-server/main/compose.yaml
printf 'QUIRE_PUBLIC_URL=https://books.example.com\n' > .env
docker compose up -d
docker compose logs quire
```

Replace `https://books.example.com` with your public address. Point your reverse proxy or host-installed Cloudflare Tunnel at `http://127.0.0.1:8080`. The supplied Compose file binds that port to localhost. A Cloudflare Tunnel handles public HTTPS; it does not need Caddy alongside it.

Open your public address and enter the one-time setup code from the logs to create the administrator account. For a local trial, set `QUIRE_PUBLIC_URL=http://localhost:8080` and open that address instead.

Books and account data are stored in the `quire-data` volume. See [production deployment](docs/PRODUCTION.md) for proxy setup, storage, and upgrades.

## What it provides

- A browser reader with personal uploads and shared libraries.
- Progress, bookmarks, highlights, notes, folders, privacy settings, and reading-history sync with Quire apps.
- Account invitations, roles, device revocation, and shared-library membership.
- Watched folders that import books without moving the source files.
- MangaBaka OAuth and tracking for the hosted reader.
- OPDS 1.2 and 2.0 feeds with revocable app passwords, plus a proxy for external catalogs.
- Server backups and restore commands.

Supported files include EPUB, PDF, CBZ, CBR, CB7, FB2/FBZ, and DRM-free MOBI/AZW3. Uploads default to 2 GiB per book. [File formats and limits](docs/OPERATIONS.md) explains configuration and archive support.

## Updates

Back up the server data before upgrading, then run:

```sh
docker compose pull
docker compose up -d
```

The data volume is retained. The `latest` image follows successful builds from `main`; version and commit tags are also available.

The image uses reader commit `c0072b722b5dd69aa8c6738d99a4c3d6fcf42f1b`. Source builds can select another revision with `QUIRE_READER_REF`.

## Documentation

| Guide | Contents |
| --- | --- |
| [Production deployment](docs/PRODUCTION.md) | Docker, HTTPS, reverse proxies, and upgrades |
| [Operations](docs/OPERATIONS.md) | Commands, configuration, accounts, watched folders, and file limits |
| [Backups](docs/BACKUPS.md) | Backup, restore, and recovery |
| [Tracking](docs/tracking.md) | MangaBaka OAuth setup |
| [OPDS](docs/OPDS.md) | Catalog feeds, app passwords, and external sources |
| [API](docs/API.md) | Authentication, uploads, and synchronization |
| [OpenAPI contract](api/openapi.json) | Endpoint schemas |
| [CI caches](docs/CI-CACHES.md) | Workflow cache configuration |

## Development

Source builds require Go 1.27.1 or newer. Build the browser reader alongside this repository:

```sh
cd ../quire-reader
npm ci
npm run build:web
cd ../quire-server
go build -o bin/quire-server ./cmd/quire-server
bin/quire-server serve -web-dir ../quire-reader/dist-web
```

Run the server checks:

```sh
go test ./...
go vet ./...
```

To build the Docker image locally:

```sh
docker compose -f compose.yaml -f compose.build.yaml up -d --build
```

## License

The server is [AGPL-3.0-only](LICENSE). The API contract is separately licensed under MIT, as is the [Quire reader](https://github.com/astraldeath/quire). Quire is pronounced “kwire”, rhyming with “choir”.
