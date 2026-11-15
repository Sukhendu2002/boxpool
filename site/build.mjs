// Static-only documentation build. Author content in docs/, not generated HTML.
import {
  readFile,
  writeFile,
  mkdir,
  rm,
  readdir,
  copyFile,
} from "node:fs/promises";
import path from "node:path";
import { statSync } from "node:fs";
import { fileURLToPath } from "node:url";
import MarkdownIt from "markdown-it";

const root = fileURLToPath(new URL("../", import.meta.url));
export const output = path.join(root, "dist/site");
export const repository = "https://github.com/Sukhendu2002/boxpool";
export const escape = (value) =>
  String(value).replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );
export const route = (source) =>
  source === "CONTRIBUTING.md"
    ? "/docs/contributing/"
    : source === "docs/README.md"
      ? "/docs/"
      : `/docs/${source.slice(5, -3)}/`;
export const groups = [
  [
    "Start here",
    [
      ["docs/getting-started.md", "Overview"],
      ["docs/installation.md", "Installation"],
      ["docs/first-workspace.md", "Your first workspace"],
      ["docs/local-agents.md", "Use your local agent"],
    ],
  ],
  [
    "Work with Boxpool",
    [
      ["docs/guide.md", "Workspaces & files"],
      ["docs/pool.md", "Add another machine"],
      ["docs/expose.md", "App previews"],
      ["docs/desktop.md", "Desktop takeover"],
    ],
  ],
  [
    "Reference",
    [
      ["docs/commands.md", "CLI commands"],
      ["docs/troubleshooting.md", "Troubleshooting"],
      ["docs/security.md", "Security & trust"],
    ],
  ],
  [
    "Contribute",
    [
      ["CONTRIBUTING.md", "Contributor guide"],
      ["docs/development.md", "Development"],
      ["docs/agent-protocol.md", "Guest protocol"],
      ["docs/distribution.md", "Distribution"],
      ["docs/README.md", "All guides & validation"],
    ],
  ],
];

export function slug(text) {
  return text
    .toLowerCase()
    .replace(/[^\p{L}\p{N}\p{M}\s_-]/gu, "")
    .replace(/\s/g, "-");
}

