# interim

Temporary file hosting on S3-compatible storage. Uploads are public by default,
every file expires after the configured `RETENTION` period (7 days by default),
and downloads are served through short-lived signed URLs from a private bucket.

Built in Go with SQLite metadata, a server-rendered web UI, a full REST API, and
Docker as the primary deployment target.

## Features

- Anonymous uploads with no account.
- User hashes (`interim_…`) that act as bearer API keys.
- Configurable retention via `RETENTION` (default `7d`), enforced by metadata
  and a background cleanup worker.
- Private S3 bucket. No permanent object URLs; download links are presigned and
  expire after `DOWNLOAD_URL_TTL`.
- Configurable per-file and total storage caps, enforced with atomic
  reservations so concurrent uploads cannot oversubscribe capacity.
- Per-browser upload history stored in `localStorage`, including a one-time
  delete token that is never exposed on the public download page.
- API documentation at `/api` and OpenAPI 3.1 at `/api/openapi.json` /
  `/api/openapi.yaml`.
- First-run admin setup, session auth, CSRF protection, file management, bulk
  deletion, and user-hash revocation.
- A deliberately minimal UI: system sans-serif, no webfonts, no logo image,
  native controls, automatic light/dark mode, monospace filenames.

## Quick start with Docker

```sh
cp .env.example .env
# edit PUBLIC_BASE_URL, S3_* and capacity limits
docker compose up --build
```

Open `http://localhost:8080` (or your public URL), then visit `/admin` to create
the admin account. The setup page is only available while no admin exists.

The SQLite database, generated session secret, and WAL files live in the
`interim-data` volume at `/data`. Back up the volume, not individual files
while the container is running.

## Configuration

All runtime configuration is read from the environment. `.env` is loaded
automatically for local runs, and Compose uses it via `env_file`.

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
| `S3_PART_SIZE` | no | `16MB` | 5MB–5GB. |
| `S3_UPLOAD_CONCURRENCY` | no | `3` | 1–16. |
| `MAX_STORAGE` | no | `20GB` | Hard total cap, binary units. |
| `MAX_FILE_SIZE` | no | `5GB` | Hard per-file cap. Cannot exceed `MAX_STORAGE`. |
| `RETENTION` | no | `7d` | File lifetime. Supports `7d`, `30d`, `1.5d`, or Go durations like `12h`. 1h–3650d. Existing files keep their original expiry. |
| `DOWNLOAD_URL_TTL` | no | `5m` | 30s–1h. |

### sss3 and other S3 servers

interim intentionally does not hard-code AWS behaviour. The AWS SDK v2 client
uses SigV4, path-style addressing by default, and forces checksum calculation
and validation to `when_required` for compatibility with smaller S3 clones.

For an internal sss3 endpoint:

```dotenv
S3_ENDPOINT=http://sss3:9000
S3_PUBLIC_ENDPOINT=https://objects.example.com
S3_REGION=us-east-1
S3_FORCE_PATH_STYLE=true
S3_BUCKET=interim
```

`S3_PUBLIC_ENDPOINT` matters: the uploader talks to the internal endpoint, but a
presigned URL is signed for the host the browser will use. Both endpoints must
point at the same bucket and credentials. If you do not expose S3 directly, put
it behind a reverse proxy; do not rewrite the signed host/path.

The bucket should be private. interim sets `Content-Disposition: attachment` on
uploaded objects and creates the signed URL itself.

### Other provider notes

- **AWS S3:** leave both endpoints empty, set the real region,
  `S3_FORCE_PATH_STYLE=false`, and use an IAM role or static keys.
- **MinIO:** path-style, any consistent region, endpoint includes scheme.
- **Cloudflare R2:** path-style, region `auto` or `us-east-1`, endpoint
  `https://<account>.r2.cloudflarestorage.com`. Presign against the R2 host.
- **Backblaze B2:** path-style, region is the cluster ID
  (`us-west-004`), create the bucket in the B2 console.
- **Wasabi:** path-style is safest, use the provider's region endpoint.

## Capacity and retention

`MAX_STORAGE` is tracked in SQLite and enforced with a reservation row created
before the request body is streamed to S3. Reservations prevent two concurrent
uploads from both believing they fit. A successful upload converts its
reservation into a committed file; failures release it. Stale reservations are
cleared at startup and after six hours.

When committed storage plus reservations reaches `MAX_STORAGE`:

- `/api/v1/status` reports `accepting_uploads: false`;
- uploads return HTTP `507 storage_full`;
- the home page disables upload controls and shows
  **“Storage maxed out — please try again later.”**

`RETENTION` is read from the environment when the process starts. Each upload
gets an expiry of `created_at + RETENTION`; changing the value applies to new
uploads, not existing rows. Expired file pages return HTTP `410`,
metadata/download endpoints refuse them, and a worker deletes the S3 object
before removing the SQLite row.

