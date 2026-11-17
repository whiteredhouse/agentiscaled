#!/bin/sh
# Runs install.sh, as root, inside a fresh Linux distribution container
# against the real package repository, with the distribution's own package
# manager:
#
#   docker run --rm -v "$PWD:/src:ro" fedora:latest sh /src/qa/install/distro-check.sh
#
# It installs the latest release from the repository, runs it again, pins
# that release, reinstalls it over a binary install that replaced the
# packaged file, and checks that a version the repository lacks is refused,
# checking after each step that the package manager owns the binary at the
# version asked for. With INSTALL_CHECK_OLDER set to an older release in the
# repository, it also downgrades to it and upgrades back.
set -eu

src=$(cd "$(dirname "$0")/../.." && pwd)
older=${INSTALL_CHECK_OLDER:-}

# The script needs curl, and the binary-install step tar and gzip; slim
# images may lack them.
need=''
for tool in curl tar gzip; do
	command -v "$tool" >/dev/null 2>&1 || need="$need $tool"
done
if [ -n "$need" ]; then
	if command -v apt-get >/dev/null 2>&1; then
		# shellcheck disable=SC2086 # one word per tool
		apt-get update -qq && apt-get install -y -qq $need ca-certificates >/dev/null
	elif command -v dnf >/dev/null 2>&1; then
		# shellcheck disable=SC2086
		dnf install -y -q $need
	elif command -v yum >/dev/null 2>&1; then
		# shellcheck disable=SC2086
		yum install -y -q $need
	elif command -v zypper >/dev/null 2>&1; then
		# shellcheck disable=SC2086
		zypper --non-interactive -q install $need
	fi
fi

step() {
	echo
	echo "==== $*"
}

# owned VERSION: the binary and the package database both say VERSION.
owned() {
	out=$(/usr/bin/agentiscaled version)
	case $out in
	"agentiscaled $1 "*) ;;
	*)
		echo "FAIL: /usr/bin/agentiscaled is '$out', want $1" >&2
		exit 1
		;;
	esac
	if command -v dpkg-query >/dev/null 2>&1; then
		have=$(dpkg-query -W -f '${Version}' agentiscaled)
	else
		have=$(rpm -q --qf '%{VERSION}' agentiscaled)
	fi
	if [ "$have" != "$1" ]; then
		echo "FAIL: the package database has agentiscaled $have, want $1" >&2
		exit 1
	fi
	echo "ok: agentiscaled $1, owned by the package manager"
}

install() {
	sh "$src/install.sh" -method package "$@"
}

# shellcheck source=/dev/null
. /etc/os-release
echo "Distribution: ${PRETTY_NAME:-unknown}"

step "latest"
install
latest=$(/usr/bin/agentiscaled version | sed -n 's/^agentiscaled \([^ ]*\) .*/\1/p')
owned "$latest"
if command -v apt-get >/dev/null 2>&1; then
	grep -q '^deb \[signed-by=/usr/share/keyrings/agentiscale-archive-keyring.gpg\] https://pkgs.agentiscale.net/deb stable main$' \
		/etc/apt/sources.list.d/agentiscale.list
else
	grep -q '^gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-agentiscale$' /etc/yum.repos.d/agentiscale.repo \
		/etc/zypp/repos.d/agentiscale.repo 2>/dev/null
fi
echo "ok: the repository is configured"

step "latest again"
install
owned "$latest"

step "pin v$latest"
install -version "v$latest"
owned "$latest"

step "pin v$latest over an older binary install that replaced the packaged file"
sh "$src/install.sh" -method binary -dir /usr/bin -version "${INSTALL_CHECK_REPLACED:-v0.10.0}"
install -version "v$latest"
owned "$latest"

step "a version the repository lacks"
if install -version v0.0.1; then
	echo "FAIL: -method package installed v0.0.1" >&2
	exit 1
fi
owned "$latest"

if [ -n "$older" ]; then
	step "pin $older (a downgrade)"
	install -version "$older"
	owned "${older#v}"

	step "latest again (an upgrade)"
	install
	owned "$latest"
fi

echo
echo "All steps passed on ${PRETTY_NAME:-unknown}"
