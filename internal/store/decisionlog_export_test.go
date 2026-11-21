package store

// DecisionLogStatements are the log's two statements, for their query plans.
func DecisionLogStatements() (rows, summary string) { return decisionLogRows, decisionLogSummary }
