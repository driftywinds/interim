# Security policy

## Reporting a vulnerability

Please use GitHub's **Private vulnerability reporting** for this repository:
open the **Security** tab and choose **Report a vulnerability**. Do not open a
public issue for a suspected vulnerability.

Include the affected version/commit, a description, reproduction steps, and
the impact you believe is possible. Do not include live credentials, private
bucket contents, or other people's data.

There is no guaranteed response time. This is a small, single-maintainer
project; reports are triaged as time allows. Please allow a reasonable window
for a fix before public disclosure.

## Supported versions

Security fixes are made on the `main` branch and in the latest tagged release.
Older tags are not maintained.

## Scope

In scope:

- authentication and session handling;
- upload/download authorization and presigned URL handling;
- quota, retention, and storage accounting;
- admin, worker, and S3 configuration;
- injection, XSS, CSRF, and path handling in the web UI/API.

Out of scope:

- vulnerabilities in S3/B2/sss3, Cloudflare, Caddy, or the host OS;
- misconfiguration of a deployment, including exposed admin credentials,
  writable buckets, or an origin reachable without its trusted proxy;
- denial of service against a single self-hosted instance;
- reports that require physical access to the server or the admin's browser.

## Deployment expectations

The security model assumes a private bucket, TLS, a trusted reverse proxy, and
an operator who has changed every secret in `.env`. See
[docs/security.md](docs/security.md) and [docs/deployment.md](docs/deployment.md).
