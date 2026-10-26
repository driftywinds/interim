# S3-compatible storage

interim does not hard-code AWS behaviour. The AWS SDK v2 client uses SigV4,
path-style addressing by default, and forces checksum calculation and validation
to `when_required` for compatibility with smaller S3 clones.

For an internal sss3 endpoint:

```dotenv
S3_ENDPOINT=http://sss3:9000
S3_PUBLIC_ENDPOINT=https://objects.example.com
S3_REGION=us-east-1
S3_FORCE_PATH_STYLE=true
S3_BUCKET=interim
```

`S3_PUBLIC_ENDPOINT` matters: the server signs with the internal client but the
browser uploads to the public host. Both endpoints must point at the same bucket
and credentials. If you do not expose S3 directly, put it behind a reverse
proxy; do not rewrite the signed host/path.

The bucket should be private. The browser receives a PUT URL scoped to one
staging object key and a short expiry. interim sets `Content-Disposition:
attachment` through the signed PUT headers; in S3 download mode it also adds a
response override to signed GETs. In worker mode the Worker sets the response
header.

## Upload staging and write-only credentials

Presigned browser PUTs write to `<S3_OBJECT_PREFIX>uploads/<upload-id>`. After
verifying the staging object's size, the server copies it with `CopyObject` to
`<S3_OBJECT_PREFIX>objects/<random>` and commits only the live key. The client
never receives a URL for the live key, and the public endpoint should reject
`X-Amz-Copy-Source` and `X-Amz-Copy-Source-Range` so a presigned PUT cannot be
switched into a server-side copy.

For providers that support write-only credentials, set
`S3_PUT_ACCESS_KEY` and `S3_PUT_SECRET_KEY`. Those credentials are used only
for presigning browser PUTs; `S3_ACCESS_KEY` / `S3_SECRET_KEY` retain read,
copy, list, and delete access. On AWS, the upload identity needs only
`s3:PutObject` on the bucket/prefix. On B2, use a key with `writeFiles` only.
sss3 has no write-only credential, so its public proxy must enforce the
CopyObject block instead. Do not assume B2, R2, MinIO, or NooBaa rejects an
unsigned `X-Amz-Copy-Source`; use write-only upload credentials and/or a proxy
rule, and verify the provider's behavior.

## Required bucket CORS

Direct uploads require CORS on the bucket. Allow the interim origin, the `PUT`
method, and the headers included in the create-upload response:

```json
[
  {
    "AllowedOrigins": ["https://files.example.com"],
    "AllowedMethods": ["PUT"],
    "AllowedHeaders": ["content-type", "content-disposition"],
    "ExposeHeaders": ["ETag"],
    "MaxAgeSeconds": 3000
  }
]
```

API-only clients such as curl do not need CORS, but do need a browser-reachable
`S3_PUBLIC_ENDPOINT` and a provider that supports presigned PUT requests. If
`PUBLIC_BASE_URL` is HTTPS, the public S3 endpoint must also be HTTPS to avoid
mixed-content blocking. The app also calls `HeadObject` and `ListObjectsV2` for
verification and the orphan janitor. If the provider does not support
`ListObjectsV2`, the janitor logs the failure and still cleans known expired
intents.

## Provider notes

- **AWS S3:** leave both endpoints empty, set the real region,
  `S3_FORCE_PATH_STYLE=false`, and use an IAM role or static keys.
- **MinIO:** path-style, any consistent region, endpoint includes scheme.
- **Cloudflare R2:** path-style, region `auto` or `us-east-1`, endpoint
  `https://<account>.r2.cloudflarestorage.com`. Presign against the R2 host.
- **Backblaze B2:** path-style, region is the cluster ID
  (`us-west-004`), create the bucket in the B2 console.
- **Wasabi:** path-style is safest, use the provider's region endpoint.

## Specialized routes

- [sss3 with Cloudflare and Caddy](sss3-cloudflare.md): CORS proxy, Cloudflare
  Tunnel, and the ~100 MB request-body cap.
- [Tailscale-only sss3](tailscale-sss3.md): large uploads through a VPS and
  tailnet without Cloudflare in the data path.
- [B2 private bucket with Cloudflare Worker](b2-worker.md): private downloads
  with B2-to-Cloudflare free egress.
