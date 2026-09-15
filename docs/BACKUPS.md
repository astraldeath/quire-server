# Server backups

SQLite uses WAL, full synchronous writes, transactions and a schema version. Keep the complete persistent data directory private. To back up this initial server, stop it, copy the entire data directory (or Docker volume), then restart it. Restore with the service stopped. A raw server backup contains password hashes and hashed sessions and needs protection. Reader `.quire-backup` files are a separate format and are not imported by this service.

## Portable archives

Administration → Backups downloads a complete `.quire-server-backup` ZIP archive (browser limit: 512 MB). The snapshot includes accounts/password hashes, libraries/grants/invitations, metadata, private reading records, settings, watch registrations, and every referenced managed EPUB—including cached watched copies. Active sessions, logs, environment files, TLS configuration, and watched originals are excluded. Archives are unencrypted; keep them in protected storage away from the server.

For larger libraries, stop the server before using the CLI, then restart it after the archive completes:

```sh
quire-server backup -data ./data -output ./server.quire-server-backup
```

Restore writes only to a **new, nonexistent directory**, validates the manifest and SHA-256 checksums, and never overwrites the active server:

```sh
quire-server restore -input ./server.quire-server-backup -data ./restored-data
quire-server serve -data ./restored-data -web-dir ./web
```

Stop the previous server before starting the restored one on the same port, and retain the previous data directory until verified. Restore revokes all sessions and pauses automatic scans; check source paths and mounts before enabling scans in Administration → Settings. Cached watched books remain readable without their original mount. Deployment flags, environment variables, and TLS/reverse-proxy settings must be supplied separately.

Format 1 supports this server's schema version 7, at most 100,002 ZIP entries, a 1 GiB SQLite snapshot, and 100 GiB unpacked data. Backup uses temporary disk space for the SQLite snapshot and archive. File changes wait while a web backup is created; reading and metadata sync remain available. The command refuses to overwrite an existing archive.
