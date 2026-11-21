#!/usr/bin/env bash
# Size report for non-test Go files under cmd/ and internal/. REPORT ONLY:
# it flags files over 800 lines and packages over 5000 lines, and it always
# exits 0, so it can never fail a build. CI writes it to the job summary.
set -uo pipefail
export LC_ALL=C

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root" || exit 0
tmp=$(mktemp -d) || exit 0
trap 'rm -rf "$tmp"' EXIT

files=$(find cmd internal -name '*.go' ! -name '*_test.go' 2>/dev/null | sort)
printf '%s\n' "$files" | xargs wc -l | awk '$2 != "total" { print $1 "\t" $2 }' > "$tmp/counts"

echo "Size report (report only: the flags never fail the build)"
echo
echo "## Ten largest files (flag: over 800 lines)"
sort -t $'\t' -k1,1nr -k2,2 "$tmp/counts" | head -n 10 |
	awk -F'\t' '{ printf "%6d  %s%s\n", $1, $2, ($1 > 800 ? "   <-- over 800 lines" : "") }'
echo
echo "## Packages (lines, files; flag: over 5000 lines)"
awk -F'\t' '{ d = $2; sub(/\/[^\/]*$/, "", d); lines[d] += $1; count[d]++ }
	END { for (d in lines) printf "%d\t%d\t%s\n", lines[d], count[d], d }' "$tmp/counts" |
	sort -t $'\t' -k1,1nr -k3,3 |
	awk -F'\t' '{ printf "%6d lines %4d files  %s%s\n", $1, $2, $3, ($1 > 5000 ? "   <-- over 5000 lines" : "") }'
echo
echo "## cmd/curia non-test files: $(printf '%s\n' "$files" | grep -c '^cmd/curia/[^/]*$' || true)"
echo
echo "## Duplication (dupl -t 80, non-test files; not part of the build)"
printf '%s\n' "$files" | sed "s|^|$root/|" > "$tmp/list"
if (cd "$tmp" && go run github.com/mibk/dupl@v1.0.0 -files -t 80 < "$tmp/list" > "$tmp/dupl" 2> "$tmp/dupl.err"); then
	sed "s|$root/||g" "$tmp/dupl" | awk '
		function flush() {
			if (n == 0) return
			s = ""
			for (i = 1; i <= n; i++) s = s (i > 1 ? " ; " : "") m[i]
			printf "%d\t%d\t%s\n", n, big, s
			n = 0; big = 0
		}
		/^found / { flush(); next }
		/^  /     {
			loc = $1; sub(/.*:/, "", loc)
			split(loc, r, ",")
			if (r[2] - r[1] + 1 > big) big = r[2] - r[1] + 1
			m[++n] = $1
		}
		END { flush() }' > "$tmp/groups"
	echo "clone groups: $(grep -c '^found ' "$tmp/dupl" || true)"
	if [ -s "$tmp/groups" ]; then
		echo "five largest (by instances, then lines):"
		sort -t $'\t' -k1,1nr -k2,2nr -k3,3 "$tmp/groups" | head -n 5 |
			while IFS=$'\t' read -r n big s; do
				echo "  $n instances, up to $big lines:"
				printf '%s\n' "$s" | sed 's/ ; /\n    /g; s/^/    /'
			done
	fi
else
	echo "dupl did not run: $(head -n 3 "$tmp/dupl.err" 2>/dev/null)"
fi
exit 0
