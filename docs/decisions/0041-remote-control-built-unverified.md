# 0041. Remote Control is built against fakes and has never run

Status: accepted (2026-11-20, Ruling 213; owner decision D-10 = B; code in `cmd/curia/rc.go`, `internal/deliver/rc.go`, `internal/store/rc.go`, `internal/config/rc.go`).

## Context

Remote Control (`/rc`) was documented but never run: the probe is reserved for the owner. A built-in slash command fires no UserPromptSubmit, so only the pane can confirm one.

## Decision

- Build it anyway, behind a list of five unverified assumptions kept in one comment at the top of `cmd/curia/rc.go` (and each in one named constant): `/rc <name>` is accepted as typed; the footer texts (`rcPatterns`, placeholders from the documentation's wording); the prompt returns within 5 s and the first `/rc` shows no dialog; `claude --remote-control <name> "brief"` takes the name as its own argument; a second `/rc` into a session Curia could not see as on is harmless.
- `curia rc <session|T-id|%pane|--all>` is user-only, shows a warning and confirms, types a fixed line through the guarded typing path, then reads the pane for up to 5 seconds and reports confirmed, not confirmed or unknown. A session Curia recorded as on is never typed into. `--check` is read-only and reports `on` only when the indicator is seen (D-10 = B: type `/rc` only with positive evidence of a disconnect). `rc.launch` (off, user file only) adds `--remote-control <name>` to launches.
- Not built: typing at a busy session's next stop, `remoteControlAtStartup` on install, a dashboard RC column.

## Consequences

- `rc --check` reads the indicator from the last three screen lines whatever the prompt state is; a busy pane could therefore show text that reads as `on` or `off`. Left as is (review B-9): requiring a known prompt would break `--check`, and the patterns are placeholders.

- Until the owner's probe, the patterns are placeholders, so state is mostly `unknown`. A dialog after `/rc` reads "not confirmed" and blocks further typing into that session until `curia rc --check`. Replace the table from real captures, then update this record.
