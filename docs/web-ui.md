# Web UI and browser history

The home page creates an upload intent, PUTs the selected file directly to S3
with XHR for progress, then calls the completion endpoint. Canceling aborts the
intent. Upload URLs are short-lived, so a slow upload can fail before completion;
the UI reports that and tells the user to try again.

Completed uploads are stored in `localStorage` under `interim.uploads.v1`,
including the per-file delete token. Only that browser can see the list.
Clearing site data loses the local delete ability, but the file still expires
after the configured `RETENTION` period and the admin can delete it.

When `PUBLIC_UPLOADS_ENABLED=false`, the home page asks for an admin-issued
user hash. It validates the hash with `GET /api/v1/keys/me`, requires
`approved: true`, and stores it in `localStorage` under
`interim.uploadHash.v1`. Future upload intents send it as a bearer token.
Invalid, revoked, or unapproved hashes clear the saved value and reopen the
prompt. The “Forget” button removes it from the browser. The hash is never put
in a URL or log line. Anyone with access to that browser profile can read the
stored value, so treat it like a password.

The download page never receives or displays the delete token. Its download
button links to a server endpoint that creates a fresh download link and
redirects to it. Direct object URLs do not exist in the HTML.

The UI is deliberately minimal: system sans-serif, no webfonts, no logo image,
native controls, automatic light/dark mode, and monospace filenames.

See [Admin](admin.md) for generating and approving user hashes, and
[API](api.md) for the same flow from scripts.
