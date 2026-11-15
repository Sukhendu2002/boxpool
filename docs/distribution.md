# Distribution

## Release a version

Open **Actions → Release → Run workflow**, select **main**, enter a version
such as `0.2.0` or `0.2.0-beta.1`, and run it. That is the only human action
for an ordinary release. There is no additional approval or separate npm or
Homebrew command. The equivalent from a maintainer terminal is:

```sh
gh workflow run release.yml --repo Sukhendu2002/boxpool --ref main -f version=0.2.0
```

The version examples are illustrative, not a request to publish them.
An optional `v` prefix is accepted. Prereleases use `alpha.N`, `beta.N` or
`rc.N`; image versions and arbitrary suffixes are rejected. Release must be
run from `main`. Its starting commit is fixed for the entire run, even if
another PR merges while builds are running. Merges, CLI tag pushes and image
releases do **not** publish a CLI version.

The workflow automatically:

1. Validates the version, existing tag/source binding, current channels and
   configured npm/Homebrew destinations.
2. Runs `make check` on Linux and macOS, plus the existing KVM and Docker
   integration workflows. Builds static Linux amd64/arm64 binaries and an
   ad-hoc signed Apple silicon binary with the macOS 26 SDK.
3. Stages a pinned npm archive, a Homebrew formula and `release-manifest.json`
   containing the source commit, guest-image version and SHA-256 of each
   distribution asset. Tests the packaged launcher and lints the formula.
4. Creates the version tag and a draft GitHub release. Uploads the exact tested
   assets, publishes the complete release, then publishes npm and updates the
   appropriate Homebrew formula using the existing credentials.
5. Installs the public version with the shell installer and npm on Linux
   amd64/arm64 and macOS arm64. On Mac, installs/tests the public Homebrew
   formula and verifies the installed signature/virtualization entitlement.
6. Marks a stable GitHub release as Latest only after those installation
   checks pass, and reports all destinations in the workflow summary.

| Selection | GitHub | npm | Homebrew |
| --- | --- | --- | --- |
| `0.2.0` | Release, then Latest after verification | `boxpool@latest` | `sukhendu2002/tap/boxpool` |
| `0.2.0-beta.1` | Prerelease, never Latest | `boxpool@next` | `sukhendu2002/tap/boxpool-beta` |

