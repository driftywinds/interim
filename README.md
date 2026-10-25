# interim

Temporary file hosting on S3-compatible storage. Uploads are public by default,
every file expires after the configured `RETENTION` period (7 days by default),
and downloads are served through short-lived signed URLs from a private bucket.

Built in Go with SQLite metadata, a server-rendered web UI, a full REST API, and
Docker as the primary deployment target.

## Features

- Anonymous uploads with no account.
- User hashes (`interim_…`) that act as bearer API keys.
- Direct browser-to-S3 uploads through short-lived presigned PUT URLs. File
  bytes never pass through the interim server.
- Configurable retention via `RETENTION` (default `7d`), enforced by metadata
  and a background cleanup worker.
- Private S3 bucket. No permanent object URLs; download links are presigned and
  expire after `DOWNLOAD_URL_TTL`.
- Configurable per-file and total storage caps, enforced with atomic
  reservations so concurrent uploads cannot oversubscribe capacity.
- A janitor that runs at startup and on intervals, deleting expired upload
  intents, temporary objects, orphan S3 keys, terminal intent rows, and stale
  reservations.
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

### sss3 + Caddy CORS proxy

For an sss3-only deployment, the repository includes:

- [compose.sss3.yaml](compose.sss3.yaml)
- [Caddyfile.sss3](Caddyfile.sss3)
- [.env.sss3.example](.env.sss3.example)

```sh
cp .env.sss3.example .env.sss3
# edit INTERIM_ORIGIN, S3_ACCESS_KEY, S3_SECRET_KEY, S3_BUCKET
docker compose -f compose.sss3.yaml --env-file .env.sss3 up -d
```

Stop the old sss3-only compose first. The new stack reuses `./data` and
publishes Caddy on host port `5553`, so an existing Cloudflare Tunnel for
`sss3.drifty.win` → `http://localhost:5553` keeps working — it now reaches
Caddy instead of sss3 directly.

Caddy:

- answers `OPTIONS` with `204` and the CORS headers;
- allows the exact `INTERIM_ORIGIN` (the interim web origin);
- exposes `ETag`, `Content-Length`, and `Content-Disposition`;
- forwards all other requests to sss3 while preserving the public `Host`
  header that SigV4 signs.

A typical Cloudflare Tunnel ingress while running cloudflared on the host:

```yaml
tunnel: YOUR_TUNNEL_ID
credentials-file: /etc/cloudflared/YOUR_TUNNEL_ID.json
ingress:
  - hostname: sss3.drifty.win
    service: http://localhost:5553
    originRequest:
      httpHostHeader: sss3.drifty.win
  - service: http_status:404
```

If cloudflared runs as a container on the same Docker network, use
`service: http://sss3-cors:8080` instead. The explicit public
`httpHostHeader` is important; a rewritten `Host` breaks SigV4.

The interim service should use:

```dotenv
S3_ENDPOINT=https://sss3.drifty.win
S3_PUBLIC_ENDPOINT=https://sss3.drifty.win
S3_REGION=us-east-1
S3_BUCKET=interim
S3_FORCE_PATH_STYLE=true
```

If interim is placed on the same Docker network as this stack, it can use
`S3_ENDPOINT=http://stupid-simple-s3:5553` for server-side calls while keeping
`S3_PUBLIC_ENDPOINT=https://sss3.drifty.win` for browser presigned URLs.

Cloudflare notes for this setup:

- Free/Pro Cloudflare caps proxied request bodies around 100 MB. Keep interim’s
  `MAX_FILE_SIZE` below that, or bypass Cloudflare for the S3 hostname.
- Add a Cache Rule bypass for the S3 hostname.
- Browser Integrity Check / Bot Fight Mode can reject scripted presigned PUTs
  with error `1010`; browser uploads are unaffected. Exempt the S3 hostname if
  curl/API clients must upload directly.
- Rotate the sss3 credentials before deployment.

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
| `UPLOAD_URL_TTL` | no | `30m` | Presigned PUT URL lifetime. 5m–2h. |
| `UPLOAD_INTENT_TTL` | no | `45m` | Deadline for completion; must be at least `UPLOAD_URL_TTL`. Max 6h. |
| `MAX_STORAGE` | no | `20GB` | Hard total cap, binary units. |
| `MAX_FILE_SIZE` | no | `5GB` | Hard per-file cap. Cannot exceed `MAX_STORAGE` or the 5 GB single-PUT limit. |
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

