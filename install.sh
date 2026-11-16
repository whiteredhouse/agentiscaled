#!/bin/sh
# Installs agentiscaled from a GitHub release on Linux or macOS (x64 or
# ARM64), after checking the download against the release's checksums.txt.
#
#   curl -fsSL https://raw.githubusercontent.com/whiteredhouse/agentiscaled/main/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/whiteredhouse/agentiscaled/main/install.sh | sh -s -- -version v0.11.0
#
# On Debian, Ubuntu, Fedora, RHEL and openSUSE it installs the release's
# .deb or .rpm with the system's package manager, through sudo, so the
# package manager knows the file and can remove it. Elsewhere (macOS, other
# Linux, no sudo), or with -dir, it puts the agentiscaled binary in a
# directory and needs no root.
#
# Options (or the environment variable in brackets):
#   -version VERSION  release to install, like v0.11.0, or latest (default)
#                     [AGENTISCALE_VERSION]
#   -dir DIR          put the binary in DIR, without a package
#                     [AGENTISCALE_INSTALL_DIR]
#   -method METHOD    package, binary or auto (default): auto picks package
#                     on Linux with apt, dnf, yum or zypper and root or sudo,
#                     unless -dir is given [AGENTISCALE_INSTALL_METHOD]
#
# Without a package and without -dir, the binary goes in /usr/local/bin if
# you can write to it, else ~/.local/bin.
#
# It needs curl, tar and one of sha256sum, shasum or openssl.
#
# The whole script is one brace group: the shell reads it to the closing }
# before running any of it, so a download cut short runs nothing, not even
# main without its options.
{
set -eu

main() {
	repo=https://github.com/whiteredhouse/agentiscaled
	releases=${AGENTISCALE_RELEASES_URL:-$repo/releases}
	version=${AGENTISCALE_VERSION:-latest}
	dir=${AGENTISCALE_INSTALL_DIR:-}
	method=${AGENTISCALE_INSTALL_METHOD:-auto}

	while [ $# -gt 0 ]; do
		case $1 in
		-version | --version | -dir | --dir | -method | --method)
			[ $# -ge 2 ] || fail "$1 needs a value"
			option "${1#"${1%%[!-]*}"}" "$2"
			shift 2
			;;
		-version=* | --version=* | -dir=* | --dir=* | -method=* | --method=*)
			name=${1%%=*}
			option "${name#"${name%%[!-]*}"}" "${1#*=}"
			shift
			;;
		-h | -help | --help)
			echo "usage: install.sh [-version VERSION] [-dir DIR] [-method auto|package|binary]"
			echo "  -version  release to install, like v0.11.0, or latest (default)"
			echo "  -dir      put the binary in DIR, without a package"
			echo "  -method   package (.deb/.rpm through sudo), binary, or auto (default)"
			return 0
			;;
		*) fail "unknown option $1 (options: -version, -dir, -method)" ;;
		esac
	done
	case $method in
	auto | package | binary) ;;
	*) fail "method must be auto, package or binary, not '$method'" ;;
	esac
	if [ -n "$dir" ] && [ "$method" = package ]; then
		fail "-dir installs the binary without a package; leave out one of -dir and -method package"
	fi

	command -v curl >/dev/null 2>&1 || fail "curl is needed to download the release"

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

	choose_package
	if [ -z "$manager" ]; then
		[ "$method" != package ] || fail "$why"
		command -v tar >/dev/null 2>&1 || fail "tar is needed to unpack the release"
		if [ -z "$dir" ]; then
			if [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
				dir=/usr/local/bin
			else
				dir=${HOME:?HOME is not set; pass -dir}/.local/bin
			fi
		fi
		mkdir -p "$dir" || fail "couldn't create $dir; pass -dir with a directory you can write to"
		[ -w "$dir" ] || fail "can't write to $dir; pass -dir with a directory you can write to"
	fi

	tmp=$(mktemp -d "${TMPDIR:-/tmp}/agentiscaled-install.XXXXXX")
	trap 'rm -rf "$tmp"' EXIT
	trap 'exit 1' HUP INT TERM
	fetch "$releases/download/$version/checksums.txt" "$tmp/checksums.txt"

	if [ -n "$manager" ]; then
		install_package
	else
		install_binary
	fi
}

