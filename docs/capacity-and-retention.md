# Capacity, uploads, and retention

`MAX_STORAGE` is tracked in SQLite. Creating an upload intent reserves the
declared size before interim signs anything, so concurrent uploads cannot both
believe they fit. The reservation is converted into a committed file only after
the object is verified. The actual object size is checked with `HeadObject` at
completion; a smaller object is accepted and a larger one is deleted.

Set `MAX_STORAGE=0` (or `MAX_STORAGE=unlimited`) to disable the global cap. The
per-file limit still applies, the API reports `storage.unlimited: true`, and the
home page hides the storage panel and capacity warning.

When a cap is set and committed storage plus reservations reaches `MAX_STORAGE`:

- `/api/v1/status` reports `accepting_uploads: false`;
- new upload intents return HTTP `507 storage_full`;
- the home page disables upload controls and shows
  **“Storage maxed out — please try again later.”**

`MAX_FILES` is a separate global cap on committed files plus active pending
intents (default 100000; set `0` to disable). It limits the metadata and
deep-sweep working set even when many tiny files would fit within
`MAX_STORAGE`.

Pending intents are also limited per owner (an authenticated key ID or an HMAC
of the client IP): `MAX_PENDING_UPLOADS_PER_CLIENT` defaults to 2 and
`MAX_RESERVED_BYTES_PER_CLIENT` defaults to `MAX_FILE_SIZE`. Set either to `0`
to disable that check. A single client therefore cannot reserve the entire
global quota with a handful of unauthenticated intents, though a distributed
attacker is still a WAF/rate-limit concern.

`RETENTION` is read from the environment when the process starts. Each committed
file gets an expiry of `completed_at + RETENTION`; changing the value applies to
new uploads, not existing rows. Expired file pages return HTTP `410`,
metadata/download endpoints refuse them, and a worker deletes the S3 object
before removing the SQLite row.

## Upload intents and the janitor

Every upload creates an `upload_intents` row with state `pending`. In single-PUT
mode the browser gets a presigned PUT URL for a staging key and a bearer upload
token valid until the intent expires; it can be replayed for completion or
abort.
Single-PUT completion performs `HeadObject` on the staging object, verifies the
size, and then uses server-side `CopyObject` to a separate live key under
`<S3_OBJECT_PREFIX>objects/<random>`. Metadata is replaced during the copy, the
reservation is consumed, and the file row is inserted with the live key in one
SQLite transaction. The staging key is deleted afterward on a best-effort basis;
the janitor removes leftovers.

A single PUT needs temporary space for both the staging object and the live copy;
keep that 2× requirement in mind when running close to `MAX_STORAGE`.

For a single PUT, the presigned URL never names the live key, so a client cannot
overwrite a committed file even while the PUT URL is still valid. For multipart,
the random staging key becomes the live key, but completion invalidates the
multipart upload ID, so the part URL cannot write to it afterward. This keeps
`MAX_STORAGE` and the recorded file size meaningful after completion. If no live
object is committed, the janitor removes leftovers.

When `PUBLIC_UPLOADS_ENABLED=false`, creating the intent also requires an
admin-approved user hash. Completion and abort still use only the upload token,
so a slow upload is not stranded by a later approval change.

### Multipart uploads

Set `MULTIPART_UPLOAD_ENABLED=true` to switch uploads above
`MULTIPART_THRESHOLD` (default 64 MiB) to S3 multipart. The default remains
single-PUT, so existing behavior is unchanged unless this is enabled.

- The server creates `CreateMultipartUpload` on the staging key and returns part
  metadata; the browser requests short-lived part URLs.
- Each part is PUT directly to storage, then its ETag and exact size are reported
  to interim. ETags are tracked in SQLite so completion and resume do not depend
  on browser memory.
- `MULTIPART_PART_SIZE` defaults to 16 MiB and must be at least 5 MiB. It is
  raised automatically when needed to stay under the 10,000-part limit. The
  expected size of every part is fixed by the server.
- With `MULTIPART_SIGN_PART_LENGTH=true`, the exact Content-Length is signed into
  each part URL. At completion, `ListParts` verifies provider-side sizes and
  ETags before assembly.
- `MULTIPART_CONCURRENCY` controls browser part parallelism (default 3).
- After all parts are reported, `CompleteMultipartUpload` assembles the staging
  object. For multipart, that staging key becomes the live object; there is no
  second `CopyObject`, which avoids the 5 GB single-copy limit. The final
  `HeadObject` size check still runs before commit.
- A failed part is retried individually. The web UI also keeps resumable session
  metadata in `localStorage`; re-selecting the same file continues from the
  parts already recorded. The upload token is stored in that browser record, so
  treat it like the delete token.
- Aborting or expiring an intent calls `AbortMultipartUpload` and cleans the
  database parts. If the provider abort fails, the intent stays pending so the
  janitor can retry instead of losing the upload ID. Configure a bucket
  lifecycle rule for incomplete multipart uploads as a backstop.
- Files above 5 GB require multipart. The provider's own object limit still
  applies; sss3 defaults `STUPID_MAX_OBJECT_SIZE` to 5 GB.

Abandoned or failed uploads are cleaned by a janitor:

- fast pass every minute: expired pending intents, expired files, expired
  sessions, expired reservations, terminal intent rows older than 24 hours;
- security pass at startup and every 30 minutes: list the configured S3 prefix,
  delete objects with no matching file row or live pending intent, and abort
  provider multipart uploads whose upload ID is not in `upload_intents`;
- at startup it runs before the HTTP listener, so old state is reconciled before
  accepting requests;
- during runtime, unknown objects get a 15-minute grace period to avoid racing a
  just-completed PUT. At startup no browser upload can be in flight, so unknown
  objects are removed immediately;
- browser cancellation calls the abort endpoint, which deletes the object and
  releases the reservation immediately;
- if completion reports an oversized or empty object, the object is deleted and
  the reservation released.

The upload token is separate from the file delete token. Both are stored only as
SHA-256 hashes. A completion retry for an already-committed intent returns the
existing file instead of duplicating it.

Because a presigned PUT cannot enforce a content-length condition at the storage
layer, a client can temporarily upload more than it declared. Completion checks
the staging object's real size and deletes anything that exceeds the declared
size or `MAX_FILE_SIZE`. After commit, only the untouched live copy is served.
Keep `UPLOAD_URL_TTL` short and use a bucket lifecycle rule if temporary staging
cost exposure is a concern.

For extra safety, configure a bucket lifecycle rule for the configured prefix.
The application does not require it, but it limits cost if the server is offline
longer than its upload window.