`S3_PUBLIC_ENDPOINT` matters: the server signs with the internal client but the
browser uploads to the public host. Both endpoints must point at the same bucket
and credentials. If you do not expose S3 directly, put it behind a reverse
proxy; do not rewrite the signed host/path.

The bucket should be private. The browser receives a PUT URL scoped to one
object key and a short expiry. interim sets `Content-Disposition: attachment`
through the signed PUT headers and also adds a response override to signed GETs.

### sss3 behind Cloudflare

Tested against `stupid-simple-s3` through a Cloudflare Tunnel. The following
matter for this deployment:

- **No CORS support.** sss3 answers browser `OPTIONS` preflights with `405` and
  sends no `Access-Control-Allow-*` headers. A browser cannot direct-PUT to it
  until a CORS-capable reverse proxy or Worker handles `OPTIONS` and adds the
  headers. See the Caddy example below.
- **Cloudflare body limit.** Cloudflare Free/Pro caps proxied request bodies at
  about 100 MB. Keep `MAX_FILE_SIZE` below that, or bypass Cloudflare for the S3
  hostname. interim does not implement multipart uploads.
- **Presigned PUT from non-browser clients can hit Cloudflare error 1010.**
  Cloudflare browser-integrity checks may reject a scripted presigned PUT even
  though interim's own signed SDK requests succeed. Use a browser-like
  `User-Agent`, exempt the S3 hostname from bot checks, or bypass Cloudflare.
- **Signed `Accept-Encoding` breaks sss3.** The Go SDK normally signs
  `Accept-Encoding: identity`; Cloudflare/sss3 produce `SignatureDoesNotMatch`.
  interim removes that header from the signing path and disables transport
  compression, so normal `HeadBucket`, `ListObjectsV2`, `HeadObject`, and
  presigned GET/PUT requests work.
- **HEAD through Cloudflare worked in testing.** The previously suspected
  HeadObject 403 was a Cloudflare/AWS CLI interaction, not an interim or sss3
  head failure.

Example Caddy listener to put in front of sss3 (Cloudflare Tunnel points at this
listener; Caddy preserves the original `Host` header for SigV4):

```caddyfile
:8080 {
    @options method OPTIONS
    handle @options {
        header Access-Control-Allow-Origin "https://interim.example.com"
        header Access-Control-Allow-Methods "PUT, GET, HEAD, DELETE, OPTIONS"
        header Access-Control-Allow-Headers "Content-Type, Content-Disposition"
        header Access-Control-Max-Age "86400"
        respond 204
    }
    handle {
        header Access-Control-Allow-Origin "https://interim.example.com"
        header Access-Control-Expose-Headers "ETag, Content-Length, Content-Disposition"
        reverse_proxy stupid-simple-s3:5553
    }
}
```

Set both `S3_ENDPOINT` and `S3_PUBLIC_ENDPOINT` to the public Caddy hostname
(for example `https://sss3.drifty.win`), keep `S3_FORCE_PATH_STYLE=true`, use
region `us-east-1`, and use a dedicated bucket. Cloudflare cannot raise its
request-body limit, so large-file support requires a non-proxied endpoint or a
different upload design.

### Required bucket CORS

Direct uploads require CORS on the bucket. Allow the interim origin, the `PUT`
method, and the headers included in the create-upload response:

```json
[
  {
    "AllowedOrigins": ["https://files.example.com"],
    "AllowedMethods": ["PUT"],
    "AllowedHeaders": ["content-type", "content-disposition"],
    "ExposeHeaders": ["ETag"],
    "MaxAgeSeconds": 3000
  }
]
```

API-only clients such as curl do not need CORS, but do need a browser-reachable
`S3_PUBLIC_ENDPOINT` and a provider that supports presigned PUT requests. If
`PUBLIC_BASE_URL` is HTTPS, the public S3 endpoint must also be HTTPS to avoid
mixed-content blocking. The app also calls `HeadObject` and `ListObjectsV2` for
verification and the orphan janitor. If the provider does not support
`ListObjectsV2`, the janitor logs the failure and still cleans known expired
intents.

