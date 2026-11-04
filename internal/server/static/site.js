// Shared behaviour for every page: theme toggle, mobile menu, copy buttons.
(function () {
  const root = document.documentElement;
  const KEY = "recura-theme";
  let t = null; try { t = localStorage.getItem(KEY); } catch (e) {}
  root.dataset.theme = t === "dark" ? "dark" : "light"; // light by default; dark only if the visitor chose it

  window.recura = window.recura || {};
  const toastEl = () => {
    let t = document.querySelector(".toast");
    if (!t) { t = document.createElement("div"); t.className = "toast"; t.setAttribute("role", "status"); document.body.append(t); }
    return t;
  };
  let toastTimer;
  recura.toast = (msg) => {
    const t = toastEl(); t.textContent = msg; t.classList.add("on");
    clearTimeout(toastTimer); toastTimer = setTimeout(() => t.classList.remove("on"), 1800);
  };
  recura.copy = async (text, label) => {
    try { await navigator.clipboard.writeText(text); recura.toast(label || "Copied"); }
    catch (e) {
      const ta = document.createElement("textarea"); ta.value = text; document.body.append(ta); ta.select();
      try { document.execCommand("copy"); recura.toast(label || "Copied"); } catch (_) { recura.toast("Select the text to copy it"); }
      ta.remove();
    }
  };
  recura.fmtMs = (ms) => ms == null ? "–" : ms < 1 ? ms.toFixed(2) + " ms" : ms < 1000 ? (ms < 10 ? ms.toFixed(1) : Math.round(ms)) + " ms" : (ms / 1000).toFixed(2) + " s";

  document.addEventListener("DOMContentLoaded", () => {
    const tb = document.getElementById("theme");
    if (tb) {
      const dark = () => root.dataset.theme ? root.dataset.theme === "dark" : matchMedia("(prefers-color-scheme: dark)").matches;
      const paint = () => { tb.setAttribute("aria-label", dark() ? "Switch to light theme" : "Switch to dark theme"); tb.innerHTML = dark()
        ? '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/></svg>'
        : '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M20 14.5A8 8 0 0 1 9.5 4a8 8 0 1 0 10.5 10.5z"/></svg>'; };
      paint();
      tb.addEventListener("click", () => { root.dataset.theme = dark() ? "light" : "dark"; try { localStorage.setItem(KEY, root.dataset.theme); } catch (e) {} paint(); window.dispatchEvent(new Event("themechange")); });
    }
    const mb = document.getElementById("menu"), links = document.querySelector(".links");
    if (mb && links) {
      mb.addEventListener("click", () => { const o = links.classList.toggle("open"); mb.setAttribute("aria-expanded", o); });
      links.addEventListener("click", (e) => { if (e.target.closest("a")) { links.classList.remove("open"); mb.setAttribute("aria-expanded", "false"); } });
    }
    document.querySelectorAll("[data-copy]").forEach((b) => b.addEventListener("click", () => {
      const src = b.dataset.copy.startsWith("#") ? document.querySelector(b.dataset.copy).innerText : b.dataset.copy;
      recura.copy(src.trim(), b.dataset.copyLabel);
    }));
    fetch("/healthz").then((r) => r.json()).then((j) => {
      document.querySelectorAll("[data-version]").forEach((el) => el.textContent = j.version || "dev");
    }).catch(() => {});
  });
})();
