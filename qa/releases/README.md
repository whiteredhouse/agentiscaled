# Native release acceptance

The native tests consume published executables without rebuilding them. Run them
only on disposable CI machines. They check anonymous downloads, checksums, exact
source/tag/workflow provenance and source notices, then actual Homebrew and Linux
deb/rpm installation and a real enrolled service upgrade through systemd/launchd.
Kernel PID/start and loaded-image checks, enrolled identity preservation and
service/package cleanup are assertions in the tests, not inferred from job status.
Only the loopback companion mesh is compiled from immutable release source.

The lifecycle also kills each enrolled release through its native supervisor and
requires automatic recovery with a fresh kernel PID/start, the expected loaded
executable and the same connected peer. It never issues a start/restart command
to cause that recovery. Repeated `up` updates a saved hostname without another
login/enrollment. A second data directory joins through a code file, runs its own
actual user service alongside the first node, reports live status, and survives
`down`/`up` with its identity/settings intact after the code file is removed.
Fixture request counters reject hidden login, enrollment or code reuse. The
loopback control fixture is not proof of deployed account authorization.

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
authentication, OS login/reboot/logout, signing/notarization or signed
package repositories. Those are separate capabilities.
