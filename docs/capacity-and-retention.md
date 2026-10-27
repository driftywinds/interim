# Capacity, uploads, and retention

`MAX_STORAGE` is tracked in SQLite. Creating an upload intent reserves the
declared size before interim signs anything, so concurrent uploads cannot both
believe they fit. The reservation is converted into a committed file only after
the object is verified. The actual object size is checked with `HeadObject` at
completion and must match the declared size exactly; a mismatch is rejected and
the staging object is deleted.

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

Pending intents are also limited per owner: an admin-approved key ID gets its
own bucket, while anonymous and self-service (unapproved) hashes share an HMAC
of the client network. `MAX_PENDING_UPLOADS_PER_CLIENT` defaults to 2 and
`MAX_RESERVED_BYTES_PER_CLIENT` defaults to `MAX_FILE_SIZE`. Set either to `0`
to disable that check. Minting new self-service hashes therefore cannot
multiply the reservation cap, though a distributed attacker is still a
WAF/rate-limit concern.

`RETENTION` is read from the environment when the process starts. Each committed
file gets an expiry of `completed_at + RETENTION` unless its uploader's user
hash has an override; changing the instance value applies to new uploads, not
existing rows. Expired file pages return HTTP `410`, metadata/download endpoints
refuse them, and a worker claims the SQLite row, deletes the S3 object, then
removes the row. A failed object delete releases the claim so the next janitor
pass retries.

### Per-hash limits

Admin-created user hashes can carry a per-file maximum and a retention override.
Both are edited in `/admin` under **User hashes**:

- `max_file_size`: defaults to `MAX_FILE_SIZE`; `0` means unlimited for that
  hash. A per-hash grant can exceed the instance default, but the provider's
  single-PUT or multipart object limit and `MAX_STORAGE` still apply.
- `retention`: defaults to `RETENTION`; `0` means files uploaded with that hash
  never expire. Positive values are captured into `files.retention_seconds` when
  the upload commits, so later edits do not rewrite existing files.

Anonymous uploads use the instance defaults. Self-service hashes created through
the public API are created with the current instance defaults and do not change
when the instance settings change. Hashes that predate this feature have no
stored override and inherit the current instance values. The per-hash file-size
limit is checked both when the upload intent is created and when it completes.

An unlimited-retention file is stored with a far-future expiry sentinel. It
still counts toward `MAX_STORAGE` and `MAX_FILES`, and the admin can delete it
manually. Sliding retention uses the per-file snapshot; a file from a hash with
`retention=0` is never extended or expired.

### Sliding retention from the last download

Set `RETENTION_EXTEND_ON_DOWNLOAD=true` to make retention relative to the last
download instead of upload time. Every successful `GET /f/{id}/download` or
`POST /api/v1/files/{id}/download` moves `expires_at` to `now + RETENTION`, or to
the uploader hash's per-file retention snapshot when one exists. There is no
separate download record; the new expiry is the last-download timestamp.
Existing files keep their current expiry until their next download.

Downloads go directly from the browser to storage, so interim never sees the
bytes arrive. The extension is triggered when interim issues the signed link,
which means a link prefetch or an abandoned download extends the file just like
a completed one. Two consequences are worth planning for:

- A client can keep its own file alive indefinitely by downloading it before
  each deadline. That is the intended Buzzheavier-style behavior; `MAX_STORAGE`
  and `MAX_FILES` still cap the instance as a whole.
- The file page countdown reflects the expiry at render time. A download
  triggered from that page extends the stored deadline, but the already-rendered
  countdown does not change until the page is refreshed.

The janitor still deletes the object once the refreshed deadline passes. It
claims the expired row before deleting the object, and the same claim blocks a
concurrent extension, so a download that races the janitor cannot leave a live
row pointing at missing bytes or lose a file that was just extended.

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

- fast pass every minute: expired pending intents, expired files, expired and
  idle sessions, expired reservations, terminal intent rows older than 24 hours;
- security pass every 30 minutes: list the configured S3 prefix, delete objects
  with no matching file row or live pending intent, and abort provider
  multipart uploads whose upload ID is not in `upload_intents`;
- the startup pass is opt-in (`SWEEP_ON_STARTUP=true`). Leave it off unless the
  bucket/prefix is dedicated to this database, because a restored-old or fresh
  database would otherwise treat live objects as orphans;
- both startup and runtime sweeps refuse to delete anything when the database
  knows no objects under the prefix, or when more than half of the listed objects
  are unknown;
- during runtime, unknown objects get a 15-minute grace period to avoid racing a
  just-completed PUT;
- browser cancellation calls the abort endpoint, which deletes the object and
  releases the reservation immediately;
- if completion reports an oversized or empty object, the object is deleted and
  the reservation released.

The upload token is separate from the file delete token. Both are stored only as
SHA-256 hashes. A completion retry for an already-committed intent returns the
existing file instead of duplicating it.

The single-PUT URL signs the declared `Content-Length`, so the provider rejects
a body of a different size. Completion still checks the staging object's exact
size (and `MAX_FILE_SIZE`) before copying. The uploader's PUT URL remains valid
until `UPLOAD_URL_TTL`, but it can only write the declared number of bytes to
the staging key, and that key is deleted after commit. Keep the TTL short and use
a bucket lifecycle rule if temporary staging cost exposure is a concern.

For extra safety, configure a bucket lifecycle rule for the configured prefix.
The application does not require it, but it limits cost if the server is offline
longer than its upload window.
