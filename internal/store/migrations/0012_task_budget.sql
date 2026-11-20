-- Plan 6, F16 amendment: a task's own budget in dollars; NULL = none (the
-- default cost.task_budget_default then applies). Additive.
ALTER TABLE tasks ADD COLUMN budget_usd REAL CHECK (budget_usd IS NULL OR budget_usd > 0);
