# 0037. The Docker wrapper and its caps

Status: accepted (2026-11-20, Rulings 206 and 207; owner decision D-16 = A and the owner's later "caps can be switched off"; code in `cmd/curia/docker.go`, `internal/usage/docker.go`, `internal/store/containers.go`).

## Context

F19 keeps five agents from filling a laptop. Probes showed: `docker info` reports only the VM's total memory; Compose containers never carry `curia.session`; a PATH wrapper sees only the `compose` call, not the containers Compose then creates.

## Decision

- `curia launch` puts a directory `docker-wrapper` (beside the database) first on PATH, holding a symlink named `docker` to the curia binary; `main` dispatches on the name. Only sessions Curia launches get it; nothing is written to shell config.
- The real docker is the first executable `docker` on PATH that is not this binary (file identity) and not in a wrapper directory, found at call time.
- `docker run` and `create` are labelled `curia.session` and `curia.task` (only values that look like Curia ids) and get `--memory` and `--cpus` when configured (a user's own smaller value is kept). Everything else passes through.
- Before `run`, `create` and `compose up` the wrapper counts live containers with `docker ps`: refuse (stderr, exit 1, logged) at `max_containers_per_session` (3) or `max_containers_total` (8), or when Docker's memory is over `docker_memory_refuse_pct` (80) of the VM total, taken from the monitor's last fresh reading. `compose up` is refused at a cap unless its project already runs for the same session (project name by `-p`, `COMPOSE_PROJECT_NAME` or directory name; `--scale`, extra files and profiles are not inspected).
- Each of the three counts is switched off by 0. With all three 0 the wrapper makes no `docker ps` count (it still labels and limits `run` and `create`). A repository file may only lower a nonzero value, never switch a cap off or impose one where the user has none (`PolicyLower`).
- It fails open: no config, no store, docker not answering, no fresh memory reading, all leave the command as it was.

## Consequences

- An absolute path to `docker`, the Engine API and Testcontainers bypass it; caps stop accidents, not intent.
- Memory used is the sum of `docker stats`, so it undercounts. The wrapper's own latency (spec: under 50 ms) was never measured and it has never run against real docker.
