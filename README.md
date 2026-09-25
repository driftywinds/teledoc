# TeleDoc

A Telegram-channel document archive with simple UI, written in Go, single binary using SQLite

Upload documents (pdf, docx, txt, csv, ...) to a Telegram channel; the bot archives each one, auto-tags it with your **tagging rules**, and a dark, minimal web UI lets you browse, search and filter everything. Clicking a document opens its original Telegram message.

## How it works

```
Telegram channel ──uploads──> Bot (long polling) ──> tagging rules ──> SQLite
                                                                        │
        Browser  <────────── Papra-style web UI (Go html/template + htmx) <┘
                                     │
                                     └── document card links to t.me message
```

- **Bots cannot read channel history.** The bot only sees documents uploaded *after* it is added to the channel as an admin. You can reuse an existing channel (old files won't appear) or make a fresh dedicated one.
- **Metadata only** — file bytes stay in Telegram; the archive stores names, sizes, mime types, tags and message links, so there's no 20 MB Bot API download cap to worry about.
- **Message links**: public channels get `t.me/<username>/<id>`; private channels get `t.me/c/<id>/<msg>`, which opens for channel members (you) only.

## Tagging rules (Papra-parity)

Rules have conditions (field + operator + value, each with its own case-sensitivity flag, default case-**insensitive**) and apply one or more tags. Match mode is **ALL** or **ANY**. A rule with no conditions matches every document.

| Field | Operators |
|---|---|
| File name | `contains`, `does not contain`, `equals`, `does not equal`, `starts with`, `ends with` |
| Extension | same |
| MIME type | same |

Example: *File name contains "manual" (case-insensitive) → applies tag **Manual*** — so `Casio A286 Manual.pdf` is tagged automatically.

Rules apply to new uploads immediately. Use **Run now** on a rule to re-apply it to the whole existing archive (Papra's "Apply to existing documents") and get a processed/tagged report.

## Run locally (Windows)

```powershell
# 1. Configure
copy .env.example .env   # then edit: set BOT_TOKEN (and optionally ADMIN_PASSWORD)

# 2. Run (PowerShell)
$env:BOT_TOKEN="123456:ABC..."; go run .

# 3. Open the UI
start http://localhost:9879
```

Get a bot token from [@BotFather](https://t.me/BotFather), then **add the bot to your channel as an administrator** with post-message permission.

## Run with Docker

```bash
cp .env.example .env    # set BOT_TOKEN (and optionally ADMIN_PASSWORD)
docker compose up -d --build   # UI on http://localhost:9879
```

The SQLite database persists in the `teledoc-data` volume at `/data/teledoc.db`.

## Configuration

| Env var | Default | Meaning |
|---|---|---|
| `BOT_TOKEN` | — (required) | Bot token from @BotFather |
| `ADMIN_PASSWORD` | *(empty)* | Web UI password; empty = no auth (trusted LAN only) |
| `DB_PATH` | `teledoc.db` | SQLite database location |
| `LISTEN_ADDR` | `:9879` | Web UI listen address |

## Development

```bash
go test ./...      # rule engine tests (Papra semantics)
go vet ./...
go build .
```

Layout: `internal/store` (SQLite), `internal/rules` (Papra-style rule engine), `internal/telegram` (ingestion), `internal/web` (UI, embedded templates/static including vendored htmx).
