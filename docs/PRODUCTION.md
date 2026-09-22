# Production deployment

1. Pull the combined reader/server image with `docker compose pull`. `latest` follows successful main builds; version and full-commit tags are available for a fixed deployment. The reader SHA in Dockerfile identifies the paired WebUI.
2. Set `QUIRE_PUBLIC_URL=https://books.example.com` in the deployment environment. Put an HTTPS reverse proxy in front of the loopback-only port. Forward the whole origin, including `/opds`, `/opds/*`, and the `/catalogs` reader route. Preserve Host, Origin, and Authorization; allow EPUB request bodies up to 128 MiB and sufficient transfer time. Quire does not trust forwarded IPs, so add client-specific authentication rate limits at the proxy.
3. Start with `docker compose up -d`. Complete first-admin setup using the one-time code in private service logs. Create invite codes for members. Mount watched folders read-only and grant libraries deliberately.
4. If using MangaBaka, register the exact HTTPS callback `/v1/tracking/oauth/callback`. Place `mangabaka-oauth.json` in the private data volume with `clientId` and `clientSecret`, readable only by the service. Do not put credentials in the image or repository. A localhost OAuth registration is not a production registration.
5. Check `/healthz`, sign-in across refresh, book download/read, notes, a library scan, and backup/restore using a disposable restore destination. Test the actual iPhone build separately. Tracking is driven by WebUI sync, not an unattended background scheduler.

## Storage and upgrades

Keep `/data` persistent and private, and never mount it under `/web`. The container runs as UID 65532 with a read-only image; Compose supplies a 512 MiB `/tmp` tmpfs for backups. Large backup downloads require enough temporary space; increase the limit or provide a private writable temporary mount for a larger deployment.

Before an upgrade, stop the service and snapshot the full data volume, including keys. Retain the previous image and matching reader revision. Database migrations run at startup. Rollback requires restoring the pre-upgrade volume snapshot with the previous image; do not run an older binary against a migrated database. Never overwrite the only backup. Test restore independently before deleting snapshots.

Portable server archives omit tracking credentials and disable automatic tracking on restore; reconnect the provider afterward. A complete cold volume snapshot contains secrets and must remain private. Browser IndexedDB version 2 upgrades downloaded-file storage transactionally; reload older Quire tabs if they block the upgrade. Browser caches are not a replacement for server backups.

## Release checks

Run reader tests and both reader builds, Go tests and vet, and the container build. CI tests and smoke-tests the container before publishing main and version-tag builds. Production deployment, public DNS/TLS, final OAuth registration, and physical iOS validation are operator steps; a successful local build alone is not a production deployment.

## Image publishing

The **Server checks** workflow publishes `ghcr.io/astraldeath/quire-server` after Go checks and the container startup/backup smoke test pass. Builds support Linux AMD64 and ARM64. `main` publishes `latest`, `v*` tags publish their exact tag (for example `v0.1.0`), and both publish `sha-<full-commit>`. Manual workflow runs on main can retry publishing. Version tags do not move `latest`. Pull requests and other branches never publish.

Publishing uses the repository's automatic `GITHUB_TOKEN` with `packages: write`; no registry password secret is needed. GitHub initially creates packages as private. After the first publish, open the package settings and set visibility to **Public** for anonymous pulls. If keeping it private, authenticate Docker to GHCR with a personal access token with `read:packages`. See [GitHub's Container registry documentation](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry).

Reader-only changes are included after updating `QUIRE_READER_REF` in Dockerfile (and its documentation references) and pushing the server repository. Each published server stays paired with a reviewed reader revision. Publishing does not restart deployments; run `docker compose pull` followed by `docker compose up -d` on the server when ready to update.
