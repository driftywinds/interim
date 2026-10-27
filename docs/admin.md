# Admin

`/admin` is gated by a DB-backed session created during first-run setup. Setup
requires the one-time `SETUP_TOKEN` (or `DATA_DIR/setup_token`). The admin can:

- see storage usage, the hard cap, active reservations, and every file;
- search by filename or ID and paginate;
- open files, delete one file, or select and bulk-delete (S3 object first);
- generate an approved user hash for someone and show it once;
- inspect user-hash prefixes, approval, usage, and last-used time;
- set a per-hash file-size limit and retention override;
- approve/unapprove uploads and revoke/restore authentication without exposing
  the raw hash.

Per-hash limits default to the instance `MAX_FILE_SIZE` and `RETENTION`. Set
either to `0` for unlimited: a hash with `max_file_size=0` may exceed the
instance file-size default (up to the provider limit), and a hash with
`retention=0` gets files that never expire. The retention value is captured when
each upload commits, so later edits apply to new files only. Hashes that predate
this feature inherit the instance defaults until edited.

Admin POSTs are CSRF-protected. Login is rate-limited per IP and per account;
password minimum is 12 characters. Passwords are bcrypt hashes (cost 12).
Sessions have an absolute `ADMIN_SESSION_TTL` and an idle
`ADMIN_IDLE_TIMEOUT`. **Logout** ends the current session; **Sign out
everywhere** revokes every session for the admin. The **Admin password** form
changes the password after verifying the current one and revokes every session,
including the current browser. There is intentionally only one admin account.

When `PUBLIC_UPLOADS_ENABLED=false`, only approved hashes can upload. Hashes
that existed before the switch are not grandfathered in, so approve them from
this page. A generated hash is approved automatically and shown only once.

See [Security model](security.md) for session and credential handling.
