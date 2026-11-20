package usage

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/ZachFara/curia/internal/model"
)

// testNow sits inside every captured payload's windows.
var testNow = time.Date(2026, 11, 15, 19, 0, 0, 0, time.UTC)

func fixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := fs.ReadFile(os.DirFS("testdata"), name)
	require.NoError(t, err)
	return b
}

func TestParseDocSpecExample(t *testing.T) {
	now := time.Date(2026, 11, 11, 13, 41, 0, 0, time.UTC)
	d, err := ParseDoc(fixture(t, "doc-spec.json"), now)
	require.NoError(t, err)
	assert.Equal(t, "7c1e5a52-0f1b-4c1e-9a53-5d3f0c1a2b44", d.SessionID)
	assert.Equal(t, time.Date(2026, 11, 11, 13, 40, 0, 0, time.UTC), d.At)
	assert.True(t, d.HasCost)
	assert.InDelta(t, 4.12, d.CostUSD, 1e-9)
	assert.True(t, d.HasCacheExpiry)
	assert.Equal(t, time.Date(2026, 11, 11, 14, 5, 0, 0, time.UTC), d.CacheExpiresAt)
	assert.True(t, d.HasCacheRebuild)
	assert.InDelta(t, 1.68, d.CacheRebuildUSD, 1e-9)
	assert.Equal(t, Limit{62, time.Date(2026, 11, 11, 16, 0, 0, 0, time.UTC)}, d.Limits[model.WindowFiveHour])
	assert.Equal(t, Limit{31, time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)}, d.Limits[model.WindowSevenDay])
	assert.Empty(t, d.Dropped)
	assert.Empty(t, d.ModelName)
	assert.False(t, d.HasContext)
}

func TestParseDocMinimal(t *testing.T) {
	d, err := ParseDoc([]byte(`{"schema_version":1,"session_id":"s1"}`), testNow)
	require.NoError(t, err)
	assert.Equal(t, "s1", d.SessionID)
	assert.False(t, d.HasCost || d.HasCacheExpiry || d.HasCacheRebuild || d.HasContext || d.HasCacheRecache)
	require.NotNil(t, d.Limits)
	assert.Empty(t, d.Limits)
	assert.Equal(t, testNow, d.At)
	assert.Empty(t, d.Dropped)
}

func TestParseDocRefusals(t *testing.T) {
	big := `{"schema_version":1,"session_id":"s1","x":"` + strings.Repeat("a", MaxDoc) + `"}`
	exact := `{"schema_version":1,"session_id":"s1","x":"`
	exact += strings.Repeat("a", MaxDoc-len(exact)-2) + `"}`
	require.Len(t, exact, MaxDoc)
	_, exErr := ParseDoc([]byte(exact), testNow)
	require.NoError(t, exErr, "exactly MaxDoc is accepted")

	secret := "S3CRET-VALUE"
	cases := map[string]string{
		"empty": "", "array": "[]", "null": "null", "broken": "{", "string": `"x"`,
		"big":       big,
		"version 2": `{"schema_version":2,"session_id":"s1"}`,
		"version s": `{"schema_version":"1","session_id":"s1"}`,
		"version 0": `{"session_id":"s1"}`,
		"no id":     `{"schema_version":1}`,
		"empty id":  `{"schema_version":1,"session_id":""}`,
		"space id":  `{"schema_version":1,"session_id":"a b"}`,
		"sql id":    `{"schema_version":1,"session_id":"'; DROP TABLE sessions;--"}`,
		"long id":   `{"schema_version":1,"session_id":"` + strings.Repeat("a", 65) + `"}`,
		"num id":    `{"schema_version":1,"session_id":5}`,
		"secret":    `{"schema_version":1,"session_id":"` + secret + ` x"}`,
		"junk":      `{"schema_version":1,"session_id":"s1"} ` + secret,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			_, perr := ParseDoc([]byte(in), testNow)
			require.ErrorIs(t, perr, ErrDoc)
			assert.NotContains(t, perr.Error(), secret)
			assert.NotContains(t, perr.Error(), "DROP")
		})
	}
	_, exErr = ParseDoc([]byte(`{"schema_version":1,"session_id":"`+strings.Repeat("a", 64)+`"}`), testNow)
	assert.NoError(t, exErr, "64 characters is accepted")
}

