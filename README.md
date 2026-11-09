# telegram-business-bridge-go

A small, dependency-free Go service that answers Telegram **Business Chatbot**
messages on behalf of an account owner, using any OpenAI-compatible chat API.
It is built for low-memory routers (aarch64; measured heap +5 MB worst case, see
"Resource usage" below) but runs anywhere Go runs.

> Use it only on accounts you own, and tell your contacts that they are talking to an
> automated assistant. The bridge already introduces itself on the first reply in each chat.

## Features

- Replies to Business messages through the owner's connection, and to direct messages and group mentions sent to the bot.
- Ignores any business connection that does not belong to the configured owner, or that cannot reply.
- Owner presence: the bot answers business chats **only after the owner has been inactive for
  `owner_away_minutes` (default 30)**. Sending or editing a message in *any* business chat counts as
  activity and silences every business chat; `/here` marks you online, `/away` hands over at once.
  Customers who wrote while you were online are remembered and answered by a follow-up sweep
  (every 10 s) once you have been away long enough, unless you answered them yourself or the
  message is older than `followup_max_min` (default 120). Presence and waiting chats survive restarts.
  The Bot API exposes no online/read status, so merely opening a chat cannot be detected — type a
  message or send `/here`.
- Owner takeover: when the owner types in a chat, that chat also stays silent for `owner_mute_minutes`.
- Durable memory in a single bbolt file (`bridge.db`): chat context, pause state, connections and the
  update offset survive restarts. Expired chats are pruned automatically (`memory_days`) and the file is
  compacted on boot when large, so the store cannot grow without bound.
- No backlog replay after a restart: messages older than `max_backlog_min` are skipped.
- Long-term rules (`rules.md`) synthesised by the LLM from conversations and injected into every prompt.
  A prompt policy plus a regex sanitizer block phone numbers, emails, URLs, foreign handles, prices and
  verbatim quotes, so only generic behavioural rules are persisted.
- `rules.pinned.md`: a second, owner-only rules file. The program reads and injects it verbatim and
  **never writes it**, so your hand-written rules can never be overwritten by the LLM.
- Owner commands (owner only, private chat or a business chat): `/here`, `/away`, `/pause`, `/resume`,
  `/status`, `/stats`, `/reload`, `/rules [reset|off|on]`, `/forget`.
- Bursts are coalesced: consecutive messages within `debounce_ms` produce one reply that answers the
  whole burst, saving tokens and avoiding duplicate answers.
- Limits: replies per user per hour, daily cap, bounded concurrent LLM calls, LRU chat memory with a
  per-chat character cap.
- Retries on LLM 429/5xx/transport errors and on Telegram flood control (`retry_after`).
- Fallback model chain: when the primary model keeps answering 429/5xx, the next configured
  model is tried in the same request (`fallback_models`); once a fallback answers, later calls
  start from it so an incident does not multiply provider load.
- Graceful shutdown: in-flight replies get up to `shutdown_drain_sec` to finish before exit
  (a long LLM call in its thinking phase can still hit the budget; replies in their final
  generating/sending phase are protected).
- Observability without a monitoring stack: a periodic non-sensitive stats log line
  (`stats_log_min`, default 60 min: real RSS on Linux, heap, goroutines, DB size, chats,
  replies today) and an optional loopback JSON endpoint (`health_addr`) serving `/healthz`.
- Never logs message text, tokens or API keys. Errors are sanitized (the bot token is in the URL, so transport errors carry only a kind).
- Long-poll loop with backoff, 409 conflict handling, clean shutdown on SIGTERM.

## Layout

| File | Purpose |
|---|---|
| `config.go` | Loads and validates `config.json` |
| `telegram.go` | Minimal Telegram Bot API client and update types |
| `llm.go` | OpenAI-compatible chat client |
| `storage.go` | bbolt store: meta/chats buckets, flush worker, restore, pruning, compaction |
| `runtime_stats.go` | Non-sensitive metrics snapshot + periodic stats log |
| `health.go` | Optional loopback `/healthz` endpoint |
| `deploy/` | systemd unit, logrotate config, deployment notes |
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
| `rules_pinned_file` | `rules.pinned.md` next to the config | owner-only rules; never written by the program |
| `fallback_models` | `[]` | model names tried after the primary on 429/5xx |
| `shutdown_drain_sec` | 15 | seconds to wait for in-flight replies on SIGTERM (0 = off) |
| `stats_log_min` | 60 | minutes between non-sensitive stats log lines (0 = off) |
| `debounce_ms` | 700 | wait this long for a burst before replying (0 = reply per message) |
| `owner_away_minutes` | 30 | owner inactivity required before the bot answers business chats (0 = always answer) |
| `followup_max_min` | 120 | max age of a waiting message the follow-up sweep will still answer (0 = no follow-ups) |
| `max_tokens` | 1200 | completion token cap per reply |
| `temperature` | 0 | sampling temperature; 0 keeps the provider default |
| `health_addr` | `""` (off) | loopback address for `GET /healthz`, e.g. `127.0.0.1:8099` |

The LLM rewrites the whole file (`UNCHANGED` means "no change"). Phone numbers (including
spaced ones), emails, URLs, foreign Telegram handles, money amounts, IDs, secrets and
verbatim quotes are rejected by a line-level sanitizer (headings included); if more than
half of the output looks sensitive the old file is kept, and a distill that was in flight
when you run `/rules reset|off` or `/forget` cannot write afterwards. This is a
best-effort guard (strict prompt + regex + quote detector), not a hard guarantee — run
`/rules` to review what was stored. `/rules reset|off|on` and `/reload` manage it at runtime.

Telegram setup: in BotFather enable Business Mode for the bot, then in the owner's Telegram
go to Settings → Telegram Business → Chatbots and add the bot with "Reply to messages".

## Deploying on a router

`deploy/` ships a systemd unit, a logrotate config, an OpenWrt procd init script
and a step-by-step README (install, health check, what to watch during the first days).

### OpenWrt (procd)

```sh
scp tg-bridge root@router:/root/tg-business-bridge/bin/tg-bridge.new
ssh root@router 'chmod +x /root/tg-business-bridge/bin/tg-bridge.new && \
  mv /root/tg-business-bridge/bin/tg-bridge.new /root/tg-business-bridge/bin/tg-bridge'
scp deploy/openwrt/tg-business-bridge.init root@router:/etc/init.d/tg-business-bridge
ssh root@router 'chmod +x /etc/init.d/tg-business-bridge && \
  /etc/init.d/tg-business-bridge enable && /etc/init.d/tg-business-bridge restart'
```

Logs: `logread | grep tg-bridge`. Back up `bin/tg-bridge`, `config.json` and
`state.json`/`bridge.db` before replacing the binary.

## Build

Requires Go 1.23 or newer (bbolt v1.4).

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

## Resource usage

Measured worst case on the reference workload (500 chats, 20 messages each, 8000 code
points per chat, ASCII): **+5.0 MB live heap, +4.7 MB after restart, 8.0 MB `bridge.db`**;
a full 500-chat snapshot flush takes ~23 ms (normal flushes write only dirty chats).
`go test -run TestWorstCaseRamAndDisk -v .` reproduces the numbers; the bounds are pinned
in that test so a regression fails the suite.

## License

No license has been chosen yet. All rights reserved until one is added.
