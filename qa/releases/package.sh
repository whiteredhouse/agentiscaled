#!/usr/bin/env bash
# Executed only inside disposable native Ubuntu/Fedora containers.
set -euo pipefail
kind=$1
arch=$2
shift 2
case "$arch:$(uname -m)" in
  amd64:x86_64) rpm_arch=x86_64 ;;
  arm64:aarch64) rpm_arch=aarch64 ;;
  *) echo 'Container architecture mismatch' >&2; exit 1 ;;
esac
if command -v go >/dev/null 2>&1; then
  echo 'Consumer container must not have a Go toolchain' >&2
  exit 1
fi
export HOME=/tmp/release-consumer-home
mkdir -p "$HOME/.config/agentiscale" "$HOME/.config/systemd/user"
printf 'opaque state fixture\n' > "$HOME/.config/agentiscale/state-sentinel"
printf 'settings fixture\n' > "$HOME/.config/agentiscale/agentiscaled.json"
printf 'service file fixture; never started\n' > "$HOME/.config/systemd/user/agentiscaled-fixture.service"
sha256sum "$HOME/.config/agentiscale/"* "$HOME/.config/systemd/user/"* > /tmp/sentinels.sha256
while [[ $# -gt 0 ]]; do
  version=$1
  commit=$2
  binary_sha=$3
  license_sha=$4
  notice_sha=$5
  shift 5
  case "$kind" in
    deb)
      # Ubuntu's minimal image excludes /usr/share/doc by default. Include
      # this package's documentation so we can validate its shipped bytes.
      dpkg --path-include=/usr/share/doc/agentiscaled \
        '--path-include=/usr/share/doc/agentiscaled/*' \
        -i "/payload/agentiscaled_${version}_${arch}.deb"
      dpkg-query -s agentiscaled
      ;;
    rpm)
      rpm -Uvh --includedocs "/payload/agentiscaled-${version}-1.${rpm_arch}.rpm"
      rpm -q agentiscaled
      ;;
    *) exit 1 ;;
  esac
  [[ "$(/usr/bin/agentiscaled version)" == "agentiscaled $version (commit $commit, linux/$arch)" ]]
  [[ "$(sha256sum /usr/bin/agentiscaled | cut -d ' ' -f 1)" == "$binary_sha" ]]
  [[ "$(sha256sum /usr/share/doc/agentiscaled/LICENSE | cut -d ' ' -f 1)" == "$license_sha" ]]
  [[ "$(sha256sum /usr/share/doc/agentiscaled/NOTICE | cut -d ' ' -f 1)" == "$notice_sha" ]]
  sha256sum -c /tmp/sentinels.sha256
done
