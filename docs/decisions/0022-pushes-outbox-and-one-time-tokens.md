# 0022. Pushes, the outbox and one-time tokens

Status: accepted (Plan 4 setup). Probes N1 and N2 can amend the request shape; they have not run yet.

## Context

Spec F11 wants a blocking ticket to reach the owner's phone and be answerable from it, and a quiet-hours setting. Curia has no daemon, and network use is opt-in (spec section 7). A long-lived per-device token would have to sit in clear on the machine and in every notification.

## Decision

- **Pushes go through an outbox table, `pushes`, and are sent by whichever Curia process is alive: each `curia mcp` whose configuration has ntfy on, and `curia serve`.** There is still no daemon. A flush runs at once when a tool call enqueued something and every 60 seconds, which is what sends a held push when quiet hours end and retries a failed one. A push held while no agent session and no `curia serve` runs waits for the next one to start.
- **What is pushed:** every new blocking ticket and every new review (not an update of a pending one), one push per configured device; assume tickets never. Nothing is sent unless `[ntfy] url` is set and `[ntfy.devices]` has an entry; with neither, no connection is opened. `pushes_once` makes one row per `(kind, ref, device)`.
- **Claim, check, send:** `store.ClaimPush` is one write transaction that takes the oldest due row (or a `sending` one older than 2 minutes) and gives it a new `token_hash`. The subject is read again and the push is `dropped` when the ticket is no longer open, the review no longer pending or the device no longer configured. Sending is plain `net/http`, limit 5 seconds, a redirect is not followed (it would carry the token). A 4xx other than 429 fails for good; any other error retries with backoff (30 s doubling, at most 15 minutes) up to 5 attempts.
- **Quiet hours** (`[ntfy] quiet = "22:00-07:00"`, local time) hold back only ntfy pushes; the bell and the macOS notification are unchanged.
- **A phone button carries a one-time token bound to one ticket and one device's push.** The token is 32 random bytes, base64url without padding; the store keeps only its SHA-256. The spec's "per-device token" becomes per-device topics (a device is revoked by removing its topic). At most three buttons, the recommended option first; an irreversible ticket gets none and the endpoint refuses it. A review is pushed without buttons: nothing is approved from the phone.
- **`curia serve` has one answer route, `POST /v1/answer`, and binds only to loopback or a Tailscale address,** with no TLS of its own (the tunnel or `tailscale serve` carries it). It answers through `deliver.Answer` with `Via: model.AnsweredNtfy`, so a phone answer travels the same typing path as `curia decide`. Every token failure is the same `401` text, a token is spent once and only for a `sent` push at most 24 hours old, and ten failures in a minute give `429`.
- **The access token comes only from `CURIA_NTFY_TOKEN`; topics live in the config file.** No token is ever logged or printed.

## Consequences

- Anyone who can read a topic within 24 hours and reach the endpoint can answer that one ticket once (owner question 2). A runaway agent can send many pushes (each distinct ticket once per device); an option is one config key later.
- If probe N1 shows ntfy does not allow headers or a body in an action, the token moves into the action URL's path and the log rule gains "never the path"; if probe N2 shows the app overrides `Content-Type`, the endpoint accepts the recorded type too.
- Plan sections 4.7 and 4.8 and decisions 9 to 16 of section 13 carry the detail and the reasons.
