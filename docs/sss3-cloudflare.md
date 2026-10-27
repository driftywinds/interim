# sss3 with Cloudflare and Caddy

This is the small-file route. Cloudflare caps proxied request bodies at about
100 MB. If the home machine is behind restrictive NAT/CGNAT and you want files
larger than that, use the [Tailscale-only route](tailscale-sss3.md) instead.

Upgrade note: single-PUT completion performs a server-side `CopyObject` from a
staging key to an unexposed live key. Multipart completion uses the staging key
as the live object and avoids that copy. `S3_ENDPOINT` must still reach sss3
directly, not through the public Caddy listener, because that listener strips
`X-Amz-Copy-Source` and therefore cannot perform the single-PUT copy.

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
- strips every `X-Amz-*` request header before proxying, which prevents a
  presigned PUT from switching into `CopyObject`, `UploadPartCopy`, an ACL,
  tagging, object-lock, SSE, storage-class or future copy variant (an allowlist
  rather than a denylist);
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

The interim service should use a direct/internal endpoint for server-side calls
and the public Caddy hostname only for browser presigned URLs:

```dotenv
S3_ENDPOINT=http://stupid-simple-s3:5553
S3_PUBLIC_ENDPOINT=https://sss3.drifty.win
S3_REGION=us-east-1
S3_BUCKET=interim
S3_FORCE_PATH_STYLE=true
S3_PUBLIC_ENDPOINT_BLOCKS_COPY=true
```

`S3_ENDPOINT` must not point at the public Caddy listener: that listener strips
`X-Amz-Copy-Source`, so it cannot perform `CopyObject`. Put interim on the same
Docker network as this stack, or otherwise give it a direct/private route to
sss3. Keep `S3_PUBLIC_ENDPOINT` on the public Caddy hostname.

If multipart is enabled and sss3 rejects normal part PUTs with
`SignatureDoesNotMatch`, set `MULTIPART_SIGN_PART_LENGTH=false` and keep
`STUPID_MAX_PART_SIZE` at or below interim's `MULTIPART_PART_SIZE`. The checked-in
Compose file defaults the provider cap to 16 MiB; raise both values together if
you need an automatically larger part size for very large files. Raise
`STUPID_MAX_OBJECT_SIZE` above its 5 GiB default when `MAX_FILE_SIZE` exceeds
5 GB.

## Cloudflare constraints

Tested against `stupid-simple-s3` through a Cloudflare Tunnel. The following
matter for this deployment:

- **No CORS support.** sss3 answers browser `OPTIONS` preflights with `405` and
  sends no `Access-Control-Allow-*` headers. A browser cannot direct-PUT to it
  until a CORS-capable reverse proxy or Worker handles `OPTIONS` and adds the
  headers. See the Caddy example below.
- **Cloudflare body limit.** Cloudflare Free/Pro caps proxied request bodies at
  about 100 MB. Multipart uploads split files into separate requests, so each
  part must stay below that limit; the default 16 MiB part size is safe. Keep
  `MULTIPART_PART_SIZE` below the Cloudflare cap, or bypass Cloudflare for the
  S3 hostname.
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
- Pin the sss3 image by digest in production, for example with `SSS3_IMAGE`.
- The checked-in [Caddyfile.sss3](../Caddyfile.sss3) strips every `X-Amz-*`
  request header, so new S3 copy/ACL/tagging variants cannot slip past a fixed
  denylist. Keep that `header_up -X-Amz-*` rule on every public sss3 proxy, and
  use `S3_PUT_ACCESS_KEY`/`S3_PUT_SECRET_KEY` when your provider supports a
  write-only identity.

## Example CORS listener

An alternative Caddy listener to put in front of sss3 (Cloudflare Tunnel points
at this listener; Caddy preserves the original `Host` header for SigV4). The
`header_up -X-Amz-*` rule is the important part: it removes every S3 operation
header before the request reaches sss3, including future copy/ACL/tagging
variants that a denylist would miss.

```caddyfile
:8080 {
    @options method OPTIONS
    handle @options {
        header Access-Control-Allow-Origin "https://interim.example.com"
        header Access-Control-Allow-Methods "PUT, GET, HEAD, OPTIONS"
        header Access-Control-Allow-Headers "Content-Type, Content-Disposition, Content-Length"
        header Access-Control-Max-Age "86400"
        respond 204
    }
    handle {
        header Access-Control-Allow-Origin "https://interim.example.com"
        header Access-Control-Expose-Headers "ETag, Content-Length, Content-Disposition"
        reverse_proxy stupid-simple-s3:5553 {
            header_up Host {http.request.hostport}
            header_up -X-Amz-*
        }
    }
}
```

Set `S3_PUBLIC_ENDPOINT` to the public Caddy hostname (for example
`https://sss3.drifty.win`) and keep `S3_ENDPOINT` on a direct/private sss3
address. Keep `S3_FORCE_PATH_STYLE=true`, use region `us-east-1`, and use a
dedicated bucket. Cloudflare cannot raise its request-body limit, so large-file
support requires a non-proxied endpoint or a different upload design.