func TestParseDocDropsBadFieldsOnly(t *testing.T) {
	now := testNow
	rfc := func(d time.Duration) string { return `"` + now.Add(d).Format(time.RFC3339) + `"` }
	good := `"session_id":"s1","schema_version":1,"cost_usd":2.5,"cache_rebuild_usd":0.75`
	cases := []struct {
		name, extra, dropped string
	}{
		{"cost -1", `"cost_usd":-1`, "cost_usd"},
		{"cost huge", `"cost_usd":1e308`, "cost_usd"},
		{"cost overflow", `"cost_usd":1e999`, "cost_usd"},
		{"cost string", `"cost_usd":"4.12"`, "cost_usd"},
		{"cost bool", `"cost_usd":true`, "cost_usd"},
		{"cost object", `"cost_usd":{}`, "cost_usd"},
		{"cost NaN", `"cost_usd":NaN`, ""},
		{"expiry text", `"cache_expires_at":"soon"`, "cache_expires_at"},
		{"expiry number", `"cache_expires_at":1794771246`, "cache_expires_at"},
		{"expiry future", `"cache_expires_at":` + rfc(3*time.Hour), "cache_expires_at"},
		{"expiry past", `"cache_expires_at":` + rfc(-8*24*time.Hour), "cache_expires_at"},
		{"rebuild", `"cache_rebuild_usd":1001`, "cache_rebuild_usd"},
		{"pct 101", `"rate_limits":{"five_hour":{"used_percentage":101}}`, "rate_limits.five_hour"},
		{"pct -1", `"rate_limits":{"five_hour":{"used_percentage":-1}}`, "rate_limits.five_hour"},
		{"pct string", `"rate_limits":{"seven_day":{"used_percentage":"62"}}`, "rate_limits.seven_day"},
		{"pct missing", `"rate_limits":{"seven_day":{"resets_at":1}}`, "rate_limits.seven_day"},
		{"window array", `"rate_limits":{"seven_day":[]}`, "rate_limits.seven_day"},
		{"limits string", `"rate_limits":"x"`, "rate_limits"},
		{"resets far", `"rate_limits":{"five_hour":{"used_percentage":5,"resets_at":` + rfc(9*24*time.Hour) + `}}`, "rate_limits.five_hour.resets_at"},
		{"resets bool", `"rate_limits":{"five_hour":{"used_percentage":5,"resets_at":true}}`, "rate_limits.five_hour.resets_at"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := `{` + good + `,` + c.extra + `}`
			if c.dropped == "" {
				_, err := ParseDoc([]byte(in), now)
				require.ErrorIs(t, err, ErrDoc, "invalid JSON is refused whole")
				return
			}
			d, err := ParseDoc([]byte(in), now)
			require.NoError(t, err)
			assert.Equal(t, []string{c.dropped}, d.Dropped)
			if c.dropped == "cost_usd" {
				assert.True(t, d.HasCacheRebuild)
				assert.InDelta(t, 0.75, d.CacheRebuildUSD, 1e-9, "the rest is kept")
			} else if c.dropped != "cache_rebuild_usd" {
				assert.True(t, d.HasCost)
				assert.InDelta(t, 2.5, d.CostUSD, 1e-9, "the rest is kept")
			}
			if strings.HasSuffix(c.dropped, "resets_at") {
				l, ok := d.Limits[model.WindowFiveHour]
				require.True(t, ok, "the reading is kept without a reset time")
				assert.True(t, l.ResetsAt.IsZero())
				assert.InDelta(t, 5, l.UsedPct, 1e-9)
			}
		})
	}
	t.Run("several sorted and deduplicated", func(t *testing.T) {
		d, err := ParseDoc([]byte(`{"schema_version":1,"session_id":"s1","cost_usd":-1,"cost_usd":-2,"at":"x","cache_rebuild_usd":-5}`), now)
		require.NoError(t, err)
		assert.Equal(t, []string{"at", "cache_rebuild_usd", "cost_usd"}, d.Dropped)
	})
	t.Run("duplicate keys last wins", func(t *testing.T) {
		d, err := ParseDoc([]byte(`{"schema_version":1,"session_id":"s1","cost_usd":-1,"cost_usd":3}`), now)
		require.NoError(t, err)
		assert.InDelta(t, 3, d.CostUSD, 1e-9)
		assert.Empty(t, d.Dropped)
	})
	t.Run("bounds are inclusive and edge values kept", func(t *testing.T) {
		d, err := ParseDoc([]byte(`{"schema_version":1,"session_id":"s1","cost_usd":100000,"cache_rebuild_usd":1000,"rate_limits":{"five_hour":{"used_percentage":0},"seven_day":{"used_percentage":100}}}`), now)
		require.NoError(t, err)
		assert.Empty(t, d.Dropped)
		assert.Len(t, d.Limits, 2)
	})
}

