# API and synchronization

The [OpenAPI contract](../api/openapi.json) is licensed under MIT; the server implementation is [AGPL-3.0-only](../LICENSE). Reader clients can generate code from the contract without copying server implementation. See also the [OPDS](OPDS.md#http-contract) and [tracking](tracking.md) guides.

## File transfer

Discovery at `GET /.well-known/quire` advertises `limits.maxUploadBytes`, `limits.maxDownloadBytes` and the `server-assigned-upload` capability. Uploads default to 2 GiB; downloads retain an independent 8 GiB ceiling. See [upload configuration and format limits](OPERATIONS.md#files-and-upload-limits).

Authenticated clients can stream `application/octet-stream` to `POST /v1/books/files`. Administrators can upload to a shared library with `POST /v1/admin/libraries/{library}/books`. Both return HTTP 201 with `{bookId, size}` after computing SHA-256, validating the format and persisting availability. HTTP 413 means the configured upload limit was exceeded, including for chunked requests.

- `GET /v1/files`: list this account's available file identities and sources.
- `PUT /v1/books/{sha256}/file`: upload a raw book with a known SHA-256 identity, verified before registration. The configured upload limit applies.
- `GET /v1/books/{sha256}/file`: download an accessible book.
- `DELETE /v1/books/{sha256}/file`: remove the uploaded source only; watched-only files return 403.

Removing a file does not remove reading data. Identical watched and uploaded files remain independent sources. Interrupted uploads never become available.

## Build and update information

`GET /v1/updates` requires a signed-in session and returns `{ current: { version, revision, readerRevision }, latest: null | { version, revision, publishedAt, notes, notesUrl }, available, checkedAt, canManage, error? }`. All timestamps are RFC3339 strings; `checkedAt` is empty until a successful check. `canManage` is true only for administrators. This is read-only; there is no installation endpoint.

`available` is true only when the published revision differs and its source timestamp is newer than the installed build. Unknown local build identity returns an explanatory `error` and `available: false`; clients must not present that as up to date. Upstream failure also returns HTTP 200 with `error`, retaining the last successful `latest` and `checkedAt` if present. Clients should indicate stale results when `error` is present. The server shares a six-hour cache across accounts. Treat `notes` as plain text; `notesUrl` is restricted to this project's HTTPS GitHub URLs. Older servers return 404 for this endpoint.

## Synchronization

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

## Privacy synchronization

`POST /v1/privacy/sync`: account-private passcode verifier and book restrictions, using a separate revision. Send `{ "revision": 0, "state": null }` to read. A write sends `state: { "credential": { "salt": "32 lowercase hex characters", "hash": "64 lowercase hex characters" }, "books": { "SHA-256 book ID": "hidden" } }`. Book modes are `hidden` or `locked`; absent IDs mean normal. An empty account has `credential: null` and `books: {}`. A credential is required for any restriction and cannot be cleared after creation. The verifier uses PBKDF2-HMAC-SHA256, 600000 iterations, the UTF-8 hex salt, and a 256-bit result; plaintext passcodes are never transmitted.

Privacy responses contain `{ "revision": 1, "state": { ... }, "accepted": true }`. Writes require the current revision; a stale write returns HTTP 200 with `accepted: false` and the current snapshot without mutation. Identical current-state writes do not increment revisions. Reads ignore the supplied revision so clients can detect a restored server snapshot; future-revision writes return 409. Requests are limited to 1 MiB and 10000 book restrictions. The client merges independent edits, keeps the stricter concurrent restriction, and retries at the returned revision. A concurrent credential conflict retains the server credential and is reported to the user. Privacy records are included in server backups; no biometric preference or unlocked state is transmitted.

## Reading statistics

`POST /v1/statistics/sync`: authenticated private reading history; submit `{ "cursor": 0, "activities": [] }` and receive `{ "cursor": 0, "activities": [], "hasMore": false }`. Uses its own account-scoped cursor, with at most 100 activities submitted or returned per page and a 1 MiB request limit. Pull subsequent pages until `hasMore` is false.

Statistics activities contain a UUID `id`, SHA-256 `bookId`, integer epoch-millisecond `startedAt` and `endedAt`, `activeMs`, `words`, `sampledMs`, unique integer `chapters`, nullable number `volume`, boolean `finished`, and optional boolean `baseline` and integer `chapterThrough`. Timestamps range from 0 through 8640000000000000. Records span at most 300000 ms, with `sampledMs <= activeMs <= endedAt - startedAt`; words range from 0 through 100000 and positive words require positive sampled time. Chapters contain at most 1000 values in 1..100000; volume must be positive and at most 100000; chapterThrough ranges from 1 through 100000. Baselines have zero duration, active time, words and sampled time; only baselines may specify chapterThrough. Required fields cannot be omitted or null except volume. Unknown fields and malformed records return 400. Immutable identical retries are accepted (chapter order is insignificant); an existing identity with changed content or a future cursor returns 409, atomically rolling back the batch. History survives book removal and is included in server backups. It is private to the authenticated account, never external telemetry.
