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

`RETENTION` is read from the environment when the process starts. Each committed
file gets an expiry of `completed_at + RETENTION`; changing the value applies to
new uploads, not existing rows. Expired file pages return HTTP `410`,
metadata/download endpoints refuse them, and a worker deletes the S3 object
before removing the SQLite row.

## Upload intents and the janitor

Every direct upload creates an `upload_intents` row with state `pending`. The
browser gets a presigned PUT URL and a one-time upload token. Completion performs
`HeadObject`, verifies the size, consumes the reservation, and inserts the file
row in one SQLite transaction.

When `PUBLIC_UPLOADS_ENABLED=false`, creating the intent also requires an
admin-approved user hash. Completion and abort still use only the one-time
upload token, so a slow upload is not stranded by a later approval change.

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
the real object size and deletes anything that exceeds the declared size or
`MAX_FILE_SIZE`. Keep `UPLOAD_URL_TTL` short and use a bucket lifecycle rule if
temporary cost exposure is a concern.

For extra safety, configure a bucket lifecycle rule for the configured prefix.
The application does not require it, but it limits cost if the server is offline
longer than its upload window.
