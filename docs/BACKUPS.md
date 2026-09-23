# Server backups

Quire supports full data-directory snapshots and portable `.quire-server-backup` archives. Reader `.quire-backup` files use a different format and cannot be imported by the server.

## Full snapshots

Stop the service, copy the entire data directory or Docker volume, then restart it. Restore with the service stopped. Include the database, managed files and keys. These snapshots contain password hashes, hashed sessions and credentials; keep them private. Use a pre-upgrade snapshot with the previous image when rolling back a database migration.

SQLite uses WAL, full synchronous writes and transactions. Copying only the database file while the server is running is not a complete backup.

## Portable archives

Administration > Backups downloads a `.quire-server-backup` ZIP archive (browser limit: 512 MiB). The snapshot includes accounts/password hashes, libraries/grants/invitations, metadata, private reading records, settings, watch registrations, and every referenced managed book, including cached watched copies in their original formats. Active sessions, tracking credentials, catalog source credentials, OPDS app passwords, logs, environment files, TLS configuration and watched originals are excluded. Archives are unencrypted; keep them in protected storage away from the server.

For larger libraries, stop the server before using the CLI, then restart it after the archive completes:

```sh
quire-server backup -data ./data -output ./server.quire-server-backup
```

Restore writes only to a **new, nonexistent directory**, validates the manifest and SHA-256 checksums, and never overwrites the active server:

```sh
quire-server restore -input ./server.quire-server-backup -data ./restored-data
quire-server serve -data ./restored-data -web-dir ./web
```

Stop the previous server before starting the restored one on the same port, and retain the previous data directory until verified. Restore revokes all sessions and pauses automatic scans; check source paths and mounts before enabling scans in Administration > Settings. Cached watched books remain readable without their original mount. Supply deployment flags, environment variables and TLS/reverse-proxy settings separately. Reconnect tracking providers, re-enter catalog credentials and create new OPDS app passwords. Tracking links remain, but automatic tracking is disabled on restore.

Format 1 accepts database schema versions 6 through 14, at most 100,002 ZIP entries, a 1 GiB SQLite snapshot, 8 GiB per book and 100 GiB unpacked data. Backup uses temporary disk space for the SQLite snapshot and archive. File changes wait while a web backup is created; reading and metadata sync remain available. The command refuses to overwrite an existing archive.
