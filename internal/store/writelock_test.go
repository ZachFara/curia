package store_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/store"
)

// writerEnv, when set, makes TestWritersInSeveralProcesses run as one of
// its writer processes: "<db path>,<writer number>".
const writerEnv = "CURIA_STORE_TEST_WRITER"

// Writer processes, the time each spends writing, and how long each write
// transaction holds the write lock, standing in for a hook that is
// descheduled, or waits on a slow disk, while it holds it.
const (
	writerProcs = 4
	writerSpan  = 2500 * time.Millisecond
	writerHold  = 5 * time.Millisecond
)

func TestWritersInSeveralProcesses(t *testing.T) {
	// Hooks of several sessions are separate processes writing one file.
	// SQLite's own wait for the write lock is a poll that backs off to
	// 100 ms sleeps, and a writer that commits and writes again (or a new
	// one) takes the lock without waiting, so under steady contention a
	// waiter can miss every poll until busy_timeout and fail with
	// SQLITE_BUSY although no writer ever held the lock for long. Every
	// write of every process must succeed, and none may be lost.
	if v := os.Getenv(writerEnv); v != "" {
		runWriterProcess(t, v)
		return
	}
	path := dbPath(t)
	upsert(t, open(t, path), model.Session{ID: "shared", Agent: "claude", State: model.StateWorking})

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	outs := make([]string, writerProcs)
	errs := make([]error, writerProcs)
	var wg sync.WaitGroup
	for p := range writerProcs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(self, "-test.run=^TestWritersInSeveralProcesses$", "-test.count=1", "-test.v")
			cmd.Env = append(os.Environ(), fmt.Sprintf("%s=%s,%d", writerEnv, path, p))
			out, runErr := cmd.CombinedOutput()
			outs[p], errs[p] = string(out), runErr
		}()
	}
	wg.Wait()

	total, counts := 0, make([]int, writerProcs)
	for p := range writerProcs {
		if errs[p] != nil {
			t.Errorf("writer %d: %v\n%s", p, errs[p], outs[p])
		}
		n, ok := writesDone(outs[p])
		if !ok {
			t.Errorf("writer %d reported no count:\n%s", p, outs[p])
		}
		counts[p] = n
		total += n
	}
	got, found, err := open(t, path).GetSession(context.Background(), "shared")
	if err != nil || !found {
		t.Fatalf("GetSession(shared) = found %v, %v", found, err)
	}
	if got.ContextUsed != total {
		t.Errorf("ContextUsed = %d after %d committed increments: updates were lost", got.ContextUsed, total)
	}
	t.Logf("%d processes committed %d write transactions (%v)", writerProcs, total, counts)
}

// runWriterProcess is one writer of TestWritersInSeveralProcesses: for
// writerSpan it increments the shared session's ContextUsed back to back,
// holding each transaction for writerHold, and logs how many it committed.
func runWriterProcess(t *testing.T, v string) {
	path, _, _ := strings.Cut(v, ",")
	s := open(t, path)
	ctx := context.Background()
	n := 0
	for end := time.Now().Add(writerSpan); time.Now().Before(end); {
		err := s.UpdateSessionTx(ctx, "shared", func(_ *store.Tx, cur model.Session, _ bool) (model.Session, error) {
			time.Sleep(writerHold)
			cur.ContextUsed++
			return cur, nil
		})
		if err != nil {
			t.Logf("writes done: %d", n)
			t.Fatalf("write %d: %v", n, err)
		}
		n++
	}
	t.Logf("writes done: %d", n)
}

// writesDone reads the count runWriterProcess logged.
func writesDone(out string) (int, bool) {
	_, rest, ok := strings.Cut(out, "writes done: ")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(strings.SplitN(rest, "\n", 2)[0]))
	return n, err == nil
}
