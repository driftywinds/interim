# Security model

- File URLs are unguessable random IDs and public metadata reveals no object
  key.
- Direct uploads are scoped to a staging object key and expire quickly. Upload
  tokens are bearer tokens valid until the intent expires; they are never
  placed in URLs and can be replayed for completion or abort.
- Multipart part URLs are short-lived and scoped to one upload ID and part
  number. Part URLs target the server-selected random staging key and cannot be
  pointed at another object. After completion the upload ID is invalidated, so
  the same URL cannot overwrite the committed object. Multipart requires either
  a write-only upload identity or an explicit
  `S3_PUBLIC_ENDPOINT_BLOCKS_COPY=true` assertion for a proxy that rejects
  copy/ACL/tagging/object-lock headers. Browser resume metadata stores the
  upload token in `localStorage`, so it has the same sensitivity as the delete
  token.
- Download links are short-lived signed S3 GETs or worker-proxied token URLs;
  objects are private.
- User hashes, upload tokens, and delete tokens are hashed at rest.
- With public uploads disabled, upload intents require a valid, non-revoked,
  admin-approved hash. Existing hashes are not grandfathered in; approval is an
  explicit admin action.
- Uploads are capped and capacity-reserved before signing. Actual object size is
  verified with `HeadObject` on a staging key, then copied to a live key that
  the client never sees. A committed file cannot be overwritten with the still-
  valid staging PUT URL.
- Multipart completion requires every part number from 1 through `part_count` to
  be recorded with its exact expected size. Part URLs sign Content-Length by
  default, and completion verifies provider-side sizes/ETags with `ListParts`
  before assembly. The final `HeadObject` size check runs before commit; abort
  and expiry call `AbortMultipartUpload`.
- Multipart staging becomes the live object after `CompleteMultipartUpload`.
  There is no second `CopyObject` for large files, avoiding the 5 GB copy limit.
  Single PUTs still need temporary space for both the staging object and the
  live copy, so size `MAX_STORAGE` with that 2× requirement in mind.
- First-run setup requires a one-time `SETUP_TOKEN` (or `DATA_DIR/setup_token`),
  with rate limiting, so a fresh instance cannot be claimed by a scanner.
- Admin cookies are HttpOnly, SameSite=Lax, Secure when configured, and backed
  by DB sessions with per-session CSRF tokens. Secure deployments use `__Host-`
  cookie names.
- Generated user hashes are held server-side for the one-time admin flash; the
  raw hash is not placed in a cookie.
- Forwarded client IPs are trusted only from `TRUSTED_PROXY_CIDRS`, and the
  X-Forwarded-For chain is read from right to left.
- Multipart part URL/report/status/complete/abort endpoints have per-intent and
  per-IP rate budgets. Abort failures keep the intent pending rather than losing
  the provider upload ID; the janitor retries and also aborts unknown provider
  multipart uploads under the configured prefix.
- Responses set a restrictive CSP, `nosniff`, frame denial, and
  `Referrer-Policy: no-referrer`.

See [Configuration](configuration.md) for `PUBLIC_UPLOADS_ENABLED` and
[Admin](admin.md) for approval and revocation.