func TestParseDocResetsAtForms(t *testing.T) {
	at := testNow.Add(2 * time.Hour)
	in := fmt.Sprintf(`{"schema_version":1,"session_id":"s1","rate_limits":{"five_hour":{"used_percentage":1,"resets_at":%q},"seven_day":{"used_percentage":1,"resets_at":%d}}}`,
		at.Format(time.RFC3339), at.Unix())
	d, err := ParseDoc([]byte(in), testNow)
	require.NoError(t, err)
	assert.Empty(t, d.Dropped)
	assert.True(t, d.Limits[model.WindowFiveHour].ResetsAt.Equal(d.Limits[model.WindowSevenDay].ResetsAt))
	assert.True(t, d.Limits[model.WindowFiveHour].ResetsAt.Equal(at))
}

func TestParseDocAtBounds(t *testing.T) {
	for name, tc := range map[string]struct {
		at      string
		kept    bool
		dropped bool
	}{
		"past 25h":   {testNow.Add(-25 * time.Hour).Format(time.RFC3339), false, true},
		"future 2m":  {testNow.Add(2 * time.Minute).Format(time.RFC3339), false, true},
		"junk":       {"x", false, true},
		"past 15m":   {testNow.Add(-15 * time.Minute).Format(time.RFC3339), true, false},
		"future 30s": {testNow.Add(30 * time.Second).Format(time.RFC3339), true, false},
	} {
		t.Run(name, func(t *testing.T) {
			d, err := ParseDoc([]byte(fmt.Sprintf(`{"schema_version":1,"session_id":"s1","at":%q}`, tc.at)), testNow)
			require.NoError(t, err)
			if tc.kept {
				want, perr := time.Parse(time.RFC3339, tc.at)
				require.NoError(t, perr)
				assert.True(t, d.At.Equal(want))
			} else {
				assert.Equal(t, testNow, d.At)
			}
			assert.Equal(t, tc.dropped, len(d.Dropped) == 1 && d.Dropped[0] == "at")
		})
	}
}

func TestParseDocNullIsAbsent(t *testing.T) {
	d, err := ParseDoc([]byte(`{"schema_version":1,"session_id":"s1","at":null,"cost_usd":null,"cache_expires_at":null,"cache_rebuild_usd":null,"rate_limits":null}`), testNow)
	require.NoError(t, err)
	assert.False(t, d.HasCost || d.HasCacheExpiry || d.HasCacheRebuild)
	assert.Empty(t, d.Dropped)
	assert.Empty(t, d.Limits)
	d, err = ParseDoc([]byte(`{"schema_version":1,"session_id":"s1","rate_limits":{"five_hour":null,"seven_day":{"used_percentage":3,"resets_at":null}}}`), testNow)
	require.NoError(t, err)
	assert.Empty(t, d.Dropped)
	assert.Len(t, d.Limits, 1)
}

