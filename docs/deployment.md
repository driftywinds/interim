# Deployment notes

- Put interim behind a TLS-terminating reverse proxy and set
  `PUBLIC_BASE_URL` to the HTTPS origin. Set `TRUST_PROXY=true` only when the
  proxy is trusted; it is needed for correct client-IP rate limiting.
- The S3 bucket must be browser-reachable at `S3_PUBLIC_ENDPOINT`, with the CORS
  policy in [S3-compatible storage](storage.md). The bucket itself stays
  private; only presigned PUTs and short-lived download URLs are exposed.
- Run one instance per SQLite database. The reservation and intent state
  machines rely on SQLite's single-writer behaviour.
- Use a dedicated `S3_OBJECT_PREFIX`. The janitor treats objects under that
  prefix as interim-owned and deletes ones with no matching file or live intent.
- Keep the bucket private. Do not enable public ACLs or public bucket policies.
- The Docker example uses a read-only root filesystem, a writable `/data`
  volume, and a non-root user.
- Back up the `/data` volume. Only S3 holds file bytes; SQLite holds the mapping,
  expiry, delete-token hashes, sessions, quotas, upload intents, and admin
  credentials.

For route-specific deployment, see [sss3 with Cloudflare and Caddy](sss3-cloudflare.md),
[Tailscale-only sss3](tailscale-sss3.md), or
[B2 private bucket with Cloudflare Worker](b2-worker.md). The security
properties are summarized in [Security model](security.md).
