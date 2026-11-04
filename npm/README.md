# boxpool

Run `npx boxpool doctor` or install with `npm install -g boxpool`.
Requires Node.js 20+, Linux amd64/arm64 or Apple silicon macOS.

There are no dependencies or install hooks. On first execution the launcher
downloads the matching GitHub release binary, verifies the SHA-256 digest
embedded in this package, and caches it. Every run verifies the cached binary
before execution. `boxpool@X.Y.Z` always runs release `vX.Y.Z`; use
`npx boxpool@latest` to pick up a new package version. Set `BOXPOOL_CACHE_DIR`
to choose the cache location (default: `$XDG_CACHE_HOME/boxpool` or
`~/.cache/boxpool`). A valid cached binary works offline.

The launcher passes arguments, terminal input/output, signals and exit codes
to the binary. Install first, then run `sudo boxpool up` on Linux, or
`boxpool up` as your user on macOS. Prefer the shell installer or Homebrew
for Linux service setup; do not run `sudo npx` to download code as root.

See [installation and host prerequisites](https://boxpool.run) and the
[macOS signing notes](https://github.com/Sukhendu2002/boxpool/blob/main/docs/development.md#releases).