func TestParseDocIgnoresUnknownAndOwnedFields(t *testing.T) {
	d, err := ParseDoc([]byte(`{"schema_version":1,"session_id":"s1","model":"m","context_used":"x","context_max":{},"extra":{"a":[1]},"rate_limits":{"extra":1,"five_hour":{"used_percentage":4,"extra":[]}}}`), testNow)
	require.NoError(t, err)
	assert.Empty(t, d.Dropped)
	assert.False(t, d.HasContext)
	assert.Empty(t, d.ModelName)
	assert.Len(t, d.Limits, 1)
}

func TestFromStatusLineBuilt(t *testing.T) {
	d, err := FromStatusLine(fixture(t, "statusline-built.json"), testNow)
	require.NoError(t, err)
	assert.Equal(t, "built-session-1", d.SessionID)
	assert.True(t, d.HasCost)
	assert.InDelta(t, 4.12, d.CostUSD, 1e-9)
	assert.Equal(t, "Opus 5.5", d.ModelName)
	assert.True(t, d.HasContext)
	assert.Equal(t, 71, d.ContextPct)
	assert.Equal(t, Limit{62, time.Unix(1794784200, 0).UTC()}, d.Limits[model.WindowFiveHour])
	assert.Equal(t, Limit{31, time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)}, d.Limits[model.WindowSevenDay])
	assert.Empty(t, d.Dropped)
	assert.True(t, d.HasCacheExpiry)
	assert.Equal(t, time.Unix(1794771246, 0).UTC(), d.CacheExpiresAt)
	assert.True(t, d.HasCacheRecache)
	assert.EqualValues(t, 42211, d.CacheRecacheTokens)
	assert.True(t, d.HasCacheWarm && d.CacheWarm)
	assert.Equal(t, "1h", d.CacheTTL)
	assert.False(t, d.HasCacheRebuild, "no price table: tokens never become money")
}

func TestFromStatusLineCaptured(t *testing.T) {
	d, err := FromStatusLine(fixture(t, "statusline-captured.json"), testNow)
	require.NoError(t, err)
	assert.Equal(t, "5cd98763-d94d-417d-b2ee-d172b8f63e81", d.SessionID)
	assert.InDelta(t, 0.011722364999999998, d.CostUSD, 1e-12)
	assert.Equal(t, "Haiku 5.5", d.ModelName)
	assert.Equal(t, 4, d.ContextPct)
	assert.Equal(t, Limit{40, time.Unix(1794784200, 0).UTC()}, d.Limits[model.WindowFiveHour])
	assert.Equal(t, Limit{79, time.Unix(1795147200, 0).UTC()}, d.Limits[model.WindowSevenDay])
	assert.Equal(t, time.Unix(1794771246, 0).UTC(), d.CacheExpiresAt)
	assert.EqualValues(t, 42211, d.CacheRecacheTokens)
	assert.True(t, d.CacheWarm)
	assert.Equal(t, "1h", d.CacheTTL)
	assert.Empty(t, d.Dropped)
	assert.Equal(t, "Haiku 5.5 · ctx 4% · $0.01 · 5h 40%", StatusText(d))
}

func TestFromStatusLineFirstResponseHasNoCache(t *testing.T) {
	d, err := FromStatusLine(fixture(t, "statusline-captured-first-response.json"), testNow)
	require.NoError(t, err)
	assert.False(t, d.HasCacheExpiry || d.HasCacheRecache || d.HasCacheWarm, "absent prompt_cache means no reading")
	assert.Empty(t, d.CacheTTL)
	assert.Len(t, d.Limits, 2)
	assert.Empty(t, d.Dropped)
}

