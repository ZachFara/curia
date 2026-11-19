---
curia_id: D-1
status: answered
scope: repo
mode: blocking
repo: "api-server"
task: "T-1"
date: <date>
tags: ["audit", "storage"]
---
# D-1 Which database should the audit log write to
## Question
```
Which database should the audit log write to?
```
## Context
```
The audit log records every login and token rotation; it must survive a restart.
```
## Options
- A
```
Postgres: Another server to run and back up.
```
- B (recommended) (chosen)
```
SQLite file: One file beside the service; no extra server.
```
- C
```
Append-only files: Simple, but no queries.
```
## Answer
```
B: SQLite file
```
## Reasoning
```
one database to run
```
