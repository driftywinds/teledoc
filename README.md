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

**Mass imports:** re-uploading a folder of files to backfill the archive works — ingestion keeps pace (~1 file/second) and archiving is never throttled. Reactions are sent by a background queue at ~1/second (Telegram's reaction quota is much smaller than the upload quota), so during a big import the 👍 may trail the archive by a few seconds per file; if a reaction still hits flood control, the bot waits out Telegram's `retry_after` and retries (up to 3 attempts) instead of dropping it.

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
| `TEMP_DIR` | `<os temp>/teledoc-downloads` | Where downloaded files live before being served |
| `TEMP_FILE_TTL_MINUTES` | `10` | How long a downloaded file stays available |
| `NO_COLOR` | *(unset)* | Any non-empty value disables colored log output |
| `FORCE_COLOR` | *(unset)* | `1` forces colored log output even when piped |

## Downloading documents

Every document row has a download button next to the delete button. Files are
never stored permanently: the archive keeps metadata only, and a download is a
short-lived copy fetched from Telegram on demand.

**How a download works**

1. **Click.** The server looks up the document's Telegram file id and logs
   `web: download triggered for document N (...)`.
2. **Fetch.** The bot asks Telegram for the file and streams it into
   `TEMP_DIR` as `teledoc-<id>-<file name>` (in-flight fetches use a
   `.teledoc-part-*` name and are renamed on success). If Telegram answers with
   a flood-control `429`, the server waits out the demanded `retry_after` (as
   long as it fits a short budget) and retries instead of failing the click.
   Logged as `web: downloaded document N (...) ... available until HH:MM:SS`.
3. **Redirect.** The browser is redirected to `/temp/<id>/<file name>`, served
   straight from the site (with range support), so it keeps working unchanged
   behind a reverse proxy on your own domain.
4. **Expire.** The file stays available for `TEMP_FILE_TTL_MINUTES` minutes.
   A background sweeper (every 30 s) then deletes it and logs
   `web: temp window closed for document N`. Clicking download again **inside**
   the window re-opens the same file without re-downloading it or restarting
   the timer; after the window it is fetched fresh.

**Rules and limits**

- **Files up to 20 MB** (the Bot API download cap) are served as above.
  **Files over 20 MB** open the document's Telegram message instead - bots
  cannot fetch those, so Telegram is the only way to get them.
- Temp URLs are guarded by the same login as the rest of the site (when
  `ADMIN_PASSWORD` is set), so a leaked `/temp/...` link is useless to someone
  without an active session.
- **Crash cleanup.** If the process dies mid-window, the leftover files are
  removed by an orphan sweep on the next run. It only ever touches files with
  the `teledoc-` prefix, so `TEMP_DIR` is safe to point at a shared or mapped
  directory.
- **Archived before downloads existed?** A download needs the Telegram file id,
  which the bot only receives when a file is delivered to it. Such documents
  log `document has no Telegram file id (archived before file ids were
  stored)`. Re-send or re-forward the file in the channel once - the bot links
  the new delivery to the existing archive entry automatically (matched by
  channel, file name and size) and downloads start working; no need to delete
  anything.
- Behind Caddy (or any reverse proxy), no extra configuration is needed: the
  proxy just forwards `/temp/*` like every other path.
- **Docker:** `docker-compose.yml` maps `./temp` to the container's temp
  directory so downloads survive restarts long enough to be swept properly.

## Logging

All output goes through `internal/logs`. Every line has the same shape:

```
web 2026/09/28 08:38:25 web: download triggered for document 28 (...)
^^^ origin  ^^^^^^^^^^^^^^^^^^^ timestamp
```

- **Origins:** `tg` (ingestion), `tg-api` (Telegram client calls), `web` (UI
  and downloads), `rules` (tagging engine) and `app` (startup, shutdown and
  anything logged through the standard `log` package).
- **Colors:** the origin prints in yellow and the timestamp in purple, but only
  when stdout is a real terminal. Redirects, pipes, `docker logs` and
  `NO_COLOR` get plain text, so log files stay clean. On Windows, ANSI support
  is switched on for the console automatically.
- **Non-blocking:** lines are handed to a buffered queue (1024 lines) and one
  goroutine writes them. A slow or stuck console can never stall a request
  handler, the temp-file sweeper or shutdown: overflow is dropped and counted,
  and a `logs: dropped N line(s)` summary prints once output recovers. Each
  individual write has a 2 s timeout, so one stalled write costs one line
  rather than freezing the pipeline.
- **Shutdown:** on Ctrl+C / SIGTERM you should see `shutting down (Ctrl+C)...`,
  `web server stopped` and `bye`. The final `bye` lines use a bounded direct
  write so shutdown can never hang on the console. If Telegram's long poll has
  not unwound after 10 s the process exits anyway with
  `bye (Telegram polling still unwinding; exiting)`; in-flight state is safe to
  abandon because SQLite commits per statement and reactions are cosmetic.

### Troubleshooting: logs stop appearing

Startup lines print but download and shutdown lines never show, although the
app keeps working. This was a real bug, fixed in `internal/logs`: detecting
whether stdout is a terminal wrapped its handle in a second `os.File`
(`os.NewFile`), and when the garbage collector later finalized that duplicate it
closed the real stdout handle, so every later write failed silently. Terminal
detection now only inspects the original `*os.File`. If you ever change
`isTerminal`, never build a new `os.File` from `f.Fd()`. The regression test is
`TestSupportsColorDoesNotCloseTheFile`.

If you keep the `internal/logs` sources in git, make sure `.gitignore` uses
`/logs/` (top level only) rather than `logs/`, which would also swallow
`internal/logs/` and leave fresh clones unable to build.

## Development

```bash
go test ./...      # rule engine, store (incl. hashtag sync/provenance), bot, web/download and logging tests
go vet ./...
go build .
```

Layout: `internal/store` (SQLite), `internal/rules` (Papra-style rule engine), `internal/telegram` (ingestion, hashtag parsing, reactions), `internal/web` (UI, temp-file downloads, embedded templates/static including vendored htmx), `internal/logs` (colored, non-blocking log pipeline).