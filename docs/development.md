# Development

Go 1.27 is used in the Docker image.

```sh
go test ./...
go vet ./...
go run ./cmd/interim
```

The test suite uses an in-memory fake object store and covers setup token
enforcement, auth, upload-intent creation, staging-to-live copy-on-complete,
single-PUT completion, multipart part URLs/reporting/status/completion/abort,
exact part-size validation, oversized-provider-part rejection, abort-failure
retention, orphan multipart sweeping, endpoint rate limits, multipart janitor
cleanup, idempotent completion, direct upload verification, ownership,
per-client and global quota rejection, abort, janitor expiry, orphan sweeping,
expiry cleanup, admin deletion, restricted-upload enforcement, admin-generated
hashes, approval changes, trusted-proxy parsing, cookie naming, and schema
migrations. S3 integration, CORS, CopyObject, multipart, and presigned PUT
behaviour must be tested against your provider.

Build the binary directly:

```sh
CGO_ENABLED=0 go build -trimpath -o bin/interim ./cmd/interim
```

For runtime settings, see [Configuration](configuration.md). For production
constraints, see [Deployment notes](deployment.md).
