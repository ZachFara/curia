#!/usr/bin/env bash
# Layout check (docs/decisions/0006): every tracked or untracked-but-not-ignored
# file must sit in an allowed place. Run by `make check`.
#
# The allowlists below mirror the plan's File structure table. Adding a
# directory or root file is a change to a shared file, made by the
# orchestrator, never by a task.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

# Files allowed at the repository root.
root_files=(
	.gitignore
	.golangci.yml
	AGENTS.md
	CLAUDE.md
	Makefile
	PROGRESS.md
	go.mod
	go.sum
	lefthook.yml
)

# Top-level directories allowed to hold files.
top_dirs=(cmd internal e2e testdata docs scripts .github)

# Packages allowed under internal/ (plan File structure table).
internal_pkgs=(model config store state transcript hook tmux fleet attention ui install)

# Commands allowed under cmd/.
cmd_dirs=(curia)

# Directories allowed to contain Go source.
go_dirs=(cmd internal e2e)

in_list() {
	local needle=$1
	shift
	local item
	for item in "$@"; do
		[[ $item == "$needle" ]] && return 0
	done
	return 1
}

violations=()

while IFS= read -r -d '' path; do
	# Skip files deleted from the working tree but still in the index.
	[[ -e $path ]] || continue

	if [[ $path != */* ]]; then
		in_list "$path" "${root_files[@]}" || violations+=("$path: file not allowed at the repository root")
		continue
	fi

	top=${path%%/*}
	if ! in_list "$top" "${top_dirs[@]}"; then
		violations+=("$path: top-level directory '$top/' is not allowed")
		continue
	fi

	rest=${path#*/}
	case $top in
	internal)
		pkg=${rest%%/*}
		if [[ $rest != */* ]] || ! in_list "$pkg" "${internal_pkgs[@]}"; then
			violations+=("$path: 'internal/$pkg' is not a package in the plan's File structure table")
			continue
		fi
		;;
	cmd)
		sub=${rest%%/*}
		if [[ $rest != */* ]] || ! in_list "$sub" "${cmd_dirs[@]}"; then
			violations+=("$path: only cmd/curia/ is allowed under cmd/")
			continue
		fi
		;;
	esac

	if [[ $path == *.go ]] && ! in_list "$top" "${go_dirs[@]}"; then
		violations+=("$path: Go source is only allowed under cmd/, internal/ and e2e/")
	fi
done < <(git ls-files -z --cached --others --exclude-standard)

if ((${#violations[@]} > 0)); then
	echo "layout check failed:" >&2
	printf '  %s\n' "${violations[@]}" >&2
	echo "Throwaway files belong in .scratch/ (gitignored). See AGENTS.md." >&2
	exit 1
fi

echo "layout check passed"