func TestFromStatusLineWithoutLimitsOrCost(t *testing.T) {
	d, err := FromStatusLine(fixture(t, "statusline-captured-startup.json"), testNow)
	require.NoError(t, err)
	assert.Empty(t, d.Limits, "absent rate_limits is no reading, never zero")
	require.NotNil(t, d.Limits)
	assert.True(t, d.HasCost, "a cost of 0 is a reading")
	assert.Zero(t, d.CostUSD)
	assert.False(t, d.HasContext, "null context percentage is absent")
	assert.Empty(t, d.Dropped)
	assert.Equal(t, "Haiku 5.5 · $0.00", StatusText(d))

	d, err = FromStatusLine([]byte(`{"session_id":"s1"}`), testNow)
	require.NoError(t, err)
	assert.False(t, d.HasCost)
	assert.Empty(t, d.Limits)
	assert.Equal(t, "", StatusText(d))
}

func TestFromStatusLineBadFields(t *testing.T) {
	cases := []struct{ want, extra string }{
		{"cost.total_cost_usd", `"cost":{"total_cost_usd":-1}`},
		{"cost", `"cost":[]`},
		{"context_window.used_percentage", `"context_window":{"used_percentage":101}`},
		{"model.display_name", `"model":{"display_name":5}`},
		{"prompt_cache.expires_at", `"prompt_cache":{"expires_at":99999999999}`},
		{"prompt_cache.recache_tokens_if_cold", `"prompt_cache":{"recache_tokens_if_cold":-3}`},
		{"prompt_cache.warm", `"prompt_cache":{"warm":"yes"}`},
		{"prompt_cache.ttl", `"prompt_cache":{"ttl":"forever and a day"}`},
		{"rate_limits.five_hour", `"rate_limits":{"five_hour":{"used_percentage":"9"}}`},
		{"rate_limits.five_hour.resets_at", `"rate_limits":{"five_hour":{"used_percentage":9,"resets_at":1}}`},
		{"prompt_cache", `"prompt_cache":7`},
		{"prompt_cache.recache_tokens_if_cold", `"prompt_cache":{"recache_tokens_if_cold":1.5}`},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			d, err := FromStatusLine([]byte(`{"session_id":"s1","cost":{"total_cost_usd":1},`+c.extra+`}`), testNow)
			require.NoError(t, err)
			assert.Equal(t, c.want, d.Dropped[0])
		})
	}
	t.Run("model id is the fallback", func(t *testing.T) {
		d, err := FromStatusLine([]byte(`{"session_id":"s1","model":{"id":"claude-x"}}`), testNow)
		require.NoError(t, err)
		assert.Equal(t, "claude-x", d.ModelName)
		d, err = FromStatusLine([]byte(`{"session_id":"s1","model":{"display_name":"\u001b[31m","id":"claude-y"}}`), testNow)
		require.NoError(t, err)
		assert.Equal(t, "claude-y", d.ModelName)
	})
	t.Run("null cache fields are absent", func(t *testing.T) {
		d, err := FromStatusLine([]byte(`{"session_id":"s1","prompt_cache":{"expires_at":null,"warm":null,"ttl":null,"recache_tokens_if_cold":null}}`), testNow)
		require.NoError(t, err)
		assert.False(t, d.HasCacheExpiry || d.HasCacheRecache || d.HasCacheWarm)
		assert.Empty(t, d.Dropped)
	})
	t.Run("a cold cache is still a reading", func(t *testing.T) {
		d, err := FromStatusLine([]byte(`{"session_id":"s1","prompt_cache":{"warm":false,"expires_at":`+fmt.Sprint(testNow.Add(-time.Hour).Unix())+`}}`), testNow)
		require.NoError(t, err)
		assert.True(t, d.HasCacheWarm)
		assert.False(t, d.CacheWarm)
		assert.True(t, d.HasCacheExpiry)
	})
}

func TestFromStatusLineRefusals(t *testing.T) {
	for name, in := range map[string]string{
		"no id": `{"cost":{"total_cost_usd":1}}`, "array": `[]`, "empty": ``, "bad id": `{"session_id":"a b"}`,
		"big": `{"session_id":"s1","x":"` + strings.Repeat("a", MaxDoc) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := FromStatusLine([]byte(in), testNow)
			require.ErrorIs(t, err, ErrDoc)
		})
	}
}

