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

Every direct upload creates an `upload_intents` row with state `pending`. The
browser gets a presigned PUT URL for a staging key and a bearer upload token
valid until the intent expires; it can be replayed for completion or abort.
Completion performs `HeadObject` on the staging object, verifies the size, and
then uses server-side `CopyObject` to a separate live key under
`<S3_OBJECT_PREFIX>objects/<random>`. Metadata is replaced during the copy, the
reservation is consumed, and the file row is inserted with the live key in one
SQLite transaction. The staging key is deleted afterward on a best-effort basis;
the janitor removes leftovers.

The presigned URL never names the live key, so a client cannot overwrite a
committed file even while the PUT URL is still valid. This keeps `MAX_STORAGE`
and the recorded file size meaningful after completion. If no live copy is
committed, the janitor removes leftovers.

When `PUBLIC_UPLOADS_ENABLED=false`, creating the intent also requires an
admin-approved user hash. Completion and abort still use only the upload token,
so a slow upload is not stranded by a later approval change.

Abandoned or failed uploads are cleaned by a janitor:

- fast pass every minute: expired pending intents, expired files, expired
  sessions, expired reservations, terminal intent rows older than 24 hours;
- security pass at startup and every 30 minutes: list the configured S3 prefix
  and delete objects with no matching file row or live pending intent;
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
