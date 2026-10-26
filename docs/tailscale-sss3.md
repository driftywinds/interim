# Tailscale-only sss3 hosting

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

## 1. Home machine: sss3 and Caddy

Start the existing stack from [compose.sss3.yaml](../compose.sss3.yaml) and
[.env.sss3.example](../.env.sss3.example):

```sh
cp .env.sss3.example .env.sss3
# edit INTERIM_ORIGIN, S3_ACCESS_KEY, S3_SECRET_KEY, S3_BUCKET
docker compose -f compose.sss3.yaml --env-file .env.sss3 up -d
```

`compose.sss3.yaml` publishes Caddy on `127.0.0.1:5553`. In
[Caddyfile.sss3](../Caddyfile.sss3), the reverse proxy must preserve the
**host and port**:

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

## 2. VPS: DNS, firewall, and TLS edge

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

## 3. interim configuration

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

## 4. Verify the route

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

## Troubleshooting this route

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

## Operational notes

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
