#!/bin/sh
# Installs agentiscaled from a GitHub release on Linux or macOS (x64 or
# ARM64), after checking the archive against the release's checksums.txt.
#
#   curl -fsSL https://raw.githubusercontent.com/whiteredhouse/agentiscaled/main/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/whiteredhouse/agentiscaled/main/install.sh | sh -s -- -version v0.11.0 -dir ~/bin
#
# Options (or the environment variable in brackets):
#   -version VERSION  release to install, like v0.11.0, or latest (default)
#                     [AGENTISCALE_VERSION]
#   -dir DIR          directory to put the agentiscaled binary in
#                     [AGENTISCALE_INSTALL_DIR]. Default: /usr/local/bin if
#                     you can write to it, else ~/.local/bin
#
# It needs curl, tar and one of sha256sum, shasum or openssl. It asks for no
# password: pick a directory you can write to.
#
# The whole script is a function called on its last line, so a download cut
# short runs nothing.
set -eu

main() {
	repo=https://github.com/whiteredhouse/agentiscaled
	releases=${AGENTISCALE_RELEASES_URL:-$repo/releases}
	version=${AGENTISCALE_VERSION:-latest}
	dir=${AGENTISCALE_INSTALL_DIR:-}

	while [ $# -gt 0 ]; do
		case $1 in
		-version | --version)
			[ $# -ge 2 ] || fail "$1 needs a value"
			version=$2
			shift 2
			;;
		-version=* | --version=*)
			version=${1#*=}
			shift
			;;
		-dir | --dir)
			[ $# -ge 2 ] || fail "$1 needs a value"
			dir=$2
			shift 2
			;;
		-dir=* | --dir=*)
			dir=${1#*=}
			shift
			;;
		-h | -help | --help)
			echo "usage: install.sh [-version VERSION] [-dir DIR]"
			echo "  -version  release to install, like v0.11.0, or latest (default)"
			echo "  -dir      where to put agentiscaled (default /usr/local/bin if writable, else ~/.local/bin)"
			return 0
			;;
		*) fail "unknown option $1 (options: -version, -dir)" ;;
		esac
	done

	command -v curl >/dev/null 2>&1 || fail "curl is needed to download the release"
	command -v tar >/dev/null 2>&1 || fail "tar is needed to unpack the release"

	case $(uname -s) in
	Linux) os=linux ;;
	Darwin) os=darwin ;;
	*) fail "agentiscaled runs on Linux and macOS, not $(uname -s)" ;;
	esac
	case $(uname -m) in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) fail "agentiscaled runs on x64 and ARM64, not $(uname -m)" ;;
	esac
	# A shell under Rosetta reports x86_64 on an Apple silicon Mac.
	if [ "$os/$arch" = darwin/amd64 ] && [ "$(sysctl -n hw.optional.arm64 2>/dev/null || true)" = 1 ]; then
		arch=arm64
	fi

	if [ "$version" = latest ]; then
		# releases/latest redirects to the latest release's tag page.
		url=$(curl -fsSL --retry 3 -o /dev/null -w '%{url_effective}' "$releases/latest") ||
			fail "couldn't look up the latest release at $releases/latest"
		version=${url##*/tag/}
	fi
	printf '%s\n' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' ||
		fail "version must be a release like v0.11.0 or latest, not '$version'"

	if [ -z "$dir" ]; then
		if [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
			dir=/usr/local/bin
		else
			dir=${HOME:?HOME is not set; pass -dir}/.local/bin
		fi
	fi
	mkdir -p "$dir" || fail "couldn't create $dir; pass -dir with a directory you can write to"
	[ -w "$dir" ] || fail "can't write to $dir; pass -dir with a directory you can write to"

	tmp=$(mktemp -d "${TMPDIR:-/tmp}/agentiscaled-install.XXXXXX")
	trap 'rm -rf "$tmp"' EXIT
	trap 'exit 1' HUP INT TERM

	archive="agentiscaled_${version#v}_${os}_${arch}.tar.gz"
	echo "Downloading agentiscaled $version for $os/$arch"
	fetch "$releases/download/$version/checksums.txt" "$tmp/checksums.txt"
	fetch "$releases/download/$version/$archive" "$tmp/$archive"
	want=$(awk -v f="$archive" '$2 == f || $2 == "*" f { print $1 }' "$tmp/checksums.txt")
	[ -n "$want" ] || fail "checksums.txt of $version lists no $archive"
	got=$(sha256 "$tmp/$archive")
	[ "$want" = "$got" ] || fail "checksum mismatch for $archive: checksums.txt says $want, the download is $got"

	tar -xzf "$tmp/$archive" -C "$tmp" agentiscaled || fail "couldn't unpack agentiscaled from $archive"
	chmod 755 "$tmp/agentiscaled"
	# Copy next to the target, then rename over it: a running node keeps its
	# old binary, and nothing ever sees a half-written file.
	cp "$tmp/agentiscaled" "$dir/.agentiscaled.new.$$" || fail "couldn't write to $dir"
	mv -f "$dir/.agentiscaled.new.$$" "$dir/agentiscaled"
	echo "Installed $("$dir/agentiscaled" version 2>&1 | head -n 1) to $dir/agentiscaled"

	case ":${PATH:-}:" in
	*":$dir:"*) ;;
	*) echo "$dir is not on your PATH: add it, or run $dir/agentiscaled" ;;
	esac
}

fail() {
	echo "install.sh: $*" >&2
	exit 1
}

fetch() {
	curl -fsSL --retry 3 -o "$2" "$1" || fail "couldn't download $1"
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{ print $1 }'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{ print $1 }'
	elif command -v openssl >/dev/null 2>&1; then
		openssl dgst -sha256 "$1" | awk '{ print $NF }'
	else
		fail "sha256sum, shasum or openssl is needed to check the download"
	fi
}

main "$@"
