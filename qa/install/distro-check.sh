#!/bin/sh
# Runs install.sh, as root, inside a fresh Linux distribution container
# against the real releases, with the distribution's own package manager:
#
#   docker run --rm -v "$PWD:/src:ro" fedora:latest sh /src/qa/install/distro-check.sh
#
# It installs the latest release, downgrades to a pinned older one, runs the
# same pin again, and upgrades back, checking after each step that the
# package manager owns the binary at the version asked for.
set -eu

src=$(cd "$(dirname "$0")/../.." && pwd)
older=${INSTALL_CHECK_OLDER:-v0.10.0}

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

step "pin $older (a downgrade)"
install -version "$older"
owned "${older#v}"

step "pin $older again"
install -version "$older"
owned "${older#v}"

step "pin $older over a binary install that replaced the packaged file"
sh "$src/install.sh" -method binary -dir /usr/bin
install -version "$older"
owned "${older#v}"

step "latest again (an upgrade)"
install
owned "$latest"

echo
echo "All steps passed on ${PRETTY_NAME:-unknown}"