The beta formula is keg-only: it can coexist with the default install without
replacing the `boxpool` on PATH. Use
`"$(brew --prefix sukhendu2002/tap/boxpool-beta)/bin/boxpool" version` to run it.
A pinned beta shell install is also possible with `BOXPOOL_VERSION` as described
in [installation](installation.md#shell-installer). These channels are available
after their first corresponding release through this workflow.

Publishing is serialized. Channel version checks also prevent an older queued
or retried release from replacing a newer one. The GitHub release, npm package
and Homebrew updates are separate service writes, so publication is not atomic;
a failed run can have completed only some of them.

## Resume a failed release

Use **Re-run failed jobs** on the original Actions run. Build/package artifacts
are retained for 30 days. The retry uses the same source commit and retained
bundle, rather than rebuilding or asking for credentials interactively.

Existing GitHub assets must match byte-for-byte. Existing npm packages must
match the archive's SHA-512 integrity. An identical tap formula is a no-op.
Mismatched content or source commits fail closed; the workflow never overwrites
release assets or republishes an npm version. Restore a missing npm channel tag
with the configured OIDC dist-tag permission when necessary. npm can accept a
publish before its package and channel are visible; verification allows a
five-minute polling budget for processing. Conflicting bytes or a newer
channel fail immediately.

Avoid **Re-run all jobs** after any publication: rebuilding with newer runner
inputs can produce different bytes, which the immutable-content checks reject.
After artifact retention expires or a conflicting version was published, choose
a new version. Do not move an existing release tag to newer code.

A beta and a stable version are distinct packages/binaries. If `main` has not
moved since a tested beta, selecting the stable version builds/tests that same
source with the stable version embedded. Otherwise the run tests the newer
starting commit on `main`. Changing the
version label on a beta binary is not a stable release.

## Validation boundaries

Release checks run without interactive human gates. Hosted Macs can verify
builds, signatures and installs, but cannot boot VZ guests on their hardware.
Real-Mac VM tests and unfamiliar-user onboarding remain separate validation;
a green release pipeline does not establish those milestone gates. The workflow
does not upgrade a running daemon, restart workspaces or migrate user state.
Mac binaries remain ad-hoc signed, not notarized.

Guest images publish independently using `image-vX.Y.Z` tags. CLI releases
reuse their compiled image/digest pins and never rebuild guest images. Follow
the [image release guide](../image/README.md#ci-and-release) and update pins in a
normal PR after a new image is tested. Existing image URLs remain valid.

The sections below describe one-time infrastructure setup and additional
clean-host validation; these are not steps to repeat for every release.

## Domain and Go imports

1. Acquire `boxpool.run`, verify it for GitHub Pages, then enable Pages
   with **GitHub Actions** as the source in this repository's settings.
2. Set **Custom domain** to `boxpool.run` in Pages settings before adding
   DNS. Point the apex ALIAS/ANAME to `sukhendu2002.github.io`, or use the
   A records from [GitHub's current domain instructions](https://docs.github.com/en/pages/configuring-a-custom-domain-for-your-github-pages-site/managing-a-custom-domain-for-your-github-pages-site).
   Enable **Enforce HTTPS** after certificate provisioning.
3. Run the [Distribution site workflow](../.github/workflows/pages.yml) on
   `main`. It copies `scripts/install.sh` verbatim to `/install`, alongside
   the landing page and Markdown documentation built by `site/build.mjs`.
   Node.js is needed at build time; reading guides needs no JavaScript. See
   the [site contributor guide](../site/README.md) for local builds.
4. Verify the deployed endpoints:

   ```sh
   curl -fsSL https://boxpool.run/install -o /tmp/boxpool-install.sh
   cmp scripts/install.sh /tmp/boxpool-install.sh
   curl -fsSL 'https://boxpool.run/boxpool?go-get=1'
   ```

   The Go response must contain
   `boxpool.run/boxpool git https://github.com/Sukhendu2002/boxpool` in a
   `go-import` meta tag. The same tag is on the root page for discovery.
   After tagging, verify from a fresh directory with
   `GOPROXY=direct go list -m boxpool.run/boxpool@vX.Y.Z`.

## npm bootstrap and releases

The package has no runtime dependencies, postinstall, or other install
hooks. First execution downloads the binary from GitHub, bounded to 128 MiB
and three minutes; subsequent executions work offline with a verified
cache. Package versions and binary release tags always match. SHA-256
digests are embedded at packaging time; a corrupted cached binary must be
replaced by a verified download before it can run.

1. After a tagged release succeeds, download its npm archive:

   ```sh
   gh release download vX.Y.Z --repo Sukhendu2002/boxpool --pattern 'boxpool-*.tgz'
   npm publish ./boxpool-X.Y.Z.tgz --access public --ignore-scripts
   ```

   Use `--tag next` for a prerelease, so it does not become `latest`. This
   first publish requires the maintainer's npm account and package-name
   ownership. The workflow never requests or stores an npm token.
2. In the npm package's settings, configure a trusted GitHub publisher:
   user `Sukhendu2002`, repository `boxpool`, workflow `release.yml`, no
   environment. Allow direct `npm publish`, and allow `npm dist-tag` for
   recovery of a missing channel pointer. See
   [npm trusted publishing](https://docs.npmjs.com/trusted-publishers/).
   The publishing job uses Node 24, pinned npm 11.21.0 and OIDC provenance.
3. Set repository Actions variable `NPM_PUBLISH=true`. The manual Release
   workflow requires this and the Homebrew configuration before proceeding.
   It publishes stable versions to `latest` and prereleases to `next`; retries
   verify already-published package integrity instead of publishing twice.

If npm recognizes the release tarball but returns `E404` for
`PUT https://registry.npmjs.org/boxpool`, confirm the package exists and
check its trusted publisher in the maintainer's npm account. Match the
case-sensitive user, repository and workflow fields above, leave the
environment empty, and allow direct publishing. An unvalidated publisher
expires after two days; replace an expired configuration and validate it
with a successful publish. See [npm's troubleshooting and expiry guidance](https://docs.npmjs.com/trusted-publishers/).
Fix registry authorization, then re-run failed jobs on the original Release
run. Existing matching GitHub assets are verified instead of overwritten.

`BOXPOOL_CACHE_DIR` overrides the launcher's default
`$XDG_CACHE_HOME/boxpool` or `~/.cache/boxpool`. Cache entries are separated
by version, OS and architecture. Installation itself works with npm's
`--ignore-scripts`. Runtime invocation still downloads on a cold cache.
Linux daemon setup copies the native binary with `boxpool up`; for root
commands, use its installed `/usr/local/bin/boxpool`. Do not use
`sudo npx` or change daemon socket permissions.

## Homebrew tap bootstrap

1. Create the public repository `Sukhendu2002/homebrew-tap` with an initial
   `main` commit. Homebrew expands `sukhendu2002/tap` to this repository.
2. Create `HOMEBREW_TAP_TOKEN` as an Actions secret in `boxpool`, using a
   fine-grained token limited to **Contents: read/write** on that tap only.
   `GITHUB_TOKEN` cannot push to a different repository.
3. Set Actions variable `HOMEBREW_PUBLISH=true`. A stable Release run copies
   the generated `boxpool.rb` into `Formula/boxpool.rb` and commits it.
   A prerelease updates `Formula/boxpool-beta.rb` without changing the stable
   formula. To bootstrap from an existing
   stable release, copy that release's `boxpool.rb` into the tap manually.

The formula downloads raw release binaries with platform-specific SHA-256
digests. It requires Apple silicon and macOS 14+ on Macs. It preserves the
signed bytes and does not install or start a service; `boxpool up` manages
the systemd unit or launch agent. Homebrew's test checks the CLI version.

## Validation before public beta

Automated checks run on Linux and macOS: ShellCheck, offline shell install
tests, Node launcher tests, and package/formula generation tests. They
exercise tampering, invalid manifests, interrupted downloads, cache
corruption, concurrent first runs, argument/stdio/exit-code preservation
and signal forwarding. The release builds also validate macOS signatures,
run the packaged launcher against the built Linux binary, and lint the
generated formula with Homebrew.

After domain/registry/tap setup, use clean Ubuntu amd64 and arm64 hosts
and a clean Apple silicon Mac (macOS 14+, macOS 26+ for VM networking):

1. Run `curl -fsSL https://boxpool.run/install | sh`. Add the printed
   directory to `PATH` if needed. `boxpool version` must match the latest
   stable tag; `boxpool doctor` must execute and report prerequisites.
2. Run `brew install sukhendu2002/tap/boxpool && brew test boxpool`.
   `boxpool doctor` must execute. On macOS, also verify the installed
   binary with `codesign --verify --strict "$(brew --prefix)/bin/boxpool"`
   and check its virtualization entitlement as in the release guide.
3. Run `npx --yes --ignore-scripts boxpool@X.Y.Z doctor`.
   `npx --yes boxpool@X.Y.Z version` must match `vX.Y.Z` even if a newer
   release exists. Repeat with networking disabled to check the warm cache.
4. Install host prerequisites, then run `sudo boxpool up` and
   `sudo /usr/local/bin/boxpool sh` on Linux, or `boxpool up` and
   `boxpool sh` on macOS. Expect a working sandbox shell. Run `boxpool down`
   (with sudo on Linux) after testing. Doctor may fail on a clean host
   before prerequisites are installed; this is an expected host check,
   not an installer failure.

The installer compares the binary against the checksum manifest fetched
over HTTPS from the same GitHub release. npm embeds that digest and
Homebrew pins it in the tap. These checks detect binary corruption or
tampering, but do not provide an independent publisher signature if the
release account itself is compromised. Keep macOS Gatekeeper enabled;
follow the [per-file signing guidance](development.md#releases) if blocked.
