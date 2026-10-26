# Deployment notes

- Put interim behind a TLS-terminating reverse proxy and set
  `PUBLIC_BASE_URL` to the HTTPS origin. The checked-in Compose file binds the
  Go listener to `127.0.0.1:8080`; keep it off public interfaces. If the proxy
  is another container, attach it to the Compose network instead of publishing
  the app port publicly.
- Set `TRUST_PROXY=true` only behind a proxy you control, and list the proxy
  source addresses in `TRUSTED_PROXY_CIDRS` (for example
  `127.0.0.1/32,::1/128`). Forwarded headers from other peers are ignored.
- Keep the one-time `SETUP_TOKEN` (or `DATA_DIR/setup_token`) private. The file
  is removed after the first admin is created.
- The S3 bucket must be browser-reachable at `S3_PUBLIC_ENDPOINT`, with the CORS
  policy in [S3-compatible storage](storage.md). The bucket itself stays
  private; only presigned PUTs and short-lived download URLs are exposed.
- Server-side `HeadObject`, `CopyObject`, `ListObjectsV2`, and delete calls use
  `S3_ENDPOINT`. On sss3 with the public CopyObject block, this endpoint must be
  internal or otherwise exempt from that block.
- Run one instance per SQLite database. The reservation and intent state
  machines rely on SQLite's single-writer behaviour.
- Use a dedicated `S3_OBJECT_PREFIX`. The janitor treats objects under that
  prefix as interim-owned and deletes ones with no matching file or live intent.
- Keep the bucket private. Do not enable public ACLs or public bucket policies.
- The Docker example uses a read-only root filesystem, a writable `/data`
  volume, and a non-root user.
- Back up the `/data` volume. Only S3 holds file bytes; SQLite holds the mapping,
  expiry, delete-token hashes, sessions, quotas, upload intents, setup state,
  and admin credentials.
- The HTTP server uses 10s header, 30s read, 30s write, and 60s idle timeouts.
  File bytes go directly to S3, so these do not constrain large uploads.
- Completion detaches from the request context and clears the per-response write
  deadline, with a 30-minute background deadline. A proxy can still time out
  before the response arrives; completion is idempotent, so retrying it is safe.
  Graceful shutdown waits up to two minutes; Compose uses a 150s stop grace.
- HTML references static assets with a content hash (`app.js?v=...`). A new
  build gets new asset URLs, so Cloudflare/browser caches cannot serve an old
  JavaScript client after a deploy. Static assets are immutable-cached under
  those versioned URLs; rebuild the image to regenerate the hash.
- A single PUT temporarily needs space for both the staging object and the live
  copy. Multipart uses the staging key as the live object and avoids that second
  copy.
- For production, pin container images by digest (especially the sss3 image,
  which holds the data and RW credential) and pin GitHub Actions to commit SHAs
  rather than mutable major tags.
- Add a Cloudflare rate limit for the Worker download path `/d/*`; tokens are
  bearer credentials until they expire.

For route-specific deployment, see [sss3 with Cloudflare and Caddy](sss3-cloudflare.md),
[Tailscale-only sss3](tailscale-sss3.md), or
[B2 private bucket with Cloudflare Worker](b2-worker.md). The security
properties are summarized in [Security model](security.md).
