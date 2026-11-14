#!/bin/sh
# Offline tests for scripts/install.sh. Run by `make check` (the install-script
# gate). Nothing here touches the network or the real home directory: release
# assets are a local directory served through file:// URLs, and HOME and the
# install directory are temporary.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
script=$here/install.sh
fails=0
n=0

ok() {
	n=$((n + 1))
}

fail() {
	n=$((n + 1))
	fails=$((fails + 1))
	printf 'FAIL: %s\n' "$*" >&2
}

# expect_eq WHAT WANT GOT
expect_eq() {
	if [ "$2" = "$3" ]; then ok; else fail "$1: want '$2', got '$3'"; fi
}

# expect_has WHAT NEEDLE HAYSTACK
expect_has() {
	case "$3" in
	*"$2"*) ok ;;
	*) fail "$1: output does not contain '$2': $3" ;;
	esac
}

work=$(mktemp -d "${TMPDIR:-/tmp}/curia-install-test.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM

# in_script CMD... runs CMD in a subshell that has sourced install.sh.
in_script() {
	(
		CURIA_INSTALL_NO_MAIN=1
		export CURIA_INSTALL_NO_MAIN
		# shellcheck source=install.sh
		. "$script"
		set +e
		"$@"
	)
}

fake_uname() {
	case "$1" in
	-s) echo "$FAKE_S" ;;
	-m) echo "$FAKE_M" ;;
	esac
}

# platform S M runs detect_os and detect_arch for that uname answer and prints
# "os/arch", or "FAIL" when either refuses.
platform() {
	(
		CURIA_INSTALL_NO_MAIN=1
		export CURIA_INSTALL_NO_MAIN
		. "$script"
		FAKE_S=$1
		FAKE_M=$2
		uname() { fake_uname "$@"; }
		o=$(detect_os 2>/dev/null) || {
			echo FAIL
			exit 0
		}
		a=$(detect_arch 2>/dev/null) || {
			echo FAIL
			exit 0
		}
		echo "$o/$a"
	)
}

expect_eq "darwin arm64" darwin/arm64 "$(platform Darwin arm64)"
expect_eq "darwin x86_64" darwin/amd64 "$(platform Darwin x86_64)"
expect_eq "linux x86_64" linux/amd64 "$(platform Linux x86_64)"
expect_eq "linux aarch64" linux/arm64 "$(platform Linux aarch64)"
expect_eq "freebsd" FAIL "$(platform FreeBSD amd64)"
expect_eq "armv7l" FAIL "$(platform Linux armv7l)"
expect_eq "windows" FAIL "$(platform MINGW64_NT-10.0 x86_64)"

tag() {
	(
		CURIA_INSTALL_NO_MAIN=1
		export CURIA_INSTALL_NO_MAIN
		. "$script"
		(normalize_tag "$1") 2>/dev/null || echo FAIL
	)
}

expect_eq "tag without v" v0.1.0 "$(tag 0.1.0)"
expect_eq "tag with v" v0.1.0 "$(tag v0.1.0)"
expect_eq "tag prerelease" v1.2.3-rc.1 "$(tag v1.2.3-rc.1)"
expect_eq "tag empty" FAIL "$(tag '')"
expect_eq "tag word" FAIL "$(tag latest)"
expect_eq "tag path" FAIL "$(tag 'v0.1.0/../../x')"
expect_eq "tag space" FAIL "$(tag 'v0.1.0 x')"
expect_eq "tag query" FAIL "$(tag 'v0.1.0?x=1')"

# Release fixtures: a fake binary in the archive layout GoReleaser produces.
os=$(in_script detect_os)
arch=$(in_script detect_arch)
rel=$work/rel
mkdir -p "$rel/v0.1.0" "$work/pkg"
printf '#!/bin/sh\necho "curia version v0.1.0"\n' >"$work/pkg/curia"
chmod 755 "$work/pkg/curia"
echo "licence" >"$work/pkg/LICENSE"
echo "readme" >"$work/pkg/README.md"
archive=curia_0.1.0_${os}_${arch}.tar.gz
tar -czf "$rel/v0.1.0/$archive" -C "$work/pkg" curia LICENSE README.md
sum=$(in_script sha256_of "$rel/v0.1.0/$archive")
other=curia_0.1.0_other_other.tar.gz
printf '%s  %s\n%s  %s\n' "0000000000000000000000000000000000000000000000000000000000000000" "$other" "$sum" "$archive" >"$rel/v0.1.0/checksums.txt"

# run_install DIR [ENV...] runs the installer against the fixtures; stdout and
# stderr go to $work/out and the exit status is printed.
run_install() {
	d=$1
	shift
	set +e
	env HOME="$work/home" CURIA_VERSION=v0.1.0 CURIA_INSTALL_DIR="$d" \
		CURIA_RELEASE_URL="file://$rel" "$@" sh "$script" >"$work/out" 2>&1
	rc=$?
	set -e
	echo "$rc"
}

if ! command -v curl >/dev/null 2>&1; then
	echo "SKIP: end-to-end tests need curl (file:// downloads)"
