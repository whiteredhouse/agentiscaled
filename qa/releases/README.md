# Native release acceptance

This suite consumes published executables, without rebuilding them. Run it only
on disposable CI machines, never against a developer's applications or nodes.

The release-verification workflow requires native macOS arm64/amd64 and Linux
arm64/amd64 consumers. All release packages and manifests pass anonymous download,
SHA-256, notice and exact source/tag/workflow provenance checks before execution.
Native jobs install and upgrade the generated Homebrew casks; Linux also installs
and upgrades deb/rpm packages in digest-pinned containers with Go unavailable.

A separate lifecycle fixture uses controlled OAuth/control endpoints and a real
loopback mesh. Only the mesh companion is compiled, from the immutable public
source; published node executables are never rebuilt. It enrolls an old executable,
starts its actual OS service, restarts, upgrades and restarts again. Every start
must be ready, attached to the router and load the expected executable. Enrolled
key/credential/peer identity and configuration are preserved; inspection uses a
copy of the stopped database. Cleanup stops and removes the service. This checks
release upgrade; it does not simulate production account authentication or OS login,
reboot and crash recovery.

PRs replay the reviewed baseline in pins.json. The default branch discovers every
adjacent stable pair from that baseline onward on a six-hour schedule and after a
successful Release workflow. Missing provenance or tap publication fails clearly;
a later run can retry after publication finishes. History and matrix limits fail
rather than silently dropping a release. Actions, Python/Go, operational Homebrew,
its installer and package images are pinned. The vendor Homebrew bootstrap may
fetch current tooling before the operational checkout is pinned; the runner OS is
managed by GitHub. This is not a fully hermetic environment.

Each required platform publishes separate consumer/lifecycle reports. The proof
job requires every report, retains the original provenance records and creates
release-proof-<attempt> with verifier revision, run/attempt and immutable inputs.
All runs execute fresh; reports are retained for 14 days. The reviewed baseline
is available at the artifact root; every other verified pair is under pairs/.
A green result requires all four targets. Signing/notarization and signed package
repositories are separate capabilities, not claims made by this acceptance suite.
