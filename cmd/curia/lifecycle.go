package main

import (
	"time"

	"github.com/ZachFara/curia/internal/lifecycle"
)

// The session lifecycle's seams are the commands' own: lifecycle.Clock
// follows clock, which tests replace.
func init() {
	lifecycle.Clock = func() time.Time { return clock() }
	lifecycle.Reconcile = reconcile
	lifecycle.LogWarn = logWarn
	lifecycle.SubprocessTimeout = subprocessTimeout
	lifecycle.SpawnBin = executablePath
}