func TestStatusTextIsOneCleanLine(t *testing.T) {
	d := Doc{
		ModelName: "Opus 5.5", HasContext: true, ContextPct: 71, HasCost: true, CostUSD: 4.12,
		Limits: map[model.UsageWindow]Limit{
			model.WindowFiveHour: {UsedPct: 62},
			model.WindowSevenDay: {UsedPct: 99},
		},
	}
	assert.Equal(t, "Opus 5.5 · ctx 71% · $4.12 · 5h 62%", StatusText(d))
	assert.Equal(t, "Opus 5.5", StatusText(Doc{ModelName: "Opus 5.5"}))
	assert.Equal(t, "", StatusText(Doc{}))
	assert.Equal(t, "", StatusText(Doc{Limits: map[model.UsageWindow]Limit{model.WindowSevenDay: {UsedPct: 5}}}), "the five-hour reading only")

	for _, name := range []string{
		"\x1b[31mX\nY" + strings.Repeat("z", 100),
		strings.Repeat("漢", 100),
		"\x1b]0;title\x07evil\r\n\t",
	} {
		d.ModelName = name
		d.CostUSD = 99999.99
		got := StatusText(d)
		assert.NotContains(t, got, "\n")
		assert.NotContains(t, got, "\x1b")
		for _, r := range got {
			assert.False(t, unicode.IsControl(r), "control rune %q in %q", r, got)
		}
		assert.LessOrEqual(t, cells(got), 80, got)
		assert.Contains(t, got, "5h 62%", "the numbers survive a long name")
	}
	d.ModelName = "X"
	d.ContextPct = 150
	d.CostUSD = math.NaN()
	assert.Equal(t, "X · 5h 62%", StatusText(d), "out-of-range values are not shown")
}

func TestSchemaFileMatchesParser(t *testing.T) {
	var schema struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
		Defs       map[string]struct {
			Required   []string `json:"required"`
			Properties map[string]struct {
				Minimum *float64 `json:"minimum"`
				Maximum *float64 `json:"maximum"`
			} `json:"properties"`
		} `json:"$defs"`
		Examples []json.RawMessage `json:"examples"`
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "ingest-schema.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &schema))

	// The names ParseDoc reads or ignores by name.
	names := []string{"schema_version", "session_id", "at", "cost_usd", "cache_expires_at", "cache_rebuild_usd", "rate_limits", "model", "context_used", "context_max"}
	var got []string
	for k := range schema.Properties {
		got = append(got, k)
	}
	assert.ElementsMatch(t, names, got)
	assert.Equal(t, []string{"schema_version", "session_id"}, schema.Required)
	assert.Equal(t, []string{"used_percentage"}, schema.Defs["window"].Required)

	require.NotEmpty(t, schema.Examples)
	now := time.Date(2026, 11, 11, 13, 41, 0, 0, time.UTC)
	d, err := ParseDoc(schema.Examples[0], now)
	require.NoError(t, err)
	assert.Empty(t, d.Dropped)
	assert.True(t, d.HasCost && d.HasCacheExpiry && d.HasCacheRebuild)
	assert.Len(t, d.Limits, 2)

	// The schema's numeric bounds are the parser's.
	var props map[string]struct{ Minimum, Maximum *float64 }
	raw, err := json.Marshal(schema.Properties)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &props))
	check := func(field string, lo, hi *float64, doc func(v float64) string) {
		require.NotNil(t, lo, field)
		require.NotNil(t, hi, field)
		for _, v := range []float64{*lo, *hi} {
			d, err := ParseDoc([]byte(doc(v)), now)
			require.NoError(t, err)
			assert.Empty(t, d.Dropped, "%s=%v is inside the schema", field, v)
		}
		for _, v := range []float64{*lo - 0.01, *hi + 0.01} {
			d, err := ParseDoc([]byte(doc(v)), now)
			require.NoError(t, err)
			assert.NotEmpty(t, d.Dropped, "%s=%v is outside the schema", field, v)
		}
	}
	for _, f := range []string{"cost_usd", "cache_rebuild_usd"} {
		check(f, props[f].Minimum, props[f].Maximum, func(v float64) string {
			return fmt.Sprintf(`{"schema_version":1,"session_id":"s","%s":%v}`, f, v)
		})
	}
	up := schema.Defs["window"].Properties["used_percentage"]
	check("used_percentage", up.Minimum, up.Maximum, func(v float64) string {
		return fmt.Sprintf(`{"schema_version":1,"session_id":"s","rate_limits":{"five_hour":{"used_percentage":%v}}}`, v)
	})
}

