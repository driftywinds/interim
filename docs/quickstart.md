# Quick start

Docker is the primary deployment target. The image can also run directly with
the Go binary; see [Development](development.md).

## Run with Docker

```sh
cp .env.example .env
# edit PUBLIC_BASE_URL, S3_* and capacity limits
docker compose up --build
```

The Compose file binds the Go listener to `127.0.0.1:8080`; put Caddy,
cloudflared, or another trusted proxy in front of it for public access. When you
do, set `TRUST_PROXY=true` and `TRUSTED_PROXY_CIDRS` to that proxy's source
range; otherwise all per-IP limits collapse into one global bucket. Open
`http://localhost:8080/setup` and enter the one-time setup token:

```sh
docker exec interim cat /data/setup_token
```

Alternatively set `SETUP_TOKEN` in `.env` before starting. The setup page is
only available while no admin exists, and the generated token file is deleted
after successful setup.

The SQLite database, generated session secret, and WAL files live in the
`interim-data` volume at `/data`. Back up the volume, not individual files
while the container is running.

## Choose a storage route

| Route | Use it when |
| --- | --- |
| [S3-compatible storage](storage.md) | You have AWS S3, MinIO, R2, B2, Wasabi, or another S3 endpoint that browsers can reach. |
| [sss3 with Cloudflare and Caddy](sss3-cloudflare.md) | You run sss3 behind a Cloudflare Tunnel and keep uploads under Cloudflare's ~100 MB request-body cap. |
| [Tailscale-only sss3](tailscale-sss3.md) | sss3 is on a home machine behind CGNAT and you need uploads larger than Cloudflare allows. |
| [B2 private bucket with Cloudflare Worker](b2-worker.md) | You want B2's free Cloudflare egress with private, expiring downloads. |

The bucket must allow browser `PUT` requests from the interim origin. The exact
CORS policy and provider notes are in [S3-compatible storage](storage.md).
