# Deploying on a router / small Linux box

These files are a starting point, not a policy. Adjust paths and users to your
distribution.

## 1. Install

```sh
sudo useradd --system --home /opt/tg-bridge --shell /usr/sbin/nologin tgbridge
sudo mkdir -p /opt/tg-bridge
# copy the arm64 binary and your config (never commit config.json)
sudo install -m 0755 tg-bridge /opt/tg-bridge/tg-bridge
sudo install -m 0600 config.json /opt/tg-bridge/config.json
sudo install -m 0600 -o tgbridge -g tgbridge /dev/null /opt/tg-bridge/bridge.log
sudo chown -R tgbridge:tgbridge /opt/tg-bridge
```

Optional but recommended in `config.json`:

```json
{
  "stats_log_min": 60,
  "health_addr": "127.0.0.1:8099",
  "fallback_models": ["<a cheaper/second model>"],
  "shutdown_drain_sec": 15
}
```

`health_addr` must stay on loopback unless a reverse proxy protects it.

## 2. systemd

```sh
sudo cp deploy/tg-bridge.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now tg-bridge
systemctl status tg-bridge
journalctl -u tg-bridge -f          # or tail /opt/tg-bridge/bridge.log
```

The unit restarts on failure and gives in-flight replies up to
`TimeoutStopSec`/`shutdown_drain_sec` to finish on stop.

## 3. Log rotation

```sh
sudo cp deploy/logrotate.conf /etc/logrotate.d/tg-bridge
```

## 4. Health check

```sh
curl -fsS http://127.0.0.1:8099/healthz
# {"status":"ok","paused":false,"replied_today":12,"chats_ram":3,"chats_db":3,...}
```

A simple uptime probe / cron can alert when the curl fails twice in a row.
The periodic `stats rss_mb=... heap_mb=... db_mb=...` log line lets you watch
real RSS and DB growth during the first days without extra tooling.

## 5. Watch during the first days

- `stats rss_mb` should stay flat; a slow climb means a leak.
- `db_mb` should stay near a few MB; it is pruned by `memory_days` and
  `max_stored_chats`, and compacted on boot above `db_compact_mb`.
- `cat rules.md` occasionally: it must contain only generic advice, never
  phone numbers, prices, names or message quotes. `rules.pinned.md` is yours;
  the program never edits it.
