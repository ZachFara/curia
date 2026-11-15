package usage

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// checker is a small assertion helper (the module has no assertion library):
// assert reports and goes on, require stops the test.
type checker struct{ stop bool }

var (
	assert  = checker{}
	require = checker{stop: true}
)

func (c checker) fail(t testing.TB, msg []any, format string, a ...any) bool {
	t.Helper()
	text := fmt.Sprintf(format, a...)
	if len(msg) > 0 {
		text += ": " + fmt.Sprintf(fmt.Sprint(msg[0]), msg[1:]...)
	}
	if c.stop {
		t.Fatal(text)
	} else {
		t.Error(text)
	}
	return false
}

func (c checker) Equal(t testing.TB, want, got any, msg ...any) bool {
	t.Helper()
	if eq(want, got) {
		return true
	}
	return c.fail(t, msg, "want %#v, got %#v", want, got)
}

func (c checker) EqualValues(t testing.TB, want, got any, msg ...any) bool {
	t.Helper()
	return c.Equal(t, fmt.Sprint(want), fmt.Sprint(got), msg...)
}

func eq(a, b any) bool {
	return reflect.DeepEqual(a, b)
}

func (c checker) NoError(t testing.TB, err error, msg ...any) bool {
	t.Helper()
	if err == nil {
		return true
	}
	return c.fail(t, msg, "unexpected error: %v", err)
}

func (c checker) ErrorIs(t testing.TB, err, target error, msg ...any) bool {
	t.Helper()
	if errors.Is(err, target) {
		return true
	}
	return c.fail(t, msg, "error %v is not %v", err, target)
}

func (c checker) True(t testing.TB, v bool, msg ...any) bool {
	t.Helper()
	if v {
		return true
	}
	return c.fail(t, msg, "want true")
}

func (c checker) False(t testing.TB, v bool, msg ...any) bool {
	t.Helper()
	if !v {
		return true
	}
	return c.fail(t, msg, "want false")
}

func length(v any) int { return reflect.ValueOf(v).Len() }

func (c checker) Empty(t testing.TB, v any, msg ...any) bool {
	t.Helper()
	if v == nil || length(v) == 0 {
		return true
	}
	return c.fail(t, msg, "want empty, got %#v", v)
}

func (c checker) NotEmpty(t testing.TB, v any, msg ...any) bool {
	t.Helper()
	if v != nil && length(v) > 0 {
		return true
	}
	return c.fail(t, msg, "want not empty")
}

func (c checker) Len(t testing.TB, v any, n int, msg ...any) bool {
	t.Helper()
	if length(v) == n {
		return true
	}
	return c.fail(t, msg, "want length %d, got %d", n, length(v))
}

func (c checker) NotNil(t testing.TB, v any, msg ...any) bool {
	t.Helper()
	if rv := reflect.ValueOf(v); v != nil && (rv.Kind() != reflect.Map || !rv.IsNil()) {
		return true
	}
	return c.fail(t, msg, "want not nil")
}

func (c checker) Contains(t testing.TB, s, sub string, msg ...any) bool {
	t.Helper()
	if strings.Contains(s, sub) {
		return true
	}
	return c.fail(t, msg, "%q does not contain %q", s, sub)
}

func (c checker) NotContains(t testing.TB, s, sub string, msg ...any) bool {
	t.Helper()
	if !strings.Contains(s, sub) {
		return true
	}
	return c.fail(t, msg, "%q contains %q", s, sub)
}

func (c checker) InDelta(t testing.TB, want, got, delta float64, msg ...any) bool {
	t.Helper()
	if math.Abs(want-got) <= delta {
		return true
	}
	return c.fail(t, msg, "want %v, got %v", want, got)
}

func (c checker) Zero(t testing.TB, v float64, msg ...any) bool {
	t.Helper()
	if v == 0 {
		return true
	}
	return c.fail(t, msg, "want zero, got %v", v)
}

func (c checker) LessOrEqual(t testing.TB, got, limit int, msg ...any) bool {
	t.Helper()
	if got <= limit {
		return true
	}
	return c.fail(t, msg, "%d > %d", got, limit)
}

func (c checker) ElementsMatch(t testing.TB, want, got []string, msg ...any) bool {
	t.Helper()
	w, g := append([]string(nil), want...), append([]string(nil), got...)
	sort.Strings(w)
	sort.Strings(g)
	return c.Equal(t, w, g, msg...)
}
