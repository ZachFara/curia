package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ZachFara/curia/internal/model"
)

// A filesystem without flock must not stop the store from opening or
// writing: it falls back to SQLite's own locking and says so once.
func TestStoreWorksWhereFlockIsUnsupported(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	oldFlock, oldLog := flockSys, logf
	flockSys = func(int, int) error { return syscall.ENOTSUP }
	logf = func(format string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, a...))
	}
	t.Cleanup(func() { flockSys, logf = oldFlock, oldLog })

	s, err := Open(filepath.Join(t.TempDir(), "curia.db"))
	if err != nil {
		t.Fatalf("Open where flock is unsupported: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for i := range 3 {
		sess := model.Session{ID: fmt.Sprintf("s%d", i), Agent: "claude", State: model.StateWorking, UpdatedAt: time.Now()}
		if err := s.UpsertSession(context.Background(), sess); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 1 || !strings.Contains(lines[0], "fair queuing is off") {
		t.Errorf("logged %q, want exactly one line saying fair queuing is off", lines)
	}
}