For belt-and-braces protection, configure your bucket lifecycle to expire
objects under the configured prefix after the same period and to abort
incomplete multipart uploads after one day. The application still enforces
expiry on its own.

## API

Start at `/api` for the rendered documentation or `/api/openapi.json` for the
OpenAPI 3.1 document. Examples use `PUBLIC_BASE_URL`.

```sh
# Create a user hash (shown once)
curl -X POST https://files.example.com/api/v1/keys \
  -H 'Content-Type: application/json' \
  -d '{"name":"cli"}'

# Upload
curl -X POST https://files.example.com/api/v1/files \
  -H 'Authorization: Bearer interim_...' \
  -H 'X-File-Size: 12345' \
  -F 'file=@sample.bin'

# Get a signed download URL (expires after DOWNLOAD_URL_TTL)
curl -X POST https://files.example.com/api/v1/files/FILE_ID/download

# Delete with the owning user hash
curl -X DELETE https://files.example.com/api/v1/files/FILE_ID \
  -H 'Authorization: Bearer interim_...'
```

Anonymous uploads receive a one-time `delete_token`; use
`X-Delete-Token: <token>` with `DELETE` to remove them. Raw user hashes and
delete tokens are stored only as SHA-256 hashes.

### Endpoints

| Method | Path | Auth | Purpose |
| --- | --- | --- | --- |
| `POST` | `/api/v1/keys` | no | Create user hash. |
| `GET` | `/api/v1/keys/me` | bearer | Key prefix and usage. |
| `POST` | `/api/v1/files` | optional | Multipart upload. |
| `GET` | `/api/v1/files` | bearer | List the caller's active files. |
| `GET` | `/api/v1/files/{id}` | no | Public metadata. |
| `POST` | `/api/v1/files/{id}/download` | no | Short-lived signed URL. |
| `DELETE` | `/api/v1/files/{id}` | bearer or token | Delete. |
| `GET` | `/api/v1/status` | no | Capacity and retention status. |

Rate limits apply to user-hash creation, download-link creation, and admin
login. Errors use `{"error":{"code","message","request_id"}}`.

## Web UI and browser history

The home page stores completed uploads in `localStorage` under
`interim.uploads.v1`, including the one-time delete token. Only that browser can
see the list. Clearing site data loses the local delete ability, but the file
still expires after the configured `RETENTION` period and the admin can delete it.

The download page never receives or displays the delete token. Its download
button POSTs to the server, which creates a fresh presigned URL and redirects
to it. Direct object URLs do not exist in the HTML.

## Admin

`/admin` is gated by a DB-backed session created during first-run setup. The
admin can:

- see storage usage, the hard cap, active reservations, and every file;
- search by filename or ID and paginate;
- open files, delete one file, or select and bulk-delete (S3 object first);
- inspect user-hash prefixes, usage, and last-used time;
- revoke/restore user hashes without exposing the raw hash.

Admin POSTs are CSRF-protected. Login is rate-limited. Passwords are bcrypt
hashes (cost 12). There is intentionally only one admin account.

## Development

Go 1.27 is used in the Docker image.

```sh
go test ./...
go vet ./...
go run ./cmd/interim
```

The test suite uses an in-memory fake object store and covers setup, auth,
upload, signed download generation, ownership, quota rejection, expiry cleanup,
and admin deletion. S3 integration itself must be tested against your provider.

Build the binary directly:

```sh
CGO_ENABLED=0 go build -trimpath -o bin/interim ./cmd/interim
```

## Deployment notes

- Put interim behind a TLS-terminating reverse proxy and set
  `PUBLIC_BASE_URL` to the HTTPS origin. Set `TRUST_PROXY=true` only when the
  proxy is trusted; it is needed for correct client-IP rate limiting.
- Run one instance per SQLite database. The storage reservation logic is safe
  for a single process; SQLite is not a multi-writer cluster database.
- Keep the bucket private. Do not enable public ACLs or public bucket policies.
- The Docker example uses a read-only root filesystem, a writable `/data`
  volume, and a non-root user.
- Back up the `/data` volume. Only S3 holds file bytes; SQLite holds the mapping,
  expiry, delete-token hashes, sessions, quotas, and admin credentials.

## Security model

- File URLs are unguessable random IDs and public metadata reveals no object
  key.
- Download links are short-lived signed S3 GETs; objects are private.
- User hashes and delete tokens are hashed at rest.
- Uploads are capped and capacity-reserved before streaming.
- Admin cookies are HttpOnly, SameSite=Lax, Secure when configured, and backed
  by DB sessions with per-session CSRF tokens.
- Responses set a restrictive CSP, `nosniff`, frame denial, and
  `Referrer-Policy: no-referrer`.
