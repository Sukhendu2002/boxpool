# Distribution

The [release workflow](../.github/workflows/release.yml) builds the Linux
amd64/arm64 and ad-hoc signed macOS arm64 binaries. It generates a pinned
Homebrew formula and a dependency-free npm launcher from those exact
artifacts and their SHA-256 manifest. PRs build the same packages without
publishing. Packages and the formula are also attached to tag releases,
so registry setup does not block binary publication.

The public installer, npm package and Homebrew tap are available; see the
[user quickstart](../README.md). This page is the maintainer's setup and
release runbook. The bootstrap steps below describe provisioning for a
new registry/domain or replacement infrastructure, not user prerequisites.

## Domain and Go imports

1. Acquire `boxpool.run`, verify it for GitHub Pages, then enable Pages
   with **GitHub Actions** as the source in this repository's settings.
2. Set **Custom domain** to `boxpool.run` in Pages settings before adding
   DNS. Point the apex ALIAS/ANAME to `sukhendu2002.github.io`, or use the
   A records from [GitHub's current domain instructions](https://docs.github.com/en/pages/configuring-a-custom-domain-for-your-github-pages-site/managing-a-custom-domain-for-your-github-pages-site).
   Enable **Enforce HTTPS** after certificate provisioning.
3. Run the [Distribution site workflow](../.github/workflows/pages.yml) on
   `main`. It copies `scripts/install.sh` verbatim to `/install`, alongside
   the static pages in `site/`. No build framework or JavaScript is needed.
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
   environment. Allow direct `npm publish`. See
   [npm trusted publishing](https://docs.npmjs.com/trusted-publishers/).
   The workflow uses Node 24 with npm 11.5.1+ and OIDC provenance.
3. Set repository Actions variable `NPM_PUBLISH=true`. Future tag builds
   publish stable versions to `latest`, prereleases to `next`. Re-running
   publication of an existing version is not supported by npm; inspect
   partial releases and rerun only the failed jobs.

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
3. Set Actions variable `HOMEBREW_PUBLISH=true`. The next stable tag copies
   the generated `boxpool.rb` into `Formula/boxpool.rb` and commits it.
   Prereleases never update the stable tap. To bootstrap from an existing
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
