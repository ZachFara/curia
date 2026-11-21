# 0036. The web page: limits and how it proves who is asking

Status: accepted (2026-11-20, Rulings 208 and 209; owner decision D-9 = A; code in `cmd/curia/serve_web.go`, `serve_web_write.go`, `serve_token.go`, `webtmpl/`, `internal/store/devices.go`).

## Context

F26 puts the board, timeline, decision log, usage and settings on `curia serve`'s listener for a phone over Tailscale. D-9 limited it on purpose: read, three task moves, and confirmed messages; settings view-only.

## Decision

- Off by default (`serve.web`). Writes are a second switch, also off (`serve.web_write`); neither can be changed from a repository file.
- Authentication: a device token from `curia serve token <device>` (user-only, shown once, only its SHA-256 stored in `web_devices`), exchanged at `/login` for an `HttpOnly`, `SameSite=Strict` cookie. Sessions live in memory only (a restart means logging in again), at most 64, for `serve.web_session` (12h). A session dies when its device's token is revoked (revoke deletes the row). The Host header must equal the host of `serve.public_url`; an Origin, when sent, must equal its origin.
- Limits: 5 wrong tokens a minute per address (128 addresses remembered), 20 state-changing requests a minute per device, 8 KiB write bodies, 100 cards per column, 200 timeline rows.
- Every response carries a strict CSP with no script, `nosniff`, `no-referrer`, `DENY` framing and `no-store`. The pages refresh by `<meta refresh>`. No transcript, diff, ticket context or tool input is shown; a timeline turn's last words are replaced by "turn ended".
- Writes: `POST /task/move` (backlog, ready, dropped, from backlog or ready only), `POST /say/send` (held 10 minutes) and `POST /say/confirm`. A POST needs the exact Origin, a live session, the rate limit and the session's CSRF token. A web move never makes a task eligible for auto-spawn.
- Not on the page, by decision: answers, reviews, spawn, settings changes, modes, schedules, broadcasts. Routes are flat (`/timeline`, not `/v1/...`). The code is in `package main` because `notify` cannot import `store` and a new package needs shared-file registration.

## Consequences

- With writes on, a stolen cookie can move tasks and steer a running agent (message plus confirm; the CSRF token is readable with the cookie). That is why writes are separate and off.
- Behind `tailscale serve` the Host rewrite and the rate limit's view of the address (127.0.0.1, so one global bucket) are unverified. The page has never been opened from a phone.
- Reversing D-9 (a fuller page) means revisiting this record first.