func FuzzParsers(f *testing.F) {
	for _, n := range []string{
		"statusline-captured.json", "statusline-captured-first-response.json", "statusline-captured-startup.json",
		"statusline-built.json", "doc-spec.json", "summary-captured.json", "summary-built.json",
	} {
		f.Add(fixture(f, n))
	}
	f.Add([]byte(`{"schema_version":1,"session_id":"s","cost_usd":1e999,"rate_limits":{"five_hour":{"used_percentage":NaN}}}`))
	f.Add([]byte(`{"session_id":"s","prompt_cache":{"expires_at":-1e300,"recache_tokens_if_cold":1e300}}`))
	f.Fuzz(func(t *testing.T, in []byte) {
		for _, parse := range []func([]byte, time.Time) (Doc, error){ParseDoc, FromStatusLine} {
			d, err := parse(in, testNow)
			if err != nil {
				require.ErrorIs(t, err, ErrDoc)
				continue
			}
			require.True(t, ValidSessionID(d.SessionID))
			require.NotNil(t, d.Limits)
			require.False(t, math.IsNaN(d.CostUSD) || math.IsInf(d.CostUSD, 0) || d.CostUSD < 0)
			require.False(t, d.HasContext && (d.ContextPct < 0 || d.ContextPct > 100))
			require.False(t, d.HasCacheRecache && d.CacheRecacheTokens < 0)
			require.LessOrEqual(t, len([]rune(d.ModelName)), 24)
			for w, l := range d.Limits {
				require.True(t, w == model.WindowFiveHour || w == model.WindowSevenDay)
				require.True(t, l.UsedPct >= 0 && l.UsedPct <= 100)
			}
			require.LessOrEqual(t, cells(StatusText(d)), 80)
			require.NotContains(t, StatusText(d), "\n")
		}
		if usd, ok := BranchCost(in, "curia/T-2-add-hello"); ok {
			require.False(t, math.IsNaN(usd) || math.IsInf(usd, 0) || usd < 0)
		}
	})
}

func TestFromStatusLineContextWindow(t *testing.T) {
	d, err := FromStatusLine(fixture(t, "statusline-captured.json"), testNow)
	require.NoError(t, err)
	assert.Equal(t, 1000000, d.ContextSize)
	assert.Equal(t, 4, d.ContextPct)
	d, err = FromStatusLine(fixture(t, "statusline-captured-startup.json"), testNow)
	require.NoError(t, err)
	assert.Equal(t, 0, d.ContextSize, "null percentage: no context reading")
	for _, cw := range []string{
		`{"context_window_size":-1,"used_percentage":5}`, `{"context_window_size":0,"used_percentage":5}`,
		`{"context_window_size":1e12,"used_percentage":5}`, `{"context_window_size":"1000000","used_percentage":5}`,
		`{"context_window_size":1000000,"used_percentage":250}`, `{"context_window_size":1000000,"used_percentage":"5"}`,
	} {
		d, err = FromStatusLine([]byte(`{"session_id":"s1","context_window":`+cw+`}`), testNow)
		require.NoError(t, err, cw)
		assert.Equal(t, 0, d.ContextSize, cw)
	}
}
