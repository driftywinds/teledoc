# TeleDoc

> **Note:** This project is entirely AI-generated — built by an AI coding agent — with supervision and tidying up of the code by **drifty**.

A Telegram-channel document archive with simple UI, written in Go, single binary using SQLite

Upload documents (pdf, docx, txt, csv, ...) to a Telegram channel; the bot archives each one, auto-tags it with your **tagging rules** and **caption #hashtags**, reacts to the message with a status emoji, and a dark, minimal web UI lets you browse, search and filter everything. Clicking a document opens its original Telegram message.

## How it works

```
Telegram channel ──uploads──> Bot (long polling) ──> tagging rules ──> SQLite
                                     │                                │
                                     └─ caption #hashtags             │
        Browser  <────────── Papra-style web UI (Go html/template + htmx) <┘
                                     │
                                     └── document card links to t.me message
```

The bot also reacts to each document message in the channel with a status emoji (👍 archived · 👀 duplicate · 🤔 tagging failed) — see [Status reactions](#status-reactions).

- **Bots cannot read channel history.** The bot only sees documents uploaded *after* it is added to the channel as an admin. You can reuse an existing channel (old files won't appear) or make a fresh dedicated one.
- **Metadata only** — file bytes stay in Telegram; the archive stores names, sizes, mime types, tags and message links, so there's no 20 MB Bot API download cap to worry about. Deleting a document in the web UI removes only the archive entry — the file stays on Telegram.
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

## Caption hashtags

Hashtags in a document's caption tag the document directly on upload, bypassing the rules engine entirely — the Telegram-native way to tag. A caption like `quarterly report #work` tags the document under **work**.

- Tag resolution is **case-insensitive**: `#work` reuses an existing `Work`/`WORK` tag whatever its casing; if no tag exists in any casing, a new one is created **lowercased**.
- Detection uses Telegram's own hashtag recognition (caption entities), so emoji and unicode in captions are handled correctly.
- Rules still run independently on top — both sources can tag the same document.

## Editing captions

When you edit a posted document's caption (fix a typo, add or remove a hashtag), the bot re-syncs the hashtags on the archived document:

- **Adding** a hashtag in an edit tags the document; **removing** a hashtag untags it — but only if that tag's origin was a caption hashtag.
- **Rules and manual web-UI tags always take precedence.** A tag that arrived from a rule or a manual action is never removed by a caption edit, and mentioning it as a hashtag doesn't hand ownership to the caption. If a rule/manual action applies a tag the caption created first, the rule claims it and the hashtag becomes a no-op from then on.
- Edits of non-document posts, and edits of documents that are not in the archive (posted before the bot joined, or deleted via the web UI), are ignored — web-UI deletion stays authoritative.
- **How long are edits picked up?** While the bot is running, forever — an edit to a months-old post still re-tags it. If the bot is offline, Telegram only queues undelivered updates for roughly **24 hours**, so edits made during a longer outage are lost. Short outages replay cleanly (ingestion is idempotent, so no double-tagging or duplicate reactions).

## Status reactions

The channel doubles as a status indicator: the bot reacts to each document message.

| Reaction | Meaning |
|---|---|
| 👍 | Archived (with or without tags) |
| 👀 | Duplicate: a file with this name was already archived from the channel (case-insensitive) |
| 🤔 | Archived, but tagging failed — needs attention (also flipped to on edit-time tagging failures) |

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
go test ./...      # rule engine, store (incl. hashtag sync/provenance) and bot tests
go vet ./...
go build .
```

Layout: `internal/store` (SQLite), `internal/rules` (Papra-style rule engine), `internal/telegram` (ingestion, hashtag parsing, reactions), `internal/web` (UI, embedded templates/static including vendored htmx).
