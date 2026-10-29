# interim documentation

[interim](../README.md) is a temporary file host backed by S3-compatible
storage. Files expire after the configured retention period, and downloads use
short-lived signed or Worker-proxied URLs from a private bucket.

Start with [Quick start](quickstart.md) if you want a running instance, or read
[Configuration](configuration.md) for every environment variable.

## Getting started

| Guide | Contents |
| --- | --- |
| [Quick start](quickstart.md) | Docker setup, first admin account, and choosing a storage route. |
| [Configuration](configuration.md) | Full environment-variable reference. |
| [S3-compatible storage](storage.md) | Endpoint rules, bucket CORS, and provider notes for AWS, MinIO, R2, B2, and Wasabi. |

## Storage and deployment routes

| Guide | Contents |
| --- | --- |
| [sss3 with Cloudflare and Caddy](sss3-cloudflare.md) | Small-file sss3 route, CORS proxy, Cloudflare Tunnel, and Cloudflare limits. |
| [Tailscale-only sss3](tailscale-sss3.md) | Large files through a VPS and tailnet, bypassing Cloudflare's request-body cap. |
| [B2 private bucket with Cloudflare Worker](b2-worker.md) | Free B2-to-Cloudflare egress while keeping expiring, private downloads. |
| [Deployment notes](deployment.md) | TLS, proxies, SQLite, backups, and production constraints. |

## Operations

| Guide | Contents |
| --- | --- |
| [Capacity, uploads, and retention](capacity-and-retention.md) | Quota reservations, upload intents, expiry, and the janitor. |
| [Security model](security.md) | Credential storage, upload/download controls, and admin protections. |

## Using interim

| Guide | Contents |
| --- | --- |
| [API](api.md) | Examples, upload flow, endpoints, and limits. |
| [Web UI and browser history](web-ui.md) | Browser uploads, local history, and restricted uploads. |
| [Admin](admin.md) | Admin capabilities, user hashes, and approval. |

## Contributing

| Guide | Contents |
| --- | --- |
| [Contributing](../CONTRIBUTING.md) | Development workflow and repository rules. |
| [Development](development.md) | Tests, vet, local runs, and direct builds. |
| [Security policy](../SECURITY.md) | Private vulnerability reporting and scope. |
| [License](../LICENSE) | GNU AGPL v3.0 license terms. |

The Cloudflare Worker has its own deployment guide in
[worker/README.md](../worker/README.md).
