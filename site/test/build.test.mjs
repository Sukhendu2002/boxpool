import { test, before } from "node:test";
import assert from "node:assert/strict";
import { readFile, readdir, stat } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import {
  build,
  output,
  renderMarkdown,
  resolveLink,
  route,
} from "../build.mjs";

const root = fileURLToPath(new URL("../../", import.meta.url));
const sources = new Set([
  "docs/README.md",
  "docs/guide.md",
  "docs/first-workspace.md",
  "docs/benchmarks/m1-boot.md",
  "CONTRIBUTING.md",
]);
const decode = (text) =>
  text.replace(
    /&(?:amp|quot|lt|gt|#39);/g,
    (c) =>
      ({ "&amp;": "&", "&quot;": '"', "&lt;": "<", "&gt;": ">", "&#39;": "'" })[
        c
      ],
  );
async function walk(dir) {
  const children = await readdir(dir, { withFileTypes: true });
  return (
    await Promise.all(
      children.map((e) =>
        e.isDirectory()
          ? walk(path.join(dir, e.name))
          : [path.join(dir, e.name)],
      ),
    )
  ).flat();
}
before(build);

test("Markdown preserves fenced commands, tables and safe HTML while assigning unique heading anchors", () => {
  const md =
    "# A guide\n\n## My `workspace`!\n\n## My workspace\n\n## My workspace-1\n\n| Host | Type |\n| --- | --- |\n| Mac | VM |\n\n```sh\nprintf '<hello>\\n'\n```\n\n<script>alert(1)</script>\n\n[bad](javascript:alert(1))";
  const rendered = renderMarkdown(md, "docs/guide.md", sources);
  assert.deepEqual(
    rendered.headings.map((h) => h.id),
    ["a-guide", "my-workspace", "my-workspace-1", "my-workspace-1-1"],
  );
  assert.match(rendered.html, /<table>/);
  assert.match(rendered.html, /printf '&lt;hello&gt;\\n'/);
  assert.match(rendered.html, /data-copy hidden/);
  assert.ok(rendered.text.includes("printf '<hello>\\n'"));
  assert.doesNotMatch(rendered.html, /<script>|href="javascript:/);
});

test("relative links work from nested docs and distinguish rendered guides from repository files", () => {
  assert.equal(
    resolveLink("Makefile", "CONTRIBUTING.md", sources),
    "https://github.com/Sukhendu2002/boxpool/blob/main/Makefile",
  );
  assert.equal(
    resolveLink("../guide.md#snapshots", "docs/benchmarks/m1-boot.md", sources),
    "/docs/guide/#snapshots",
  );
  assert.equal(
    resolveLink("../README.md", "docs/benchmarks/m1-boot.md", sources),
    "/docs/",
  );
  assert.equal(
    resolveLink("../CONTRIBUTING.md", "docs/guide.md", sources),
    "/docs/contributing/",
  );
  assert.equal(
    resolveLink("../scripts/dev-sign.sh", "docs/guide.md", sources),
    "https://github.com/Sukhendu2002/boxpool/blob/main/scripts/dev-sign.sh",
  );
  assert.equal(
    resolveLink("../internal/", "docs/guide.md", sources),
    "https://github.com/Sukhendu2002/boxpool/tree/main/internal/",
  );
  assert.equal(
    resolveLink("https://example.org/docs?q=x#y", "docs/guide.md", sources),
    "https://example.org/docs?q=x#y",
  );
  const result = renderMarkdown(
    "# Files\n\n[Start](first-workspace.md#open-a-shell)",
    "docs/guide.md",
    sources,
  );
  assert.match(result.html, /href="\/docs\/first-workspace\/#open-a-shell"/);
});

test("every generated local link, asset and fragment resolves; every page has unique IDs", async () => {
  const files = await walk(output),
    htmlFiles = files.filter((f) => f.endsWith(".html"));
  assert.ok(htmlFiles.length >= 30);
  const pages = new Map(
    await Promise.all(
      htmlFiles.map(async (file) => [file, await readFile(file, "utf8")]),
    ),
  );
  for (const [file, html] of pages) {
    const ids = [...html.matchAll(/\bid="([^"]+)"/g)].map((m) => decode(m[1]));
    assert.equal(ids.length, new Set(ids).size, `duplicate ID in ${file}`);
    for (const [, encoded] of html.matchAll(/\b(?:href|src)="([^"]+)"/g)) {
      const href = decode(encoded);
      if (/^(?:[a-z][a-z\d+.-]*:|\/\/)/i.test(href)) continue;
      const url = new URL(
        href,
        `https://local.test/${path.relative(output, file)}`,
      );
      let target = path.join(output, decodeURIComponent(url.pathname));
      if (url.pathname.endsWith("/")) target = path.join(target, "index.html");
      assert.ok(
        (await stat(target).catch(() => null))?.isFile(),
        `missing ${href} in ${path.relative(output, file)}`,
      );
      if (url.hash) {
        const content = pages.get(target) || (await readFile(target, "utf8"));
        const targetIds = [...content.matchAll(/\bid="([^"]+)"/g)].map((m) =>
          decode(m[1]),
        );
        assert.ok(
          targetIds.includes(decodeURIComponent(url.hash.slice(1))),
          `missing fragment ${href} in ${path.relative(output, file)}`,
        );
      }
    }
  }
});

test("search index covers each canonical Markdown page with a real route and readable text", async () => {
  const index = JSON.parse(
    await readFile(path.join(output, "search-index.json"), "utf8"),
  );
  const docs = (await walk(path.join(root, "docs"))).filter((f) =>
    f.endsWith(".md"),
  );
  assert.equal(index.length, docs.length + 1);
  assert.equal(new Set(index.map((p) => p.url)).size, index.length);
  for (const page of index) {
    assert.ok(page.title && page.text.length > page.title.length);
    assert.ok(page.url.startsWith("/docs/"));
    assert.ok((await stat(path.join(output, page.url, "index.html"))).isFile());
  }
  assert.ok(
    index.some(
      (p) =>
        p.url === route("docs/first-workspace.md") &&
        p.text.includes("root@hello"),
    ),
  );
});

test("installer, Go discovery and static deployment contracts survive the site build", async () => {
  assert.deepEqual(
    await readFile(path.join(output, "install")),
    await readFile(path.join(root, "scripts/install.sh")),
  );
  for (const name of ["index.html", "boxpool/index.html"]) {
    assert.match(
      await readFile(path.join(output, name), "utf8"),
      /<meta name="go-import" content="boxpool\.run\/boxpool git https:\/\/github.com\/Sukhendu2002\/boxpool">/,
    );
  }
  assert.ok((await stat(path.join(output, ".nojekyll"))).isFile());
  assert.equal(
    (await walk(output)).some((f) =>
      /node_modules|package-lock|\.mjs$/.test(f),
    ),
    false,
  );
});