### Other provider notes

- **AWS S3:** leave both endpoints empty, set the real region,
  `S3_FORCE_PATH_STYLE=false`, and use an IAM role or static keys.
- **MinIO:** path-style, any consistent region, endpoint includes scheme.
- **Cloudflare R2:** path-style, region `auto` or `us-east-1`, endpoint
  `https://<account>.r2.cloudflarestorage.com`. Presign against the R2 host.
- **Backblaze B2:** path-style, region is the cluster ID
  (`us-west-004`), create the bucket in the B2 console.
- **Wasabi:** path-style is safest, use the provider's region endpoint.

## Capacity, uploads, and retention

`MAX_STORAGE` is tracked in SQLite. Creating an upload intent reserves the
declared size before interim signs anything, so concurrent uploads cannot both
believe they fit. The reservation is converted into a committed file only after
the object is verified. The actual object size is checked with `HeadObject` at
completion; a smaller object is accepted and a larger one is deleted.

When committed storage plus reservations reaches `MAX_STORAGE`:

- `/api/v1/status` reports `accepting_uploads: false`;
- new upload intents return HTTP `507 storage_full`;
- the home page disables upload controls and shows
  **“Storage maxed out — please try again later.”**

`RETENTION` is read from the environment when the process starts. Each committed
file gets an expiry of `completed_at + RETENTION`; changing the value applies to
new uploads, not existing rows. Expired file pages return HTTP `410`,
metadata/download endpoints refuse them, and a worker deletes the S3 object
before removing the SQLite row.

### Upload intents and the janitor

Every direct upload creates an `upload_intents` row with state `pending`. The
browser gets a presigned PUT URL and a one-time upload token. Completion performs
`HeadObject`, verifies the size, consumes the reservation, and inserts the file
row in one SQLite transaction.

Abandoned or failed uploads are cleaned by a janitor:

- fast pass every minute: expired pending intents, expired files, expired
  sessions, expired reservations, terminal intent rows older than 24 hours;
- security pass at startup and every 30 minutes: list the configured S3 prefix
  and delete objects with no matching file row or live pending intent;
- at startup it runs before the HTTP listener, so old state is reconciled before
  accepting requests;
- during runtime, unknown objects get a 15-minute grace period to avoid racing a
  just-completed PUT. At startup no browser upload can be in flight, so unknown
  objects are removed immediately;
- browser cancellation calls the abort endpoint, which deletes the object and
  releases the reservation immediately;
- if completion reports an oversized or empty object, the object is deleted and
  the reservation released.

The upload token is separate from the file delete token. Both are stored only as
SHA-256 hashes. A completion retry for an already-committed intent returns the
existing file instead of duplicating it.

Because a presigned PUT cannot enforce a content-length condition at the storage
layer, a client can temporarily upload more than it declared. Completion checks
the real object size and deletes anything that exceeds the declared size or
`MAX_FILE_SIZE`. Keep `UPLOAD_URL_TTL` short and use a bucket lifecycle rule if
temporary cost exposure is a concern.

For extra safety, configure a bucket lifecycle rule for the configured prefix.
The application does not require it, but it limits cost if the server is offline
longer than its upload window.

## API

Start at `/api` for the rendered documentation or `/api/openapi.json` for the
OpenAPI 3.1 document. Examples use `PUBLIC_BASE_URL`.

```sh
# Create a user hash (shown once)
curl -X POST https://files.example.com/api/v1/keys \
  -H 'Content-Type: application/json' \
  -d '{"name":"cli"}'

# Create an upload intent
curl -X POST https://files.example.com/api/v1/uploads \
  -H 'Authorization: Bearer interim_...' \
  -H 'Content-Type: application/json' \
  -d '{"name":"sample.bin","size":12345,"content_type":"application/octet-stream"}'

# PUT the file bytes directly to the signed URL from that response
curl -X PUT 'SIGNED_S3_URL' \
  -H 'Content-Type: application/octet-stream' \
  -H 'Content-Disposition: attachment; filename=sample.bin' \
  --data-binary @sample.bin

# Verify and commit it
curl -X POST 'COMPLETE_URL' \
  -H 'X-Upload-Token: UPLOAD_TOKEN'

# Get a signed download URL (expires after DOWNLOAD_URL_TTL)
curl -X POST https://files.example.com/api/v1/files/FILE_ID/download

# Delete with the owning user hash
curl -X DELETE https://files.example.com/api/v1/files/FILE_ID \
  -H 'Authorization: Bearer interim_...'
```

