# interim

Temporary file hosting on S3-compatible storage. Uploads are public by default,
every file expires after the configured `RETENTION` period (7 days by default),
and downloads are served through short-lived URLs from a private bucket.

Built in Go with SQLite metadata, a server-rendered web UI, a full REST API, and
Docker as the primary deployment target.

## Features

- Anonymous uploads with no account.
- User hashes (`interim_…`) that act as bearer API keys.
- Direct browser-to-S3 uploads through short-lived presigned PUT URLs. File
  bytes never pass through the interim server.
- Configurable retention via `RETENTION` (default `7d`), enforced by metadata
  and a background cleanup worker.
- Private S3 bucket. No permanent object URLs; download links are short-lived
  (signed S3 GETs or worker-proxied token URLs) and expire after
  `DOWNLOAD_URL_TTL`.
- Configurable per-file and total storage caps (the total cap can be disabled),
  enforced with atomic reservations so concurrent uploads cannot oversubscribe
  capacity.
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

### sss3 + Caddy CORS proxy (Cloudflare Tunnel)

This is the small-file route. Cloudflare caps proxied request bodies at about
100 MB. If the home machine is behind restrictive NAT/CGNAT and you want files
larger than that, use the [Tailscale-only route](#tailscale-only-local-s3-hosting)
instead.

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
  `MAX_FILE_SIZE` below that, or use the
  [Tailscale-only route](#tailscale-only-local-s3-hosting) to bypass Cloudflare
  for the S3 hostname.
- Add a Cache Rule bypass for the S3 hostname.
- Browser Integrity Check / Bot Fight Mode can reject scripted presigned PUTs
  with error `1010`; browser uploads are unaffected. Exempt the S3 hostname if
  curl/API clients must upload directly.
- Rotate the sss3 credentials before deployment.

### Tailscale-only local S3 hosting

Use this route when sss3 runs on a home machine behind CGNAT or restrictive
NAT, and Cloudflare's ~100 MB request-body cap is not acceptable. File bytes
never touch Cloudflare:

```text
browser
  -> https://sss3.example.com
     VPS: Caddy, public TLS
  -> 100.x.y.z:5554
     Tailscale Serve (tailnet only)
  -> 127.0.0.1:5553
     home: Caddy, CORS, Host preservation
  -> sss3
```

The public entry point is a small VPS. The home machine makes only an outbound
Tailscale connection, so no port forwarding, public IPv4, or stable home IP is
needed. `tailscale serve` is **not** Funnel; it is reachable only inside the
tailnet and does not have Funnel's public bandwidth limits. The interim UI can
still be served through Cloudflare Tunnel; only the S3 hostname bypasses
Cloudflare.

interim signs the browser's PUT/GET for `sss3.example.com`; the browser sends
the bytes to the VPS, and the VPS forwards them to the home machine. interim
never proxies the file bytes, and Cloudflare never sees the upload.

#### 1. Home machine: sss3 and Caddy

Start the existing stack:

```sh
cp .env.sss3.example .env.sss3
# edit INTERIM_ORIGIN, S3_ACCESS_KEY, S3_SECRET_KEY, S3_BUCKET
docker compose -f compose.sss3.yaml --env-file .env.sss3 up -d
```

`compose.sss3.yaml` publishes Caddy on `127.0.0.1:5553`. In
`Caddyfile.sss3`, the reverse proxy must preserve the **host and port**:

```caddyfile
reverse_proxy stupid-simple-s3:5553 {
    header_up Host {http.request.hostport}
}
```

Do not use `{http.request.host}` here. That placeholder strips the port. A
request to `http://100.x.y.z:5554` is signed for
`Host: 100.x.y.z:5554`; stripping the port to `100.x.y.z` produces
`SignatureDoesNotMatch` from sss3. If the checked-in `Caddyfile.sss3` still has
`{http.request.host}`, change that line before reloading.

Reload Caddy after the change:

```sh
docker exec sss3-cors caddy reload --config /etc/caddy/Caddyfile
```

or restart the container:

```sh
docker restart sss3-cors
```

Expose the loopback Caddy listener to the tailnet:

```sh
sudo tailscale serve --bg --tcp=5554 tcp://127.0.0.1:5553
tailscale serve status
tailscale ip -4
```

`5554` is the tailnet-facing port; `5553` is the local Caddy port. The command
prints the tailnet name and the `100.x.y.z` IPv4 address. Keep that address for
the VPS configuration. If Tailscale asks you to enable HTTPS certificates for
the tailnet, accept it.

The Serve mapping is persistent across reboots because of `--bg`. To remove it:

```sh
sudo tailscale serve --tcp=5554 off
```

`INTERIM_ORIGIN` in `.env.sss3` must exactly match the origin serving the
interim UI, for example:

```dotenv
INTERIM_ORIGIN=https://interim.example.com
```

That origin is the only one Caddy allows in its CORS response.

#### 2. VPS: DNS, firewall, and TLS edge

On the VPS:

- Point `sss3.example.com` at the VPS with an `A` (or `AAAA`) record.
- Set the record to **DNS only** in Cloudflare if Cloudflare manages the zone.
  A proxied record puts the 100 MB limit back in the path and hides the real
  certificate.
- Remove `sss3.example.com` from any Cloudflare Tunnel ingress. A tunnel
  hostname is proxied by definition and would override the VPS record.
- Allow inbound TCP `80` and `443`.
- Make sure Tailscale is connected to the same tailnet as the home machine.

Verify tailnet reachability from the VPS:

```sh
tailscale ping 100.x.y.z
nc -vz 100.x.y.z 5554
```

Add a site block to the VPS Caddyfile. If Caddy already serves other sites,
append this block; do not replace the existing configuration:

```caddyfile
sss3.example.com {
    reverse_proxy 100.x.y.z:5554 {
        header_up Host {http.request.hostport}
    }
}
```

The VPS Caddy only terminates public TLS and proxies. The home Caddy still
handles CORS, so do not add a second `Access-Control-Allow-Origin` here; two
values make browsers reject the response. Caddy obtains the public certificate
automatically once the DNS record and ports are correct.

Reload and test:

```sh
sudo caddy validate --config /etc/caddy/Caddyfile
sudo systemctl reload caddy

curl -vkI https://sss3.example.com/
```

A `403` or `404` from sss3 is expected for an unsigned request. What matters is
a valid Let's Encrypt certificate and an HTTP response from sss3, not a
Cloudflare error or a `502`. A `Via: 1.1 Caddy` header is normal; both Caddys
may add one.

#### 3. interim configuration

Set the public S3 hostname for browser presigned URLs, and the tailnet address
for server-side calls:

```dotenv
PUBLIC_BASE_URL=https://interim.example.com

S3_PUBLIC_ENDPOINT=https://sss3.example.com
S3_ENDPOINT=http://100.x.y.z:5554

S3_REGION=us-east-1
S3_BUCKET=interim
S3_FORCE_PATH_STYLE=true
S3_AUTO_CREATE_BUCKET=false

MAX_FILE_SIZE=5GB
UPLOAD_URL_TTL=2h
UPLOAD_INTENT_TTL=3h
```

Important details:

- `S3_PUBLIC_ENDPOINT` is embedded in browser presigned PUT/GET URLs. It must
  be the public VPS hostname.
- `S3_ENDPOINT` is used by interim for `HeadObject`, `ListObjectsV2`, and
  deletes. It can use the tailnet address and never goes through Cloudflare.
  If interim cannot reach the tailnet address, use
  `S3_ENDPOINT=https://sss3.example.com`; server-side calls then take the VPS
  round trip.
- If interim shares a Docker network with sss3, `S3_ENDPOINT` can instead be
  `http://stupid-simple-s3:5553`. If it points at the Caddy container, use the
  Caddy port and keep `{http.request.hostport}`.
- The app's CSP already includes `S3_PUBLIC_ENDPOINT`, so no code change is
  needed for the browser to reach the new public S3 hostname.
- `S3_BUCKET` must exactly match sss3's `STUPID_BUCKET_NAME` in `.env.sss3`.
  `S3_ACCESS_KEY` and `S3_SECRET_KEY` must exactly match
  `STUPID_RW_ACCESS_KEY` and `STUPID_RW_SECRET_KEY`.
- `S3_AUTO_CREATE_BUCKET=false` is preferred once sss3's configured bucket
  exists. sss3 creates `STUPID_BUCKET_NAME` at startup.
- `MAX_FILE_SIZE` may be up to `5GB`; interim still uses a single S3 PUT. The
  former Cloudflare 100 MB cap is gone because the browser talks to the VPS,
  not Cloudflare.
- Increase `UPLOAD_URL_TTL` and `UPLOAD_INTENT_TTL` for slow home uplinks.
  `UPLOAD_INTENT_TTL` must be at least `UPLOAD_URL_TTL`.

Restart interim after changing `.env`:

```sh
docker compose up -d
docker logs -f interim
```

If the interim UI is served through Cloudflare Tunnel, `PUBLIC_BASE_URL` and
`INTERIM_ORIGIN` stay on the Cloudflare hostname. The browser still uploads
directly to `sss3.example.com`, which resolves to the VPS.

#### 4. Verify the route

From the VPS:

```sh
dig +short sss3.example.com
curl -vkI https://sss3.example.com/
```

The address should be the VPS public IP, and the response should come from
sss3/Caddy, not Cloudflare. Then upload a file larger than 100 MB through the
interim UI.

Useful checks:

- `tailscale serve status` on the home machine should show
  `tcp://100.x.y.z:5554 -> tcp://127.0.0.1:5553`.
- `tailscale ping 100.x.y.z` shows whether traffic is direct or relayed. DERP
  relay works, but direct is faster for large files.
- The object should appear under the sss3 data directory on the home machine.
- `curl -I` should not show `server: cloudflare` for the S3 hostname.

#### Troubleshooting this route

- **`SignatureDoesNotMatch` on startup or signed requests:** a proxy stripped
  the port. Use `header_up Host {http.request.hostport}` in both Caddys. Also
  verify `S3_REGION=us-east-1`, the bucket name, and the credentials.
- **`CreateBucket` fails:** the bucket name differs from sss3's configured
  bucket, or the credentials do not match. Set `S3_BUCKET` equal to
  `STUPID_BUCKET_NAME` and restart sss3 so it creates the bucket, then set
  `S3_AUTO_CREATE_BUCKET=false`.
- **`502` from the VPS:** `tailscale serve` is not running, the tailnet ACL
  blocks the VPS, or the home Caddy is down. Run `nc -vz 100.x.y.z 5554` from
  the VPS and `tailscale serve status` at home.
- **Browser CORS error:** `INTERIM_ORIGIN` must be the exact scheme and host of
  the page making the request. Do not add CORS headers on both Caddys.
- **Certificate error:** the DNS record is proxied, port 80/443 is blocked, or
  the record still points at Cloudflare Tunnel.
- **Upload still fails at 100 MB:** the S3 hostname is still resolving to
  Cloudflare. Check the DNS record and remove the tunnel ingress.
- **Upload URL expires:** raise `UPLOAD_URL_TTL` and `UPLOAD_INTENT_TTL`.

#### Operational notes

- Tailscale ACLs should allow only the VPS to reach the home machine on TCP
  `5554`. The public internet reaches only the VPS Caddy; sss3 remains
  tailnet-only.
- The VPS relays every upload and download, so its monthly traffic allowance
  and bandwidth matter. The home disk still stores the bytes.
- Tailscale Serve is tailnet-only. Do not use Tailscale Funnel for this route;
  Funnel is public and has its own bandwidth limits.
- `sss3` is still HTTP-only and path-style. Keep `S3_FORCE_PATH_STYLE=true` and
  region `us-east-1`.
- sss3's default `STUPID_MAX_OBJECT_SIZE` is `5GB`. Keep interim's
  `MAX_FILE_SIZE` at or below that, or raise both limits.

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
| `MAX_STORAGE` | no | `20GB` | Hard total cap, binary units. Set to `0` or `unlimited` to disable the global cap. |
| `MAX_FILE_SIZE` | no | `5GB` | Hard per-file cap. Cannot exceed `MAX_STORAGE` when a cap is set, or the 5 GB single-PUT limit. |
| `RETENTION` | no | `7d` | File lifetime. Supports `7d`, `30d`, `1.5d`, or Go durations like `12h`. 1h–3650d. Existing files keep their original expiry. |
| `DOWNLOAD_URL_TTL` | no | `5m` | 30s–1h. Also the worker download token lifetime. |
| `DOWNLOAD_WORKER_ENABLED` | no | `false` | Use Cloudflare Worker download URLs instead of presigned S3 GETs. |
| `DOWNLOAD_WORKER_URLS` | worker mode yes | — | Comma/space-separated worker base URLs. The `/d/<token>` path is appended. |
| `DOWNLOAD_WORKER_SECRET` | worker mode yes | — | At least 32 characters; must match the worker's `DOWNLOAD_TOKEN_SECRET`. |

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
through the signed PUT headers; in S3 download mode it also adds a response
override to signed GETs. In worker mode the Worker sets the response header.

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

### B2 private bucket with Cloudflare Worker downloads

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
DOWNLOAD_WORKER_SECRET=replace-with-at-least-32-random-characters

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

Deploy the Worker from [worker/README.md](worker/README.md). It includes the
wrangler config, read-only B2 key scope, custom-domain setup, and the
multi-account/multi-URL deployment notes.

Cloudflare's free Worker allowance is per account, not per Worker. Multiple
Worker URLs are useful for separate deployments or redundancy, but using extra
accounts to evade the free allowance is a terms-of-service risk. Cloudflare's
large-file/video delivery policy still applies to the download domain.

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

Set `MAX_STORAGE=0` (or `MAX_STORAGE=unlimited`) to disable the global cap. The
per-file limit still applies, the API reports `storage.unlimited: true`, and the
home page hides the storage panel and capacity warning.

When a cap is set and committed storage plus reservations reaches `MAX_STORAGE`:

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

# Get a download URL (expires after DOWNLOAD_URL_TTL)
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
| `POST` | `/api/v1/files/{id}/download` | no | Short-lived download URL. |
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
button links to a server endpoint that creates a fresh download link and
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
  policy shown above. The bucket itself stays private; only presigned PUTs and
  short-lived download URLs are exposed.
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
- Download links are short-lived signed S3 GETs or worker-proxied token URLs;
  objects are private.
- User hashes, upload tokens, and delete tokens are hashed at rest.
- Uploads are capped and capacity-reserved before signing. Actual object size is
  verified with `HeadObject` before a file is committed.
- Admin cookies are HttpOnly, SameSite=Lax, Secure when configured, and backed
  by DB sessions with per-session CSRF tokens.
- Responses set a restrictive CSP, `nosniff`, frame denial, and
  `Referrer-Policy: no-referrer`.
