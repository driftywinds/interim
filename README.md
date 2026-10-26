# interim

Temporary file hosting on S3-compatible storage. Uploads are public by default
and can be locked down to admin-approved user hashes with
`PUBLIC_UPLOADS_ENABLED=false`. Every file expires after the configured
`RETENTION` period (7 days by default), and downloads are served through
short-lived URLs from a private bucket.

Built in Go with SQLite metadata, a server-rendered web UI, a full REST API, and
Docker as the primary deployment target.

## Features

- Anonymous uploads, or an admin-approved upload allowlist.
- User hashes (`interim_…`) for API access, upload approval, and revocation.
- Direct browser-to-S3 uploads through short-lived presigned PUT URLs.
- Private bucket downloads with expiring signed or Worker-proxied URLs.
- Configurable retention, per-file limits, and total storage caps.
- First-run admin setup, file management, and a background janitor.

## Quick start

```sh
cp .env.example .env
# edit PUBLIC_BASE_URL, S3_* and capacity limits
docker compose up --build
```

Open `http://localhost:8080/setup`, then use the one-time token from
`docker exec interim cat /data/setup_token` (or your configured `SETUP_TOKEN`)
to create the admin account. The setup page is only available while no admin
exists.

The SQLite database, session secret, and WAL files live in the `interim-data`
volume at `/data`. Back up the volume, not individual files while the
container is running.

## Documentation

- **Start here:** [Quick start](docs/quickstart.md) ·
  [Configuration](docs/configuration.md) ·
  [S3-compatible storage](docs/storage.md)
- **Deployment routes:** [sss3 with Cloudflare and Caddy](docs/sss3-cloudflare.md) ·
  [Tailscale-only sss3](docs/tailscale-sss3.md) ·
  [B2 private bucket with Cloudflare Worker](docs/b2-worker.md)
- **Operations:** [Capacity, uploads, and retention](docs/capacity-and-retention.md) ·
  [Deployment notes](docs/deployment.md) ·
  [Security model](docs/security.md)
- **Usage:** [API](docs/api.md) · [Web UI and browser history](docs/web-ui.md) ·
  [Admin](docs/admin.md)
- **Contributing:** [Development](docs/development.md)
- **All documentation:** [docs/README.md](docs/README.md)
- **Cloudflare Worker setup:** [worker/README.md](worker/README.md)

## Development

```sh
go test ./...
go vet ./...
go run ./cmd/interim
```

Go 1.27 is used in the Docker image. See [Development](docs/development.md) for
the test coverage and direct-build command.
