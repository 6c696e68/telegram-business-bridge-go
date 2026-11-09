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
- Owner commands (owner only, private chat): `/pause`, `/resume`, `/status`, `/reload`.
- Limits: replies per user per hour, daily cap, bounded concurrent LLM calls, LRU chat memory.
- Retries on LLM 429/5xx/transport errors and on Telegram flood control (`retry_after`).
- Never logs message text, tokens or API keys. Errors are sanitized (the bot token is in the URL, so transport errors carry only a kind).
- Long-poll loop with backoff, 409 conflict handling, clean shutdown on SIGTERM.

## Layout

| File | Purpose |
|---|---|
| `config.go` | Loads and validates `config.json` |
| `telegram.go` | Minimal Telegram Bot API client and update types |
| `llm.go` | OpenAI-compatible chat client |
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
