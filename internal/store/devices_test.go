package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ZachFara/curia/internal/store"
)

func devHash(i int) string { return fmt.Sprintf("%064x", i+1) }

// Review C2: the cap of MaxDevices and the duplicate-name refusal are the
// store's own errors, not a raw constraint failure.
func TestAddDeviceCapAndDuplicateName(t *testing.T) {
	s := open(t, dbPath(t))
	ctx := context.Background()
	for i := range store.MaxDevices {
		if err := s.AddDevice(ctx, fmt.Sprintf("d%d", i), devHash(i), t0); err != nil {
			t.Fatalf("device %d: %v", i, err)
		}
	}
	if err := s.AddDevice(ctx, "one-too-many", devHash(99), t0); !errors.Is(err, store.ErrTooManyDevices) {
		t.Errorf("device %d: err = %v, want ErrTooManyDevices", store.MaxDevices+1, err)
	}
	if _, err := s.DeleteDevice(ctx, "d0"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddDevice(ctx, "d1", devHash(98), t0); !errors.Is(err, store.ErrDeviceExists) {
		t.Errorf("same name: err = %v, want ErrDeviceExists", err)
	}
	if err := s.AddDevice(ctx, "fresh", devHash(97), t0); err != nil {
		t.Errorf("after a revoke there is room: %v", err)
	}
	if err := s.AddDevice(ctx, "bad", strings.Repeat("g", 64), t0); err == nil {
		t.Error("a malformed hash was stored")
	}
}
