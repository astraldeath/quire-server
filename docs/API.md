# API and synchronization

The reusable contract is [api/openapi.json](../api/openapi.json), licensed separately under MIT. Server implementation is AGPL-3.0-only; see [LICENSE](../LICENSE). Contract code can be generated for the MIT reader without copying server implementation.

- `POST /v1/sessions`: username, password, deviceName; returns a bearer token once.
- `GET /v1/sessions`, `DELETE /v1/sessions/{id}`: list/revoke your devices.
- `POST /v1/sync`: submit up to 50 operations and read up to 100 changes.
- `POST /v1/statistics/sync`: authenticated private reading history; submit `{ "cursor": 0, "activities": [] }` and receive `{ "cursor": 0, "activities": [], "hasMore": false }`. Uses its own account-scoped cursor, with at most 100 activities submitted or returned per page and a 1 MiB request limit. Pull subsequent pages until `hasMore` is false.

Statistics activities contain a UUID `id`, SHA-256 `bookId`, integer epoch-millisecond `startedAt` and `endedAt`, `activeMs`, `words`, `sampledMs`, unique integer `chapters`, nullable number `volume`, boolean `finished`, and optional boolean `baseline` and integer `chapterThrough`. Timestamps range from 0 through 8640000000000000. Records span at most 300000 ms, with `sampledMs <= activeMs <= endedAt - startedAt`; words range from 0 through 100000 and positive words require positive sampled time. Chapters contain at most 1000 values in 1..100000; volume must be positive and at most 100000; chapterThrough ranges from 1 through 100000. Baselines have zero duration, active time, words and sampled time; only baselines may specify chapterThrough. Required fields cannot be omitted or null except volume. Unknown fields and malformed records return 400. Immutable identical retries are accepted (chapter order is insignificant); an existing identity with changed content or a future cursor returns 409, atomically rolling back the batch. History survives book removal and is included in server backups. It is private to the authenticated account, never external telemetry.

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
