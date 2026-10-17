// Keyboard shortcuts and the file finder (ADR-0018, W3). Pages work
// without this script; it only adds shortcuts.
"use strict";

document.addEventListener("keydown", (e) => {
  if (e.ctrlKey || e.metaKey || e.altKey || e.defaultPrevented) return;
  if (e.target.closest("input, textarea, select, [contenteditable]")) return;
  const d = document.body.dataset;
  switch (e.key) {
    case "t":
      if (d.finder) {
        e.preventDefault();
        location.href = d.finder;
      }
      break;
    case "y":
      if (d.permalink) {
        e.preventDefault();
        history.replaceState(null, "", d.permalink + location.search + location.hash);
        delete d.permalink;
      }
      break;
    case "?": {
      const dlg = document.getElementById("keys");
      if (dlg && !dlg.open) dlg.showModal();
      break;
    }
  }
});

// The file finder: a fuzzy match of the typed characters, in order, over
// every path, best matches first.
(() => {
  const data = document.getElementById("finder-data");
  const input = document.getElementById("finder-input");
  const results = document.getElementById("finder-results");
  if (!data || !input || !results) return;
  const paths = JSON.parse(data.textContent);
  const base = data.dataset.base;
  const shown = 100;
  let sel = 0;

  // score returns a match score (higher is better), or -1 for no match:
  // matches at the start of a name and in a row count more, and shorter
  // paths win ties.
  const score = (path, q) => {
    const p = path.toLowerCase();
    let s = 0, i = 0, prev = -2;
    for (const c of q) {
      const j = p.indexOf(c, i);
      if (j < 0) return -1;
      if (j === prev + 1) s += 3;
      if (j === 0 || p[j - 1] === "/" || p[j - 1] === "." || p[j - 1] === "_" || p[j - 1] === "-") s += 2;
      prev = j;
      i = j + 1;
    }
    if (p.slice(p.lastIndexOf("/") + 1).includes(q)) s += 10;
    return s - path.length / 1000;
  };

  const href = (p) => base + "/" + p.split("/").map(encodeURIComponent).join("/");

  const render = () => {
    const q = input.value.trim().toLowerCase();
    let list = paths;
    if (q) {
      list = paths
        .map((p) => [p, score(p, q)])
        .filter(([, s]) => s >= 0)
        .sort((a, b) => b[1] - a[1])
        .map(([p]) => p);
    }
    sel = Math.min(sel, Math.max(0, Math.min(list.length, shown) - 1));
    results.replaceChildren(
      ...list.slice(0, shown).map((p, i) => {
        const li = document.createElement("li");
        const a = document.createElement("a");
        a.href = href(p);
        a.textContent = p;
        if (i === sel) li.className = "sel";
        li.append(a);
        return li;
      }),
    );
    if (list.length === 0) {
      const li = document.createElement("li");
      li.className = "muted";
      li.textContent = "No matching files.";
      results.append(li);
    }
  };

  input.addEventListener("input", () => {
    sel = 0;
    render();
  });
  input.addEventListener("keydown", (e) => {
    const n = results.querySelectorAll("li a").length;
    if (e.key === "ArrowDown" && n) {
      e.preventDefault();
      sel = (sel + 1) % n;
      render();
    } else if (e.key === "ArrowUp" && n) {
      e.preventDefault();
      sel = (sel - 1 + n) % n;
      render();
    } else if (e.key === "Enter") {
      const a = results.querySelector("li.sel a");
      if (a) location.href = a.href;
    } else if (e.key === "Escape") {
      history.back();
    }
  });
  render();
})();
