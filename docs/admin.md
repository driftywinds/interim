# Admin

`/admin` is gated by a DB-backed session created during first-run setup. The
admin can:

- see storage usage, the hard cap, active reservations, and every file;
- search by filename or ID and paginate;
- open files, delete one file, or select and bulk-delete (S3 object first);
- generate an approved user hash for someone and show it once;
- inspect user-hash prefixes, approval, usage, and last-used time;
- approve/unapprove uploads and revoke/restore authentication without exposing
  the raw hash.

Admin POSTs are CSRF-protected. Login is rate-limited. Passwords are bcrypt
hashes (cost 12). There is intentionally only one admin account.

When `PUBLIC_UPLOADS_ENABLED=false`, only approved hashes can upload. Hashes
that existed before the switch are not grandfathered in, so approve them from
this page. A generated hash is approved automatically and shown only once.

See [Security model](security.md) for session and credential handling.
