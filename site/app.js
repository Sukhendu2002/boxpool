// Progressive enhancement: all guides, links and code remain readable without JS.
for (const button of document.querySelectorAll("[data-copy]")) {
  if (!navigator.clipboard?.writeText) continue;
  button.hidden = false;
  button.addEventListener("click", async () => {
    const code = button.parentElement.querySelector("code");
    const status = document.querySelector("#copy-status");
    try {
      await navigator.clipboard.writeText(code.textContent);
      button.textContent = "Copied";
      status.textContent = "Code copied to clipboard.";
    } catch {
      button.textContent = "Select code";
      status.textContent =
        "Could not copy. Select the code and copy it manually.";
      const range = document.createRange();
      range.selectNodeContents(code);
      window.getSelection().removeAllRanges();
      window.getSelection().addRange(range);
    }
    setTimeout(() => {
      button.textContent = "Copy";
    }, 2200);
  });
}
for (const tablist of document.querySelectorAll("[data-tabs]")) {
  const tabs = [...tablist.querySelectorAll("[data-tab]")];
  tablist.setAttribute("role", "tablist");
  const select = (selected) => {
    for (const tab of tabs) {
      const active = tab === selected;
      tab.setAttribute("aria-selected", String(active));
      tab.tabIndex = active ? 0 : -1;
      const panel = document.getElementById(tab.getAttribute("aria-controls"));
      panel.hidden = !active;
    }
  };
  tabs.forEach((tab, index) => {
    tab.id = `tab-${tab.dataset.tab}`;
    tab.setAttribute("role", "tab");
    tab.setAttribute("aria-controls", `install-${tab.dataset.tab}`);
    const panel = document.getElementById(`install-${tab.dataset.tab}`);
    panel.setAttribute("role", "tabpanel");
    panel.setAttribute("aria-labelledby", tab.id);
    tab.addEventListener("click", () => select(tab));
    tab.addEventListener("keydown", (event) => {
      let next;
      if (event.key === "ArrowRight") next = tabs[(index + 1) % tabs.length];
      if (event.key === "ArrowLeft")
        next = tabs[(index + tabs.length - 1) % tabs.length];
      if (event.key === "Home") next = tabs[0];
      if (event.key === "End") next = tabs.at(-1);
      if (next) {
        event.preventDefault();
        select(next);
        next.focus();
      }
    });
  });
  select(tabs[0]);
}
const mobile = matchMedia("(max-width: 700px)");
const menu = document.querySelector(".docs-menu");
if (menu) {
  const adapt = () => {
    menu.open = !mobile.matches;
  };
  adapt();
  mobile.addEventListener("change", adapt);
}
const input = document.querySelector("#docs-query");
if (input) {
  const status = document.querySelector("#search-status");
  const results = document.querySelector("#search-results");
  let indexPromise,
    revision = 0;
  const loadIndex = () =>
    (indexPromise ||= fetch("/search-index.json")
      .then((response) => {
        if (!response.ok) throw new Error("Search index unavailable");
        return response.json();
      })
      .catch((error) => {
        indexPromise = undefined;
        throw error;
      }));
  const search = async () => {
    const current = ++revision;
    const query = input.value.trim().slice(0, 120);
    const url = new URL(location.href);
    if (query) url.searchParams.set("q", query);
    else url.searchParams.delete("q");
    history.replaceState(null, "", url);
    results.replaceChildren();
    if (!query) {
      status.textContent =
        "Try a command, a feature or a question. Search stays on this device.";
      return;
    }
    status.textContent = "Searching…";
    try {
      const index = await loadIndex();
      if (current !== revision) return;
      const words = query.toLowerCase().split(/\s+/);
      const score = (page) =>
        words.reduce(
          (total, word) =>
            total +
            (page.title.toLowerCase().includes(word) ? 10 : 0) +
            (page.headings.some((h) => h.toLowerCase().includes(word)) ? 3 : 0),
          0,
        );
      const matches = index
        .filter((page) =>
          words.every((word) =>
            `${page.title} ${page.text}`.toLowerCase().includes(word),
          ),
        )
        .sort(
          (a, b) =>
            Number(a.historical) - Number(b.historical) || score(b) - score(a),
        );
      status.textContent = matches.length
        ? `${matches.length} matching ${matches.length === 1 ? "page" : "pages"}. Showing up to 30.`
        : "No matches. Try fewer words, or browse the documentation index.";
      for (const page of matches.slice(0, 30)) {
        const item = document.createElement("li"),
          link = document.createElement("a"),
          excerpt = document.createElement("p");
        link.href = page.url;
        link.textContent = page.title;
        const position = page.text.toLowerCase().indexOf(words[0]);
        const start = Math.max(0, position - 60);
        excerpt.textContent = `${start ? "…" : ""}${page.text.slice(start, start + 220)}…`;
        item.append(link, excerpt);
        if (page.historical) {
          const label = document.createElement("small");
          label.textContent =
            "Validation record — read its date, version and status.";
          item.append(label);
        }
        results.append(item);
      }
    } catch {
      if (current === revision)
        status.textContent =
          "Search could not load. Try again or browse the documentation index.";
    }
  };
  input.value =
    new URLSearchParams(location.search).get("q")?.slice(0, 120) || "";
  let debounce;
  input.addEventListener("input", () => {
    revision++;
    clearTimeout(debounce);
    debounce = setTimeout(search, 180);
  });
  input.form.addEventListener("submit", (event) => {
    event.preventDefault();
    clearTimeout(debounce);
    search();
  });
  search();
}
document.addEventListener("keydown", (event) => {
  if (
    event.key !== "/" ||
    event.ctrlKey ||
    event.metaKey ||
    event.altKey ||
    event.target.closest("input,textarea,select,[contenteditable]")
  )
    return;
  event.preventDefault();
  if (input) input.focus();
  else location.assign("/docs/search/");
});
