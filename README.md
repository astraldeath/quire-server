# Quire Server

Planned optional self-hosted sync service for Quire. Intended license: AGPL-3.0. Application code has not been implemented.

The full [AGPL-3.0 license text](https://www.gnu.org/licenses/agpl-3.0.html) must be added before publishing. Downloading the canonical text failed during initial setup.

The initial architecture is maintained in the sibling reader repository at `../quire-reader/docs/architecture.md`.

## Server requirements

- Multiple users with private libraries and owner-controlled account creation.
- Login through a `user@server` identifier.
- Sync reading data without requiring an EPUB upload.
- Accept uploads and scan read-only watched folders assigned to users.
- Allow separate server-file deletion only for uploaded files; never delete watched originals.
- Preserve reading data and existing device downloads when a server file disappears.
- Proposed deployment: Go service, SQLite, filesystem storage, and one Docker container.

The versioned API will be owned here, with explicitly MIT-licensed reusable contract artifacts for the separate reader. Implementation and contract details will follow architecture review.
