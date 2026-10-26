# Quick start

Docker is the primary deployment target. The image can also run directly with
the Go binary; see [Development](development.md).

## Run with Docker

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

## Choose a storage route

| Route | Use it when |
| --- | --- |
| [S3-compatible storage](storage.md) | You have AWS S3, MinIO, R2, B2, Wasabi, or another S3 endpoint that browsers can reach. |
| [sss3 with Cloudflare and Caddy](sss3-cloudflare.md) | You run sss3 behind a Cloudflare Tunnel and keep uploads under Cloudflare's ~100 MB request-body cap. |
| [Tailscale-only sss3](tailscale-sss3.md) | sss3 is on a home machine behind CGNAT and you need uploads larger than Cloudflare allows. |
| [B2 private bucket with Cloudflare Worker](b2-worker.md) | You want B2's free Cloudflare egress with private, expiring downloads. |

The bucket must allow browser `PUT` requests from the interim origin. The exact
CORS policy and provider notes are in [S3-compatible storage](storage.md).
