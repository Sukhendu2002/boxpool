# Website and documentation

The landing page and documentation are static files hosted by GitHub Pages at
[boxpool.run](https://boxpool.run). No server or browser framework is required.
Node.js 20.19+ and the build-only `markdown-it` dependency render the canonical
Markdown in `docs/` and `CONTRIBUTING.md`.

## Work locally

From the repository root:

```sh
npm ci --prefix site
make site
make test-site
python3 -m http.server 8765 --directory dist/site --bind 127.0.0.1
```

Open `http://localhost:8765`. Stop the server with Ctrl-C. Re-run `make site`
after edits; no watcher or production server is needed. The generated files
stay in ignored `dist/site/`; do not commit them or `node_modules/`.
`make check` remains independent of the website dependencies.

## Where to change things

- `index.html`: the landing page body. `build.mjs` supplies the shared shell.
- `styles.css`, `app.js`, `favicon.svg`: shared presentation and optional browser
  enhancements. All guides and install commands remain readable without JS.
- `../docs/*.md`: user and implementation guides. All Markdown under `docs/`
  is published; `docs/README.md` becomes `/docs/`. Nested validation pages retain
  their directory names. `CONTRIBUTING.md` becomes `/docs/contributing/`.
- `build.mjs`: Markdown rendering, shared layout, navigation groups, search
  index, link rewriting and distribution endpoint copying. Add important new
  guides to `groups` for the sidebar and next/previous navigation.
- `test/`: renderer and complete-output checks. These validate internal links
  and fragments, search entries, assets and distribution endpoints offline.

Use relative Markdown links in guides so they also work on GitHub. Links to
published guides become website routes; other repository files and directories
link to their sources on GitHub. Raw HTML in Markdown is escaped. Code fences
and tables are supported. Search downloads a static index and matches locally;
queries are never sent to a search service.

The docs follow `main`, not a versioned release. Mark unreleased commands and
include a source-build path until a release ships them. Keep dated benchmarks
clearly identified as historical evidence.

## Deployment contracts

The Pages workflow builds and tests on PRs; only `main` deploys. Changes to
Markdown docs, contribution guidance, the installer or site inputs rebuild it.

- `/install` is a byte-for-byte copy of `scripts/install.sh`.
- `/` and `/boxpool/` retain `go-import` metadata for `boxpool.run/boxpool`.
- `.nojekyll` keeps the output as generated.
- `/docs/search/` uses `/search-index.json`; copy and tabs are progressive
  enhancements. Search includes guides and dated validation, with their titles.

Before opening a PR, inspect narrow and wide layouts and check keyboard focus,
tab arrows, copying, search, sidebar navigation and a no-JavaScript page.
Do not replace the installer or Go discovery paths with application routes.
