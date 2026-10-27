# B2 private bucket with Cloudflare Worker downloads

For Backblaze B2, downloads can be proxied by a Cloudflare Worker so B2 →
Cloudflare traffic is covered by the Bandwidth Alliance while the bucket stays
private. The Worker must stream the bytes; a Worker that only signs a B2 URL and
redirects does not get the free-egress treatment.

```text
browser -> interim /files/{id}/download
        -> 303 https://dl1.example.com/d/<token>
        -> Cloudflare Worker
        -> private B2 object
        -> browser
```

Uploads still go directly to B2 with presigned PUTs, so the Cloudflare 100 MB
request-body limit does not apply to uploads.

Configure interim:

```dotenv
DOWNLOAD_WORKER_ENABLED=true
DOWNLOAD_WORKER_URLS=https://dl1.example.com,https://dl2.example.com
DOWNLOAD_WORKER_SECRET=<openssl rand -base64 48>

S3_ENDPOINT=https://s3.us-west-004.backblazeb2.com
S3_PUBLIC_ENDPOINT=https://s3.us-west-004.backblazeb2.com
S3_REGION=us-west-004
S3_BUCKET=interim
S3_FORCE_PATH_STYLE=true
S3_AUTO_CREATE_BUCKET=false
```

- `DOWNLOAD_WORKER_ENABLED=false` keeps the existing presigned S3 GET behavior.
- `DOWNLOAD_WORKER_URLS` is rotated for new download links. All workers must
  share the same `DOWNLOAD_WORKER_SECRET` and read the same private B2 bucket.
- The token includes the object key, filename, and expiry. The Worker verifies
  it, signs a path-style B2 request, and streams the response.
- The Worker sets `Cache-Control: private, no-store`; do not cache tokenized
  downloads.
- `S3_PUBLIC_ENDPOINT` remains the B2 endpoint for uploads, not the Worker.
- The Worker sets `Content-Disposition`, supports `HEAD`, and forwards `Range`
  so large downloads can resume.
- `B2_KEY_PREFIX` is required and must end with `/`; the Worker fails closed
  outside that prefix.
- Worker URLs must use HTTPS. `DOWNLOAD_WORKER_URLS` rejects plain HTTP.
- A token can be replayed until it expires, and every range/HEAD request counts
  against the Worker quota. Add a Cloudflare rate-limiting rule for `/d/*`
  (for example 60 requests/minute per IP), keep `DOWNLOAD_URL_TTL` short, and
  monitor request volume. interim clamps a token's expiry to the file's own
  `expires_at`, so a link cannot outlive retention by a full TTL.
- The Worker rejects token keys containing `..`, empty segments, backslashes, or
  control characters before signing a B2 request. Keep `B2_KEY_PREFIX` scoped to
  the bucket prefix as defence in depth.

Deploy the Worker from [worker/README.md](../worker/README.md). It includes the
wrangler config, read-only B2 key scope, custom-domain setup, and the
multi-account/multi-URL deployment notes.

Cloudflare's free Worker allowance is per account, not per Worker. Multiple
Worker URLs are useful for separate deployments or redundancy, but using extra
accounts to evade the free allowance is a terms-of-service risk. Cloudflare's
large-file/video delivery policy still applies to the download domain.

The Worker is stateless and cannot revoke an issued token early; expiry is the
revocation mechanism. Treat `/d/*` as public and rate-limit it at Cloudflare.
Deleting the file removes the object, but a token already issued remains valid
(and returns 404) until it expires.
