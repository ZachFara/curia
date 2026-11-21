-- The badge's forecast read (store.UsageReadings, on every tmux status
-- refresh) answered from one index: every column it reads is here (n is the
-- rowid), so SQLite never visits a usage_samples row for it.
-- The CHECK on used_pct means the WHERE leaves out no row; it only makes the
-- index usable by a statement that states the same term, which only
-- UsageReadings does, so every other usage statement keeps
-- usage_samples_window and its plan.
CREATE INDEX usage_samples_forecast ON usage_samples(window, at, used_pct, resets_at) WHERE used_pct >= 0;
