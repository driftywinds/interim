# Configuration

All runtime configuration is read from the environment. `.env` is loaded
automatically for local runs, and Compose uses it via `env_file`. See
[.env.example](../.env.example) for commented defaults.

| Variable | Required | Default | Notes |
| --- | --- | --- | --- |
| `PUBLIC_BASE_URL` | yes | — | Public origin used in links and API examples. No trailing slash or path prefix. |
| `LISTEN_ADDR` | no | `:8080` | HTTP listen address. |
| `DATA_DIR` | no | `./data` | Directory for SQLite and generated files. |
| `DATABASE_PATH` | no | `DATA_DIR/interim.db` | Explicit SQLite path. |
| `SESSION_SECRET` | no | auto-generated | At least 32 chars. If absent, a 0600 secret is generated in `DATA_DIR`. |
| `SETUP_TOKEN` | no | auto-generated | One-time token required to create the first admin. At least 32 chars. If absent, `DATA_DIR/setup_token` is generated (0600) and deleted after setup. |
| `COOKIE_SECURE` | no | `auto` | `auto` follows the scheme of `PUBLIC_BASE_URL`. `__Host-` cookie names are used when secure. |
| `TRUST_PROXY` | no | `false` | Trust forwarded headers from peers in `TRUSTED_PROXY_CIDRS`. |
| `TRUSTED_PROXY_CIDRS` | with `TRUST_PROXY` | — | Comma/space-separated proxy CIDRs or IPs. Required and validated when `TRUST_PROXY=true`. |
| `ADMIN_SESSION_TTL` | no | `12h` | 15m–168h. |
| `LOG_LEVEL` | no | `info` | `debug`, `info`, `warn`, `error`. |
| `S3_ENDPOINT` | AWS no | — | Include scheme. Leave empty for AWS S3. Used for server-side Head/Copy/List/Delete. |
| `S3_PUBLIC_ENDPOINT` | no | `S3_ENDPOINT` | Endpoint embedded in presigned URLs; must be browser-reachable. |
| `S3_REGION` | no | `us-east-1` | Any non-empty value for servers that ignore regions. |
| `S3_BUCKET` | yes | — | Must be a bare bucket name. |
| `S3_ACCESS_KEY` / `S3_SECRET_KEY` | AWS no | — | Leave both empty to use the instance-role/default credential chain. |
| `S3_PUT_ACCESS_KEY` / `S3_PUT_SECRET_KEY` | no | — | Optional write-only identity used only for presigned browser PUTs. Required for multipart unless the next flag is set. |
| `S3_PUBLIC_ENDPOINT_BLOCKS_COPY` | no | `false` | Set true only when the public endpoint rejects copy headers. Allows multipart without write-only upload credentials. |
| `S3_FORCE_PATH_STYLE` | no | `true` | Keep true for sss3/MinIO/Garage; false for AWS. |
| `S3_AUTO_CREATE_BUCKET` | no | `false` | Attempts `CreateBucket` if missing. Prefer pre-creating it. |
| `S3_OBJECT_PREFIX` | no | `files` | Key prefix; normalized with a trailing `/`. |
| `PUBLIC_UPLOADS_ENABLED` | no | `true` | `false` restricts upload intents to admin-approved user hashes and disables public hash creation. Existing hashes start unapproved. |
| `UPLOAD_URL_TTL` | no | `30m` | Presigned PUT / part URL lifetime. 5m–2h. |
| `UPLOAD_INTENT_TTL` | no | `45m` | Deadline for completion; must be at least `UPLOAD_URL_TTL`. Max 6h. |
| `MULTIPART_UPLOAD_ENABLED` | no | `false` | Enable S3 multipart uploads for files above `MULTIPART_THRESHOLD`. |
| `MULTIPART_THRESHOLD` | no | `64MB` | Size above which multipart is used. `0` makes every upload multipart. Max 5GB. |
| `MULTIPART_PART_SIZE` | no | `16MB` | Requested part size. 5MB–5GB; raised automatically for the 10,000-part limit. |
| `MULTIPART_CONCURRENCY` | no | `3` | Browser part parallelism, 1–8. |
| `MULTIPART_SIGN_PART_LENGTH` | no | `true` | Sign the exact part Content-Length into presigned URLs. Disable only if the provider cannot verify signed Content-Length. |
| `MAX_PENDING_UPLOADS_PER_CLIENT` | no | `2` | Per-key or per-IP pending intent cap. `0` disables. |
| `MAX_RESERVED_BYTES_PER_CLIENT` | no | `MAX_FILE_SIZE` | Per-key or per-IP reserved-byte cap. `0` disables. |
| `MAX_STORAGE` | no | `20GB` | Hard total cap, binary units. Set to `0` or `unlimited` to disable the global cap. |
| `MAX_FILE_SIZE` | no | `5GB` | Hard per-file cap. Cannot exceed `MAX_STORAGE` when a cap is set. Files above 5GB require multipart uploads. |
| `MAX_FILES` | no | `100000` | Global committed-file count cap. `0` disables. |
| `RETENTION` | no | `7d` | File lifetime. Supports `7d`, `30d`, `1.5d`, or Go durations like `12h`. 1h–3650d. Existing files keep their original expiry. |
| `DOWNLOAD_URL_TTL` | no | `5m` | 30s–1h. Also the worker download token lifetime. |
| `DOWNLOAD_WORKER_ENABLED` | no | `false` | Use Cloudflare Worker download URLs instead of presigned S3 GETs. |
| `DOWNLOAD_WORKER_URLS` | worker mode yes | — | Comma/space-separated HTTPS worker base URLs. The `/d/<token>` path is appended. |
| `DOWNLOAD_WORKER_SECRET` | worker mode yes | — | At least 32 characters; must match the worker's `DOWNLOAD_TOKEN_SECRET`. |

For storage endpoint details and provider-specific settings, see
[S3-compatible storage](storage.md). For Worker-backed downloads, see
[B2 private bucket with Cloudflare Worker](b2-worker.md). For quota and expiry
behavior, see [Capacity, uploads, and retention](capacity-and-retention.md).
