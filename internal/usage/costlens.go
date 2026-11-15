package usage

import "encoding/json"

// MaxSummary is the size in bytes of the largest analyzer summary read.
const MaxSummary = 4 << 20

const maxBranchPart = 1000000

// BranchCost reads the cost-lens analyzer's summary and returns the sum of
// the four cost parts of the currentProject branch named branch. Nothing
// else in the summary is read. ok is false when the summary is not an object
// of that shape, the branch is absent, or a part is not a finite number from
// 0 to 1000000.
func BranchCost(summary []byte, branch string) (usd float64, ok bool) {
	if len(summary) > MaxSummary {
		return 0, false
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(summary, &top) != nil || top == nil {
		return 0, false
	}
	proj, st := object(top["currentProject"])
	if st != present {
		return 0, false
	}
	raw := proj["branches"]
	if isNull(raw) || raw[0] != '[' {
		return 0, false
	}
	var entries []json.RawMessage
	if json.Unmarshal(raw, &entries) != nil {
		return 0, false
	}
	for _, e := range entries {
		obj, st := object(e)
		if st != present {
			continue
		}
		if name, nst := text(obj["branch"]); nst == present && name == branch {
			return sumCost(obj)
		}
	}
	return 0, false
}

// sumCost adds the four cost parts of one branch entry.
func sumCost(entry map[string]json.RawMessage) (float64, bool) {
	spend, st := object(entry["spend"])
	if st != present {
		return 0, false
	}
	cost, st := object(spend["cost"])
	if st != present {
		return 0, false
	}
	var sum float64
	for _, k := range []string{"input", "output", "cacheWrite", "cacheRead"} {
		v, st := number(cost[k])
		if st != present || v < 0 || v > maxBranchPart {
			return 0, false
		}
		sum += v
	}
	return sum, true
}
