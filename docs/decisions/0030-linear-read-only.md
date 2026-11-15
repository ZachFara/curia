# 0030. Linear is read-only

Status: accepted (Plan 5 setup). The plan numbers this record 0028. Probe L1 (the owner's) has not run; until it does the tests use a built response.

## Context

Spec F13 and the non-goals: Linear is an import source, adapters only map fields (F5), and Curia writes nothing back. It is the only connection Plan 5 opens, from a command the owner runs.

## Decision

- `curia task import --linear [--repo <dir>] [--team <KEY>]` sends one fixed GraphQL query (`linear.Query`) to `linear.url` (default `https://api.linear.app/graphql`) with at most 4 pages of 50, 20 seconds per request, 4 MiB per response and no redirect followed. The client has one exported method; nothing sends a mutation.
- **The key** comes only from `CURIA_LINEAR_API_KEY` (Keychain item `env:CURIA_LINEAR_API_KEY`), is sent only in the `Authorization` header and is never logged, printed or stored. **The URL** comes only from the config file, never from a flag, so `gosec` G704 does not apply.
- **The filter is fixed:** issues assigned to me in backlog or unstarted states, optionally of one team. Priority 1 and 2 become P1, 0 and 3 become P2, 4 becomes P3. A re-import updates title, body and tags only; priority, status and everything spawn wrote stay (Plan 3's `ImportTask` rule). A task's status is never mapped.
- Everything from Linear is bounded and cleaned on entry; an identifier that does not match `^[A-Z][A-Z0-9]{0,9}-[0-9]{1,7}$` skips the issue.

## Consequences

- Importing a teammate's issue needs a code change; a priority changed in Linear after the first import is not picked up.
- A second workspace needs a second environment.
- The query and filter names follow Linear's published API and are unverified until probe L1.
