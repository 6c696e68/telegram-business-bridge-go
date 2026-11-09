# telegram-business-bridge-go

A small, dependency-free Go service that answers Telegram **Business Chatbot**
messages on behalf of an account owner, using any OpenAI-compatible chat API.
It is built for low-memory routers (aarch64, ~9 MB RSS) but runs anywhere Go runs.

> Use it only on accounts you own, and tell your contacts that they are talking to an
> automated assistant. The bridge already introduces itself on the first reply in each chat.

## Features

- Replies to Business messages through the owner's connection, and to direct messages and group mentions sent to the bot.
- Ignores any business connection that does not belong to the configured owner, or that cannot reply.
- Owner takeover: when the owner types in a chat, the bot stays silent there for a configurable time.
- Durable memory in a single bbolt file (`bridge.db`): chat context, pause state, connections and the
  update offset survive restarts. Expired chats are pruned automatically (`memory_days`) and the file is
  compacted on boot when large, so the store cannot grow without bound.
- No backlog replay after a restart: messages older than `max_backlog_min` are skipped.
- Long-term rules (`rules.md`) synthesised by the LLM from conversations and injected into every prompt.
  A prompt policy plus a regex sanitizer block phone numbers, emails, URLs, foreign handles, prices and
  verbatim quotes, so only generic behavioural rules are persisted; the owner can edit the file by hand.
- Owner commands (owner only, private chat or a business chat): `/pause`, `/resume`, `/status`, `/reload`,
  `/rules [reset|off|on]`, `/forget`.
- Limits: replies per user per hour, daily cap, bounded concurrent LLM calls, LRU chat memory with a
  per-chat character cap.
- Retries on LLM 429/5xx/transport errors and on Telegram flood control (`retry_after`).
- Never logs message text, tokens or API keys. Errors are sanitized (the bot token is in the URL, so transport errors carry only a kind).
- Long-poll loop with backoff, 409 conflict handling, clean shutdown on SIGTERM.

## Layout

| File | Purpose |
|---|---|
| `config.go` | Loads and validates `config.json` |
| `telegram.go` | Minimal Telegram Bot API client and update types |
| `llm.go` | OpenAI-compatible chat client |
| `storage.go` | bbolt store: meta/chats buckets, flush worker, restore, pruning, compaction |
| `rules.go` | rules.md loading, sanitizer, distiller job, atomic writes |
| `bridge.go` | Dispatch, policy, limits, LRU memory, reply pipeline, poll loop |
| `util.go` | Logging, character and UTF-16 truncation, command parsing |
| `main.go` | Entry point (`-config`, `-version`) |
| `*_test.go` | Unit tests, HTTP integration tests, fuzz target, optional load test |

## Configuration

Create `config.json` (keep it `chmod 600`, never commit it):

```json
{
  "bot_token": "<from @BotFather>",
  "owner_id": 123456789,
  "owner_username": "your_username",
  "api_base": "https://your-openai-compatible-host/v1",
  "api_key": "<key>",
  "model": "your-model"
}
```

Other keys and their defaults are in `config.go`. `system_prompt.txt` next to the config
is optional; `{owner_tag}` is replaced with `@owner_username`.

### Storage and retention

| Key | Default | Meaning |
|---|---|---|
| `db_file` | `bridge.db` next to the config | bbolt store (chats, pause, connections, offset) |
| `flush_sec` | 30 | write cadence (dirty rows only; flushes on shutdown too) |
| `memory_days` | 7 | chats older than this are deleted from the store |
| `max_stored_chats` | 1000 (≥ `max_chats`) | hard cap of stored chats; the oldest are pruned |
| `max_backlog_min` | 10 | skip messages older than X minutes (0 = disabled) |
| `history_max_chars` | 8000 | per-chat history cap in code points (bounds RAM) |
| `db_compact_mb` | 20 | compact `bridge.db` on boot when bigger than this (0 = off) |

`state.json` from older versions is imported once into `bridge.db` and then ignored;
it can be deleted. Only one process may open the database at a time.

### Long-term rules (`rules.md`)

| Key | Default | Meaning |
|---|---|---|
| `rules_enabled` | true | master switch for synthesis and injection |
| `rules_file` | `rules.md` next to the config | human-editable rules file |
| `rules_max_chars` | 4000 | hard cap for the file and the injected block |
| `rules_update_every` | 10 | synthesise after every N successful replies |
| `rules_allow_url` | false | allow URLs in rules (blocked by default) |

The LLM rewrites the whole file (`UNCHANGED` means "no change"). Phone numbers, emails,
URLs, foreign Telegram handles, money amounts, IDs and verbatim quotes are rejected by a
line-level sanitizer; if more than half of the output looks sensitive the old file is kept.
`/rules reset|off|on` and `/reload` manage it at runtime.

Telegram setup: in BotFather enable Business Mode for the bot, then in the owner's Telegram
go to Settings → Telegram Business → Chatbots and add the bot with "Reply to messages".

## Build

```sh
go build -trimpath -ldflags="-s -w" -o tg-bridge .
# for a 64-bit ARM router:
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -buildid=" -o tg-bridge .
```

Run: `./tg-bridge -config /path/to/config.json`

## Test

```sh
go test -race -count=1 .                                   # unit + integration
go test -run '^$' -fuzz=FuzzHandleUpdate -fuzztime=60s .  # fuzz
LOAD_N=600 go test -run TestLoad -v .                      # load test (needs LOAD_N)
```

Only one process may poll a bot token. If a second poller runs, Telegram answers
`409 Conflict` and the bridge backs off.

## License

No license has been chosen yet. All rights reserved until one is added.