Upload intents return a one-time `delete_token`; use
`X-Delete-Token: <token>` with `DELETE /api/v1/files/{id}` to remove anonymous
files. Raw user hashes, upload tokens, and delete tokens are stored only as
SHA-256 hashes.

### Endpoints

| Method | Path | Auth | Purpose |
| --- | --- | --- | --- |
| `POST` | `/api/v1/keys` | no | Create user hash. |
| `GET` | `/api/v1/keys/me` | bearer | Key prefix and usage. |
| `POST` | `/api/v1/uploads` | optional | Reserve quota and sign a PUT URL. |
| `POST` | `/api/v1/uploads/{id}/complete` | upload token | Verify and commit the object. |
| `DELETE` | `/api/v1/uploads/{id}` | upload token | Abort a pending upload and release quota. |
| `GET` | `/api/v1/files` | bearer | List the caller's active files. |
| `GET` | `/api/v1/files/{id}` | no | Public metadata. |
| `POST` | `/api/v1/files/{id}/download` | no | Short-lived signed URL. |
| `DELETE` | `/api/v1/files/{id}` | bearer or token | Delete. |
| `GET` | `/api/v1/status` | no | Capacity and retention status. |

Rate limits apply to user-hash creation, upload-intent creation, download-link
creation, and admin login. Errors use
`{"error":{"code","message","request_id"}}`.

## Web UI and browser history

The home page creates an upload intent, PUTs the selected file directly to S3
with XHR for progress, then calls the completion endpoint. Canceling aborts the
intent. Upload URLs are short-lived, so a slow upload can fail before completion;
the UI reports that and tells the user to try again.

Completed uploads are stored in `localStorage` under `interim.uploads.v1`,
including the one-time delete token. Only that browser can see the list.
Clearing site data loses the local delete ability, but the file still expires
after the configured `RETENTION` period and the admin can delete it.

The download page never receives or displays the delete token. Its download
button links to a server endpoint that creates a fresh presigned GET and
redirects to it. Direct object URLs do not exist in the HTML.

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
upload-intent creation, idempotent completion, direct upload verification,
ownership, quota rejection, abort, janitor expiry, orphan sweeping, expiry
cleanup, and admin deletion. S3 integration, CORS, and presigned PUT behaviour
must be tested against your provider.

Build the binary directly:

```sh
CGO_ENABLED=0 go build -trimpath -o bin/interim ./cmd/interim
```

## Deployment notes

- Put interim behind a TLS-terminating reverse proxy and set
  `PUBLIC_BASE_URL` to the HTTPS origin. Set `TRUST_PROXY=true` only when the
  proxy is trusted; it is needed for correct client-IP rate limiting.
- The S3 bucket must be browser-reachable at `S3_PUBLIC_ENDPOINT`, with the CORS
  policy shown above. The bucket itself stays private; only signed PUT/GET URLs
  are exposed.
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

## Security model

- File URLs are unguessable random IDs and public metadata reveals no object
  key.
- Direct uploads are scoped to one object key and expire quickly; upload tokens
  are one-time and never placed in URLs.
- Download links are short-lived signed S3 GETs; objects are private.
- User hashes, upload tokens, and delete tokens are hashed at rest.
- Uploads are capped and capacity-reserved before signing. Actual object size is
  verified with `HeadObject` before a file is committed.
- Admin cookies are HttpOnly, SameSite=Lax, Secure when configured, and backed
  by DB sessions with per-session CSRF tokens.
- Responses set a restrictive CSP, `nosniff`, frame denial, and
  `Referrer-Policy: no-referrer`.
