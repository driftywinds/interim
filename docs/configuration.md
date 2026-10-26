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
| `COOKIE_SECURE` | no | `auto` | `auto` follows the scheme of `PUBLIC_BASE_URL`. |
| `TRUST_PROXY` | no | `false` | Trust `X-Forwarded-For` / `X-Real-IP` for rate limits. |
| `ADMIN_SESSION_TTL` | no | `12h` | 15m–168h. |
| `LOG_LEVEL` | no | `info` | `debug`, `info`, `warn`, `error`. |
| `S3_ENDPOINT` | AWS no | — | Include scheme. Leave empty for AWS S3. |
| `S3_PUBLIC_ENDPOINT` | no | `S3_ENDPOINT` | Endpoint embedded in presigned URLs; must be browser-reachable. |
| `S3_REGION` | no | `us-east-1` | Any non-empty value for servers that ignore regions. |
| `S3_BUCKET` | yes | — | Must be a bare bucket name. |
| `S3_ACCESS_KEY` / `S3_SECRET_KEY` | AWS no | — | Leave both empty to use the instance-role/default credential chain. |
| `S3_FORCE_PATH_STYLE` | no | `true` | Keep true for sss3/MinIO/Garage; false for AWS. |
| `S3_AUTO_CREATE_BUCKET` | no | `false` | Attempts `CreateBucket` if missing. Prefer pre-creating it. |
| `S3_OBJECT_PREFIX` | no | `files` | Key prefix; normalized with a trailing `/`. |
| `PUBLIC_UPLOADS_ENABLED` | no | `true` | `false` restricts upload intents to admin-approved user hashes and disables public hash creation. Existing hashes start unapproved. |
| `UPLOAD_URL_TTL` | no | `30m` | Presigned PUT URL lifetime. 5m–2h. |
| `UPLOAD_INTENT_TTL` | no | `45m` | Deadline for completion; must be at least `UPLOAD_URL_TTL`. Max 6h. |
| `MAX_STORAGE` | no | `20GB` | Hard total cap, binary units. Set to `0` or `unlimited` to disable the global cap. |
| `MAX_FILE_SIZE` | no | `5GB` | Hard per-file cap. Cannot exceed `MAX_STORAGE` when a cap is set, or the 5 GB single-PUT limit. |
| `RETENTION` | no | `7d` | File lifetime. Supports `7d`, `30d`, `1.5d`, or Go durations like `12h`. 1h–3650d. Existing files keep their original expiry. |
| `DOWNLOAD_URL_TTL` | no | `5m` | 30s–1h. Also the worker download token lifetime. |
| `DOWNLOAD_WORKER_ENABLED` | no | `false` | Use Cloudflare Worker download URLs instead of presigned S3 GETs. |
| `DOWNLOAD_WORKER_URLS` | worker mode yes | — | Comma/space-separated worker base URLs. The `/d/<token>` path is appended. |
| `DOWNLOAD_WORKER_SECRET` | worker mode yes | — | At least 32 characters; must match the worker's `DOWNLOAD_TOKEN_SECRET`. |

For storage endpoint details and provider-specific settings, see
[S3-compatible storage](storage.md). For Worker-backed downloads, see
[B2 private bucket with Cloudflare Worker](b2-worker.md). For quota and expiry
behavior, see [Capacity, uploads, and retention](capacity-and-retention.md).
