# Native release acceptance

The native tests consume published executables without rebuilding them. Run them
only on disposable CI machines. They check anonymous downloads, checksums, exact
source/tag/workflow provenance and source notices, then actual Homebrew and Linux
deb/rpm installation and a real enrolled service upgrade through systemd/launchd.
Kernel PID/start and loaded-image checks, enrolled identity preservation and
service/package cleanup are assertions in the tests, not inferred from job status.
Only the loopback companion mesh is compiled from immutable release source.

After upgrading, the suite sends SIGKILL to the isolated service through its
native manager and waits for automatic recovery without issuing start/restart.
It requires a fresh kernel PID/start, the verified release image, ready mesh and
the same peer identity. Published `status`, `down` and `up` then verify connected
and stopped status, actual service removal and identity-preserving resumption.
The original key/credential and saved options must survive, with exactly one
sign-in enrollment across the primary lifecycle. These checks run on all four native targets.

The same case reuses those verified downloads, upgraded installation and companion
mesh for a second node joined by code. Sign-in is locked before that node starts;
all enrollment is locked after its one accepted code request. The suite removes
the input code, resumes the real service, updates its log level through a running
`up`, and requires a fresh verified process with unchanged key/credential/peer and
all other options preserved. After the initial join and negative controls, counters
must show no further authentication or enrollment requests.
The primary service's PID/start, readiness, settings and unit must remain intact
through every secondary action. Both services are cleaned independently on failure.

The loopback control fixture rejects invalid/reused codes and mints one-use
peer-bound tokens through the real mesh admin API. Companion protocol tests run
before its build. This exercises the public mesh contract; it does not establish
production account ownership or deployed join-code policy. Raw codes must be absent
from both command output streams and node/HOME files; the caller-owned input and
private fixture descriptor stay outside those scanned directories.

Run the standard suite on a native runner with the declared target:

```sh
RELEASE_SYSTEM=linux RELEASE_ARCH=amd64 RELEASE_DISCOVER=false \
  .venv-evidence/bin/python -m pytest -c qa/releases/pytest.ini --rootdir=. \
  qa/releases --junitxml=reports/junit.xml --maxfail=1
```

The reviewed baseline in pins.json is mandatory. Discovery checks every adjacent
stable pair from it onward, including intermediate releases. Empty, duplicate,
missing-baseline and excessive plans fail. Each pair owns and cleans its state
and package installation, including on failure. Python/Go, package images and
operational Homebrew are pinned. Python tool distributions use a reviewed wheel
hash lock. The runner OS and vendor Homebrew bootstrap are not fully hermetic.

`pins.json` keeps the reviewed upgrade baseline and test environment unchanged.
The separately versioned `catalog.json` lists audited published releases; its
first two entries must exactly preserve that baseline. Both ordinary PR plans
and main discovery require every adjacent catalog pair with its exact reviewed
metadata. Discovery still includes later stable releases, but cannot omit an
audited intermediate release or silently promote a pending catalog declaration.
Each catalog pair also needs its explicit native case binding on all four
platform requirements, alongside the existing wildcard for discovered releases.

Add a catalog release only after checking its actual published source, checksum
manifest, provenance and Homebrew revision. Pending declarations contain only a
version and immutable source, and cannot be selected or tested as audited inputs.
The checked-in catalog contains audited v0.1.10, v0.1.11 and v0.1.12 releases.
Its ordinary plans retain both 0.1.10-to-0.1.11 and 0.1.11-to-0.1.12 upgrades.
Synthetic future-release unit fixtures remain separate from those published inputs.

Four push-only, single-job workflows produce Exacly0.6 evidence, one per native
platform. Their shared setup is .github/actions/release-setup. Each runs these
same tests and packages exactly manifest.json and junit.xml using the installed
`exacly evidence package` command. A public model binds one platform requirement
to each producer. Four independently reviewed trust policies and readiness gates
are needed to establish all-platform acceptance; one admitted result is not the
aggregate. Packaging alone does not authenticate origin or establish coverage.

PR, scheduled, release-completion and manual matrix runs remain ordinary test
runs. Their JUnit and JSON diagnostics aid review; Exacly0.6 does not admit those
multi-job/non-push workflows. Live admission requires a real protected-main push.
No synthetic event metadata substitutes for it. Main producers discover releases
available at that push; later publication is checked by the scheduled/release
matrix until the next admitted main push. Do not claim admission of newer releases
from an older implementation's report.

Captured JUnit output includes immutable inputs and concise phase diagnostics.
Original provenance is verified by the actual artifact checks; public GitHub
attestations remain its source. This suite does not test production account
authentication, OS login/logout/reboot behavior, signing/notarization or signed
package repositories. Those are separate capabilities.
