# Claude Desktop on macOS

`.github/workflows/desktop.yml` runs `preflight.py` on a clean GitHub-hosted
macOS runner (Apple Silicon). It installs the real Claude Desktop app, launches
it and checks that it can be driven, as a base for testing `agentiscaled
client connect` against the real app.

The version, download URL and SHA-256 in `pins.json` come from the
[Homebrew cask](https://github.com/Homebrew/homebrew-cask/blob/044a7741849cf80d448b6fca992c062ed899fce1/Casks/c/claude.rb)
at a fixed commit. Before launch the script checks the package digest, the
bundle identity, the Apple code signature and Gatekeeper's assessment. After
launch it records the window, process and sign-in state.

`native_ui.m` is a small Accessibility controller the job compiles with the
runner's SDK. Before every UI action it checks that the target is still the
same app process. It proves two things and undoes both:

- native window control: minimize the app's window, read the change back,
  restore it;
- renderer control: move focus to another control inside the app's web view,
  read it back, restore the original focus.

It never presses a sign-in button, types, submits a form or navigates. The job
uses no credentials and does not sign in, so it never calls an agent from
Claude Desktop.

Only `preflight.json` is uploaded: fixed identities, statuses and failure
reasons. App profiles, cookies, keychains, screenshots, raw app logs and UI
text stay on the runner.

Run the unit tests anywhere with:

    python3 -I -m unittest discover -s qa/desktop -p 'test_*.py' -v

To move to a newer Claude Desktop, update `pins.json` from a newer cask commit.
