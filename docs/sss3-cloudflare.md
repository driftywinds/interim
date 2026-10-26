# sss3 with Cloudflare and Caddy

This is the small-file route. Cloudflare caps proxied request bodies at about
100 MB. If the home machine is behind restrictive NAT/CGNAT and you want files
larger than that, use the [Tailscale-only route](tailscale-sss3.md) instead.

## Checked-in sss3 stack

For an sss3-only deployment, the repository includes:

- [compose.sss3.yaml](../compose.sss3.yaml)
- [Caddyfile.sss3](../Caddyfile.sss3)
- [.env.sss3.example](../.env.sss3.example)

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

## Cloudflare constraints

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
  though interim's own signed SDK requests succeed; browser uploads are
  unaffected. Use a browser-like `User-Agent`, exempt the S3 hostname from bot
  checks, or bypass Cloudflare.
- **Signed `Accept-Encoding` breaks sss3.** The Go SDK normally signs
  `Accept-Encoding: identity`; Cloudflare/sss3 produce `SignatureDoesNotMatch`.
  interim removes that header from the signing path and disables transport
  compression, so normal `HeadBucket`, `ListObjectsV2`, `HeadObject`, and
  presigned GET/PUT requests work.
- **HEAD through Cloudflare worked in testing.** The previously suspected
  HeadObject 403 was a Cloudflare/AWS CLI interaction, not an interim or sss3
  head failure.
- Add a Cache Rule bypass for the S3 hostname.
- Rotate the sss3 credentials before deployment.

## Example CORS listener

An alternative Caddy listener to put in front of sss3 (Cloudflare Tunnel points
at this listener; Caddy preserves the original `Host` header for SigV4):

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
