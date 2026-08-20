# PageRelay

English | [中文](./README.zh-CN.md)

> A lightweight file management and public sharing service for teams and intranets. One Go binary serves both an admin console and a public share endpoint — metadata in SQLite, files on local disk. No object storage, no database ops, no frontend build step.

## Live Demo

- **URL**: <https://pagerelay.348580.xyz/login>
- **Account**: `demo`
- **Password**: `1234567890`

> The demo is reset periodically — please don't upload sensitive data. Try uploading a file, toggling a share, and opening the public link in an incognito window to see the two isolated endpoints.

## Why PageRelay

Teams constantly produce files that need **internal management** and **external sharing**: product docs, quotes, deliverables, demo pages. The usual answers — chat attachments (no recall), public cloud drives (data leaves the org), or self-built object storage + gateway + frontend (heavy ops) — all miss something.

PageRelay sits in between: **self-hostable, single binary, isolated share endpoints, revocable links, online preview, per-user quotas and audit logs.** It covers the 80% of daily file distribution without the operational drag.

## Features

- **Dual-listener design** — admin console (logged-in users) and public share endpoint (anonymous visitors) on separate ports/domains; the public domain exposes no admin surface.
- **Share is a toggle** — turning a share off instantly invalidates the old link; turning it back on generates a new one. No confusing expiry dates.
- **Workspace UI** — drag-and-drop upload, lazy directory tree, sortable/searchable table, multi-select, batch move/delete, ZIP download, `⌘/Ctrl + K` command palette.
- **Online preview** — images, PDF, video, audio, text, code, Markdown, HTML, SVG. HTML/SVG run sandboxed on the share domain; Markdown is rendered in safe mode.
- **Create & edit** Markdown, HTML, and plain-text files in place.
- **Multi-user** — admins create/disable accounts, reset passwords, assign storage quotas; each account's files are isolated by ID.
- **Security** — Argon2id password hashing, CSRF protection, path-traversal guards, per-account directory isolation, operation audit logs.

## Architecture

```
                      ┌────────────────────────────────────────────┐
                      │            single Go binary               │
   logged-in ──HTTP──▶│  admin listener (:8080)                   │
                      │   login / files / share records / users   │
                      │   audit / JSON API                        │
                      │                                           │
   anonymous──HTTP──▶│  share listener (:8081)                   │
                      │   /s/<token>  public share entry          │
                      │   /p/<token>  sandboxed preview           │
                      │                                           │
                      │  metadata: SQLite (WAL)                   │
                      │  files:    local disk (data/<user-id>/)   │
                      └────────────────────────────────────────────┘
```

Frontend assets are embedded via `//go:embed web/*` — no `web/` directory needed on the server.

## Quick Start

Requires Go 1.26+.

```sh
git clone https://github.com/abcwyc/PageRelay.git
cd PageRelay
go run . \
  -listen 127.0.0.1:8080 \
  -share-listen 127.0.0.1:8081 \
  -data ./data \
  -db ./filehub.db
```

On first launch without a password file, a bootstrap admin `admin` / `admin` is created — you'll be forced to set a new password on first login. For a safer bootstrap, use a password file you can delete afterward:

```sh
echo -n 'my-strong-password' > /tmp/admin.secret
go run . -admin-password-file /tmp/admin.secret
rm /tmp/admin.secret   # after login + password change
```

Open <http://127.0.0.1:8080> to sign in.

## Deployment

### 1. Build a static binary

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags='-s -w' -o dist/filehub-linux-amd64 .
```

`modernc.org/sqlite` is a pure-Go driver, so the result is fully static — no libc on the target host.

### 2. systemd unit

```ini
[Unit]
Description=PageRelay
After=network.target

[Service]
Type=simple
User=filehub
Group=filehub
ExecStart=/usr/local/bin/filehub \
  -listen 0.0.0.0:8080 \
  -share-listen 0.0.0.0:8081 \
  -public-base https://share.example.com \
  -db /var/lib/filehub/filehub.db \
  -data /srv/filehub/users
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now filehub
```

### 3. Reverse proxy + TLS (recommended)

Bind the admin and share listeners to separate domains (e.g. `relay.example.com` and `share.example.com`) with Nginx/Caddy and enable TLS. Once HTTPS is in place, add `-public-base https://share.example.com` and `-cookie-secure` to the service args so share links use `https` and session cookies carry the `Secure` flag.

## Usage

- **Upload** — drag files onto the workspace drop zone, or onto a folder in the tree to target that directory.
- **Browse** — lazy tree on the left; sortable, searchable table on the right. Click a file to preview.
- **Edit** — create and edit Markdown / HTML / plain text in place.
- **Batch** — checkbox or `Shift`-click to multi-select, then batch move / delete / ZIP-download.
- **Share** — each file has a share toggle; copying the generated link gives anonymous access. Turning it off invalidates the link immediately. New uploads default to shared.
- **Admin** — manage accounts, quotas, and view audit logs under *System Admin*.

## Configuration

| Flag | Default | Description |
| --- | --- | --- |
| `-listen` | `127.0.0.1:8080` | Admin listener |
| `-share-listen` | `127.0.0.1:8081` | Public share listener |
| `-public-base` | empty | Public base URL, e.g. `https://share.example.com`, used to build share links |
| `-db` | `./filehub.db` | SQLite database path |
| `-data` | `./data` | File storage root |
| `-admin-user` | `admin` | Bootstrap admin username |
| `-admin-password` | empty | Bootstrap admin password (plaintext, discouraged) |
| `-admin-password-file` | empty | Read bootstrap password from a file (deletable after first login) |
| `-cookie-secure` | `false` | Mark session cookies `Secure` (enable with HTTPS) |

## Security Model

- Per-account physical directories; only logical paths are exposed, with path-traversal guards.
- Argon2id password hashing; CSRF tokens on all writes.
- HTML/SVG previews sandboxed on the share domain (separate cookie scope); Markdown rendered in safe mode.
- Shares are revocable; re-enabling mints a new token.
- Key admin operations are recorded in the audit log.
- Storage quotas per account; over-quota uploads are rejected.
- Use TLS + `-cookie-secure` in production to protect credentials and sessions in transit.

## Test & Build

```sh
node --check web/app.js       # frontend syntax check (no build step)
go test -race ./...
go vet ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags='-s -w' -o dist/filehub-linux-amd64 .
```

Tests cover login, CSRF, account isolation, quota enforcement, default shares, share revoke/regenerate, safe HTML/Markdown preview, legacy link migration, and logo static assets.

## Migration Notes

When upgrading to PageRelay 2.0:

- Previously valid time-limited links become permanent.
- Already-revoked links stay invalid.
- Historical files with no share record are not auto-published.

## License

[MIT](./LICENSE) — source is fully open. Feel free to learn, audit, customize, and self-host. Pull requests are welcome.
