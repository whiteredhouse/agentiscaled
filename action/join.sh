#!/usr/bin/env bash
# Joins a GitHub Actions job to an agentiscale network with the job's OIDC
# token and runs the node in the background. action.yml passes the inputs as
# AGENTISCALE_* variables.
set -euo pipefail

fail() {
	echo "::error title=agentiscale::$*"
	exit 1
}

network=${AGENTISCALE_NETWORK:-}
[[ "$network" =~ ^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$ ]] ||
	fail "network-id must be your network's ID, as the console shows it under your network's name"
[ -n "${ACTIONS_ID_TOKEN_REQUEST_URL:-}" ] && [ -n "${ACTIONS_ID_TOKEN_REQUEST_TOKEN:-}" ] ||
	fail "the job can't ask GitHub for an OIDC token: give it 'permissions: id-token: write'"
control=${AGENTISCALE_CONTROL:-https://control.agentiscale.net}
# Control builds the audience it expects from its URL without trailing
# slashes.
while [ "${control%/}" != "$control" ]; do control=${control%/}; done
data_dir=$AGENTISCALE_DATA_DIR
timeout=${AGENTISCALE_TIMEOUT:-120}
[[ "$timeout" =~ ^[0-9]+$ ]] || fail "timeout-seconds must be a whole number"

case "${RUNNER_OS:-}/${RUNNER_ARCH:-}" in
Linux/X64) platform=linux_amd64 ;;
Linux/ARM64) platform=linux_arm64 ;;
macOS/X64) platform=darwin_amd64 ;;
macOS/ARM64) platform=darwin_arm64 ;;
*) fail "agentiscaled runs on Linux and macOS runners (x64 or ARM64), not ${RUNNER_OS:-?}/${RUNNER_ARCH:-?}" ;;
esac

# ---- the release, checked against its checksums ----
version=${AGENTISCALE_VERSION:-}
if [ -z "$version" ]; then
	# Pinned to a release tag, the action installs that release.
	if [[ "${AGENTISCALE_ACTION_REF:-}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
		version=$AGENTISCALE_ACTION_REF
	else
		version=latest
	fi
fi
releases=https://github.com/whiteredhouse/agentiscaled/releases
if [ "$version" = latest ]; then
	version=$(curl -sSfI --retry 3 "$releases/latest" | tr -d '\r' | sed -n 's|^[Ll]ocation: .*/tag/||p')
fi
[[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail "version must be a release like v0.12.0 or latest, not '$version'"
if [ "$(printf '%s\n' v0.11.0 "$version" | sort -V | head -n 1)" != v0.11.0 ]; then
	fail "$version can't join with an OIDC token; use v0.11.0 or later"
fi
install_dir="${RUNNER_TEMP:-/tmp}/agentiscaled-$version"
bin="$install_dir/agentiscaled"
if [ ! -x "$bin" ]; then
	archive="agentiscaled_${version#v}_${platform}.tar.gz"
	mkdir -p "$install_dir"
	curl -sSfL --retry 3 -o "$install_dir/$archive" "$releases/download/$version/$archive"
	curl -sSfL --retry 3 -o "$install_dir/checksums.txt" "$releases/download/$version/checksums.txt"
	want=$(awk -v f="$archive" '$2 == f { print $1 }' "$install_dir/checksums.txt")
	if command -v sha256sum >/dev/null; then
		got=$(sha256sum "$install_dir/$archive" | awk '{ print $1 }')
	else
		got=$(shasum -a 256 "$install_dir/$archive" | awk '{ print $1 }')
	fi
	[ -n "$want" ] && [ "$want" = "$got" ] || fail "checksum mismatch for $archive"
	tar -xzf "$install_dir/$archive" -C "$install_dir" agentiscaled
	rm -f "$install_dir/$archive"
fi
echo "$install_dir" >>"$GITHUB_PATH"
echo "Installed $("$bin" version 2>&1 | head -n 1)"

# ---- join with a token for this network only ----
mkdir -p "$data_dir"
chmod 700 "$data_dir"
token_file="$data_dir/oidc-token"
trap 'rm -f "$token_file"' EXIT
answer=$(curl -sSf --retry 3 -G -H "Authorization: Bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" \
	--data-urlencode "audience=$control/networks/$network" "$ACTIONS_ID_TOKEN_REQUEST_URL") ||
	fail "GitHub didn't issue an OIDC token"
token=$(printf '%s' "$answer" | sed -n 's/.*"value"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
[ -n "$token" ] || fail "GitHub's OIDC answer had no token"
echo "::add-mask::$token"
(umask 077 && printf '%s\n' "$token" >"$token_file")
# The node API listens only on its Unix socket, so it takes no TCP port
# from the job.
"$bin" up -foreground -y -clients none -data-dir "$data_dir" -control "$control" \
	-network "$network" -oidc-token-path "$token_file" \
	-hostname "${AGENTISCALE_HOSTNAME:-github-${GITHUB_RUN_ID:-0}}" -bind-addr ""
rm -f "$token_file"

# ---- run the node until the job ends ----
log="$data_dir/node.log"
nohup "$bin" run -data-dir "$data_dir" >>"$log" 2>&1 &
pid=$!
echo "$pid" >"$data_dir/node.pid"
deadline=$((SECONDS + timeout))
until "$bin" status -data-dir "$data_dir" 2>/dev/null | grep -q '^Mesh connection: connected'; do
	if ! kill -0 "$pid" 2>/dev/null; then
		tail -n 40 "$log" || true
		fail "the node exited before it connected to the network; its log is above"
	fi
	if [ "$SECONDS" -ge "$deadline" ]; then
		tail -n 40 "$log" || true
		fail "the node didn't connect to the network within ${timeout}s; its log is above"
	fi
	sleep 2
done
peer=$("$bin" status -data-dir "$data_dir" | sed -n 's/^Node: *\([^ ]*\).*/\1/p')
echo "Joined network $network as $peer"
{
	echo "data-dir=$data_dir"
	echo "peer-id=$peer"
	echo "log=$log"
} >>"$GITHUB_OUTPUT"
