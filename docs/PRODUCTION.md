# Production deployment

1. Pull the combined reader/server image with `docker compose pull`. Use `latest` for successful main builds or a version/full-commit tag for a fixed deployment. [Dockerfile](../Dockerfile) pins the paired reader revision.
2. Set `QUIRE_PUBLIC_URL=https://books.example.com` in the deployment environment. Put an HTTPS reverse proxy in front of the loopback-only port. Forward the whole origin, including `/opds`, `/opds/*`, and the `/catalogs` reader route. Preserve Host, Origin, and Authorization; allow request bodies and transfer time for your [configured upload limit](OPERATIONS.md#files-and-upload-limits), which defaults to 2 GiB. Quire does not trust forwarded IPs, so add client-specific authentication rate limits at the proxy.
3. Start with `docker compose up -d`. Complete first-admin setup using the one-time code in private service logs. Create invite codes for members. Mount watched folders read-only and assign members to the libraries they need.
4. If using MangaBaka, register the exact HTTPS callback `/v1/tracking/oauth/callback`. Place `mangabaka-oauth.json` in the private data volume with `clientId` and `clientSecret`, readable only by the service. Do not put credentials in the image or repository. A localhost OAuth registration is not a production registration.
5. Check `/healthz`, sign-in after refresh, book reading and downloads, notes, and a library scan. Test [backup and restore](BACKUPS.md) in a new directory. Test installed iPhone clients separately if you use them. Tracking runs after WebUI sync and has no unattended scheduler.

## Storage and upgrades

Keep `/data` persistent and private, and never mount it under `/web`. The container runs as UID 65532 with a read-only image; Compose supplies a 512 MiB `/tmp` tmpfs for backups. Large backup downloads require enough temporary space; increase the limit or provide a private writable temporary mount for a larger deployment.

Before an upgrade, stop the service and snapshot the full data volume, including keys. Retain the previous image and matching reader revision. Database migrations run at startup. Rollback requires restoring the pre-upgrade volume snapshot with the previous image; do not run an older binary against a migrated database. Never overwrite the only backup. Test restore independently before deleting snapshots.

Portable server archives omit tracking credentials and disable automatic tracking on restore; reconnect the provider afterward. A complete cold volume snapshot contains secrets and must remain private. If a browser storage upgrade is blocked, reload older Quire tabs. Keep server backups even when books are cached on devices.

## Release validation

Run reader tests and both reader builds, Go tests and vet, and the container build. CI tests and smoke-tests the container before publishing main and version-tag builds. Check public DNS/TLS, the production OAuth callback and physical iOS clients after deploying.

## Image publishing

The **Server checks** workflow publishes `ghcr.io/astraldeath/quire-server` after Go checks and the container startup/backup smoke test pass. Builds support Linux AMD64 and ARM64. `main` publishes `latest`, `v*` tags publish their exact tag (for example `v0.1.0`), and both publish `sha-<full-commit>`. Manual workflow runs on main can retry publishing. Version tags do not move `latest`. Pull requests and other branches never publish.

Publishing uses the repository's automatic `GITHUB_TOKEN` with `packages: write`; no registry password secret is needed. GitHub initially creates packages as private. After the first publish, open the package settings and set visibility to **Public** for anonymous pulls. If keeping it private, authenticate Docker to GHCR with a personal access token with `read:packages`. See [GitHub's Container registry documentation](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry).

To include reader changes, update `QUIRE_READER_REF` in Dockerfile and its documentation references, then push the server repository. Publishing does not restart deployments; run `docker compose pull` followed by `docker compose up -d` on the server when ready to update.
