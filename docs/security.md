# Security model

- File URLs are unguessable random IDs and public metadata reveals no object
  key.
- Direct uploads are scoped to one object key and expire quickly; upload tokens
  are one-time and never placed in URLs.
- Download links are short-lived signed S3 GETs or worker-proxied token URLs;
  objects are private.
- User hashes, upload tokens, and delete tokens are hashed at rest.
- With public uploads disabled, upload intents require a valid, non-revoked,
  admin-approved hash. Existing hashes are not grandfathered in; approval is an
  explicit admin action.
- Uploads are capped and capacity-reserved before signing. Actual object size is
  verified with `HeadObject` before a file is committed.
- Admin cookies are HttpOnly, SameSite=Lax, Secure when configured, and backed
  by DB sessions with per-session CSRF tokens.
- Responses set a restrictive CSP, `nosniff`, frame denial, and
  `Referrer-Policy: no-referrer`.

See [Configuration](configuration.md) for `PUBLIC_UPLOADS_ENABLED` and
[Admin](admin.md) for approval and revocation.
