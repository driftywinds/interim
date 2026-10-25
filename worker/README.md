# interim download worker

Private-B2 download proxy for interim. It receives a short-lived HMAC token,
fetches the private object from B2, and streams it to the browser.

The Worker must proxy the bytes. A Worker that only signs a B2 URL and redirects
does not route B2 traffic through Cloudflare, so it does not get the B2 →
Cloudflare free-egress treatment.

## 1. Backblaze setup

1. Create a **private** B2 bucket.
2. Create a read-only application key:
   - bucket access: only the interim bucket
   - prefix: `files/`
   - capabilities: `readFiles`, `listFiles` (list is not required by the Worker,
     but helps operational checks)
3. Note the B2 S3 endpoint, for example:
   - bucket region: `us-west-004`
   - endpoint: `https://s3.us-west-004.backblazeb2.com`

Interim uploads directly to B2; it does not go through this Worker. Configure
B2 bucket CORS for the interim origin, the `PUT` method, and the
`Content-Type` and `Content-Disposition` headers. The generic CORS example is in
the main README.

## 2. Worker configuration

Install dependencies and create your local config:

```sh
cd worker
npm ci
cp example.wrangler.toml wrangler.toml
```

`wrangler.toml` is gitignored. Edit it with your real domains and B2 details:

```toml
name = "interim-download"
routes = [
  { pattern = "dl1.example.com", custom_domain = true }
]

[vars]
B2_ENDPOINT = "https://s3.us-west-004.backblazeb2.com"
B2_REGION = "us-west-004"
B2_BUCKET = "interim"
B2_ACCESS_KEY_ID = "read-only-key-id"
B2_KEY_PREFIX = "files/"
CORS_ORIGIN = "https://interim.example.com"
```

Set secrets:

```sh
npx wrangler secret put B2_SECRET_ACCESS_KEY
npx wrangler secret put DOWNLOAD_TOKEN_SECRET
```

`DOWNLOAD_TOKEN_SECRET` must exactly match interim's
`DOWNLOAD_WORKER_SECRET`. It must be at least 32 characters.

Deploy:

```sh
npx wrangler deploy
```

The custom domain must be proxied through Cloudflare. The Worker gets a public
TLS certificate automatically.

## 3. interim configuration

```dotenv
DOWNLOAD_WORKER_ENABLED=true
DOWNLOAD_WORKER_URLS=https://dl1.example.com
DOWNLOAD_WORKER_SECRET=<same value as DOWNLOAD_TOKEN_SECRET>
```

The download URL path is always `/d/<token>`. The configured URL must not
include a path.

When `DOWNLOAD_WORKER_ENABLED=false`, interim keeps its existing presigned S3
download behavior.

## Multiple worker URLs / accounts

Interim accepts a comma- or whitespace-separated list:

```dotenv
DOWNLOAD_WORKER_URLS=https://dl1.example.com,https://dl2.example.com
```

It rotates new download links across the list. All workers must:

- use the same `DOWNLOAD_TOKEN_SECRET`
- read the same private B2 bucket/prefix
- be reachable over HTTPS

To deploy to a second Cloudflare account, copy your local `wrangler.toml` to
`wrangler.dl2.toml`, change `name` and the route, then deploy with that account
and config. Both local files are gitignored:

```sh
CLOUDFLARE_ACCOUNT_ID=<account-id> npx wrangler deploy --config wrangler.dl2.toml
```

Set the same B2 read key and token secret in each account.

A Worker custom domain must belong to a zone in the **same Cloudflare account**
as that Worker. If the second account does not own a separate domain/zone, set
`workers_dev = true` in its config and use its
`https://<worker-name>.<account-subdomain>.workers.dev` URL instead of a custom
domain. Do not try to attach `dl2.drifty.win` to a Worker in a different account
unless that zone is actually in that account.

Cloudflare's free Worker request allowance is per account, not per Worker.
Using multiple accounts to evade the free allowance is a terms-of-service and
quota-abuse risk. Use multiple endpoints only for legitimate redundancy or
separate deployments, and review Cloudflare's terms for your traffic profile.

## Quota and cost

The Worker makes one invocation per download, `HEAD`, or range request. Large
files do not cost more requests, but download managers that issue many ranges
do.

| Plan | Included | Overage |
|---|---:|---:|
| Workers Free | 100,000 requests/day | Hard stop |
| Workers Paid | 10,000,000 requests/month | $0.30 per extra million |

Workers Paid also includes 30 million CPU milliseconds/month; B2 → Cloudflare
and Cloudflare → user have no egress charges. B2 storage and API calls are
separate.

See [Cloudflare Workers pricing](https://developers.cloudflare.com/workers/platform/pricing/).

## What the Worker does

- Verifies the HMAC token and expiry.
- Rejects objects outside `B2_KEY_PREFIX`.
- Signs a path-style B2 S3 request with the read-only key.
- Streams the B2 response without buffering it in Worker memory.
- Forwards `Range` / `If-Range` and preserves `206 Partial Content`.
- Supports `HEAD`.
- Sets `Content-Disposition: attachment`.
- Sets `Cache-Control: private, no-store` so a cached response cannot outlive
  the token.
- Optionally adds CORS headers for `fetch()`-style API clients.

## Why no edge cache

The token is short-lived. If Cloudflare cached a successful download, it could
serve that object after the token expired. B2 egress through Cloudflare is free
anyway, so this worker deliberately disables caching.

If you later want caching, use a separate public-delivery mode with its own
access model; do not cache tokenized private downloads.

## Cloudflare terms

Cloudflare's Free/Pro/Business CDN terms may restrict applications serving
disproportionate amounts of large files or video. The B2 Bandwidth Alliance does
not override Cloudflare's terms. Review Cloudflare's delivery policy before
using this for a large public file host.
