# Security model

- File URLs are unguessable random IDs and public metadata reveals no object
  key.
- Direct uploads are scoped to a staging object key and expire quickly. Upload
  tokens are bearer tokens valid until the intent expires; they are never
  placed in URLs and can be replayed for completion or abort.
- Multipart part URLs are short-lived and scoped to one upload ID and part
  number. The upload ID/ETag state is server-side; the client cannot make a
  part URL target the live key. Browser resume metadata stores the upload token
  in `localStorage`, so it has the same sensitivity as the delete token.
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
  be recorded. The final `CompleteMultipartUpload` and `HeadObject` checks run
  before the live-key copy; abort/expiry call `AbortMultipartUpload`.
- First-run setup requires a one-time `SETUP_TOKEN` (or `DATA_DIR/setup_token`),
  with rate limiting, so a fresh instance cannot be claimed by a scanner.
- Admin cookies are HttpOnly, SameSite=Lax, Secure when configured, and backed
  by DB sessions with per-session CSRF tokens. Secure deployments use `__Host-`
  cookie names.
- Generated user hashes are held server-side for the one-time admin flash; the
  raw hash is not placed in a cookie.
- Forwarded client IPs are trusted only from `TRUSTED_PROXY_CIDRS`, and the
  X-Forwarded-For chain is read from right to left.
- Responses set a restrictive CSP, `nosniff`, frame denial, and
  `Referrer-Policy: no-referrer`.

See [Configuration](configuration.md) for `PUBLIC_UPLOADS_ENABLED` and
[Admin](admin.md) for approval and revocation.
