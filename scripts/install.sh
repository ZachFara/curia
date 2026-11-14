#!/bin/sh
# Install curia from a GitHub release, without Homebrew and without sudo:
#
#   curl -fsSL https://raw.githubusercontent.com/ZachFara/curia/main/scripts/install.sh | sh
#
# Environment:
#   CURIA_VERSION      release to install, for example v0.1.0 (default: the latest)
#   CURIA_INSTALL_DIR  where to put the binary (default: $HOME/.local/bin)
#   CURIA_RELEASE_URL  base URL that holds <tag>/<file> for each release asset
#                      (default: the GitHub releases download URL; the tests
#                      point it at a local directory)
#
# The script downloads the archive for this OS and architecture, checks its
# SHA-256 against the release's checksums.txt and stops at the first problem.
# Sourcing it with CURIA_INSTALL_NO_MAIN=1 defines the functions and runs nothing.

set -eu

REPO="ZachFara/curia"

say() {
	printf '%s\n' "$*"
}

die() {
	printf 'curia install: %s\n' "$*" >&2
	exit 1
}

# detect_os prints the GoReleaser OS name for this machine.
detect_os() {
	case "$(uname -s)" in
	Darwin) echo darwin ;;
	Linux) echo linux ;;
	*) die "unsupported operating system: $(uname -s) (supported: macOS, Linux)" ;;
	esac
}

# detect_arch prints the GoReleaser architecture name for this machine.
detect_arch() {
	case "$(uname -m)" in
	x86_64 | amd64) echo amd64 ;;
	arm64 | aarch64) echo arm64 ;;
	*) die "unsupported architecture: $(uname -m) (supported: amd64, arm64)" ;;
	esac
}

# normalize_tag prints the release tag (with a leading v) for a version given
# with or without one, and refuses anything that is not a plain version.
normalize_tag() {
	v=${1#v}
	case "$v" in
	'' | *[!0-9A-Za-z.+-]* | [!0-9]*) die "invalid version: $1 (expected something like v0.1.0)" ;;
	esac
	echo "v$v"
}

# fetch URL DEST downloads URL to DEST and fails on any HTTP or network error.
fetch() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL -o "$2" "$1" || die "download failed: $1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$2" "$1" || die "download failed: $1"
	else
		die "neither curl nor wget is installed"
	fi
}

# latest_tag prints the tag of the latest release, read from the redirect of
# the releases/latest page (no API token or rate limit involved).
latest_tag() {
	command -v curl >/dev/null 2>&1 || die "curl is required to look up the latest release; set CURIA_VERSION instead"
	url=$(curl -fsSL -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest") ||
		die "could not look up the latest release"
	tag=${url##*/}
	case "$tag" in
	v[0-9]*) echo "$tag" ;;
	*) die "could not find a release (got: $url); has one been published?" ;;
	esac
}

# sha256_of FILE prints the file's SHA-256 as lowercase hex.
sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{print $1}'
	else
		die "neither sha256sum nor shasum is installed; cannot verify the download"
	fi
}

# expected_sum NAME CHECKSUMS prints the hash listed for NAME, or nothing.
expected_sum() {
	awk -v n="$1" '$2 == n || $2 == "*" n { print $1; exit }' "$2" | tr 'A-F' 'a-f'
}

# verify ARCHIVE NAME CHECKSUMS fails unless ARCHIVE has the hash listed for NAME.
verify() {
	want=$(expected_sum "$2" "$3")
	[ -n "$want" ] || die "$2 is not listed in the checksums file; refusing to install"
	got=$(sha256_of "$1")
	[ "$got" = "$want" ] || die "checksum mismatch for $2 (expected $want, got $got); refusing to install"
}

# path_has DIR succeeds when DIR is one of the entries of PATH.
path_has() {
	case ":$PATH:" in
	*":$1:"*) return 0 ;;
	*) return 1 ;;
	esac
}

main() {
	os=$(detect_os)
	arch=$(detect_arch)
	if [ -n "${CURIA_VERSION:-}" ]; then
		tag=$(normalize_tag "$CURIA_VERSION")
	else
		tag=$(latest_tag)
	fi
	version=${tag#v}
	dir=${CURIA_INSTALL_DIR:-${HOME:?HOME is not set}/.local/bin}
	base=${CURIA_RELEASE_URL:-https://github.com/$REPO/releases/download}/$tag
	name=curia_${version}_${os}_${arch}.tar.gz

	tmp=$(mktemp -d "${TMPDIR:-/tmp}/curia-install.XXXXXX") || die "could not create a temporary directory"
	trap 'rm -rf "$tmp"' EXIT HUP INT TERM

	say "Installing curia $tag for $os/$arch"
	fetch "$base/$name" "$tmp/$name"
	fetch "$base/checksums.txt" "$tmp/checksums.txt"
	verify "$tmp/$name" "$name" "$tmp/checksums.txt"

	mkdir "$tmp/x"
	tar -xzf "$tmp/$name" -C "$tmp/x" || die "could not unpack $name"
	[ -f "$tmp/x/curia" ] || die "$name does not contain a curia binary"

	mkdir -p "$dir" || die "could not create $dir; set CURIA_INSTALL_DIR to a directory you can write to"
	[ -w "$dir" ] || die "$dir is not writable; set CURIA_INSTALL_DIR to a directory you can write to (this script never uses sudo)"
	cp "$tmp/x/curia" "$dir/.curia.new.$$" || die "could not write to $dir"
	chmod 755 "$dir/.curia.new.$$"
	mv -f "$dir/.curia.new.$$" "$dir/curia" || die "could not install $dir/curia"

	say "Installed $dir/curia"
	if ! command -v tmux >/dev/null 2>&1; then
		say "Note: tmux (3.3 or newer) is not installed; Curia needs it."
	fi
	if path_has "$dir"; then
		say "Next: run  curia install"
	else
		say ""
		say "$dir is not on your PATH. Add it by putting this line in your shell profile"
		say "(~/.zshrc, ~/.bashrc or similar), then start a new shell:"
		say ""
		say "  export PATH=\"$dir:\$PATH\""
		say ""
		say "Next: run  $dir/curia install"
	fi
}

if [ "${CURIA_INSTALL_NO_MAIN:-}" != 1 ]; then
	main
fi
