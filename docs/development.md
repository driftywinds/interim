# Development

Go 1.27 is used in the Docker image.

```sh
go test ./...
go vet ./...
go run ./cmd/interim
```

The test suite uses an in-memory fake object store and covers setup, auth,
upload-intent creation, idempotent completion, direct upload verification,
ownership, quota rejection, abort, janitor expiry, orphan sweeping, expiry
cleanup, admin deletion, restricted-upload enforcement, admin-generated hashes,
approval changes, and the legacy-schema migration. S3 integration, CORS, and
presigned PUT behaviour must be tested against your provider.

Build the binary directly:

```sh
CGO_ENABLED=0 go build -trimpath -o bin/interim ./cmd/interim
```

For runtime settings, see [Configuration](configuration.md). For production
constraints, see [Deployment notes](deployment.md).
