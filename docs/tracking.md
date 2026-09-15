# MangaBaka tracking

Optional hosted WebUI integration. Configure a separate OAuth client for each deployed origin.

The WebUI exposes Tracking from individual book actions and details. Each Quire
user owns their links. A book can opt into an existing series match, or retain a
separate MangaBaka entry. Grouping books does not link them automatically. Track series explicitly applies a match
to all current settled books in that local series in one transaction, using each
book's volume number (unknown volume leaves external volume unchanged). Individual
book overrides are preserved. New books can be included by saving the series tracker
again. Series unlink removes only series-scoped links. Linking
never edits library metadata. Unlinking never deletes the external entry.

Authentication uses a server-side OAuth client with PKCE S256. Register a Web
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

Automatic tracking is explicit per book. After Quire sync, the WebUI requests a
tracking pass. This uses settled reading-position records, processes one due book
per pass, and persists failures for retry after one minute. Offline reading changes
use the existing Quire sync queue. Updates resume when the WebUI reconnects; there
is no unattended server scheduler yet.

Starting a book can change `plan_to_read` or `considering` to `reading`. At >=99.9%
(displayed as 100%), an explicitly configured volume number can advance external
volume progress. Completion of the whole external entry is a separate book-only
option. No chapter counts are inferred from EPUB sections. External progress is
never lowered; notes, ratings, and other external fields are untouched. New external
entries are private. Automatic reads do not resume paused or dropped entries.

Normal authenticated users can access `/v1/tracking`, `/v1/tracking/search`,
`/v1/tracking/account`, `/v1/tracking/books/{id}`, and `/v1/tracking/sync`.
Authentication and cookie session binding follow the existing Quire API.

Provider contracts checked against https://mangabaka.org/api.json and
https://mangabaka.org/.well-known/openid-configuration on 2026-09-14.

Native iOS/desktop tracking controls and chapter mapping are not
included in this WebUI integration. Real-account writes require the owner's authorization;
automated tests use a local fake provider.
