# Security model

- File URLs are unguessable random IDs and public metadata reveals no object
  key.
- Direct uploads are scoped to a staging object key and expire quickly. Upload
  tokens are bearer tokens valid until the intent expires; they are never
  placed in URLs and can be replayed for completion or abort.
- Single-PUT URLs sign the declared `Content-Length`; completion also requires
  the object size to match exactly. A client cannot reserve one byte and PUT a
  multi-gigabyte object.
- Multipart part URLs are short-lived and scoped to one upload ID and part
  number. Part URLs target the server-selected random staging key and cannot be
  pointed at another object. After completion the upload ID is invalidated, so
  the same URL cannot overwrite the committed object. Multipart requires either
  a write-only upload identity or an explicit
  `S3_PUBLIC_ENDPOINT_BLOCKS_COPY=true` assertion for a proxy that strips every
  `X-Amz-*` request header. That flag is trusted, not verified; the write-only
  identity is the robust option. Browser resume metadata stores the upload token
  in `localStorage`, so it has the same sensitivity as the delete token.
- Download links are short-lived signed S3 GETs or worker-proxied token URLs;
  objects are private.
- User hashes, upload tokens, and delete tokens are hashed at rest.
- With public uploads disabled, upload intents require a valid, non-revoked,
  admin-approved hash. Existing hashes are not grandfathered in; approval is an
  explicit admin action.
- Anonymous and self-service (unapproved) hashes share the per-IP reservation
  bucket. Only admin-approved hashes get an independent quota, so minting new
  hashes cannot multiply the pending/byte caps.
- Uploads are capped and capacity-reserved before signing. Actual object size is
  verified with `HeadObject` on a staging key, then copied to a live key that
  the client never sees. The copy carries `CopySourceIfMatch` with the verified
  ETag, closing the Head/Copy race. A committed file cannot be overwritten with
  the still-valid staging PUT URL. The staging key itself is protected and its
  reservation is held until that URL expires, so a post-terminal PUT cannot
  create untracked bytes outside `MAX_STORAGE`.
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
  cookie names. Sessions have absolute and idle timeouts; `/admin/logout-all`
  revokes every session for the admin. Changing the password verifies the
  current password, rehashes with bcrypt cost 12, and revokes all sessions.
- Login has a per-IP counter and a separate per-account counter. Both use a
  second limiter that a flood cannot evict; failed-login logs contain a hash of
  the attempted username, not the raw text. Passwords must be at least 12
  characters.
- Generated user hashes are held server-side for the one-time admin flash; the
  raw hash is not placed in a cookie.
- The anonymous user hash, per-file delete tokens, and resumable multipart upload
  tokens are stored in `localStorage` on the interim origin. Any script running
  there can read them, so the strict CSP (`script-src 'self'`, no inline scripts)
  and template escaping are load-bearing. There is no HttpOnly alternative for
  anonymous bearer credentials without an account/login system.
- Forwarded client IPs are trusted only from `TRUSTED_PROXY_CIDRS`, and the
  X-Forwarded-For chain is read from right to left. `X-Real-IP` is never used as
  a fallback. IPv6 clients are collapsed to their /64 for rate-limit and quota
  keys, so rotating within a /64 does not reset the counters.
- When `TRUSTED_CLIENT_IP_HEADER` is set, that header is used only after the
  connecting peer matches `TRUSTED_PROXY_CIDRS`, then the value must parse as an
  IP. This covers reverse proxies that overwrite
  `X-Forwarded-For` with their own peer address but pass
  `CF-Connecting-IP` unchanged. Do not set it to a client-supplied header on an
  origin that is reachable without the trusted proxy.
- Access logs hash/remove file and upload IDs, and failed-login logs omit the
  attempted username. `/healthz` only reveals database/storage detail to a
  loopback caller.
- Startup orphan sweeping is opt-in (`SWEEP_ON_STARTUP=false` by default) and
  refuses to run when the database knows no objects under the prefix or when
  more than half of the listed objects are unknown. Use a dedicated bucket and
  prefix; never share them with another instance or application.
- Multipart part URL/report/status/complete/abort endpoints have per-intent and
  per-IP rate budgets. Abort failures keep the intent pending rather than losing
  the provider upload ID; the janitor retries and also aborts unknown provider
  multipart uploads under the configured prefix.
- Responses set a restrictive CSP, `nosniff`, frame denial, and
  `Referrer-Policy: no-referrer`. `connect-src` is narrowed to the configured S3
  origin, or the regional AWS S3 host when `S3_PUBLIC_ENDPOINT` is empty. HSTS
  is sent without `includeSubDomains`.
- `PUBLIC_UPLOADS_ENABLED=true` is an open-hosting mode. It has no content
  scanning or abuse-report workflow; use the allowlist mode for a private
  instance and plan for abuse handling if the origin is public.

See [Configuration](configuration.md) for `PUBLIC_UPLOADS_ENABLED` and
[Admin](admin.md) for approval and revocation.
