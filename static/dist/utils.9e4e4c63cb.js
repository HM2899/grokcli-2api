/* shared utils */
window.G2A = window.G2A || {};
(function (G2A) {
  "use strict";

  function $(id) {
    if (id == null || id === "") return null;
    try { return document.getElementById(String(id)); } catch (_) { return null; }
  }

  function $$(sel, root) {
    try { return Array.from((root || document).querySelectorAll(sel)); } catch (_) { return []; }
  }

  function toast(msg, ok) {
    if (ok === undefined) ok = true;
    const el = $("toast") || document.getElementById("toast");
    if (!el) {
      try { console.log(msg); } catch (_) {}
      return;
    }
    const body = $("toast-body");
    if (body) body.textContent = String(msg ?? "");
    else el.textContent = String(msg ?? "");
    el.className = "g2a-message show " + (ok ? "ok" : "err");
    clearTimeout(toast._t);
    toast._t = setTimeout(function () {
      el.classList.remove("show");
      el.className = "g2a-message";
    }, 3800);
  }

  function pad2(n) { return String(n).padStart(2, "0"); }

  function fmtTime(ts) {
    if (ts == null || ts === "") return "—";
    try {
      let ms = Number(ts);
      if (!Number.isFinite(ms)) return String(ts);
      // accept seconds or milliseconds
      if (ms < 1e12) ms = ms * 1000;
      const d = new Date(ms);
      if (Number.isNaN(d.getTime())) return String(ts);
      // Always display in Asia/Shanghai so usage / logs match 上海日切.
      try {
        const parts = new Intl.DateTimeFormat("en-CA", {
          timeZone: "Asia/Shanghai",
          year: "numeric",
          month: "2-digit",
          day: "2-digit",
          hour: "2-digit",
          minute: "2-digit",
          hour12: false,
        }).formatToParts(d);
        const get = (t) => (parts.find((p) => p.type === t) || {}).value || "";
        return get("year") + "-" + get("month") + "-" + get("day") + " " + get("hour") + ":" + get("minute");
      } catch (_) {
        // Fallback: browser local (usually already Shanghai for CN hosts).
        return (
          d.getFullYear() + "-" + pad2(d.getMonth() + 1) + "-" + pad2(d.getDate()) +
          " " + pad2(d.getHours()) + ":" + pad2(d.getMinutes())
        );
      }
    } catch (e) {
      return String(ts);
    }
  }

  function fmtRemaining(ts) {
    if (ts == null || ts === "") return "—";
    let exp = Number(ts);
    if (!Number.isFinite(exp)) return "—";
    if (exp > 1e12) exp = exp / 1000; // ms -> sec
    const sec = Math.floor(exp - Date.now() / 1000);
    if (Number.isNaN(sec)) return "—";
    if (sec <= 0) return "已过期";
    const d = Math.floor(sec / 86400);
    const h = Math.floor((sec % 86400) / 3600);
    const m = Math.floor((sec % 3600) / 60);
    if (d >= 2) return d + "天" + h + "小时";
    if (d === 1) return "1天" + h + "小时";
    if (h > 0) return h + "小时" + m + "分";
    if (m > 0) return m + "分";
    return sec + "秒";
  }

  function remainingClass(ts) {
    if (ts == null || ts === "") return "";
    let exp = Number(ts);
    if (!Number.isFinite(exp)) return "";
    if (exp > 1e12) exp = exp / 1000;
    const sec = exp - Date.now() / 1000;
    if (sec <= 0) return "bad";
    if (sec < 2 * 3600) return "warn"; // <2h
    if (sec < 24 * 3600) return "blue"; // <1d
    return "ok";
  }

  function fmtExpiry(ts) {
    if (ts == null || ts === "") return '<span class="g2a-muted">—</span>';
    const abs = fmtTime(ts);
    const rem = fmtRemaining(ts);
    const cls = remainingClass(ts) || "";
    const pill = cls
      ? `<span class="g2a-tag ${cls} g2a-expiry-pill">${esc(rem)}</span>`
      : `<span class="g2a-muted">${esc(rem)}</span>`;
    return `<div class="g2a-expiry"><div class="g2a-expiry-abs mono">${esc(abs)}</div><div class="g2a-expiry-rem">${pill}</div></div>`;
  }

  function esc(s) {
    return String(s ?? "").replace(/[&<>"']/g, function (c) {
      return ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c];
    });
  }

  async function copyText(text) {
    const t = String(text ?? "");
    if (!t) return false;
    try {
      if (navigator.clipboard && window.isSecureContext) {
        await navigator.clipboard.writeText(t);
        return true;
      }
    } catch (e) {}
    try {
      const ta = document.createElement("textarea");
      ta.value = t;
      ta.setAttribute("readonly", "");
      ta.style.cssText = "position:fixed;left:-9999px;top:0";
      document.body.appendChild(ta);
      ta.focus();
      ta.select();
      ta.setSelectionRange(0, t.length);
      const ok = document.execCommand("copy");
      document.body.removeChild(ta);
      return ok;
    } catch (e) {
      return false;
    }
  }

  function currentOrigin() {
    try {
      if (location.protocol === "http:" || location.protocol === "https:") return location.origin;
    } catch (e) {}
    return "";
  }

  function currentAdminUrl() {
    const origin = currentOrigin();
    if (origin) return origin.replace(/\/$/, "") + "/admin";
    const port = location.port || "3000";
    return "http://<your-host>:" + port + "/admin";
  }

  function setBusy(el, busy, label) {
    if (!el) return;
    if (busy) {
      if (!el.dataset.label) el.dataset.label = el.textContent;
      el.classList.add("busy");
      el.disabled = true;
      if (label) el.textContent = label;
    } else {
      el.classList.remove("busy");
      el.disabled = false;
      if (el.dataset.label) el.textContent = el.dataset.label;
    }
  }

  function emptyState(msg) {
    return '<div class="g2a-empty">' + esc(msg || "暂无数据") + "</div>";
  }


  const THEME_KEY = "g2a_theme";

  function getTheme() {
    try {
      const t = localStorage.getItem(THEME_KEY);
      if (t === "light" || t === "dark") return t;
    } catch (_) {}
    try {
      if (window.matchMedia && window.matchMedia("(prefers-color-scheme: light)").matches) return "light";
    } catch (_) {}
    return "dark";
  }

  function applyTheme(theme) {
    const t = theme === "light" ? "light" : "dark";
    document.documentElement.setAttribute("data-theme", t);
    document.documentElement.style.colorScheme = t;
    if (document.body) document.body.setAttribute("data-theme", t);
    try { localStorage.setItem(THEME_KEY, t); } catch (_) {}
    // sync all toggle buttons
    document.querySelectorAll("[data-theme-toggle]").forEach((btn) => {
      const light = t === "light";
      btn.setAttribute("aria-label", light ? "切换到黑夜模式" : "切换到白天模式");
      btn.title = light ? "切换到黑夜模式" : "切换到白天模式";
      const ico = btn.querySelector(".ico");
      const label = btn.querySelector(".label");
      if (ico) ico.textContent = light ? "🌙" : "☀️";
      if (label) label.textContent = light ? "黑夜" : "白天";
    });
    return t;
  }

  function toggleTheme() {
    const cur = document.documentElement.getAttribute("data-theme") || getTheme();
    return applyTheme(cur === "light" ? "dark" : "light");
  }

  function bindThemeToggle(root) {
    const scope = root || document;
    scope.querySelectorAll("[data-theme-toggle]").forEach((btn) => {
      if (btn.dataset.boundTheme === "1") return;
      btn.dataset.boundTheme = "1";
      btn.addEventListener("click", (e) => {
        e.preventDefault();
        const next = toggleTheme();
        try { toast(next === "light" ? "已切换到白天模式" : "已切换到黑夜模式"); } catch (_) {}
      });
    });
    applyTheme(getTheme());
  }

  // Apply ASAP (before body paint if deferred scripts run late, still ok)
  try { applyTheme(getTheme()); } catch (_) {}

  // Install helpers (idempotent)
  G2A.$ = $;
  G2A.$$ = $$;
  G2A.toast = toast;
  G2A.fmtTime = fmtTime;
  G2A.fmtRemaining = fmtRemaining;
  G2A.remainingClass = remainingClass;
  G2A.fmtExpiry = fmtExpiry;
  G2A.esc = esc;
  G2A.copyText = copyText;
  G2A.currentOrigin = currentOrigin;
  G2A.currentAdminUrl = currentAdminUrl;
  G2A.setBusy = setBusy;
  G2A.emptyState = emptyState;
  G2A.THEME_KEY = THEME_KEY;
  G2A.getTheme = getTheme;
  G2A.applyTheme = applyTheme;
  G2A.toggleTheme = toggleTheme;
  G2A.bindThemeToggle = bindThemeToggle;
})(window.G2A);

// Ensure theme applied even if other scripts run first
try {
  if (window.G2A && typeof G2A.applyTheme === "function") G2A.applyTheme(G2A.getTheme());
} catch (_) {}


// Hard guarantee for other scripts even if IIFE order glitches
if (typeof window.G2A.$ !== "function") {
  window.G2A.$ = function (id) {
    try { return document.getElementById(String(id)); } catch (_) { return null; }
  };
}
/* g2a-cache-bust-20260712-local-solver */
