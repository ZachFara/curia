# 0023. tmux server identity

Status: accepted (Plan 4 setup). Closes Plan 2 final-review M7 and Plan 1 Open Question 13.

## Context

Curia assumes one tmux server. With two (a second `-L` socket, or a view run inside another server), reconcile marks sessions of the first as stale, `deliver` could type into a pane of the second that has the same id, and `restore` could open windows in the wrong server.

## Decision

- **`tmux.Pane` gains `Socket`, from `#{socket_path}` in the format `ListPanes` and `PaneInfo` already use** (verified on tmux 3.8). A value is kept only when it is an absolute path of at most 256 bytes with no rune below `0x20`.
- **A session's server is stored as `sessions.tmux_socket`, written on the session's first event from the `PaneInfo` call the hook already makes.** No process, statement or file read is added to the hook path. Rows from before the upgrade have no socket until their next SessionStart; an empty socket means unknown and behaves as before. `store.upsert` names its columns, so an older binary leaves the column alone.
- **`fleet.Reconcile` never marks stale a session whose socket is known and differs from the socket of the panes it was given.**
- **`deliver.Answer` and `deliver.Message` type nothing for such a session,** and the check comes before the pane is captured. The answer or message stays queued and the note says `the agent is on another tmux server (<base name of its socket>); nothing typed`.
- **`curia restore` offers only sessions whose socket is unknown or is the server it runs against.**

## Consequences

- A session started before the upgrade is protected only after its next SessionStart.
- Plan section 4.9 and decision 21 of section 13 carry the detail.
