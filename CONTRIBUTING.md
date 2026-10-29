# Contributing

Thanks for considering a change. Keep pull requests focused and small.

## Development

Requirements: Go 1.27+, Node.js 22+ (worker only), and optionally Docker.

```sh
go test ./...
go vet ./...
go build ./...
```

Worker:

```sh
cd worker
npm ci
node --check src/index.js
```

Match the existing style: `gofmt`, no new dependencies without a reason, and
tests for behavior changes. Security-sensitive changes should explain the trust
boundary they touch.

## Rules

- Never commit real credentials, `.env` files, bucket names, private hostnames,
  Cloudflare IDs, or captured browser data. Use the `*.example` files.
- Do not open a public issue for a vulnerability. Follow [SECURITY.md](SECURITY.md).
- By contributing, you agree that your contribution is licensed under the GNU
  Affero General Public License v3.0 in [LICENSE](LICENSE).
