#!/bin/sh
# Installs agentiscaled on Linux or macOS (x64 or ARM64).
#
#   curl -fsSL https://raw.githubusercontent.com/whiteredhouse/agentiscaled/main/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/whiteredhouse/agentiscaled/main/install.sh | sh -s -- -version v0.11.0
#
# On Debian, Ubuntu, Fedora, RHEL, Amazon Linux and openSUSE it adds the
# signed agentiscale package repository (https://pkgs.agentiscale.net) and
# installs the agentiscaled package from it with apt, dnf, yum or zypper,
# through sudo. The system's own updates then keep it current, and the
# package manager removes it. The repository's key is checked against the
# fingerprint below before it is trusted.
#
# Elsewhere (macOS, other Linux, no sudo), with -dir, or for a prerelease,
# it puts the agentiscaled binary from a GitHub release in a directory after
# checking it against the release's checksums.txt, and needs no root.
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
# It needs curl and one of sha256sum, shasum or openssl; the binary install
# also needs tar.
#
# The whole script is one brace group: the shell reads it to the closing }
# before running any of it, so a download cut short runs nothing, not even
# main without its options.
{
set -eu

main() {
	repo=https://github.com/whiteredhouse/agentiscaled
	releases=${AGENTISCALE_RELEASES_URL:-$repo/releases}
	pkgs=${AGENTISCALE_PACKAGES_URL:-https://pkgs.agentiscale.net}
	# Where the repository configuration and the packaged binary go; the
	# tests move them.
	root=${AGENTISCALE_ROOT:-}
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
			echo "  -method   package (from the package repository, through sudo), binary, or auto (default)"
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

	if [ "$version" != latest ]; then
		check_version
	fi
	choose_package
	if [ -n "$manager" ]; then
		package_version
	fi
	if [ "$version" = latest ]; then
		# releases/latest redirects to the latest release's tag page.
		url=$(curl -fsSL --retry 3 -o /dev/null -w '%{url_effective}' "$releases/latest") ||
			fail "couldn't look up the latest release at $releases/latest"
		version=${url##*/tag/}
		check_version
	fi
	if [ -z "$manager" ]; then
		[ "$method" != package ] || fail "$why"
		command -v tar >/dev/null 2>&1 || fail "tar is needed to unpack the release"
		if [ -z "$dir" ]; then
			if [ -d "$root/usr/local/bin" ] && [ -w "$root/usr/local/bin" ]; then
				dir=$root/usr/local/bin
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

check_version() {
	printf '%s\n' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' ||
		fail "version must be a release like v0.11.0 or latest, not '$version'"
}

# choose_package sets manager (empty for a plain binary) and the command
# prefix that gives root, or why there is no package.
choose_package() {
	manager='' why=''
	[ "$method" != binary ] && [ -z "$dir" ] || return 0
	if [ "$os" != linux ]; then
		why="there are no packages for macOS; leave out -method package"
		return 0
	fi
	case $version in
	*-*)
		why="prereleases are not in the package repository; leave out -method package"
		return 0
		;;
	esac
	if command -v apt-get >/dev/null 2>&1; then
		manager=apt-get
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

# package_version reads the versions in the package repository, which can
# trail a GitHub release while it is published. For latest it picks the
# newest; a pinned version the repository lacks falls back to the binary
# (auto) or fails (-method package).
package_version() {
	index=$(curl -fsSL --retry 3 "$pkgs/deb/dists/stable/main/binary-amd64/Packages") ||
		fail "couldn't read the package repository's index at $pkgs"
	versions=$(printf '%s\n' "$index" | sed -n 's/^Version: *\([0-9][0-9.]*\)$/\1/p' |
		sort -t . -k 1,1n -k 2,2n -k 3,3n)
	[ -n "$versions" ] || fail "the package repository at $pkgs lists no agentiscaled"
	if [ "$version" = latest ]; then
		version=v$(printf '%s\n' "$versions" | tail -n 1)
		return 0
	fi
	printf '%s\n' "$versions" | grep -qx "${version#v}" && return 0
	[ "$method" != package ] ||
		fail "agentiscaled $version isn't in the package repository; leave out -method package to install the binary"
	echo "agentiscaled $version isn't in the package repository; installing the binary instead"
	manager=''
}

# The repository's signing key: agentiscale packages <packages@agentiscale.net>,
# fingerprint E15A E9FF 8FD8 7B2E 682C  AC30 4B9C 3BE1 1769 DEC4. The script
# trusts the downloaded key files only if they have these hashes.
key_gpg_sha256=36f5784a15a5150080c048947c59522afce9f548ba1ba4ddc77c69c68787edd3
key_asc_sha256=3685c8651ad4cfd53e1c65aa9daf53a366e50d04a34febcef2db85d0688512d0

# put FILE TARGET MODE writes FILE to TARGET (under root) as root.
put() {
	if ! { $as_root mkdir -p "$root${2%/*}" && $as_root cp "$1" "$root$2" && $as_root chmod "$3" "$root$2"; }; then
		fail "couldn't write $root$2"
	fi
}

install_package() {
	v=${version#v}
	bin=$root/usr/bin/agentiscaled
	echo "Adding the agentiscale package repository ($pkgs) for $manager${as_root:+ through $as_root}"
	case $manager in
	apt-get)
		fetch "$pkgs/agentiscale.gpg" "$tmp/key"
		check_key "$tmp/key" "$key_gpg_sha256"
		put "$tmp/key" /usr/share/keyrings/agentiscale-archive-keyring.gpg 644
		printf 'deb [signed-by=/usr/share/keyrings/agentiscale-archive-keyring.gpg] %s/deb stable main\n' "$pkgs" >"$tmp/list"
		put "$tmp/list" /etc/apt/sources.list.d/agentiscale.list 644
		# Only this repository: another broken source can't stop it.
		$as_root apt-get update -o Dir::Etc::sourcelist=sources.list.d/agentiscale.list \
			-o Dir::Etc::sourceparts=- -o APT::Get::List-Cleanup=0 ||
			fail "apt-get couldn't read the agentiscale package repository"
		echo "Installing agentiscaled $v with apt-get"
		# --reinstall also puts back a packaged binary that something
		# replaced; --allow-downgrades lets a pin replace a newer version.
		$as_root apt-get install -y --reinstall --allow-downgrades "agentiscaled=$v" ||
			fail "apt-get couldn't install agentiscaled $v"
		;;
	dnf | yum | zypper)
		fetch "$pkgs/agentiscale.asc" "$tmp/key"
		check_key "$tmp/key" "$key_asc_sha256"
		put "$tmp/key" /etc/pki/rpm-gpg/RPM-GPG-KEY-agentiscale 644
		# The .repo file points at the checked key on disk, not at its URL.
		repofile=/etc/yum.repos.d/agentiscale.repo type=rpm
		if [ "$manager" = zypper ]; then
			repofile=/etc/zypp/repos.d/agentiscale.repo type=rpm-md
		fi
		cat >"$tmp/repo" <<-REPO
			[agentiscale]
			name=agentiscale
			baseurl=$pkgs/rpm/stable
			enabled=1
			type=$type
			gpgcheck=1
			repo_gpgcheck=1
			gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-agentiscale
		REPO
		put "$tmp/repo" "$repofile" 644
		echo "Installing agentiscaled $v with $manager"
		if [ "$manager" = zypper ]; then
			$as_root zypper --non-interactive --gpg-auto-import-keys refresh agentiscale ||
				fail "zypper couldn't read the agentiscale package repository"
			# --oldpackage lets a pin replace a newer version, and --force
			# reinstalls the same one.
			$as_root zypper --non-interactive install --oldpackage --force --from agentiscale "agentiscaled=$v" ||
				fail "zypper couldn't install agentiscaled $v"
		else
			rpm_install
		fi
		;;
	esac
	is_version "$bin" ||
		fail "$manager kept another agentiscaled version ($first); remove the agentiscaled package first to install $version"
	report "$bin"
}

# check_key FILE SHA256 fails unless FILE is the repository key the script
# trusts.
check_key() {
	got=$(sha256 "$1")
	[ "$got" = "$2" ] ||
		fail "the package repository's key at $pkgs isn't the one this script trusts (sha256 $got); not adding it"
}

# rpm_install installs the package with dnf or yum, from the agentiscale
# repository only. Their install neither downgrades nor reinstalls, and
# yum 3 exits 1 with "Nothing to do" then, so it tries install, downgrade
# and reinstall in turn until the binary is the release. rpm itself decides
# which applies; the script never orders installed versions.
rpm_install() {
	if [ "$(rpm -q --qf '%{VERSION}' agentiscaled 2>/dev/null || true)" = "${version#v}" ] && is_version "$bin"; then
		echo "agentiscaled ${version#v} is already installed"
		return 0
	fi
	for verb in install downgrade reinstall; do
		[ "$verb" = install ] || echo "Trying $manager $verb"
		if $as_root "$manager" "$verb" -y --disablerepo='*' --enablerepo=agentiscale "agentiscaled-$v" &&
			is_version "$bin"; then
			return 0
		fi
	done
	fail "$manager couldn't install agentiscaled $v (install, downgrade and reinstall all failed or left ${first:-no agentiscaled})"
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
	fetch "$releases/download/$version/checksums.txt" "$tmp/checksums.txt"
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