option() {
	case $1 in
	version) version=$2 ;;
	dir) dir=$2 ;;
	method) method=$2 ;;
	esac
}

# choose_package sets manager (empty for a plain binary), the package file
# and the command prefix that gives root, or why there is no package.
choose_package() {
	manager='' why=''
	[ "$method" != binary ] && [ -z "$dir" ] || return 0
	if [ "$os" != linux ]; then
		why="there are no packages for macOS; leave out -method package"
		return 0
	fi
	case $version in
	*-*)
		why="prereleases have no package this script installs; leave out -method package"
		return 0
		;;
	esac
	v=${version#v}
	if command -v apt-get >/dev/null 2>&1; then
		manager=apt-get file="agentiscaled_${v}_${arch}.deb"
	elif command -v dnf >/dev/null 2>&1; then
		manager=dnf
	elif command -v yum >/dev/null 2>&1; then
		manager=yum
	elif command -v zypper >/dev/null 2>&1; then
		manager=zypper
	else
		why="found no apt-get, dnf, yum or zypper to install a package with"
		return 0
	fi
	if [ "$manager" != apt-get ]; then
		case $arch in
		amd64) file="agentiscaled-${v}-1.x86_64.rpm" ;;
		arm64) file="agentiscaled-${v}-1.aarch64.rpm" ;;
		esac
	fi
	if [ "$(id -u)" = 0 ]; then
		as_root=''
	elif command -v sudo >/dev/null 2>&1; then
		as_root=sudo
	elif command -v doas >/dev/null 2>&1; then
		as_root=doas
	else
		manager=''
		why="installing a package needs root, and there is no sudo or doas"
	fi
}

install_package() {
	echo "Downloading agentiscaled $version ($file)"
	fetch "$releases/download/$version/$file" "$tmp/$file"
	verify "$file"
	# apt reads the file as its own unprivileged user.
	chmod 755 "$tmp"
	chmod 644 "$tmp/$file"
	# Where the packages put the binary; the tests move it.
	bin=${AGENTISCALE_PACKAGE_BIN:-/usr/bin/agentiscaled}
	echo "Installing it with $manager${as_root:+ through $as_root}"
	case $manager in
	# --reinstall also puts back a packaged binary that something replaced.
	apt-get)
		$as_root apt-get install -y --reinstall --allow-downgrades "$tmp/$file" ||
			fail "apt-get couldn't install $file"
		;;
	dnf | yum) rpm_install ;;
	# The checksum stands in for a repository signature; --oldpackage lets a
	# pinned version replace a newer one, and --force reinstalls the same one.
	zypper)
		$as_root zypper --non-interactive install --oldpackage --force --allow-unsigned-rpm "$tmp/$file" ||
			fail "zypper couldn't install $file"
		;;
	esac
	is_version "$bin" ||
		fail "$manager kept another agentiscaled version ($first); remove the agentiscaled package first to install $version"
	report "$bin"
}

# rpm_install installs the package with dnf or yum. Their install neither
# downgrades nor reinstalls, and yum 3 exits 1 with "Nothing to do" then,
# so it tries install, downgrade and reinstall in turn until the binary is
# the release. rpm itself decides which applies, prereleases included; the
# script never orders versions.
rpm_install() {
	if [ "$(rpm -q --qf '%{VERSION}' agentiscaled 2>/dev/null || true)" = "${version#v}" ] && is_version "$bin"; then
		echo "agentiscaled ${version#v} is already installed"
		return 0
	fi
	for verb in install downgrade reinstall; do
		[ "$verb" = install ] || echo "Trying $manager $verb"
		if $as_root "$manager" "$verb" -y "$tmp/$file" && is_version "$bin"; then
			return 0
		fi
	done
	fail "$manager couldn't install $file (install, downgrade and reinstall all failed or left ${first:-no agentiscaled})"
}