else
	mkdir -p "$work/home"

	# Success, directory not on PATH.
	rc=$(run_install "$work/bin1" PATH="/usr/bin:/bin")
	expect_eq "install ok exit" 0 "$rc"
	if [ -x "$work/bin1/curia" ]; then ok; else fail "install ok: binary missing or not executable"; fi
	expect_eq "installed binary runs" "curia version v0.1.0" "$("$work/bin1/curia" --version)"
	expect_has "not-on-PATH hint" "is not on your PATH" "$(cat "$work/out")"
	expect_has "PATH line" "export PATH=\"$work/bin1:\$PATH\"" "$(cat "$work/out")"
	expect_has "next step with full path" "$work/bin1/curia install" "$(cat "$work/out")"
	if ls "$work/bin1" | grep -q '^\.curia\.new'; then fail "temporary file left in the install directory"; else ok; fi

	# Success, directory on PATH, and an existing binary is replaced.
	mkdir -p "$work/bin2"
	echo old >"$work/bin2/curia"
	rc=$(run_install "$work/bin2" PATH="$work/bin2:/usr/bin:/bin")
	expect_eq "install over old exit" 0 "$rc"
	expect_eq "old binary replaced" "curia version v0.1.0" "$("$work/bin2/curia" --version)"
	case "$(cat "$work/out")" in
	*"not on your PATH"*) fail "PATH hint printed although the directory is on PATH" ;;
	*) ok ;;
	esac
	expect_has "next step" "run  curia install" "$(cat "$work/out")"

	# The version may be given without the v.
	rc=$(run_install "$work/bin3" PATH="/usr/bin:/bin" CURIA_VERSION=0.1.0)
	expect_eq "version without v" 0 "$rc"

	# Default directory is under HOME.
	rc=$(run_install "" PATH="/usr/bin:/bin")
	if [ "$rc" = 0 ] && [ -x "$work/home/.local/bin/curia" ]; then ok; else fail "default install dir under HOME (exit $rc)"; fi

	# Fail closed: checksum mismatch.
	mkdir -p "$work/rel2/v0.1.0"
	cp "$rel/v0.1.0/$archive" "$work/rel2/v0.1.0/"
	printf '%s  %s\n' "1111111111111111111111111111111111111111111111111111111111111111" "$archive" >"$work/rel2/v0.1.0/checksums.txt"
	rc=$(run_install "$work/bin4" PATH="/usr/bin:/bin" CURIA_RELEASE_URL="file://$work/rel2")
	if [ "$rc" != 0 ]; then ok; else fail "checksum mismatch did not fail"; fi
	expect_has "mismatch message" "checksum mismatch" "$(cat "$work/out")"
	if [ -e "$work/bin4/curia" ]; then fail "binary installed despite checksum mismatch"; else ok; fi

	# Fail closed: archive not listed in the checksums file.
	printf '%s  %s\n' "$sum" "curia_0.1.0_other_other.tar.gz" >"$work/rel2/v0.1.0/checksums.txt"
	rc=$(run_install "$work/bin5" PATH="/usr/bin:/bin" CURIA_RELEASE_URL="file://$work/rel2")
	if [ "$rc" != 0 ] && [ ! -e "$work/bin5/curia" ]; then ok; else fail "unlisted archive did not fail closed"; fi

	# Fail closed: archive tampered with after the checksum was made.
	mkdir -p "$work/rel3/v0.1.0"
	cp "$rel/v0.1.0/checksums.txt" "$work/rel3/v0.1.0/"
	cp "$rel/v0.1.0/$archive" "$work/rel3/v0.1.0/"
	printf 'x' >>"$work/rel3/v0.1.0/$archive"
	rc=$(run_install "$work/bin6" PATH="/usr/bin:/bin" CURIA_RELEASE_URL="file://$work/rel3")
	if [ "$rc" != 0 ] && [ ! -e "$work/bin6/curia" ]; then ok; else fail "tampered archive did not fail closed"; fi

	# Fail closed: download error (no such release).
	rc=$(run_install "$work/bin7" PATH="/usr/bin:/bin" CURIA_VERSION=v9.9.9)
	if [ "$rc" != 0 ] && [ ! -e "$work/bin7/curia" ]; then ok; else fail "download error did not fail closed"; fi
	expect_has "download error message" "download failed" "$(cat "$work/out")"

	# Fail closed: unsupported platform, through a fake uname first on PATH.
	mkdir -p "$work/fakebin"
	printf '#!/bin/sh\ncase "$1" in -s) echo FreeBSD;; -m) echo amd64;; esac\n' >"$work/fakebin/uname"
	chmod 755 "$work/fakebin/uname"
	rc=$(run_install "$work/bin8" PATH="$work/fakebin:/usr/bin:/bin")
	if [ "$rc" != 0 ] && [ ! -e "$work/bin8/curia" ]; then ok; else fail "unsupported platform did not fail closed"; fi
	expect_has "platform message" "unsupported operating system" "$(cat "$work/out")"

	# Fail closed: invalid version string.
	rc=$(run_install "$work/bin9" PATH="/usr/bin:/bin" CURIA_VERSION='v1/../x')
	if [ "$rc" != 0 ]; then ok; else fail "invalid version did not fail"; fi

	# Fail closed: unwritable directory, never escalating (skipped as root).
	if [ "$(id -u)" != 0 ]; then
		mkdir -p "$work/ro"
		chmod 555 "$work/ro"
		rc=$(run_install "$work/ro" PATH="/usr/bin:/bin")
		if [ "$rc" != 0 ]; then ok; else fail "unwritable directory did not fail"; fi
		expect_has "unwritable message" "never uses sudo" "$(cat "$work/out")"
		chmod 755 "$work/ro"
	fi
fi

if [ "$fails" -gt 0 ]; then
	printf '%d of %d install.sh checks failed\n' "$fails" "$n" >&2
	exit 1
fi
printf 'install.sh: %d checks passed\n' "$n"
