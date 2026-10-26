# API

Start at `/api` for the rendered documentation or `/api/openapi.json` for the
OpenAPI 3.1 document. Examples use `PUBLIC_BASE_URL`.

By default, upload intents are public. Set `PUBLIC_UPLOADS_ENABLED=false` to
require an admin-approved user hash. In that mode `POST /api/v1/keys` is
disabled, and every existing or self-created hash stays unapproved until an
admin approves it in `/admin`. Anonymous uploads return
`401 uploads_restricted`; a valid but unapproved hash returns
`403 upload_not_allowed`. Completion and abort still use only the upload token.

```sh
# Create a user hash (shown once; public mode only)
curl -X POST https://files.example.com/api/v1/keys \
  -H 'Content-Type: application/json' \
  -d '{"name":"cli"}'

# Create an upload intent
curl -X POST https://files.example.com/api/v1/uploads \
  -H 'Authorization: Bearer interim_...' \
  -H 'Content-Type: application/json' \
  -d '{"name":"sample.bin","size":12345,"content_type":"application/octet-stream"}'

# PUT the file bytes directly to the signed URL from that response
curl -X PUT 'SIGNED_S3_URL' \
  -H 'Content-Type: application/octet-stream' \
  -H 'Content-Disposition: attachment; filename=sample.bin' \
  --data-binary @sample.bin

# Verify and commit it
curl -X POST 'COMPLETE_URL' \
  -H 'X-Upload-Token: UPLOAD_TOKEN'

# Get a download URL (expires after DOWNLOAD_URL_TTL)
curl -X POST https://files.example.com/api/v1/files/FILE_ID/download

# Delete with the owning user hash
curl -X DELETE https://files.example.com/api/v1/files/FILE_ID \
  -H 'Authorization: Bearer interim_...'
```

Upload intents return a per-file `delete_token` (shown once); use
`X-Delete-Token: <token>` with `DELETE /api/v1/files/{id}` to remove anonymous
files. Raw user hashes, upload tokens, and delete tokens are stored only as
SHA-256 hashes.

### Multipart mode

If `MULTIPART_UPLOAD_ENABLED=true` and the file exceeds
`MULTIPART_THRESHOLD`, the create response has `mode: "multipart"` and no
single `url`. It includes `part_size`, `part_count`, `parts_url`, and
`status_url`.

1. `POST parts_url` with `X-Upload-Token` and
   `{"part_numbers":[1,2,3]}`. The response contains short-lived part URLs.
2. PUT each part body to its URL. Read the response `ETag`.
3. `PUT parts_url/{n}` with `{"etag":"...","size":123}` and
   `X-Upload-Token`. `size` must be exact: non-final parts equal `part_size`,
   and the final part equals the remaining bytes. When
   `MULTIPART_SIGN_PART_LENGTH=true`, the part URL signs that Content-Length, so
   send a fixed-length body, not chunked transfer encoding.
4. Repeat until `GET status_url` reports all parts in `completed_parts`.
5. Call `complete_url` with `X-Upload-Token`.

All parts from 1 through `part_count` must be reported before completion; parts
can be uploaded and reported concurrently. The server verifies provider-side
part sizes/ETags, assembles the multipart object, and uses the staging key as the
live object (no second copy). The final `HeadObject` size check still runs. A
provider-side size or ETag mismatch returns `413 part_size_mismatch`. A failed
part can be retried independently; `GET status_url` lets a client resume after
reload.

## Endpoints

| Method | Path | Auth | Purpose |
| --- | --- | --- | --- |
| `POST` | `/api/v1/keys` | no | Create user hash. Disabled when public uploads are off. |
| `GET` | `/api/v1/keys/me` | bearer | Key prefix, approval, and usage. |
| `POST` | `/api/v1/uploads` | optional; approved when restricted | Reserve quota and create single-PUT or multipart upload state. |
| `GET` | `/api/v1/uploads/{id}` | upload token | Multipart status and completed part numbers. |
| `POST` | `/api/v1/uploads/{id}/parts` | upload token | Request up to 100 presigned part URLs. |
| `PUT` | `/api/v1/uploads/{id}/parts/{n}` | upload token | Record a part ETag and size. |
| `POST` | `/api/v1/uploads/{id}/complete` | upload token | Assemble/verify and commit the object. |
| `DELETE` | `/api/v1/uploads/{id}` | upload token | Abort a pending upload and release quota. |
| `GET` | `/api/v1/files` | bearer | List the caller's active files. |
| `GET` | `/api/v1/files/{id}` | no | Public metadata. |
| `POST` | `/api/v1/files/{id}/download` | no | Short-lived download URL. |
| `DELETE` | `/api/v1/files/{id}` | bearer or token | Delete. |
| `GET` | `/api/v1/status` | no | Capacity and retention status. |

Rate limits apply to user-hash creation, upload-intent creation, download-link
creation, and admin login. Errors use
`{"error":{"code","message","request_id"}}`.

The upload allowlist and admin-side key generation are described in
[Admin](admin.md). Quota and expiry behavior is in
[Capacity, uploads, and retention](capacity-and-retention.md).
