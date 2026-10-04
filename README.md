# calshare

A tiny gateway that turns a private, password-protected CalDAV calendar into
a read-only ICS feed behind one secret, sharable URL, optionally hiding
specific events by regex and/or scrubbing event details before anyone sees
them.

Intended use: you've got a personal CalDAV calendar and want to share a
filtered view of it (e.g. busy/free, or hiding anything tagged "private")
with someone else, or subscribe to it from an app that only speaks plain
ICS, without handing out your real CalDAV credentials.

## Usage

Run the container:

```
docker run -p 8080:8080 \
  -e SECRET_PATH=... \
  -e BACKEND_URL=... \
  -e BACKEND_USER=... \
  -e BACKEND_PASS=... \
  ghcr.io/nadiamoe/calshare:main
```

### Env vars

| Var | Required | Description |
|---|---|---|
| `SECRET_PATH` | yes | The exact path that serves the filtered calendar, e.g. `abc123` for `/abc123`. Treat it like a password. |
| `BACKEND_URL` | yes | URL of the backend ICS resource to fetch and filter. |
| `BACKEND_USER` | yes | Username for HTTP Basic auth against `BACKEND_URL`. |
| `BACKEND_PASS` | yes | Password for HTTP Basic auth against `BACKEND_URL`. |
| `ALLOWLIST` | no | Regex tested against each event's `SUMMARY`. Only matching events are kept. Default: allow all. |
| `DENYLIST` | no | Regex tested against each event's `SUMMARY`, applied after `ALLOWLIST`. Matching events are dropped. Default: deny none. |
| `ANONYMIZE` | no | If set, replaces `SUMMARY`, `DESCRIPTION`, `LOCATION`, `ATTENDEE`, and `ORGANIZER` on every event that survives `ALLOWLIST`/`DENYLIST` with this string, dropping any parameters (e.g. `ATTENDEE;CN=...`) along with them. Timing (`DTSTART`, `DTEND`, etc.) and `UID` are left alone. Default: don't anonymize. |
| `LISTEN_ADDR` | no | Address to listen on. Default `:8080`. |

### Endpoints

- `GET /<SECRET_PATH>` — the filtered calendar, as ICS.
- `GET /health` — always `200`.
- `GET /ready` — `200` if the backend is reachable, `503` otherwise.
- Anything else, including `/` — `404`.

The gateway caches the backend's last response in memory (gzip-compressed)
and makes conditional `If-Modified-Since` requests, so an unchanged backend
calendar doesn't get re-fetched or re-filtered on every request. This is
invisible to clients.
