# MangaBaka tracking

MangaBaka tracking is available in the hosted WebUI. Each user connects their own MangaBaka account.

## Linking books and series

The WebUI exposes Tracking from individual book actions and details. Each Quire
user owns their links. A book can opt into an existing series match, or retain a
separate MangaBaka entry. Grouping books does not link them automatically. **Track series** applies a match
to all current settled books in that local series in one transaction, using each
book's volume number (unknown volume leaves external volume unchanged). Individual
book overrides are preserved. New books can be included by saving the series tracker
again. Series unlink removes only series-scoped links. Linking
never edits library metadata. Unlinking never deletes the external entry.

## OAuth setup

Configure a separate OAuth client for each deployed origin. Authentication uses a server-side OAuth client with PKCE S256. Register a Web
(confidential) client on MangaBaka with authorization_code and refresh_token grants.
Set the redirect URI to `<public-url>/v1/tracking/oauth/callback`. Configure the
client in the private data directory as `mangabaka-oauth.json` with `clientId` and
`clientSecret`, then restart the server. This file is ignored by Git and excluded
from backups. Use a separate localhost client for development;
phone browsers require a reachable HTTPS public URL and registered callback.

The callback uses a short-lived, HttpOnly Lax nonce cookie and in-memory state.
It only stages the authorization code; finishing requires the original Quire
Strict cookie and session ID on a same-origin POST. State expires after ten minutes
and is consumed once. Access and rotating refresh tokens use encrypted storage.
Expired access refreshes before a tracking pass. Invalid refresh credentials
require reconnecting; transient failures back off for one minute. Existing PAT
connections remain compatible, but the UI now offers Connect MangaBaka.
The server validates the token against `/v1/my/profile`, encrypts it with AES-GCM
using a private `tracking.key`, and never returns it through the API. Disconnect
removes the token and disables automatic tracking. Server backups retain links
but omit credentials and disable auto tracking on restore.

## Automatic progress updates

Enable automatic tracking for each book you want to update. After Quire sync, the WebUI requests a
tracking pass. This uses settled reading-position records, processes one due book
per pass, and persists failures for retry after one minute. Offline reading changes
use the existing Quire sync queue. Updates resume when the WebUI reconnects; there
is no unattended server scheduler.

Starting a book can change `plan_to_read` or `considering` to `reading`. At >=99.9%
(displayed as 100%), an explicitly configured volume number can advance external
volume progress. Completion of the whole external entry is a separate book-only
option. For links without a volume, the reader sends `currentChapter` from
validated numbered table-of-contents boundaries, so opening chapter 9 advances
external chapter progress to 9. Older positions fall back to `completedChapter`,
which remains separate for completion statistics. Arbitrary EPUB sections are not
counted as chapters. The server stores a separate chapter acknowledgement so new
chapters continue syncing after the initial reading state. MangaBaka accepts up to
10000 chapters; larger local values remain saved and show a tracking error without
sending an invalid update. External progress is
never lowered; notes, ratings, and other external fields are untouched. New external
entries use the privacy choice saved with the link (private by default for legacy
clients and existing links). Automatic reads do not resume or complete paused or
dropped entries.

Discovery advertises `current-chapter`. Readers omit the optional `currentChapter`
field when syncing to older servers, retaining the local value and continuing to
send `completedChapter` for compatibility.

## Manual edits and API

The tracker editor reads and saves state, chapter/volume progress, rating, start
and finish dates, and privacy. `GET /v1/tracking/entries/{seriesId}` returns
`{entry, accountId}`; a missing provider entry returns `entry: null`. Entry fields
use MangaBaka's snake_case names; dates are normalized to `YYYY-MM-DD`.
`POST` on the same path accepts `{expectedAccountId, is_private}` to add a missing
entry as `plan_to_read`, or update only privacy on an existing entry. `PUT` accepts
`{expectedAccountId, changes}` and sends only the explicitly edited fields. A
missing entry during `PUT` must be reloaded and added again. Both writes reject
an account mismatch. Entry access requires a live link in the requesting user's
library and a connected MangaBaka account. `/v1/tracking` also returns `accountId`.

Progress and rating accept null or numbers (0–10000 for progress, 0–100 for rating);
dates accept null or a valid date in years 1679–2262. Manual state/progress changes
acknowledge the current local positions of all links to that provider series, so
automatic sync waits for subsequent reading progress. Privacy changes update all
matching local links' creation preferences. Book and series link requests accept
an optional `private` boolean; omitting it preserves an existing preference.

Normal authenticated users can access `/v1/tracking`, `/v1/tracking/search`,
`/v1/tracking/account`, `/v1/tracking/books/{id}`, `/v1/tracking/series`,
`/v1/tracking/entries/{seriesId}`, and `/v1/tracking/sync`.
Authentication and cookie session binding follow the existing Quire API.

Provider references: [API schema](https://mangabaka.org/api.json) and
[OpenID configuration](https://mangabaka.org/.well-known/openid-configuration),
checked on 2026-09-14. Chapter bounds were rechecked on 2026-09-15.

Automatic tracking requires the owner's opt-in. Automated tests use a local fake
provider and do not write to real accounts.