export function resolveLink(href, source, sources) {
  if (/^(?:[a-z][a-z\d+.-]*:|\/\/|\/|#)/i.test(href)) return href;
  const match = href.match(/^([^?#]*)([?#].*)?$/);
  const target = path.posix.normalize(
    path.posix.join(path.posix.dirname(source), decodeURIComponent(match[1])),
  );
  if (sources.has(target)) return route(target) + (match[2] || "");
  // Files and source directories outside the published guides stay on GitHub.
  const directory =
    statSync(path.join(root, target), {
      throwIfNoEntry: false,
    })?.isDirectory() || match[1].endsWith("/");
  return `${repository}/${directory ? "tree" : "blob"}/main/${target.split("/").map(encodeURIComponent).join("/")}${match[2] || ""}`;
}

export function renderMarkdown(markdown, source, sources) {
  const md = new MarkdownIt({ html: false, linkify: true });
  const tokens = md.parse(markdown, {});
  const headings = [],
    used = new Set();
  for (let i = 0; i < tokens.length; i++) {
    const token = tokens[i];
    if (token.type === "heading_open") {
      const inline = tokens[i + 1];
      const title = inline.children
        .filter((t) => t.type === "text" || t.type === "code_inline")
        .map((t) => t.content)
        .join("");
      const base = slug(title);
      let id = base,
        count = 0;
      while (used.has(id)) id = `${base}-${++count}`;
      used.add(id);
      token.attrSet("id", id);
      headings.push({ level: Number(token.tag.slice(1)), title, id });
    }
  }
  const rewrite = (list) => {
    for (const token of list) {
      for (const attr of ["href", "src"]) {
        const value = token.attrGet(attr);
        if (value) token.attrSet(attr, resolveLink(value, source, sources));
      }
      if (token.children) rewrite(token.children);
    }
  };
  rewrite(tokens);
  const fence = md.renderer.rules.fence;
  md.renderer.rules.fence = (...args) =>
    `<div class="code-block"><button class="copy-button" type="button" data-copy hidden aria-label="Copy code">Copy</button>${fence(...args)}</div>`;
  const text = tokens
    .filter((t) => ["inline", "fence", "code_block"].includes(t.type))
    .map((t) =>
      t.children
        ? t.children
            .map((child) =>
              ["softbreak", "hardbreak"].includes(child.type)
                ? " "
                : child.content,
            )
            .join("")
        : t.content,
    )
    .join(" ")
    .replace(/\s+/g, " ");
  return {
    html: md.renderer.render(tokens, md.options, {}),
    headings,
    text,
    title: headings[0]?.title || "Boxpool documentation",
  };
}

export const logo =
  '<svg width="25" height="25" viewBox="0 0 28 28" aria-hidden="true"><path d="m14 2 11 6v12l-11 6-11-6V8Z" fill="none" stroke="currentColor" stroke-width="2"/><path d="m3 8 11 6 11-6M14 14v12M8.5 5l11 6" fill="none" stroke="currentColor" stroke-width="2"/></svg>';
function shell({ title, description, body, docs = false, url = "/" }) {
  return `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="go-import" content="boxpool.run/boxpool git ${repository}">
<title>${escape(title === "Boxpool" ? "Boxpool — Your machines. One workspace pool." : `${title} · Boxpool`)}</title>
<meta name="description" content="${escape(description)}"><meta name="theme-color" content="#f7f8f2">
<link rel="canonical" href="https://boxpool.run${url}"><meta property="og:title" content="${escape(title)}"><meta property="og:description" content="${escape(description)}"><meta property="og:type" content="website"><meta property="og:url" content="https://boxpool.run${url}">
<link rel="icon" href="/favicon.svg" type="image/svg+xml"><link rel="stylesheet" href="/styles.css"><script src="/app.js" defer></script></head>
<body class="${docs ? "docs-page" : "home-page"}"><a class="skip-link" href="#main">Skip to content</a>
<header class="site-header"><div class="header-inner"><a class="brand" href="/" aria-label="Boxpool home">${logo}<span>boxpool</span></a><nav aria-label="Main navigation"><a href="/docs/getting-started/"${docs ? ' class="nav-active"' : ""}>Docs</a><a href="/docs/contributing/">Contribute</a><a class="search-link" href="/docs/search/">Search<span class="search-hint" aria-hidden="true"> / </span></a><a class="github-link" href="${repository}">GitHub <span aria-hidden="true">↗</span></a></nav></div></header>
${body}
<footer class="site-footer"><a class="brand" href="/">${logo}<span>boxpool</span></a><p>Your hardware. Your work.</p><div><a href="${repository}/releases">Releases</a><a href="${repository}/issues">Issues</a><a href="${repository}/blob/main/LICENSE">Apache 2.0</a></div></footer>
<div class="sr-only" role="status" id="copy-status"></div></body></html>`;
}
function sidebar(source) {
  return `<details class="docs-menu" open><summary>Documentation menu</summary><nav class="sidebar" aria-label="Documentation">${groups.map(([name, links]) => `<div class="nav-group"><p>${escape(name)}</p>${links.map(([file, name]) => `<a href="${route(file)}"${file === source ? ' aria-current="page"' : ""}>${escape(name)}</a>`).join("")}</div>`).join("")}</nav></details>`;
}
function docPage(source, rendered) {
  const sequence = groups.flatMap((g) => g[1]),
    index = sequence.findIndex((x) => x[0] === source);
  const neighbors = [
    sequence[index - 1],
    index < 0 ? null : sequence[index + 1],
  ];
  const toc = rendered.headings.filter((h) => h.level === 2 || h.level === 3);
  return shell({
    title: rendered.title,
    description: rendered.text.slice(0, 180),
    docs: true,
    url: route(source),
    body: `
<div class="docs-layout">${sidebar(source)}<main id="main" class="doc-content"><div class="doc-eyebrow">DOCUMENTATION <span>·</span> MAIN BRANCH</div><article>${rendered.html}</article>
<nav class="page-neighbors" aria-label="Adjacent guides">${neighbors.map((item, i) => (item ? `<a href="${route(item[0])}"><span>${i ? "Next →" : "← Previous"}</span>${escape(item[1])}</a>` : "<span></span>")).join("")}</nav><a class="source-link" href="${repository}/blob/main/${source}">Edit this page on GitHub ↗</a></main>
${toc.length ? `<aside class="on-this-page"><nav aria-label="On this page"><p>On this page</p>${toc.map((h) => `<a class="level-${h.level}" href="#${h.id}">${escape(h.title)}</a>`).join("")}</nav></aside>` : ""}</div>`,
  });
}
async function markdownFiles(dir) {
  const entries = await readdir(path.join(root, dir), { withFileTypes: true });
  const files = await Promise.all(
    entries.map((e) =>
      e.isDirectory()
        ? markdownFiles(`${dir}/${e.name}`)
        : e.name.endsWith(".md")
          ? [`${dir}/${e.name}`]
          : [],
    ),
  );
  return files.flat().sort();
}
export async function build() {
  const files = [...(await markdownFiles("docs")), "CONTRIBUTING.md"];
  const sources = new Set(files),
    index = [];
  // Output is fixed inside dist/. Never delete an arbitrary caller-supplied path.
  await rm(output, { recursive: true, force: true });
  await mkdir(output, { recursive: true });
  const write = async (name, content) => {
    const dest = path.join(output, name);
    await mkdir(path.dirname(dest), { recursive: true });
    await writeFile(dest, content);
  };
  for (const source of files) {
    const rendered = renderMarkdown(
      await readFile(path.join(root, source), "utf8"),
      source,
      sources,
    );
    await write(
      `${route(source).slice(1)}index.html`,
      docPage(source, rendered),
    );
    index.push({
      title: rendered.title,
      url: route(source),
      text: rendered.text,
      headings: rendered.headings.map((h) => h.title),
      historical:
        source.startsWith("docs/benchmarks/") ||
        source === "docs/onboarding-check.md",
    });
  }
  const home = await readFile(path.join(root, "site/index.html"), "utf8");
  await write(
    "index.html",
    shell({
      title: "Boxpool",
      description:
        "Run isolated Linux workspaces on your own Macs and Linux machines. Start on one machine. Add spare hardware when you need it.",
      body: home,
    }),
  );
  await write(
    "docs/search/index.html",
    shell({
      title: "Search documentation",
      description: "Search Boxpool guides and reference documentation.",
      docs: true,
      url: "/docs/search/",
      body: `<div class="docs-layout search-layout">${sidebar("")}<main id="main" class="doc-content"><div class="doc-eyebrow">DOCUMENTATION</div><h1>Find your next step.</h1><form role="search" action="/docs/search/"><label for="docs-query">Search guides and references</label><div class="search-field"><input id="docs-query" name="q" type="search" maxlength="120" placeholder="Try: install, snapshots, local agents" autocomplete="off"><button type="submit">Search</button></div></form><p class="search-status" role="status" id="search-status">Search the documentation on this device. No query is sent to a service.</p><noscript><p>Search needs JavaScript. Browse the <a href="/docs/">documentation index</a> instead.</p></noscript><ol class="search-results" id="search-results"></ol></main></div>`,
    }),
  );
  await write("search-index.json", JSON.stringify(index));
  await write(
    "404.html",
    shell({
      title: "Page not found",
      description: "Find your way back to Boxpool.",
      body: '<main id="main" class="not-found"><p class="eyebrow">404 · OFF THE PATH</p><h1>Let’s get you back.</h1><p>This page could not be found. Try the guides or search for a command.</p><a class="button" href="/docs/">Browse documentation →</a> <a class="text-link" href="/docs/search/">Search docs</a></main>',
    }),
  );
  for (const asset of [
    "styles.css",
    "app.js",
    "favicon.svg",
    "boxpool/index.html",
  ])
    await write(asset, await readFile(path.join(root, "site", asset)));
  await copyFile(
    path.join(root, "scripts/install.sh"),
    path.join(output, "install"),
  );
  await write(".nojekyll", "");
  console.log(
    `Built ${files.length} Markdown pages, landing page and search → dist/site`,
  );
}
if (
  process.argv[1] &&
  path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)
)
  await build();