# is_version BINARY runs BINARY version, keeps its first line in first, and
# succeeds if it is the release being installed.
is_version() {
	first=$("$1" version 2>&1 | head -n 1) || true
	case $first in
	"agentiscaled ${version#v} "*) return 0 ;;
	*) return 1 ;;
	esac
}

install_binary() {
	archive="agentiscaled_${version#v}_${os}_${arch}.tar.gz"
	echo "Downloading agentiscaled $version for $os/$arch"
	fetch "$releases/download/$version/$archive" "$tmp/$archive"
	verify "$archive"
	# -d follows a symlink: mv would put the binary inside a linked directory.
	if [ -d "$dir/agentiscaled" ]; then
		fail "$dir/agentiscaled is a directory; move it away or pass another -dir"
	fi
	tar -xzf "$tmp/$archive" -C "$tmp" agentiscaled || fail "couldn't unpack agentiscaled from $archive"
	chmod 755 "$tmp/agentiscaled"
	# Copy next to the target, check that the copy runs there, then rename
	# it over the target: a binary that doesn't run never replaces a working
	# one, a running node keeps its old binary, and nothing ever sees a
	# half-written file.
	staged="$dir/.agentiscaled.new.$$"
	cp "$tmp/agentiscaled" "$staged" || fail "couldn't write to $dir"
	if ! out=$("$staged" version 2>&1); then
		rm -f "$staged"
		fail "the downloaded agentiscaled doesn't run here, so $dir/agentiscaled is unchanged: $(printf '%s\n' "$out" | head -n 1)"
	fi
	mv -f "$staged" "$dir/agentiscaled" || {
		rm -f "$staged"
		fail "couldn't replace $dir/agentiscaled"
	}
	report "$dir/agentiscaled"
}

# report BINARY says what was installed and whether `agentiscaled` runs it.
report() {
	out=$("$1" version 2>&1) || fail "installed $1, but it doesn't run: $(printf '%s\n' "$out" | head -n 1)"
	echo "Installed $(printf '%s\n' "$out" | head -n 1) to $1"
	found=$(command -v agentiscaled 2>/dev/null || true)
	# -ef: the same file, also through a symlinked PATH entry. dash, bash,
	# zsh and busybox sh all have it.
	# shellcheck disable=SC3013
	if [ -z "$found" ]; then
		echo "${1%/*} is not on your PATH: add it, or run $1"
	elif ! [ "$found" -ef "$1" ]; then
		echo "Your PATH finds another agentiscaled first, at $found: remove it, or run $1"
	fi
}

verify() {
	want=$(awk -v f="$1" '$2 == f || $2 == "*" f { print $1 }' "$tmp/checksums.txt")
	[ -n "$want" ] || fail "checksums.txt of $version lists no $1"
	got=$(sha256 "$tmp/$1")
	[ "$want" = "$got" ] || fail "checksum mismatch for $1: checksums.txt says $want, the download is $got"
}

fail() {
	echo "install.sh: $*" >&2
	exit 1
}

fetch() {
	curl -fsSL --retry 3 -o "$2" "$1" || fail "couldn't download $1"
}

# sha256 FILE prints FILE's SHA-256 with the first tool that works: one on
# PATH can be missing, broken or for another platform.
sha256() {
	for tool in sha256sum shasum openssl; do
		command -v "$tool" >/dev/null 2>&1 || continue
		case $tool in
		sha256sum) sum=$(sha256sum "$1" 2>/dev/null) ;;
		shasum) sum=$(shasum -a 256 "$1" 2>/dev/null) ;;
		openssl) sum=$(openssl dgst -sha256 -r "$1" 2>/dev/null) ;;
		esac || continue
		sum=${sum%%[!0-9a-f]*}
		if [ ${#sum} -eq 64 ]; then
			echo "$sum"
			return 0
		fi
	done
	fail "found no working sha256sum, shasum or openssl to check the download"
}

main "$@"
}
