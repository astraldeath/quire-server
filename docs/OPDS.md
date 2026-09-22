# OPDS catalogs

Quire serves OPDS 1.2 Atom at `/opds` and OPDS 2.0 JSON at `/opds/v2`.
Create a named OPDS password in the reader's account settings, then enter your
Quire username and that password in an OPDS reader. The secret is displayed once;
only a SHA-256 digest is stored. Revoke individual passwords in account settings.
Disabling an account blocks all its OPDS passwords immediately. OPDS passwords
cannot sign into the normal API and do not synchronize reading positions.

Set `QUIRE_PUBLIC_URL` to your public HTTPS origin. A configured loopback HTTP
origin is accepted for local development. HTTPS termination must be enforced by
the reverse proxy; do not expose its unencrypted upstream to public clients.
Failed authentication is limited per immediate network peer, without trusting
forwarded address headers.

The root links to all books, recent books, folders, and series. Book listings use
50 items per page. Nested folders, author/title/series search, JPEG covers, and
original-file acquisitions are supported. A book must be available on disk and
currently accessible to the account. Hidden and locked books are excluded from
listings, counts, covers, and downloads. Shared-library books also honor their
owner's privacy state. Direct links are checked on every request. Privacy changes
made on an offline client only take effect here after they sync.

## External catalog sources

Saved sources belong to one account and sync through their own revisioned API.
They remain separate from reading-data records. Tombstones prevent deleted
sources from silently reappearing. A stale write returns HTTP 409; clients must
show a conflict and let the user decide which metadata to keep. Retrying an
identical operation ID is idempotent. Reusing an operation ID with a different
payload returns 409.

Hosted source credentials are encrypted using the existing server secret key
mechanism (`tracking.key`). Source reads never disclose credentials. Editing a
source path or query retains credentials within the same origin. Changing its
scheme, host, or effective port drops credentials unless replacements are explicitly supplied.
An empty username and password clears credentials; deleting a source does too.
Ordinary server backups retain source names, URLs, revisions, and tombstones,
but remove source secrets, operation history, and OPDS app passwords. Restore
requires re-entering catalog credentials and creating new app passwords.

The hosted proxy requires normal authentication, browser session binding where
applicable, and ownership of a live saved source. It supports public HTTP(S)
catalogs by default and never accepts client-supplied Authorization headers.
Credentials are sent only to the original source origin and are permanently
removed after a cross-origin redirect. HTTPS requests cannot redirect to HTTP.
Authenticated HTTP sources require an explicit allowed origin for LAN use.

Set `QUIRE_OPDS_ALLOWED_ORIGINS` to a comma-separated list of exact catalog
origins to allow private/LAN destinations, for example:

```text
QUIRE_OPDS_ALLOWED_ORIGINS=http://192.168.1.20:8080,https://catalog.home:8443
```

Paths, query strings, and wildcards are not origin entries. Every redirect and
DNS answer is validated, and the validated IP address is pinned for the actual
connection. Environment HTTP proxies are disabled. Link-local, cloud metadata,
multicast, unspecified, address-translation, and special-use addresses remain
blocked even when an origin is configured. Redirects to another private origin
require that origin to be listed separately. Limits are five redirects, 4 MiB
and 30 seconds for feeds, 8 MiB and 30 seconds for covers, and 8 GiB and 30 minutes
for streamed book transfers. Client cancellation stops the upstream transfer.
Only raster JPEG, PNG, WebP, and GIF covers are proxied; book responses download
as attachments. Catalog-side 401/403 becomes an authentication-required error.

Anonymous and HTTP Basic catalogs are supported. OAuth, DRM, lending, purchase,
and indirect-acquisition flows are outside this implementation. The browser
uses this proxy when connected; standalone browsers need catalog CORS support,
while installed clients can use their native transport.

## HTTP contract

Normal session authentication protects all `/v1` routes below. JSON writes
require `Content-Type: application/json`. Discovery advertises capability `opds`.

- `GET /v1/opds/passwords` returns `{passwords:[{id,name,createdAt}]}`.
- `POST /v1/opds/passwords` accepts `{name}` and returns HTTP 201 with
  `{id,name,createdAt,password}`. `createdAt` is Unix milliseconds. Names are
  limited to 100 bytes and each account may have 50 app passwords.
- `DELETE /v1/opds/passwords/{id}` revokes that account's password and returns 204.
- `GET /v1/catalog-sources` returns `{sources:[{id,name,url,revision,deleted}]}`,
  including tombstones.
- `PUT /v1/catalog-sources/{id}` accepts
  `{name,url,baseRevision,operationId,deleted,credentials?:{username,password}}`
  and returns `{id,name,url,revision,deleted}`. New sources use base revision 0.
  Credentials must never appear in URLs. Names are limited to 200 bytes, URLs
  to 4096 bytes, IDs to 128 bytes, and accounts to 500 sources including tombstones.
- `POST /v1/catalog-sources/{id}/fetch` accepts `{url,kind}` where kind is `feed`,
  `cover`, or `book`. Feed responses are `{body,contentType,url}`; the response URL
  is the final URL after redirects. Covers and books are binary streams. Missing
  or foreign sources return 404, rejected/unreachable/oversized/truncated feed
  responses return 502, and upstream authentication failures return 401.

Both served feed endpoints accept `view=all|recent|folders|series`, `q` for search,
`page` starting at 1, `folder` for nested folder paths, and `series` for the series
name. Atom advertises `/opds/search.xml` as its OpenSearch description. File and
cover links remain under `/opds/books/{id}/file` and `/opds/books/{id}/cover` and
use the same read-only Basic authentication as feeds.
