/* multi-page admin core — clean rebuild v2 */
window.G2A = window.G2A || {};
(function () {
  "use strict";
  const $ = (id) => (window.G2A && G2A.$ ? G2A.$(id) : document.getElementById(id));
  const toast = (...a) => (window.G2A && G2A.toast ? G2A.toast(...a) : console.log(...a));
  const esc = (s) => (window.G2A && G2A.esc ? G2A.esc(s) : String(s ?? ""));
  const copyText = (...a) => (window.G2A && G2A.copyText ? G2A.copyText(...a) : Promise.resolve(false));
  const fmtTime = (...a) => (window.G2A && G2A.fmtTime ? G2A.fmtTime(...a) : String(a[0] ?? "—"));
  const fmtExpiry = (...a) => (window.G2A && G2A.fmtExpiry ? G2A.fmtExpiry(...a) : fmtTime(a[0]));
  const fmtRemaining = (...a) => (window.G2A && G2A.fmtRemaining ? G2A.fmtRemaining(...a) : "—");
  const remainingClass = (...a) => (window.G2A && G2A.remainingClass ? G2A.remainingClass(...a) : "");
  const currentOrigin = () => (window.G2A && G2A.currentOrigin ? G2A.currentOrigin() : (location.origin || ""));
  const currentAdminUrl = () => (window.G2A && G2A.currentAdminUrl ? G2A.currentAdminUrl() : ((location.origin || "") + "/admin"));
  const TOKEN_KEY = (window.G2A && G2A.TOKEN_KEY) || "g2a_admin_token";
  const adminBasePath = () => {
    const path = String(location.pathname || "");
    const i = path.indexOf("/admin");
    return (i >= 0 ? path.slice(0, i) : "") + "/admin";
  };
  const API_BASE = (window.G2A && G2A.API_BASE) || (adminBasePath() + "/api");
  let token = (window.G2A && G2A.getToken) ? G2A.getToken() : (localStorage.getItem(TOKEN_KEY) || "");
  let statusCache = null;
  let dashCache = null;
  let loginSessionId = null;
  let devicePollTimer = null;
  let regSessionId = null;
  let regSessionIds = [];
  let regBatchId = null;
  let regPollTimer = null;
  let regFinishedNotified = false;
  let regStopping = false;
  let regPollInFlight = false;
  let regPollPending = false;
  let regLastLogText = "";
  let regLastStatusText = "";
  let regLastEmailText = "";
  let regProbedIds = new Set();
  let regProbeRunning = false;
  // Survive hard refresh: remember which batch/sessions the UI was tracking.
  const REG_TRACK_KEY = "g2a_reg_track_v1";
  let keysCache = [];
  let quotaCache = {};
  let uiRefreshTimer = null;
  let accountsList = [];
  let accountsPage = 1;
  let accountsTotal = 0;
  let accountsTotalPages = 1;
  let accountsLoading = false;
  let accountsLoadSeq = 0;
  let accountsPageSize = 25;
  let accountsSearchQuery = "";
  let accountsSort = "newest";
  // "" | "1" | "0" — server-side has_sso filter
  let accountsSsoFilter = "";
  // "" | live|cooldown|disabled|quota_disabled|model_blocked|expired
  let accountsStatusFilter = "";
  let selectedAccountIds = new Set();
  function syncToken() { token = (window.G2A && G2A.getToken) ? G2A.getToken() : token; }
  function headers(json = true) {
    syncToken();
    const h = {};
    if (json) h["Content-Type"] = "application/json";
    if (token) h["X-Admin-Token"] = token;
    return h;
  }
  async function api(path, opts = {}) {
    syncToken();
    if (window.G2A && G2A.api) {
      try { return await G2A.api(path, opts); }
      catch (e) { if (e && e.status === 401) token = ""; throw e; }
    }
    const res = await fetch(API_BASE + path, {
      ...opts,
      credentials: "same-origin",
      headers: { ...headers(!(opts.body instanceof FormData) && opts.method !== "GET"), ...(opts.headers || {}) },
    });
    let data = null;
    try {
      const ct = (res.headers.get("content-type") || "").toLowerCase();
      if (ct.includes("application/json")) data = await res.json();
      else {
        const text = await res.text();
        if (/^\s*<!doctype\s+html|^\s*<html[\s>]/i.test(text || "")) {
          const err = new Error("Admin API 返回了 HTML 页面，请检查 " + API_BASE + path + " 的反向代理/部署路径。响应片段：" + String(text || "").replace(/\s+/g, " ").trim().slice(0, 180));
          err.status = res.status;
          throw err;
        }
        data = text ? { detail: text.slice(0, 300) } : null;
      }
    } catch (e) { if (e && e.status != null) throw e; data = null; }
    if (!res.ok) {
      const msg = (data && (data.detail || data.error || data.message)) || res.statusText;
      const err = new Error(typeof msg === "string" ? msg : JSON.stringify(msg));
      err.status = res.status;
      throw err;
    }
    return data;
  }
  function on(id, ev, fn) {
    const el = $(id);
    if (!el) return false;
    try {
      // Prefer property handlers for easy rebind after soft-nav content swaps.
      el[ev] = fn;
      return true;
    } catch (_) {
      return false;
    }
  }

function setLogPanel(id, text, { forceShow = false } = {}) {
  const el = $(id);
  if (!el) return;
  const val = (text == null ? "" : String(text)).trim();
  const empty = !val || val === "—" || val === "-" || val === "暂无" || val === "idle";
  if (empty && !forceShow) {
    if (id === "reg-log") regLastLogText = "";
    el.textContent = "—";
    el.classList.add("is-empty", "hidden");
    el.hidden = true;
    return;
  }
  const next = val || "—";
  // Avoid rewriting identical registration logs — this was the main flicker source
  // while stop/poll re-rendered the same progress card every 1–2s.
  if (id === "reg-log") {
    if (next === regLastLogText && !el.classList.contains("hidden")) return;
    regLastLogText = next;
  }
  if (el.textContent === next && !el.classList.contains("hidden")) {
    el.classList.remove("is-empty", "hidden");
    el.hidden = false;
    return;
  }
  el.textContent = next;
  el.classList.remove("is-empty", "hidden");
  el.hidden = false;
}

function setRegStatusText(text) {
  const el = $("reg-status");
  if (!el) return;
  const next = text == null ? "—" : String(text);
  if (next === regLastStatusText && (el.textContent || "") === next) return;
  regLastStatusText = next;
  el.textContent = next;
}

function setRegEmailText(text) {
  const el = $("reg-email");
  if (!el) return;
  const next = text == null ? "—" : String(text);
  if (next === regLastEmailText && (el.textContent || "") === next) return;
  regLastEmailText = next;
  el.textContent = next;
}

function showPanel(id) {
  const el = $(id);
  if (!el) return;
  el.classList.remove("hidden");
  el.hidden = false;
}

function hidePanel(id) {
  const el = $(id);
  if (!el) return;
  el.classList.add("hidden");
  el.hidden = true;
}


  // Event delegation for dynamic content (works after soft-nav HTML swaps).
  function delegate(rootId, eventName, selector, handler) {
    const root = $(rootId) || document;
    const key = `_g2a_deleg_${eventName}_${selector}`;
    if (root[key]) return;
    root[key] = true;
    root.addEventListener(eventName, (e) => {
      const t = e.target && e.target.closest ? e.target.closest(selector) : null;
      if (!t || (root !== document && !root.contains(t))) return;
      handler(e, t);
    });
  }


function renderStoreConn(hostId) {
  // Overview no longer displays auth / DB / Redis connection diagnostics.
  // Keep a no-op so older call sites remain safe.
  const host = $(hostId);
  if (host) {
    host.innerHTML = "";
    host.hidden = true;
    host.classList.add("hidden");
  }
}

const PAGE_META = {
  overview: { title: "概览", sub: "服务状态、账号池与 Token 健康度一览" },
  keys: { title: "API Keys", sub: "创建、复制、停用客户端访问密钥" },
  accounts: { title: "账号 / 轮询", sub: "Grok 账号、设备码登录、额度与导入导出" },
  usage: { title: "用量", sub: "Token 消耗与请求使用情况（今日 / 近 N 天 / 累计）" },
  logs: { title: "任务日志", sub: "查询后台任务结果（协议注册、SSO 导入、测活、Token 续期等）" },
  models: { title: "模型", sub: "上游模型目录（入库）与探测结果" },
  settings: { title: "系统设置", sub: "修改管理员密码、轮询策略与 sub2api / 维护参数" },
  guide: { title: "接入指南", sub: "OpenAI / Anthropic 客户端配置示例" },
};

function showAuth(setup) {
  $("boot-view")?.classList.add("hidden");
  $("auth-view")?.classList.remove("hidden");
  $("main-view")?.classList.add("hidden");
  if ($("auth-title")) $("auth-title").textContent = setup ? "初始化管理密码" : "登录管理台";
  if ($("auth-desc")) $("auth-desc").textContent = setup
    ? "首次使用，请设置管理员密码（至少 4 位）"
    : "使用管理员密码进入";
  if ($("auth-submit")) $("auth-submit").textContent = setup ? "创建并进入" : "进入";
}
function showMain() {
  $("boot-view")?.classList.add("hidden");
  $("auth-view")?.classList.add("hidden");
  $("main-view")?.classList.remove("hidden");
  startAutoUiRefresh();
}

const PAGE_HREF = { overview: "/admin", keys: "/admin/keys", accounts: "/admin/accounts", usage: "/admin/usage", logs: "/admin/logs", models: "/admin/models", settings: "/admin/settings", guide: "/admin/guide" };
let _softNavToken = 0;
let _softNavBusy = false;
let _softNavBusySince = 0;

function pageFromPath(pathname) {
  const p = (pathname || location.pathname || "").replace(/\/$/, "") || "/admin";
  if (p.endsWith("/keys")) return "keys";
  if (p.endsWith("/accounts")) return "accounts";
  if (p.endsWith("/usage")) return "usage";
  if (p.endsWith("/logs")) return "logs";
  if (p.endsWith("/models")) return "models";
  if (p.endsWith("/settings")) return "settings";
  if (p.endsWith("/guide")) return "guide";
  if (p.endsWith("/login")) return "login";
  return "overview";
}

function setActiveMenu(page) {
  document.querySelectorAll(".g2a-menu-item[data-page]").forEach((a) => {
    a.classList.toggle("is-active", a.getAttribute("data-page") === page);
  });
  document.querySelectorAll("#mobile-nav a").forEach((a) => {
    const href = (a.getAttribute("href") || "").replace(/\/$/, "");
    const map = PAGE_HREF;
    const activeHref = (map[page] || "/admin").replace(/\/$/, "");
    a.classList.toggle("active", href === activeHref);
    a.classList.toggle("is-active", href === activeHref);
  });
}

function applyPageMeta(page) {
  const meta = PAGE_META[page] || PAGE_META.overview;
  if ($("page-title")) $("page-title").textContent = meta.title;
  if ($("page-sub")) $("page-sub").textContent = meta.sub;
  document.title = meta.title + " · grokcli-2api";
  document.body.dataset.page = page;
  setActiveMenu(page);
}

async function softNavigate(name, opts) {
  opts = opts || {};
  const page = name || "overview";
  const href = PAGE_HREF[page] || "/admin";
  const cur = (location.pathname || "").replace(/\/$/, "") || "/admin";
  const target = href.replace(/\/$/, "") || "/admin";
  if (cur === target && !opts.force) {
    applyPageMeta(page);
    return true;
  }
  // Keep shell painted. NEVER full-document navigate for admin pages (that causes black flash).
  if (_softNavBusy) {
    // A previous nav is stuck/in-flight: don't queue forever; unlock stale locks.
    if (_softNavBusySince && (Date.now() - _softNavBusySince) > 8000) {
      try { clearSoftNavBusy("stale"); } catch (_) {}
    } else {
      return false;
    }
  }
  _softNavBusy = true;
  _softNavBusySince = Date.now();
  const my = ++_softNavToken;
  document.body.classList.add("is-authed");
  document.body.classList.add("g2a-softnav-busy");
  // Theme-aware paint lock (avoid forcing pure black in light mode).
  try {
    const theme = document.documentElement.getAttribute("data-theme") || "dark";
    document.documentElement.style.background = theme === "light" ? "#f5f7fb" : "#0a0a0f";
  } catch (_) {
    document.documentElement.style.background = "#0a0a0f";
  }
  // Hard timeout: never leave the UI dimmed/black if fetch hangs.
  const busyTimer = setTimeout(() => {
    if (my === _softNavToken) {
      try { clearSoftNavBusy("timeout"); } catch (_) {}
      try { toast("页面切换超时，已恢复界面", false); } catch (_) {}
    }
  }, 10000);
  try {
    const res = await fetch(href, {
      credentials: "same-origin",
      headers: { "X-Requested-With": "G2ASoftNav", "Accept": "text/html" },
      cache: "no-store",
    });
    if (!res.ok) throw new Error("页面加载失败 " + res.status);
    const html = await res.text();
    if (my !== _softNavToken) return false;
    const doc = new DOMParser().parseFromString(html, "text/html");
    const nextContent = doc.querySelector(".g2a-content");
    const curContent = document.querySelector(".g2a-content");
    if (!nextContent || !curContent) throw new Error("页面结构异常");

    // Swap only page body content; sidebar/header stay mounted.
    if (typeof curContent.replaceChildren === "function") {
      const frag = document.createDocumentFragment();
      Array.from(nextContent.childNodes).forEach((n) => frag.appendChild(document.importNode(n, true)));
      curContent.replaceChildren(frag);
    } else {
      curContent.innerHTML = nextContent.innerHTML;
    }

    const nt = doc.querySelector("#page-title");
    const ns = doc.querySelector("#page-sub");
    if (nt && $("page-title")) $("page-title").textContent = nt.textContent;
    if (ns && $("page-sub")) $("page-sub").textContent = ns.textContent;
    applyPageMeta(page);
    if (!opts.replace) history.pushState({ g2aPage: page }, "", href);
    else history.replaceState({ g2aPage: page }, "", href);

    try {
      if (typeof rebindPageControls === "function") rebindPageControls();
      try { if (window.G2A && G2A.bindThemeToggle) G2A.bindThemeToggle(document); } catch(_){}
      // Non-blocking data load so menu clicks feel instant.
      // Models page: load dedicated catalog first (do not rely on /dashboard).
      if (page === "models" && typeof loadModels === "function") {
        Promise.resolve(loadModels()).catch((e) => console.warn("soft nav loadModels", e));
      } else if (typeof loadDashboard === "function") {
        Promise.resolve(loadDashboard()).catch((e) => console.warn("soft nav loadDashboard", e));
      }
    } catch (e) {
      console.warn("soft nav loadDashboard", e);
    }
    if (page === "overview") {
      try { startAutoUiRefresh(); } catch (_) {}
    } else {
      try { if (uiRefreshTimer) { clearInterval(uiRefreshTimer); uiRefreshTimer = null; } } catch (_) {}
    }
    if (page === "settings") {
      try { await loadSystemSettings(); } catch (e) { console.warn("loadSystemSettings", e); }
    }
    if (page === "accounts") {
      try { await loadRegConfig(false); } catch (e) { console.warn("loadRegConfig", e); }
      try {
        await restoreActiveRegistration({ force: !hasTrackedRegTask(), toastIfEmpty: false });
      } catch (e) {
        console.warn("restoreActiveRegistration", e);
      }
    }
    // Page-specific renders after content swap
    try {
      if (page === "accounts" && typeof renderAccounts === "function") renderAccounts();
      if (page === "keys" && typeof renderKeys === "function") renderKeys();
      if (page === "logs" && typeof loadAdminLogs === "function") loadAdminLogs({ reset: false });
      if (page === "usage" && typeof loadUsage === "function") loadUsage();
      // models: already kicked off loadModels above; keep a second best-effort call
      if (page === "models" && typeof loadModels === "function") {
        Promise.resolve(loadModels()).catch(() => {});
      } else if (page === "models" && typeof renderModels === "function") {
        renderModels();
      }
      if (page === "guide" && typeof renderGuide === "function") renderGuide();
      if (page === "overview" && typeof renderStats === "function") renderStats();
    } catch (e) {
      console.warn("soft nav render", e);
    }
    return true;
  } catch (e) {
    console.error("softNavigate failed", e);
    try { toast((e && e.message) || "切换页面失败", false); } catch (_) {}
    // Do NOT full-page navigate (black flash). Recover in place; offer one soft reload of content.
    try { applyPageMeta(pageFromPath(location.pathname)); } catch (_) {}
    try { clearSoftNavBusy("error"); } catch (_) {}
    // Do not full-reload on soft-nav failure (causes "界面卡死/反复刷新" on flaky networks).
    // Stay on current shell; user can click menu again or use refresh button.
    return false;
  } finally {
    try { clearTimeout(busyTimer); } catch (_) {}
    if (my === _softNavToken) {
      clearSoftNavBusy("done");
    }
  }
}

function clearSoftNavBusy(reason) {
  _softNavBusy = false;
  _softNavBusySince = 0;
  try { document.body.classList.remove("g2a-softnav-busy"); } catch (_) {}
  // keep is-authed; only busy dimmer is harmful
}

function hideEmptyLogPanels() {
  try { if (!loginSessionId) setDeviceLoginIdle(true); } catch (_) {}
  ["device-log", "reg-log", "probe-result", "sso-result"].forEach((id) => {
    const el = $(id);
    if (!el) return;
    // Never auto-hide an active registration log — soft-nav rebind used to wipe the card.
    if (id === "reg-log" && (regBatchId || (regSessionIds && regSessionIds.length) || regSessionId)) {
      return;
    }
    const val = (el.textContent || "").trim();
    if (!val || val === "—" || val === "-") {
      el.classList.add("is-empty", "hidden");
      el.hidden = true;
    }
  });
  const regBox = $("reg-session-box");
  if (regBox) {
    // Keep the card visible while a registration is still tracked in this page session.
    if (regBatchId || (regSessionIds && regSessionIds.length) || regSessionId) {
      regBox.classList.remove("hidden");
      regBox.hidden = false;
      return;
    }
    const st = ((regBox.querySelector("#reg-status") && regBox.querySelector("#reg-status").textContent) || "").trim() || "idle";
    const log = $("reg-log");
    const logText = ((log && log.textContent) || "").trim();
    const emptyLog = !log || log.classList.contains("hidden") || !logText || logText === "—";
    if ((st === "idle" || st === "—" || st === "") && emptyLog) {
      regBox.classList.add("hidden");
      regBox.hidden = true;
    }
  }
}

function rebindPageControls() {
  try { bindLogsControls(); } catch (_) {}
  try { bindUsageControls(); } catch (_) {}
  try { hideEmptyLogPanels(); } catch (_) {}
  // Soft-nav replaces .g2a-content; sub2api buttons must be rebound every time.
  try { bindSub2apiUi(); } catch (_) {}
  // Soft-nav swaps DOM; re-show active registration card + keep polling if needed.
  // Full page refresh loses in-memory ids — restore from backend when missing.
  try {
    const page = document.body.dataset.page || pageFromPath(location.pathname) || "";
    if (page === "accounts") {
      // Soft-nav keeps JS heap, but hard-refresh recovery may land here first.
      if (!hasTrackedRegTask()) applyRegTrack(loadRegTrack());
      if (hasTrackedRegTask()) {
        showPanel("reg-session-box");
        // Never re-poll a finished card (avoids completion-toast spam).
        if (!regFinishedNotified) startRegPolling({ immediate: true });
      } else {
        restoreActiveRegistration({ force: true, toastIfEmpty: false }).catch(() => {});
      }
    }
  } catch (_) {}

  // Re-bind controls after soft navigation content swaps. Idempotent.
  try { if (window.G2A && G2A.bindThemeToggle) G2A.bindThemeToggle(document); } catch (_) {}

  // Header / global
  on("btn-refresh", "onclick", async () => {
    try {
      _statusFetchedAt = 0;
      statusCache = null;
      const page = document.body.dataset.page || pageFromPath(location.pathname) || "";
      if (page === "models" && typeof loadModels === "function") {
        const list = await loadModels();
        toast(`已刷新模型列表（${(list || []).length} 个）`);
      } else {
        await loadDashboard();
        toast("已刷新");
      }
    } catch (e) { toast(e.message, false); }
  });
  on("btn-logout", "onclick", async () => {
    try { await api("/logout", { method: "POST", body: "{}" }); } catch (_) {}
    try { if (window.G2A && G2A.clearToken) G2A.clearToken(); else localStorage.removeItem(TOKEN_KEY); } catch (_) {}
    document.body.classList.remove("is-authed");
    location.replace("/admin/login");
  });
  on("btn-refresh-all", "onclick", async () => {
    try {
      _statusFetchedAt = 0;
      statusCache = null;
      await loadDashboard();
      toast("已刷新");
    } catch (e) { toast(e.message, false); }
  });

  // Overview
  const bindQuota = (id) => { const el = $(id); if (el) el.onclick = () => refreshAllQuota(true); };
  const bindProbe = (id) => { const el = $(id); if (el) el.onclick = () => runProbeAll(); };
  bindQuota("btn-refresh-quota");
  bindQuota("btn-refresh-quota-2");
  bindProbe("btn-probe-all");
  bindProbe("btn-probe-all-2");
  if ($("chk-token-maintain")) $("chk-token-maintain").onchange = () => setFeatureToggle("/settings/token-maintain", !!$("chk-token-maintain").checked, "Token 自动续期");
  if ($("chk-model-health")) $("chk-model-health").onchange = () => setFeatureToggle("/settings/model-health", !!$("chk-model-health").checked, "自动健康探测");
  on("btn-refresh-tokens", "onclick", async () => {
    try {
      if ($("btn-refresh-tokens")) $("btn-refresh-tokens").disabled = true;
      const r = await api("/accounts/refresh", { method: "POST", body: JSON.stringify({ force: true }) });
      const n = r.refreshed ?? ((r.refresh && r.refresh.refreshed) != null ? r.refresh.refreshed : null) ?? (r.results || []).filter(x => x.ok && !x.skipped).length;
      toast(`Token 已刷新：${n ?? 0} 个账号`);
      // Merge immediate refresh result into caches so overview text updates now.
      statusCache = statusCache || {};
      dashCache = dashCache || {};
      const tm = Object.assign({}, statusCache.token_maintainer || {}, r.maintainer || r.token_maintainer || {});
      tm.last = Object.assign({}, tm.last || {}, {
        ok: true,
        at: Date.now() / 1000,
        force: true,
        refresh: r.refresh || {
          refreshed: n ?? 0,
          attempted: r.attempted ?? ((r.results || []).length || n || 0),
          failed: r.failed,
          skipped: r.skipped,
        },
      });
      statusCache.token_maintainer = tm;
      dashCache.token_maintainer = tm;
      try { renderMaintainer(); } catch (_) {}
      try { await refreshOverviewStatus({ force: true, render: true }); } catch (_) { await loadDashboard(); }
    } catch (e) { toast(e.message, false); }
    finally { if ($("btn-refresh-tokens")) $("btn-refresh-tokens").disabled = false; }
  });
  on("btn-normalize-keys", "onclick", async () => {
    try {
      const r = await api("/accounts/normalize", { method: "POST" });
      toast(`多账号键规范化：变更 ${r.changed ?? 0}，共 ${r.total ?? 0} 个`);
      _statusFetchedAt = 0;
      await loadDashboard();
    } catch (e) { toast(e.message, false); }
  });

  // Keys
  on("btn-create-key", "onclick", async () => {
    try {
      const name = ($("key-name") && $("key-name").value) || "default";
      const note = ($("key-note") && $("key-note").value) || "";
      const data = await api("/keys", { method: "POST", body: JSON.stringify({ name, note }) });
      const rec = data.key || data;
      const full = (rec && (rec.key || rec.secret)) || data.secret || "";
      const box = $("new-key-box");
      if (box) {
        box.classList.remove("hidden");
        box.innerHTML = `<div style="font-weight:600;margin-bottom:6px;color:var(--ok)">✓ Key 已创建 — 列表中可随时再复制</div>
          <div class="mono" id="new-key-value" style="user-select:all;word-break:break-all;cursor:pointer" title="点击复制">${esc(full)}</div>
          <div style="margin-top:8px;display:flex;gap:8px;flex-wrap:wrap">
            <button class="g2a-btn g2a-btn-primary g2a-btn-sm" id="copy-key">复制 Key</button>
            <button class="g2a-btn g2a-btn-default g2a-btn-sm" id="dismiss-key">收起</button>
          </div>`;
        const doCopy = async () => {
          if (!full) { toast("Key 为空", false); return; }
          const ok = await copyText(full);
          toast(ok ? "已复制 API Key" : "复制失败，请手动选中复制", ok);
        };
        on("copy-key", "onclick", doCopy);
        on("new-key-value", "onclick", doCopy);
        on("dismiss-key", "onclick", () => box.classList.add("hidden"));
      }
      if (full) {
        const ok = await copyText(full);
        if (ok) toast("已创建并自动复制到剪贴板");
      }
      if ($("key-name")) $("key-name").value = "";
      if ($("key-note")) $("key-note").value = "";
      await loadDashboard();
    } catch (e) { toast(e.message, false); }
  });

  // Models / accounts common
  on("btn-sync-models", "onclick", async () => {
    try {
      const r = await api("/models/sync", { method: "POST" });
      // Always re-fetch catalog so local extras (grok-build) stay visible.
      const list = await loadModels();
      toast(`已同步 ${r.count || (list || []).length || 0} 个模型`);
    } catch (e) { toast(e.message, false); }
  });
  on("btn-save-mode", "onclick", async () => {
    try {
      const mode = $("account-mode") ? $("account-mode").value : "";
      await api("/settings/account-mode", { method: "PUT", body: JSON.stringify({ mode }) });
      toast("轮询策略已保存: " + mode);
      await loadDashboard();
    } catch (e) { toast(e.message, false); }
  });

  // System settings page
  on("btn-reload-settings", "onclick", async () => {
    try {
      await loadSystemSettings(true);
      toast("已重新加载设置");
    } catch (e) { toast(e.message || "加载失败", false); }
  });
  on("btn-save-settings", "onclick", async () => {
    try { await saveSystemSettings(); } catch (e) { toast(e.message || "保存失败", false); }
  });
  on("btn-change-password", "onclick", async () => {
    try { await changeAdminPassword(); } catch (e) { toast(e.message || "修改失败", false); }
  });
  if ($("set-outbound-proxy")) {
    on("set-outbound-proxy", "oninput", () => { try { updateOutboundProxyHint(); } catch (_) {} });
  }
  if ($("set-outbound-proxy-strategy")) {
    on("set-outbound-proxy-strategy", "onchange", () => { try { updateOutboundProxyHint(); } catch (_) {} });
  }
  if ($("set-outbound-proxy-enabled")) {
    on("set-outbound-proxy-enabled", "onchange", () => { try { updateOutboundProxyHint(); } catch (_) {} });
  }
  try { updateOutboundProxyHint(); } catch (_) {}
  on("btn-refresh-acc", "onclick", async () => {
    try {
      _statusFetchedAt = 0;
      await loadDashboard();
      toast("已刷新");
    } catch (e) { toast(e.message, false); }
  });

  // Accounts toolbar
  if ($("btn-acc-search")) $("btn-acc-search").onclick = () => applyAccountSearch(true);
  if ($("btn-acc-search-clear")) $("btn-acc-search-clear").onclick = () => {
    if ($("acc-search")) $("acc-search").value = "";
    applyAccountSearch(true);
  };
  if ($("acc-search")) {
    $("acc-search").onkeydown = (e) => { if (e.key === "Enter") applyAccountSearch(true); };
  }
  if ($("acc-sort")) {
    try {
      const saved = localStorage.getItem("g2a_accounts_sort");
      if (saved) {
        accountsSort = saved;
        $("acc-sort").value = saved;
      } else {
        accountsSort = $("acc-sort").value || "newest";
      }
    } catch (_) {
      accountsSort = $("acc-sort").value || "newest";
    }
    $("acc-sort").onchange = () => {
      accountsSort = ($("acc-sort").value || "newest");
      try { localStorage.setItem("g2a_accounts_sort", accountsSort); } catch (_) {}
      accountsPage = 1;
      loadAccountsPage({ reset: true });
    };
  }
  if ($("acc-filter-sso")) {
    try {
      const savedSso = localStorage.getItem("g2a_accounts_sso_filter");
      if (savedSso === "1" || savedSso === "0" || savedSso === "") {
        accountsSsoFilter = savedSso || "";
        $("acc-filter-sso").value = accountsSsoFilter;
      } else {
        accountsSsoFilter = $("acc-filter-sso").value || "";
      }
    } catch (_) {
      accountsSsoFilter = $("acc-filter-sso").value || "";
    }
    $("acc-filter-sso").onchange = () => {
      accountsSsoFilter = ($("acc-filter-sso").value || "");
      try { localStorage.setItem("g2a_accounts_sso_filter", accountsSsoFilter); } catch (_) {}
      accountsPage = 1;
      loadAccountsPage({ reset: true });
    };
  }
  if ($("btn-acc-select-page")) $("btn-acc-select-page").onclick = () => setPageSelection(true);
  if ($("btn-acc-select-all-filtered")) $("btn-acc-select-all-filtered").onclick = () => { selectAllFilteredAccounts(); };
  if ($("btn-acc-select-none")) $("btn-acc-select-none").onclick = () => { selectedAccountIds.clear(); renderAccountsPage(); };
  if ($("btn-acc-delete-selected")) $("btn-acc-delete-selected").onclick = () => deleteSelectedAccounts();
  if ($("btn-acc-renew-selected")) $("btn-acc-renew-selected").onclick = () => renewAccounts(Array.from(selectedAccountIds));
  if ($("btn-acc-probe-selected")) $("btn-acc-probe-selected").onclick = () => probeAccounts(Array.from(selectedAccountIds));
  if ($("btn-acc-export-selected")) $("btn-acc-export-selected").onclick = () => exportSelectedAccounts();
  if ($("btn-acc-export-sso-selected")) $("btn-acc-export-sso-selected").onclick = () => exportSelectedAccountsSso();
  if ($("btn-acc-export-sso-all")) $("btn-acc-export-sso-all").onclick = () => exportAllAccountsSso();
  on("acc-page-prev", "onclick", () => { if (accountsPage > 1 && !accountsLoading) { accountsPage--; loadAccountsPage(); } });
  on("acc-page-next", "onclick", () => { if (!accountsLoading && accountsPage < (accountsTotalPages || 1)) { accountsPage++; loadAccountsPage(); } });
  on("acc-page-size", "onchange", () => {
    accountsPageSize = parseInt(($("acc-page-size") && $("acc-page-size").value) || "25", 10) || 25;
    accountsPage = 1;
    loadAccountsPage({ reset: true });
  });

  // Device login / import / export / reg
  // Always re-enable progressive device UI on each rebind.
  if (!loginSessionId) setDeviceLoginIdle(true);
  else setDeviceLoginIdle(false);
  on("btn-login-device", "onclick", () => startDeviceLogin());
  on("btn-poll-device", "onclick", () => pollDeviceSession());
  on("btn-copy-device", "onclick", () => copyDeviceCode());
  on("btn-import", "onclick", () => importJsonFiles());
  on("btn-import-sso", "onclick", () => importSsoCookies());
on("btn-export-sso", "onclick", () => exportRegistrationSso());
  if ($("btn-export")) on("btn-export", "onclick", () => exportAllAccounts());
  on("btn-logout-cli", "onclick", async () => {
    if (!confirm("注销全部 Grok 账号？（将清空数据库账号池与本地镜像）")) return;
    try {
      const r = await api("/accounts/logout", { method: "POST" });
      toast(r.message || "完成", !!r.ok);
      await loadDashboard();
    } catch (e) { toast(e.message, false); }
  });
  if ($("btn-start-reg")) on("btn-start-reg", "onclick", async () => {
    try {
      const config = readRegConfig();
      cacheRegConfigLocal(config);
      $("btn-start-reg").disabled = true;
      // Drop previous finished/stopped run before starting a new one.
      resetRegProgressForNewTask();
      const r = await api("/accounts/register-email", { method: "POST", body: JSON.stringify(buildRegBody(config)) });
      regBatchId = r.batch_id || null;
      regSessionId = r.id || r.session_id || (Array.isArray(r.session_ids) ? r.session_ids[0] : null);
      regSessionIds = Array.isArray(r.session_ids) ? r.session_ids.slice() : (regSessionId ? [regSessionId] : []);
      const startedCount = Number(r.count || regSessionIds.length || 1) || 1;
      const workers = Number(r.concurrency || config.concurrency || 1) || 1;
      showPanel("reg-session-box");
      if (Array.isArray(r.sessions) && r.sessions.length) showRegSessionGroup(r.sessions, { batch: r });
      else if (regSessionId) showRegSession(r);
      else {
        setRegStatusText("starting");
        setRegEmailText(regBatchId ? `batch ${regBatchId}` : "—");
        setLogPanel(
          "reg-log",
          [
            `[start] 多线程协议注册已启动`,
            `目标数量: ${startedCount}`,
            `并发: ${workers}`,
            `batch_id: ${regBatchId || "—"}`,
            `session_ids: ${(regSessionIds || []).join(", ") || "等待 spawner…"}`,
            `message: ${r.message || "ok"}`,
          ].join("\n"),
          { forceShow: true }
        );
      }
      // Persist track immediately so hard refresh can restore this card.
      saveRegTrack();
      toast(r.message || `已启动注册 ×${startedCount}（线程 ${workers}，同时最多 ${workers} 个）`);
      // Start path auto-saves on server; refresh form from DB shortly after
      setTimeout(() => { loadRegConfig(true).catch(() => {}); }, 300);
      startRegPolling({ immediate: true, intervalMs: 1000 });
    } catch (e) { toast(e.message, false); }
    finally { if ($("btn-start-reg")) $("btn-start-reg").disabled = false; }
  });
  if ($("btn-save-reg")) on("btn-save-reg", "onclick", () => { saveRegConfig().catch(() => {}); });
  if ($("btn-refresh-reg")) on("btn-refresh-reg", "onclick", () => {
    refreshRegistrationProgress({ toastIfEmpty: true }).catch(() => {});
  });
  if ($("btn-stop-reg")) on("btn-stop-reg", "onclick", () => { stopRegistration().catch(() => {}); });
  if ($("btn-stop-reg-inline")) on("btn-stop-reg-inline", "onclick", () => { stopRegistration().catch(() => {}); });
  if ($("btn-refresh-reg-inline")) on("btn-refresh-reg-inline", "onclick", () => {
    refreshRegistrationProgress({ toastIfEmpty: true }).catch(() => {});
  });
  if ($("btn-close-reg-inline")) on("btn-close-reg-inline", "onclick", () => {
    dismissRegProgressCard();
    toast("已关闭进度卡片（后台注册不受影响）");
  });
  if ($("btn-test-reg-proxy")) on("btn-test-reg-proxy", "onclick", async () => {
    try {
      $("btn-test-reg-proxy").disabled = true;
      const r = await api("/register-email/test-proxy", { method: "POST", body: JSON.stringify(buildProxyTestBody(readRegConfig())) });
      showPanel("reg-session-box");
      setRegEmailText("xAI 代理测试");
      const poolN = r.proxy_pool && r.proxy_pool.count != null ? Number(r.proxy_pool.count) : 0;
      let status = r.ok ? "代理可用" : "代理不可用";
      if (poolN > 1) {
        if (Array.isArray(r.results)) {
          status = r.ok
            ? `代理池 ${r.ok_count || 0}/${r.tested || r.results.length} 可用`
            : `代理池测试失败 (${r.ok_count || 0}/${r.tested || r.results.length})`;
        } else {
          status = r.ok ? `代理可用 (池 ${poolN})` : `代理不可用 (池 ${poolN})`;
        }
      }
      setRegStatusText(status);
      setLogPanel("reg-log", JSON.stringify(r, null, 2), { forceShow: true });
      toast(r.ok ? status : (status + (r.error ? ": " + r.error : "")), !!r.ok);
    } catch (e) { toast(e.message, false); }
    finally { if ($("btn-test-reg-proxy")) $("btn-test-reg-proxy").disabled = false; }
  });

  // Delegated table actions (survive soft-nav swaps)
  if ($("keys-tbody") && !$("keys-tbody")._g2aBound) {
    $("keys-tbody")._g2aBound = true;
    $("keys-tbody").addEventListener("click", async (e) => {
      const btn = e.target.closest("button");
      if (!btn) return;
      const id = btn.dataset.id;
      try {
        if (btn.dataset.act === "copy") {
          const k = keysCache[id] || {};
          let full = k.secret || k.key || "";
          let regenerated = false;
          if (!full) {
            if (!confirm("该 Key 未保存完整值，无法直接复制。是否重新生成？旧 Key 会立即。")) return;
            const data = await api("/keys/" + id + "/regenerate", { method: "POST" });
            const rec = data.key || data;
            full = (rec && (rec.key || rec.secret)) || data.secret || "";
            if (!full) { toast("重建后仍无完整值", false); await loadDashboard(); return; }
            keysCache[id] = rec; regenerated = true;
          }
          const ok = await copyText(full);
          toast(ok ? (regenerated ? "已重建并复制 API Key" : "已复制 API Key") : "复制失败", ok);
          if (regenerated) await loadDashboard();
          return;
        }
        if (btn.dataset.act === "del") {
          if (!confirm("确定删除此 Key？")) return;
          await api("/keys/" + id, { method: "DELETE" });
          toast("已删除");
        } else if (btn.dataset.act === "toggle") {
          await api("/keys/" + id, { method: "PATCH", body: JSON.stringify({ enabled: btn.dataset.on === "1" }) });
          toast("已更新");
        }
        await loadDashboard();
      } catch (err) { toast(err.message, false); }
    });
  }

  if ($("accounts-tbody") && !$("accounts-tbody")._g2aBound) {
    $("accounts-tbody")._g2aBound = true;
    $("accounts-tbody").addEventListener("click", async (e) => {
      const chk = e.target.closest(".acc-check-one");
      if (chk) {
        const id = chk.dataset.id;
        if (!id) return;
        if (chk.checked) selectedAccountIds.add(id); else selectedAccountIds.delete(id);
        updateAccountSelectionInfo(accountsTotal || 0, document.querySelectorAll(".acc-check-one").length);
        return;
      }
      const btn = e.target.closest("button");
      if (!btn) return;
      const id = btn.dataset.id;
      try {
        if (btn.dataset.act === "renew-one") { await renewAccounts([id], { confirmMany: false }); return; }
        if (btn.dataset.act === "probe-one") { await runAccountProbe(id); return; }
        if (btn.dataset.act === "quota-one") {
          setRowBusy(id, true, "查询中");
          try {
            const q = await api("/accounts/" + encodeURIComponent(id) + "/quota");
            quotaCache[id] = q;
            if (q.auto_disabled) toast("该账号额度已耗尽，已移出轮询", false);
            else if (q.ok) toast((q.display && q.display.summary) || "额度已更新");
            else toast(q.error || "额度查询失败", false);
            upsertAccountInList({
              id,
              _pool: {
                last_quota: q,
                disabled_for_quota: !!q.auto_disabled || !!q.exhausted,
                disabled_reason: q.auto_disabled ? (q.error || "额度耗尽") : undefined,
              },
            });
            refreshOneAccountLocal(id);
          } finally {
            setRowBusy(id, false);
          }
          return;
        }
        if (btn.dataset.act === "toggle-acc") {
          setRowBusy(id, true, "处理中");
          try {
            const en = btn.dataset.on === "1";
            await api("/accounts/" + encodeURIComponent(id) + "/enabled", { method: "PATCH", body: JSON.stringify({ enabled: en }) });
            toast(en ? "已启用" : "已禁用");
            upsertAccountInList({ id, _pool: { enabled: en, disabled_for_quota: en ? false : undefined, consecutive_fails: en ? 0 : undefined, in_cooldown: en ? false : undefined } });
            refreshOneAccountLocal(id);
          } finally {
            setRowBusy(id, false);
          }
          return;
        }
        if (btn.dataset.act === "rm-acc") {
          if (!confirm("确定移除此账号？将从数据库与本地镜像同步删除。")) return;
          setRowBusy(id, true, "移除中");
          try {
            await api("/accounts/" + encodeURIComponent(id), { method: "DELETE" });
            selectedAccountIds.delete(id);
            accountsList = (accountsList || []).filter((a) => a.id !== id);
            accountsTotal = Math.max(0, (accountsTotal || 1) - 1);
            const row = document.querySelector(`tr[data-acc-id="${CSS.escape(String(id))}"]`);
            if (row) row.remove();
            if ($("acc-page-info")) {
              $("acc-page-info").textContent = `${accountsPage} / ${Math.max(1, accountsTotalPages || 1)} (本页 ${document.querySelectorAll("#accounts-tbody tr[data-acc-id]").length} / 共 ${accountsTotal || 0} 个)`;
            }
            toast("已移除");
          } finally {
            setRowBusy(id, false);
          }
          return;
        }
      } catch (err) { toast(err.message, false); }
    });
  }
}

function switchTab(name) {
  softNavigate(name);
}

function buildMobileNav() {
  const host = $("mobile-nav");
  if (!host) return;
  const map = { overview: "/admin", keys: "/admin/keys", accounts: "/admin/accounts", models: "/admin/models", settings: "/admin/settings", guide: "/admin/guide" };
  const active = document.body.dataset.page || "overview";
  host.innerHTML = Object.keys(PAGE_META).map(k => `<a class="${k===active?"active is-active":""}" href="${map[k]}">${PAGE_META[k].title}</a>`).join("");
}



async function bootstrap() {
  if (window.__g2aBootstrapped) return;
  window.__g2aBootstrapped = true;
  if (location.protocol === "file:") { toast("请通过服务打开管理台", false); location.replace("/admin/login"); return; }
  // Never blank the page on navigation. Keep shell visible the whole time.
  syncToken();
  document.body.classList.add("is-authed");
  document.documentElement.classList.add("g2a-has-session");
  try { if (window.G2A && G2A.bindThemeToggle) G2A.bindThemeToggle(document); } catch(_){}
  try {
    buildMobileNav();
    const page = document.body.dataset.page || pageFromPath(location.pathname) || "overview";
    applyPageMeta(page);

    // Soft session restore: validate local token OR cookie session.
    // Never keep a stale local token that makes the UI look "logged in" while APIs 401.
    try {
      await api("/session");
      if (window.G2A && G2A.markAuthOk) G2A.markAuthOk();
    } catch (_) {
      try { if (window.G2A && G2A.clearToken) G2A.clearToken(); else { token=""; localStorage.removeItem(TOKEN_KEY); } } catch (_) {}
      document.body.classList.remove("is-authed");
      document.documentElement.classList.remove("g2a-has-session");
      location.replace("/admin/login?next=" + encodeURIComponent(location.pathname + location.search));
      return;
    }

    try {
      statusCache = await api("/status");
      if (statusCache && statusCache.setup_needed) {
        token = "";
        try { if (window.G2A && G2A.clearToken) G2A.clearToken(); else localStorage.removeItem(TOKEN_KEY); } catch (_) {}
        document.body.classList.remove("is-authed");
        document.documentElement.classList.remove("g2a-has-session");
        location.replace("/admin/login");
        return;
      }
    } catch (e) {
      console.warn("status failed", e);
      toast("无法连接服务: " + (e.message || e), false);
    }

    try {
      await loadDashboard();
      if (page === "settings") {
        try { await loadSystemSettings(); } catch (es) { console.warn("settings load", es); }
      }
    } catch (e1) {
      console.error(e1);
      if (e1 && e1.status === 401 && !(e1.soft || (window.G2A && G2A.inAuthGrace && G2A.inAuthGrace()))) {
        try { if (window.G2A && G2A.clearToken) G2A.clearToken(); } catch (_) {}
        document.body.classList.remove("is-authed");
        location.replace("/admin/login?next=" + encodeURIComponent(location.pathname + location.search));
        toast("会话已失效，请重新登录", false);
        return;
      }
      toast(e1.message || "加载失败", false);
      if (page === "accounts") renderAccounts();
      if (page === "keys") renderKeys();
      if (page === "models") {
        try {
          if (typeof loadModels === "function") loadModels();
          else renderModels();
        } catch (_) {}
      }
      if (page === "guide") { try { renderGuide(); } catch (_) {} }
      if (page === "overview") { try { renderStats(); } catch (_) {} }
    }
    try { rebindPageControls(); } catch(_){}
    if (page === "overview") startAutoUiRefresh();
    if (page === "accounts") {
      renderAccounts();
      try {
        restoreActiveRegistration({ force: !hasTrackedRegTask(), toastIfEmpty: false }).catch(() => {});
      } catch (_) {}
    }
    if (page === "keys") renderKeys();
    on("btn-logout", "onclick", async () => {
      try { await api("/logout", { method: "POST", body: "{}" }); } catch (_) {}
      try { if (window.G2A && G2A.clearToken) G2A.clearToken(); else localStorage.removeItem(TOKEN_KEY); } catch (_) {}
      document.body.classList.remove("is-authed");
      location.replace("/admin/login");
    });
    on("btn-refresh", "onclick", async () => {
      try {
        _statusFetchedAt = 0;
        statusCache = null;
        const page = document.body.dataset.page || pageFromPath(location.pathname) || "";
        if (page === "models" && typeof loadModels === "function") {
          const list = await loadModels();
          toast(`已刷新模型列表（${(list || []).length} 个）`);
        } else {
          await loadDashboard();
          toast("已刷新");
        }
      } catch (e) { toast(e.message, false); }
    });
  } catch (e) {
    if (e && e.status === 401) {
      try { if (window.G2A && G2A.clearToken) G2A.clearToken(); } catch (_) {}
      document.body.classList.remove("is-authed");
      location.replace("/admin/login");
      toast("会话已失效，请重新登录", false);
    } else {
      toast((e && e.message) || "错误", false);
    }
  }
}

let _statusFetchedAt = 0;

async function refreshOverviewStatus({ force = true, render = true } = {}) {
  // Force-refresh /status and re-render overview widgets so button actions update text immediately.
  try {
    if (force) _statusFetchedAt = 0;
    const st = await api("/status");
    statusCache = st || statusCache;
    _statusFetchedAt = Date.now();
    if (window.G2A && G2A.state) G2A.state.status = statusCache;
    // Keep dashCache in sync for fields overview prefers from either source.
    if (statusCache) {
      dashCache = dashCache || {};
      if (statusCache.token_maintainer) dashCache.token_maintainer = statusCache.token_maintainer;
      if (statusCache.model_health) dashCache.model_health = statusCache.model_health;
      if (statusCache.pool) dashCache.pool = Object.assign({}, dashCache.pool || {}, statusCache.pool);
      if (statusCache.settings) dashCache.settings = Object.assign({}, dashCache.settings || {}, statusCache.settings);
      if (statusCache.accounts) dashCache.accounts = Object.assign({}, dashCache.accounts || {}, statusCache.accounts);
    }
  } catch (e) {
    if (e && e.status === 401 && !e.soft) throw e;
    console.warn("refreshOverviewStatus", e);
  }
  if (render) {
    try { renderStats(); } catch (_) {}
    try { renderMaintainer(); } catch (_) {}
    try { renderModelHealthInfo(); } catch (_) {}
    try { renderStoreConn("overview-conn"); } catch (_) {}
  }
  return statusCache;
}

async function loadDashboard() {
  const page = document.body.dataset.page || pageFromPath(location.pathname) || "overview";
  // Always refresh lightweight status. Full /dashboard is large with 500+ accounts
  // and only needed by overview widgets — skip it on keys/accounts/models/guide.
  try {
    const now = Date.now();
    if (!statusCache || (now - _statusFetchedAt) > 5000) {
      statusCache = await api("/status");
      _statusFetchedAt = now;
      if (window.G2A && G2A.state) G2A.state.status = statusCache;
      // Keep dash fields aligned so overview text switches immediately.
      if (statusCache) {
        dashCache = dashCache || {};
        if (statusCache.token_maintainer) dashCache.token_maintainer = statusCache.token_maintainer;
        if (statusCache.model_health) dashCache.model_health = statusCache.model_health;
        if (statusCache.pool) dashCache.pool = Object.assign({}, dashCache.pool || {}, statusCache.pool);
        if (statusCache.settings) dashCache.settings = Object.assign({}, dashCache.settings || {}, statusCache.settings);
        if (statusCache.accounts) dashCache.accounts = Object.assign({}, dashCache.accounts || {}, statusCache.accounts);
      }
    }
  } catch (e) {
    if (e && e.status === 401) throw e;
    console.warn("status failed", e);
  }

  if (page === "overview") {
    // Paint from /status immediately. /dashboard is optional enrichment only.
    try { renderStats(); } catch (e) { console.error(e); }
    try { renderMaintainer(); } catch (e) { console.error(e); }
    try { renderModelHealthInfo(); } catch (e) { console.error(e); }
    try { renderStoreConn("overview-conn"); } catch (e) {}
    try {
      const dash = await api("/dashboard");
      dashCache = dash;
      if (window.G2A && G2A.state) G2A.state.dashboard = dashCache;
      try { renderStats(); } catch (e) { console.error(e); }
      try { renderMaintainer(); } catch (e) { console.error(e); }
      try { renderModelHealthInfo(); } catch (e) { console.error(e); }
      try { renderStoreConn("overview-conn"); } catch (e) {}
    } catch (e) {
      // Network blips / busy workers should not break overview.
      console.warn("dashboard failed", e);
      if (e && e.status === 401 && !e.soft) throw e;
      // Keep last dashCache if any; stats already rendered from status.
    }
  } else if (page === "keys") {
    await Promise.resolve(renderKeys());
  } else if (page === "accounts") {
    await Promise.resolve(renderAccounts());
  } else if (page === "usage") {
    try { await loadUsage(); } catch (e) { console.warn(e); }
  } else if (page === "logs") {
    try { await loadAdminLogs({ reset: false }); } catch (e) { console.warn(e); }
  } else if (page === "models") {
    // Models page intentionally skips /dashboard (large). Load the dedicated
    // /models catalog so local extras like grok-build are visible.
    try { await loadModels(); } catch (e) { console.warn(e); }
    try { renderModelHealthInfo(); } catch (e) {}
  } else if (page === "guide") {
    try { renderGuide(); } catch (e) {}
  }

  try {
    const st = statusCache || {};
    const ver = st.version || (dashCache && dashCache.version) || "";
    if ($("app-version") && ver) $("app-version").textContent = "v" + ver;
    const pill = $("status-pill");
    if (pill) {
      const mode = st.account_mode || (dashCache && dashCache.account_mode) || "";
      const live = (st.accounts && st.accounts.active_count) ?? (st.pool && st.pool.live) ?? (dashCache && dashCache.pool && dashCache.pool.live);
      const email = st.credentials_email || "";
      pill.className = "g2a-tag" + (st.credentials_ok ? " ok" : "");
      pill.textContent = [email, mode, live != null ? ("账号 " + live) : ""].filter(Boolean).join(" · ") || "—";
    }
  } catch (_) {}
}

function fmtNum(n) {
  const v = Number(n || 0);
  if (!Number.isFinite(v)) return "0";
  if (Math.abs(v) >= 1e9) return (v / 1e9).toFixed(2) + "B";
  if (Math.abs(v) >= 1e6) return (v / 1e6).toFixed(2) + "M";
  if (Math.abs(v) >= 1e4) return (v / 1e3).toFixed(1) + "k";
  return String(Math.round(v));
}

function renderStats() {
  const s = statusCache || {};
  const d = dashCache || {};
  const pool = d.pool || s.pool || {};
  const credOk = !(d.credentials && d.credentials.error) && (s.credentials_ok || (pool.live > 0));
  const pill = $("status-pill");
  if (credOk) {
    pill.className = "g2a-tag ok";
    pill.textContent = "● 已登录 " + (s.credentials_email || "") + " · " + (d.account_mode || s.account_mode || "");
  } else {
    pill.className = "g2a-tag bad";
    pill.textContent = "● 未登录 / 凭证异常";
  }
  const keys = d.keys || s.keys || {};
  const acc = d.accounts || s.accounts || {};
  const tm = d.token_maintainer || s.token_maintainer || {};
  const lastTm = tm.last || {};
  const rem = (tm.min_remaining_sec != null ? tm.min_remaining_sec : lastTm.min_remaining_sec);
  const nextWait = (tm.next_wait_sec != null ? tm.next_wait_sec : (lastTm.next_wait_sec != null ? lastTm.next_wait_sec : tm.interval_sec));
  const remLabel = (rem == null || rem === "")
    ? "—"
    : (Number(rem) < 0 ? ("已过期") : fmtRemaining(Date.now() / 1000 + Number(rem)));
  const lastRef = (lastTm.refresh && lastTm.refresh.refreshed != null) ? lastTm.refresh.refreshed : null;
  $("stats-grid").innerHTML = `
    <div class="stat"><div class="label">API Base</div><div class="value mono">${esc(d.api_base || s.api_base || "")}</div></div>
    <div class="stat"><div class="label">CLI 版本</div><div class="value mono">${esc(d.cli_version || s.cli_version || "")}</div>
      <div class="sub">上游 ${esc(d.upstream || s.upstream || "")}</div></div>
    <div class="stat"><div class="label">账号池</div><div class="value">${pool.total ?? acc.account_count ?? 0} 总量 · ${pool.live ?? pool.enabled ?? acc.active_count ?? 0} 可轮询</div>
      <div class="sub">模式 ${esc(d.account_mode || s.account_mode || "—")} · 冷却 ${pool.in_cooldown ?? 0} · 过期 ${pool.expired ?? 0} · 模型封禁 ${pool.model_blocked ?? 0} · 额度禁用 ${pool.quota_disabled ?? 0} · 禁用 ${pool.disabled ?? 0}</div></div>
    <div class="stat"><div class="label">API Keys</div><div class="value">${keys.enabled ?? 0} 启用 / ${keys.total ?? 0}</div>
      <div class="sub">请求累计 ${keys.total_requests ?? 0} · 鉴权 ${keys.auth_required ? "开启" : "关闭"}</div></div>
    <div class="stat"><div class="label">今日用量</div><div class="value mono">${fmtNum((d.usage || s.usage || {}).today_tokens || 0)} token</div>
      <div class="sub">请求 ${(d.usage || s.usage || {}).today_requests ?? 0} · 累计 ${(d.usage || s.usage || {}).total_tokens ?? 0} token</div></div>
    <div class="stat"><div class="label">Token 自动续期</div><div class="value">${(tm.running || tm.cluster_running || tm.leader_running) ? "运行中" : (tm.enabled === false ? "已关闭" : (tm.enabled ? "已启用" : "未运行"))}</div>
      <div class="sub">最短剩余 ${esc(remLabel)} · 下次 ${nextWait ?? "—"}s${lastRef != null ? ` · 上次刷新 ${lastRef}` : ""}${lastTm.at ? ` · ${fmtTime(lastTm.at)}` : ""}</div></div>
  `;
}

function renderMaintainer() {
  const d = dashCache || {};
  const s = statusCache || {};
  const tm = d.token_maintainer || s.token_maintainer || {};
  const settings = d.settings || s.settings || {};
  const enabled = tm.enabled !== false && settings.token_maintain_enabled !== false;
  const pill = $("maintainer-pill");
  const info = $("maintainer-info");
  const chk = $("chk-token-maintain");
  if (chk && document.activeElement !== chk) chk.checked = !!enabled;
  if (!pill || !info) return;
  if (!enabled) {
    pill.className = "g2a-tag warn";
    pill.textContent = "● 已关闭";
  } else if (tm.running || tm.cluster_running || tm.leader_running) {
    pill.className = "g2a-tag ok";
    pill.textContent = "● 自动续期运行中";
  } else if (tm.enabled) {
    // multi-worker: this response may come from a non-leader process
    pill.className = "g2a-tag ok";
    pill.textContent = "● 已启用（后台）";
  } else {
    pill.className = "g2a-tag bad";
    pill.textContent = "● 未运行";
  }
  const last = tm.last || {};
  const refresh = (last && last.refresh) || {};
  const refreshed = refresh.refreshed;
  const attempted = refresh.attempted;
  const deleted = refresh.deleted ?? refresh.invalidated;
  const rem = (tm.min_remaining_sec != null ? tm.min_remaining_sec : last.min_remaining_sec);
  const nextWait = (tm.next_wait_sec != null ? tm.next_wait_sec : (last.next_wait_sec != null ? last.next_wait_sec : tm.interval_sec));
  const remTxt = (rem == null || rem === "")
    ? "—"
    : (Number(rem) < 0 ? ("已过期 " + fmtRemaining(Date.now() / 1000 + Number(rem)).replace(/^-/, "")) : fmtRemaining(Date.now() / 1000 + Number(rem)));
  const lastRefreshTxt = (refreshed == null && attempted == null)
    ? "上次刷新 —"
    : `上次刷新 ${refreshed ?? 0} 个` + (attempted != null ? ` / 尝试 ${attempted}` : "") + (deleted ? ` · 删除 ${deleted}` : "");
  info.textContent = [
    enabled ? "开关: 开" : "开关: 关",
    `最短剩余: ${remTxt}`,
    enabled ? `下次检查约 ${nextWait ?? "—"}s` : "后台任务已暂停",
    lastRefreshTxt,
    last.at ? `于 ${fmtTime(last.at)}` : null,
  ].filter(Boolean).join(" · ");
}

function renderKeys() {
  const tbody = $("keys-tbody");
  if (tbody) tbody.innerHTML = `<tr><td colspan="6" class="g2a-muted">加载 API Keys…</td></tr>`;
  return api("/keys").then((data) => {
    const body = $("keys-tbody");
    if (!body) return;
    const keys = data.keys || [];
    const src = data.store_source || data.store_backend || "";
    window.__g2aKeysStore = { source: src };
    if ($("page-sub") && document.body && document.body.dataset.page === "keys") {
      $("page-sub").textContent = src === "postgres"
        ? "创建、复制、停用客户端访问密钥 · 数据源：数据库"
        : "创建、复制、停用客户端访问密钥";
    }
    keysCache = {};
    keys.forEach((k) => { keysCache[k.id] = k; });
    if (!keys.length) {
      body.innerHTML = `<tr><td colspan="6" class="g2a-muted">暂无 Key。创建后客户端访问 /v1 将需要鉴权。</td></tr>`;
      return;
    }
    body.innerHTML = keys.map((k) => {
      const canCopy = !!(k.secret || k.key);
      return `
      <tr>
        <td>${esc(k.name)}<div class="g2a-muted" style="font-size:0.75rem">${esc(k.note || "")}</div></td>
        <td class="mono" title="${canCopy ? "点击复制完整 Key" : "缺少完整 Key，需重新生成"}">${esc(k.prefix)}…</td>
        <td>${k.enabled ? '<span class="g2a-tag ok">启用</span>' : '<span class="g2a-tag bad">停用</span>'}</td>
        <td>${k.request_count || 0}</td>
        <td class="g2a-muted">${fmtTime(k.created_at)}</td>
        <td class="g2a-actions">
          <button class="g2a-btn g2a-btn-primary g2a-btn-sm" data-act="copy" data-id="${esc(k.id)}">${canCopy ? "复制" : "重建复制"}</button>
          <button class="g2a-btn g2a-btn-default g2a-btn-sm" data-act="toggle" data-id="${esc(k.id)}" data-on="${k.enabled ? 0 : 1}">${k.enabled ? "停用" : "启用"}</button>
          <button class="g2a-btn g2a-btn-danger g2a-btn-sm" data-act="del" data-id="${esc(k.id)}">删除</button>
        </td>
      </tr>`;
    }).join("");
  }).catch((e) => {
    const body = $("keys-tbody");
    if (body) body.innerHTML = `<tr><td colspan="6" class="g2a-muted">加载失败：${esc(e.message || e)}</td></tr>`;
    toast(e.message || "加载 Keys 失败", false);
  });
}

function fmtQuotaCell(p, liveQuota) {
  const q = liveQuota || p.last_quota || null;
  const poolDisabled = p.enabled === false || p.disabled_for_quota || !!(liveQuota && liveQuota.pool_disabled);
  if (!q) {
    return `<span class="g2a-muted">未查询</span>
      <div style="margin-top:4px"><button class="g2a-btn g2a-btn-default g2a-btn-sm" data-act="quota-one" data-id="${esc(p.id || "")}">查询</button></div>`;
  }
  if (q.error && !q.summary) {
    return `<span class="g2a-tag bad">查询失败</span><div class="g2a-muted" style="font-size:0.72rem;margin-top:4px">${esc(q.error)}</div>`;
  }
  const exhausted = q.exhausted || p.disabled_for_quota;
  const summary = (q.display && q.display.summary) || q.summary || "—";
  let pill;
  if (exhausted) pill = '<span class="g2a-tag bad">额度耗尽</span>';
  else if (poolDisabled) pill = '<span class="g2a-tag warn">禁用·不计入汇总</span>';
  else if (q.unlimited_or_free) pill = '<span class="g2a-tag ok">免费/促销</span>';
  else pill = '<span class="g2a-tag ok">有额度</span>';
  const detail = exhausted && p.disabled_reason
    ? `<div class="g2a-muted" style="font-size:0.72rem;margin-top:4px">${esc(p.disabled_reason)}</div>`
    : `<div class="g2a-muted" style="font-size:0.72rem;margin-top:4px">${esc(summary)}</div>`;
  return `${pill}${detail}`;
}



const ACCOUNT_STATUS_FILTERS = [
  { key: "", label: "全部", tone: "" },
  { key: "live", label: "轮询中", tone: "ok" },
  { key: "cooldown", label: "冷却中", tone: "warn" },
  { key: "model_blocked", label: "模型封禁", tone: "warn" },
  { key: "quota_disabled", label: "额度禁用", tone: "bad" },
  { key: "disabled", label: "已禁用", tone: "bad" },
  { key: "expired", label: "过期", tone: "bad" },
];

function accountStatusFilterLabel(key) {
  const hit = ACCOUNT_STATUS_FILTERS.find((x) => x.key === (key || ""));
  return hit ? hit.label : (key || "");
}

function setAccountStatusFilter(key, { reload = true } = {}) {
  accountsStatusFilter = key || "";
  try { localStorage.setItem("g2a_accounts_status_filter", accountsStatusFilter); } catch (_) {}
  if ($("acc-filter-status")) {
    try { $("acc-filter-status").value = accountsStatusFilter; } catch (_) {}
  }
  try { renderAccountStatusChips(); } catch (_) {}
  if (reload) loadAccountsPage({ reset: true });
}

function renderAccountStatusChips() {
  const el = $("acc-status-chips");
  if (!el) return;
  const cur = accountsStatusFilter || "";
  el.innerHTML = ACCOUNT_STATUS_FILTERS.map((s) => {
    const active = (s.key || "") === cur;
    const cls = ["g2a-btn", "g2a-btn-sm", active ? "g2a-btn-primary" : "g2a-btn-default"].join(" ");
    const title = s.key
      ? `只显示「${s.label}」账号；点「筛选全选」可选中该状态下全部账号`
      : "显示全部状态";
    return `<button type="button" class="${cls}" data-acc-status="${esc(s.key)}" title="${esc(title)}">${esc(s.label)}</button>`;
  }).join("");
  el.querySelectorAll("[data-acc-status]").forEach((btn) => {
    btn.onclick = () => setAccountStatusFilter(btn.getAttribute("data-acc-status") || "");
  });
}

async function selectAllFilteredAccounts() {
  const btn = $("btn-acc-select-all-filtered");
  const q = (accountsSearchQuery || ($("acc-search") && $("acc-search").value) || "").trim();
  const sort = accountsSort || "newest";
  const ssoQs = (accountsSsoFilter === "1" || accountsSsoFilter === "0")
    ? `&has_sso=${encodeURIComponent(accountsSsoFilter === "1" ? "true" : "false")}`
    : "";
  const statusQs = accountsStatusFilter
    ? `&status=${encodeURIComponent(accountsStatusFilter)}`
    : "";
  if (btn) {
    btn.disabled = true;
    if (!btn.dataset.label) btn.dataset.label = btn.textContent;
    btn.textContent = "选择中…";
  }
  try {
    const data = await api(
      `/accounts?page=1&page_size=20000&ids_only=1` +
      `&q=${encodeURIComponent(q)}&sort=${encodeURIComponent(sort)}${ssoQs}${statusQs}`
    );
    const ids = Array.isArray(data.ids) && data.ids.length
      ? data.ids
      : (Array.isArray(data.accounts) ? data.accounts.map((a) => a && a.id).filter(Boolean) : []);
    selectedAccountIds = new Set(ids.map(String));
    try { renderAccountsPage(); } catch (_) {}
    const st = accountStatusFilterLabel(accountsStatusFilter);
    toast(`已选中筛选结果 ${selectedAccountIds.size} 个` + (st && st !== "全部" ? `（${st}）` : ""));
  } catch (e) {
    toast(e.message || "筛选全选失败", false);
  } finally {
    if (btn) {
      btn.disabled = false;
      btn.textContent = btn.dataset.label || "筛选全选";
    }
  }
}

function getFilteredAccounts() {
  // Server-side filtering/pagination: accountsList holds current page rows.
  return accountsList.slice();
}


function updateAccountSelectionInfo(filteredCount, pageCount) {
  const el = $("acc-selection-info");
  if (!el) return;
  const selected = selectedAccountIds.size;
  const q = (accountsSearchQuery || "").trim();
  const st = (accountsStatusFilter || "").trim();
  const total = accountsTotal || accountsList.length;
  const stLabel = (typeof accountStatusFilterLabel === "function") ? accountStatusFilterLabel(st) : st;
  const bits = [`已选 ${selected} 个`];
  if (q || st || accountsSsoFilter) bits.push(`筛选 ${total}`);
  else bits.push(`全部 ${total}`);
  bits.push(`本页 ${pageCount}`);
  if (stLabel && st) bits.push(`状态:${stLabel}`);
  if (accountsSsoFilter === "1") bits.push("有SSO");
  if (accountsSsoFilter === "0") bits.push("无SSO");
  if (q) bits.push(`关键词:${q}`);
  el.textContent = bits.join(" · ");
  const pageCheck = $("acc-check-page");
  if (pageCheck) {
    const pageIds = Array.from(document.querySelectorAll(".acc-check-one")).map(x => x.dataset.id);
    const selectedOnPage = pageIds.filter(id => selectedAccountIds.has(id)).length;
    pageCheck.checked = pageIds.length > 0 && selectedOnPage === pageIds.length;
    pageCheck.indeterminate = selectedOnPage > 0 && selectedOnPage < pageIds.length;
  }
}

function renderAccountsPage() {
  const pageItems = accountsList.slice();
  const totalPages = Math.max(1, accountsTotalPages || 1);
  accountsPage = Math.max(1, Math.min(accountsPage || 1, totalPages));
  const __empty = $("accounts-empty");
  if (__empty) {
    const hide = (accountsTotal || accountsList.length) > 0;
    __empty.classList.toggle("hidden", hide);
    __empty.style.display = hide ? "none" : "block";
  }
  const tbody = $("accounts-tbody");
  if (!tbody) return;
  if (accountsLoading && !pageItems.length) {
    tbody.innerHTML = `<tr><td colspan="9" class="g2a-muted">加载账号中…</td></tr>`;
  } else {
    tbody.innerHTML = pageItems.map((a) => {
      const p = a._pool || { id: a.id };
      const enabled = p.enabled !== false;
      const poolLabel = poolStatusLabel(a, p);
      const usage = `${p.success_count || 0}√ / ${p.fail_count || 0}× · 共 ${p.request_count || 0}`;
      const refreshPill = a.has_refresh_token
        ? '<span class="g2a-tag ok" title="可自动 refresh">可自动续期</span>'
        : '<span class="g2a-tag warn">无 refresh</span>';
      const ssoPill = a.has_sso
        ? '<span class="g2a-tag ok" title="账号库已保存 SSO cookie">有SSO</span>'
        : '<span class="g2a-tag" title="未保存 SSO cookie">无SSO</span>';
      const liveQ = quotaCache[a.id];
      const probeCell = fmtProbeCell(p.last_probe, p.last_error, p.blocked_model_ids);
      const checked = selectedAccountIds.has(a.id) ? "checked" : "";
      const expiryCell = fmtExpiry(a.expires_at);
      return `
    <tr data-acc-id="${esc(a.id)}">
      <td><input type="checkbox" class="acc-check-one" data-id="${esc(a.id)}" ${checked} /></td>
      <td>${esc(a.email || "—")}<div class="muted mono" style="font-size:0.72rem">${esc(a.id)}</div></td>
      <td>${a.expired ? '<span class="g2a-tag bad">已过期</span>' : '<span class="g2a-tag ok">有效</span>'}</td>
      <td>${poolLabel}</td>
      <td class="g2a-muted" style="font-size:0.8rem">${usage}</td>
      <td style="font-size:0.82rem;min-width:140px">${fmtQuotaCell({ ...p, id: a.id }, liveQ)}</td>
      <td style="font-size:0.78rem;min-width:160px">${probeCell}</td>
      <td style="font-size:0.8rem;min-width:150px">
        ${expiryCell}
        <div style="margin-top:6px">${refreshPill} ${ssoPill}</div>
      </td>
      <td class="g2a-actions">
        <button class="g2a-btn g2a-btn-default g2a-btn-sm" data-act="renew-one" data-id="${esc(a.id)}" ${a.has_refresh_token ? "" : "disabled title=\"无 refresh_token，无法续期\""}>续期</button>
        <button class="g2a-btn g2a-btn-default g2a-btn-sm" data-act="probe-one" data-id="${esc(a.id)}">模型测试</button>
        <button class="g2a-btn g2a-btn-default g2a-btn-sm" data-act="quota-one" data-id="${esc(a.id)}">额度</button>
        <button class="g2a-btn g2a-btn-default g2a-btn-sm" data-act="toggle-acc" data-id="${esc(a.id)}" data-on="${enabled ? 0 : 1}">${enabled ? "禁用" : "启用"}</button>
        <button class="g2a-btn g2a-btn-danger g2a-btn-sm" data-act="rm-acc" data-id="${esc(a.id)}">移除</button>
      </td>
    </tr>`;
    }).join("") || `<tr><td colspan="9" class="g2a-muted">${(accountsTotal || 0) ? "无匹配账号" : "无账号"}</td></tr>`;
  }
  if ($("acc-page-info")) {
    const src = (window.__g2aAccountsStore && window.__g2aAccountsStore.source) || "";
    const srcTxt = src === "postgres" ? " · 数据库" : (src ? ` · ${src}` : "");
    $("acc-page-info").textContent = `${accountsPage} / ${totalPages} (本页 ${pageItems.length} / 共 ${accountsTotal || 0} 个${srcTxt})`;
  }
  if ($("acc-page-prev")) $("acc-page-prev").disabled = accountsPage <= 1 || accountsLoading;
  if ($("acc-page-next")) $("acc-page-next").disabled = accountsPage >= totalPages || accountsLoading;
  updateAccountSelectionInfo(accountsTotal || 0, pageItems.length);
}

function renderAccounts() {
  return loadAccountsPage({ reset: false });
}

let _quotaCacheHydrated = false;
async function hydrateQuotaCacheFromDB() {
  // Page rows already embed last_quota from DB — just project them into quotaCache.
  // Do NOT call /accounts/quota?cached=1 (that scans the whole pool and freezes UI).
  try {
    let changed = false;
    (accountsList || []).forEach((a) => {
      const lq = a && a._pool && a._pool.last_quota;
      if (a && a.id && lq && typeof lq === "object") {
        const prev = quotaCache[a.id];
        quotaCache[a.id] = { ...lq, account_id: a.id, cached: true };
        if (!prev) changed = true;
      }
    });
    _quotaCacheHydrated = true;
    if (changed) {
      try { renderAccountsPage(); } catch (_) {}
    }
  } catch (_) {
    // ignore
  }
}

async function loadAccountsPage({ reset = false } = {}) {
  const tbody = $("accounts-tbody");
  if (reset) accountsPage = 1;
  if (tbody) tbody.innerHTML = `<tr><td colspan="9" class="g2a-muted">加载账号中…</td></tr>`;
  accountsLoading = true;
  const seq = ++accountsLoadSeq;
  const q = (accountsSearchQuery || ($("acc-search") && $("acc-search").value) || "").trim();
  accountsSearchQuery = q;
  if ($("acc-sort") && $("acc-sort").value) accountsSort = $("acc-sort").value;
  if ($("acc-filter-sso")) accountsSsoFilter = $("acc-filter-sso").value || "";
  if ($("acc-filter-status")) accountsStatusFilter = $("acc-filter-status").value || accountsStatusFilter || "";
  const sort = accountsSort || "newest";
  const pageSize = accountsPageSize || 25;
  const page = accountsPage || 1;
  const ssoQs = (accountsSsoFilter === "1" || accountsSsoFilter === "0")
    ? `&has_sso=${encodeURIComponent(accountsSsoFilter === "1" ? "true" : "false")}`
    : "";
  const statusQs = accountsStatusFilter
    ? `&status=${encodeURIComponent(accountsStatusFilter)}`
    : "";
  try {
    const data = await api(
      `/accounts?page=${encodeURIComponent(page)}&page_size=${encodeURIComponent(pageSize)}` +
      `&q=${encodeURIComponent(q)}&sort=${encodeURIComponent(sort)}${ssoQs}${statusQs}`
    );
    if (seq !== accountsLoadSeq) return;
    const rawAccounts = Array.isArray(data && data.accounts) ? data.accounts : [];
    accountsList = rawAccounts.map((a) => ({ ...a, _pool: a._pool || { id: a.id } }));
    // hydrate quota cache from DB-backed last_quota so UI shows cached status immediately
    accountsList.forEach((a) => {
      const lq = a && a._pool && a._pool.last_quota;
      if (a && a.id && lq && typeof lq === "object") {
        quotaCache[a.id] = { ...lq, account_id: a.id, cached: true };
      }
    });
    accountsTotal = Number(data.total != null ? data.total : (data.account_count || accountsList.length)) || 0;
    accountsTotalPages = Number(data.total_pages || Math.max(1, Math.ceil((accountsTotal || 0) / pageSize))) || 1;
    accountsPage = Number(data.page || page) || 1;
    accountsPageSize = Number(data.page_size || pageSize) || pageSize;
    if (data.sort) {
      accountsSort = data.sort;
      if ($("acc-sort") && $("acc-sort").value !== data.sort) {
        try { $("acc-sort").value = data.sort; } catch (_) {}
      }
    }
    if (data.pool && statusCache) statusCache.pool = Object.assign({}, statusCache.pool || {}, data.pool);
    // Remember durable store source so UI can show "数据库" instead of auth.json.
    window.__g2aAccountsStore = {
      source: data.store_source || data.store_backend || "file",
      auth_file_role: data.auth_file_role || (data.store_source === "postgres" ? "mirror" : "primary"),
    };
    if ($("page-sub") && document.body && document.body.dataset.page === "accounts") {
      const src = window.__g2aAccountsStore.source;
      $("page-sub").textContent = src === "postgres"
        ? "Grok 账号、设备码登录、额度与导入导出 · 数据源：数据库"
        : "Grok 账号、设备码登录、额度与导入导出";
    }
    console.info(
      "[accounts] page", accountsPage, "/", accountsTotalPages,
      "rows", accountsList.length, "total", accountsTotal,
      "store", window.__g2aAccountsStore.source
    );
    accountsLoading = false;
    try { renderAccountStatusChips(); } catch (_) {}
    renderAccountsPage();
    hydrateQuotaCacheFromDB();
  } catch (e) {
    if (seq !== accountsLoadSeq) return;
    accountsLoading = false;
    console.error("[accounts] load failed", e);
    toast(e.message || "加载账号失败", false);
    if (tbody) tbody.innerHTML = `<tr><td colspan="9" class="g2a-muted">加载失败：${esc(e.message || e)}</td></tr>`;
  }
}


function applyAccountSearch(resetPage = true) {
  accountsSearchQuery = $("acc-search") ? $("acc-search").value.trim() : "";
  if (resetPage) accountsPage = 1;
  loadAccountsPage({ reset: !!resetPage });
}


function setPageSelection(checked) {
  document.querySelectorAll(".acc-check-one").forEach(el => {
    const id = el.dataset.id;
    if (!id) return;
    el.checked = !!checked;
    if (checked) selectedAccountIds.add(id);
    else selectedAccountIds.delete(id);
  });
  updateAccountSelectionInfo(getFilteredAccounts().length, document.querySelectorAll(".acc-check-one").length);
}

function setFilteredSelection(checked) {
  const list = getFilteredAccounts();
  list.forEach(a => {
    if (!a.id) return;
    if (checked) selectedAccountIds.add(a.id);
    else selectedAccountIds.delete(a.id);
  });
  renderAccountsPage();
}

async function deleteSelectedAccounts() {
  const ids = Array.from(selectedAccountIds);
  if (!ids.length) {
    toast("请先勾选要删除的账号", false);
    return;
  }
  if (!confirm(`确定删除选中的 ${ids.length} 个账号？将从数据库与本地镜像同步删除。`)) return;
  try {
    const r = await api("/accounts/delete-batch", {
      method: "POST",
      body: JSON.stringify({ ids }),
    });
    selectedAccountIds.clear();
    toast(`已删除 ${r.removed_count || 0} 个` + (r.missing_count ? `，未找到 ${r.missing_count}` : ""));
    statusCache = await api("/status");
    await loadDashboard();
  } catch (e) {
    toast(e.message, false);
  }
}


function upsertAccountInList(partial) {
  if (!partial || !partial.id) return null;
  const id = partial.id;
  let updated = null;
  let found = false;
  accountsList = (accountsList || []).map((a) => {
    if (a.id !== id) return a;
    found = true;
    const next = { ...a, ...partial };
    if (partial._pool || a._pool) {
      next._pool = { ...(a._pool || { id }), ...(partial._pool || {}) };
    }
    // keep expired flag coherent when expires_at changes
    if (partial.expires_at != null) {
      const exp = Number(partial.expires_at);
      if (Number.isFinite(exp)) next.expired = exp > 0 && exp * (exp > 1e12 ? 1 : 1000) <= Date.now();
      // if seconds
      if (Number.isFinite(exp) && exp < 1e12) next.expired = exp * 1000 <= Date.now();
    }
    updated = next;
    return next;
  });
  if (!found) {
    updated = { id, _pool: { id }, ...partial, _pool: { id, ...(partial._pool || {}) } };
    accountsList = [updated, ...(accountsList || [])];
  }
  return updated;
}

function renderOneAccountRow(account) {
  if (!account || !account.id) return "";
  const a = account;
  const p = a._pool || { id: a.id };
  const enabled = p.enabled !== false;
  const poolLabel = poolStatusLabel(a, p);
  const usage = `${p.success_count || 0}√ / ${p.fail_count || 0}× · 共 ${p.request_count || 0}`;
  const refreshPill = a.has_refresh_token
    ? '<span class="g2a-tag ok" title="可自动 refresh">可自动续期</span>'
    : '<span class="g2a-tag warn">无 refresh</span>';
  const ssoPill = a.has_sso
    ? '<span class="g2a-tag ok" title="账号库已保存 SSO cookie">SSO</span>'
    : '<span class="g2a-tag" title="未保存 SSO cookie">无SSO</span>';
  const liveQ = quotaCache[a.id];
  const probeCell = fmtProbeCell(p.last_probe, p.last_error, p.blocked_model_ids);
  const checked = selectedAccountIds.has(a.id) ? "checked" : "";
  const expiryCell = fmtExpiry(a.expires_at);
  return `
    <tr data-acc-id="${esc(a.id)}">
      <td><input type="checkbox" class="acc-check-one" data-id="${esc(a.id)}" ${checked} /></td>
      <td>${esc(a.email || "—")}<div class="muted mono" style="font-size:0.72rem">${esc(a.id)}</div></td>
      <td>${a.expired ? '<span class="g2a-tag bad">已过期</span>' : '<span class="g2a-tag ok">有效</span>'}</td>
      <td>${poolLabel}</td>
      <td class="g2a-muted" style="font-size:0.8rem">${usage}</td>
      <td style="font-size:0.82rem;min-width:140px">${fmtQuotaCell({ ...p, id: a.id }, liveQ)}</td>
      <td style="font-size:0.78rem;min-width:160px">${probeCell}</td>
      <td style="font-size:0.8rem;min-width:150px">
        ${expiryCell}
        <div style="margin-top:6px">${refreshPill} ${ssoPill}</div>
      </td>
      <td class="g2a-actions">
        <button class="g2a-btn g2a-btn-default g2a-btn-sm" data-act="renew-one" data-id="${esc(a.id)}" ${a.has_refresh_token ? "" : "disabled title=\"无 refresh_token，无法续期\""}>续期</button>
        <button class="g2a-btn g2a-btn-default g2a-btn-sm" data-act="probe-one" data-id="${esc(a.id)}">模型测试</button>
        <button class="g2a-btn g2a-btn-default g2a-btn-sm" data-act="quota-one" data-id="${esc(a.id)}">额度</button>
        <button class="g2a-btn g2a-btn-default g2a-btn-sm" data-act="toggle-acc" data-id="${esc(a.id)}" data-on="${enabled ? 0 : 1}">${enabled ? "禁用" : "启用"}</button>
        <button class="g2a-btn g2a-btn-danger g2a-btn-sm" data-act="rm-acc" data-id="${esc(a.id)}">移除</button>
      </td>
    </tr>`;
}

function patchAccountRowById(id) {
  if (!id) return;
  const row = document.querySelector(`tr[data-acc-id="${CSS.escape(String(id))}"]`);
  const acc = (accountsList || []).find((a) => a.id === id);
  if (!acc) return;
  const html = renderOneAccountRow(acc);
  if (row) {
    row.outerHTML = html;
  } else {
    // fallback: only re-render page if row not in DOM
    try { renderAccountsPage(); } catch (_) {}
  }
}

function refreshOneAccountLocal(id, patch) {
  // Local-only UI update. NEVER reload accounts list/page component.
  if (patch) upsertAccountInList({ id, ...patch });
  else upsertAccountInList({ id });
  patchAccountRowById(id);
}

function setRowBusy(id, busy, label) {
  const row = document.querySelector(`tr[data-acc-id="${CSS.escape(String(id))}"]`);
  if (row) {
    row.classList.toggle("is-busy", !!busy);
    if (busy && label) row.dataset.busyLabel = label;
    else delete row.dataset.busyLabel;
  }
  const buttons = row
    ? row.querySelectorAll("button[data-id]")
    : document.querySelectorAll(`button[data-id="${CSS.escape(String(id))}"]`);
  buttons.forEach((btn) => {
    btn.disabled = !!busy;
    if (busy) {
      if (!btn.dataset.label) btn.dataset.label = btn.textContent;
      if (label && (
        (label.includes("续期") && btn.dataset.act === "renew-one") ||
        ((label.includes("探测") || label.includes("测试") || label.includes("测活")) && btn.dataset.act === "probe-one") ||
        (label.includes("查询") && btn.dataset.act === "quota-one") ||
        (label.includes("处理") && btn.dataset.act === "toggle-acc") ||
        (label.includes("移除") && btn.dataset.act === "rm-acc")
      )) {
        btn.textContent = label;
      }
    } else if (btn.dataset.label) {
      btn.textContent = btn.dataset.label;
      delete btn.dataset.label;
    }
  });
  // Live status cell hint while busy.
  if (row) {
    const statusCell = row.children && row.children[3];
    if (statusCell) {
      if (busy && label) {
        if (!statusCell.dataset.prevHtml) statusCell.dataset.prevHtml = statusCell.innerHTML;
        statusCell.innerHTML = `<span class="g2a-tag warn">${esc(label)}</span>`;
      } else if (statusCell.dataset.prevHtml) {
        // Will be replaced by patchAccountRowById shortly; keep fallback.
        delete statusCell.dataset.prevHtml;
      }
    }
  }
}

function poolPatchFromProbeResponse(r) {
  const res = (r && (r.result || r)) || {};
  const pool = (r && r.pool) || {};
  const ok = !!(r && (r.ok || res.available));
  const nowSec = Math.floor(Date.now() / 1000);
  const patch = {
    last_probe: pool.last_probe || {
      available: ok,
      ok,
      model: res.model || pool.cooldown_model || null,
      latency_ms: res.latency_ms,
      status_code: res.status_code,
      error: res.error || (!ok ? (r && r.error) : null) || null,
      probed_at: nowSec,
    },
    last_error: ok ? (pool.last_error || null) : (pool.last_error || res.error || (r && r.error) || null),
    last_probe_status: pool.last_probe_status || (ok ? "normal" : "error"),
    consecutive_fails: pool.consecutive_fails != null ? pool.consecutive_fails : (ok ? 0 : undefined),
    probe_fail_streak: pool.probe_fail_streak,
    blocked_model_ids: pool.blocked_model_ids,
    disabled_for_quota: pool.disabled_for_quota,
  };
  if (pool.pool_status) patch.pool_status = pool.pool_status;
  if (pool.in_cooldown != null) patch.in_cooldown = !!pool.in_cooldown;
  if (pool.cooldown_count != null) patch.cooldown_count = Number(pool.cooldown_count) || 0;
  if (pool.cooldown_until !== undefined) patch.cooldown_until = pool.cooldown_until;
  if (pool.cooldown_sec !== undefined) patch.cooldown_sec = pool.cooldown_sec;
  if (pool.cooldown_reason !== undefined) patch.cooldown_reason = pool.cooldown_reason;
  if (pool.cooldown_code !== undefined) patch.cooldown_code = pool.cooldown_code;
  if (pool.cooldown_model !== undefined) patch.cooldown_model = pool.cooldown_model;
  if (pool.cooldown_tokens_actual !== undefined) patch.cooldown_tokens_actual = pool.cooldown_tokens_actual;
  if (pool.cooldown_tokens_limit !== undefined) patch.cooldown_tokens_limit = pool.cooldown_tokens_limit;
  if (Array.isArray(pool.status_stack)) patch.status_stack = pool.status_stack;

  // Fallback when backend older / no pool payload.
  if (!r || !r.pool) {
    if (ok) {
      patch.in_cooldown = false;
      patch.cooldown_count = 0;
      patch.status_stack = [];
      patch.cooldown_until = null;
      patch.cooldown_sec = null;
      patch.pool_status = "normal";
      patch.consecutive_fails = 0;
    } else if (/free-usage-exhausted|free usage|subscription:free-usage/i.test(String(res.error || r.error || ""))) {
      patch.in_cooldown = true;
      patch.pool_status = "cooldown";
      patch.cooldown_count = Math.max(1, Number(patch.cooldown_count || 0) || 1);
      patch.cooldown_code = patch.cooldown_code || "subscription:free-usage-exhausted";
    }
  }
  return patch;
}

function applyAccountLivePatch(id, partial) {
  if (!id) return;
  upsertAccountInList({ id, ...(partial || {}) });
  refreshOneAccountLocal(id, partial || null);
}

async function renewAccounts(ids, { confirmMany = true } = {}) {
  const list = Array.from(new Set((ids || []).map(x => String(x || "").trim()).filter(Boolean)));
  if (!list.length) {
    toast("请先选择要续期的账号", false);
    return;
  }
  if (confirmMany && list.length > 1) {
    if (!confirm(`确认续期选中的 ${list.length} 个账号？将调用 refresh_token 更新 access token。`)) return;
  }
  // Mark all selected rows busy for live feedback.
  list.forEach((id) => setRowBusy(id, true, "续期中"));
  try {
    const r = await api("/accounts/refresh", {
      method: "POST",
      body: JSON.stringify({ force: true, ids: list }),
    });
    const results = r.results || [];
    const byId = new Map();
    results.forEach((x) => {
      const id = x.id || x.account_id || x.auth_key;
      if (id) byId.set(String(id), x);
    });
    let n = 0, failed = 0, skipped = 0;
    list.forEach((id) => {
      const x = byId.get(String(id));
      if (!x) {
        // still clear busy; unknown result
        setRowBusy(id, false);
        return;
      }
      if (x.ok && !x.skipped) {
        n += 1;
        applyAccountLivePatch(id, {
          expires_at: x.expires_at,
          expired: false,
          has_refresh_token: x.has_refresh_token != null ? x.has_refresh_token : true,
          _pool: {
            last_error: null,
          },
        });
      } else if (x.ok && x.skipped) {
        skipped += 1;
        applyAccountLivePatch(id, {
          expires_at: x.expires_at,
          has_refresh_token: x.has_refresh_token,
        });
      } else {
        failed += 1;
        applyAccountLivePatch(id, {
          _pool: {
            last_error: x.error || x.message || "续期失败",
          },
        });
      }
      setRowBusy(id, false);
    });
    let msg = `续期完成：成功 ${n}`;
    if (failed) msg += `，失败 ${failed}`;
    if (skipped) msg += `，跳过 ${skipped}`;
    toast(msg, failed === 0);
  } catch (e) {
    list.forEach((id) => setRowBusy(id, false));
    toast(e.message, false);
  }
}

async function exportSelectedAccounts() {
  const ids = Array.from(selectedAccountIds);
  if (!ids.length) {
    toast("请先勾选要导出的账号", false);
    return;
  }
  return runJsonExportJob({
    mode: "selected",
    ids,
    buttonId: "btn-acc-export-selected",
  });
}

function _downloadTextFile(filename, content, mime) {
  try {
    const blob = new Blob([content || ""], { type: mime || "text/plain;charset=utf-8" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = filename || "export.txt";
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 1500);
    return true;
  } catch (e) {
    console.error(e);
    return false;
  }
}

async function exportSelectedAccountsSso() {
  const ids = Array.from(selectedAccountIds);
  if (!ids.length) {
    toast("请先勾选要导出 SSO 的账号", false);
    return;
  }
  const btn = $("btn-acc-export-sso-selected");
  const includePassword = !!( $("export-include-password") && $("export-include-password").checked );
  if (btn) {
    btn.disabled = true;
    if (!btn.dataset.label) btn.dataset.label = btn.textContent;
    btn.textContent = "导出中…";
  }
  try {
    const data = await api("/accounts/export-sso?download=0", {
      method: "POST",
      body: JSON.stringify({
        ids,
        only_with_sso: true,
        format: "txt",
        include_password: includePassword,
      }),
    });
    if (!data || !data.content) {
      toast("选中账号没有可导出的 SSO", false);
      return;
    }
    const filename = data.filename || "grok2api-accounts-sso-selected.txt";
    if (!_downloadTextFile(filename, data.content, "text/plain;charset=utf-8")) {
      toast("下载失败", false);
      return;
    }
    toast(`已导出 ${data.with_sso || data.count || 0} 条 SSO`, true);
  } catch (e) {
    toast("导出 SSO 失败: " + (e.message || e), false);
  } finally {
    if (btn) {
      btn.disabled = false;
      btn.textContent = btn.dataset.label || "导出选中 SSO";
    }
  }
}

async function exportAllAccountsSso() {
  const btn = $("btn-acc-export-sso-all");
  const includePassword = !!( $("export-include-password") && $("export-include-password").checked );
  if (btn) {
    btn.disabled = true;
    if (!btn.dataset.label) btn.dataset.label = btn.textContent;
    btn.textContent = "导出中…";
  }
  try {
    const qs = `download=0&format=txt&include_password=${includePassword ? 1 : 0}`;
    const data = await api(`/accounts/export-sso?${qs}`);
    if (!data || !data.content) {
      toast("没有带 SSO 的账号可导出", false);
      return;
    }
    const filename = data.filename || "grok2api-accounts-sso.txt";
    if (!_downloadTextFile(filename, data.content, "text/plain;charset=utf-8")) {
      toast("下载失败", false);
      return;
    }
    toast(`已导出 ${data.with_sso || data.count || 0} 条 SSO`, true);
  } catch (e) {
    toast("导出全部 SSO 失败: " + (e.message || e), false);
  } finally {
    if (btn) {
      btn.disabled = false;
      btn.textContent = btn.dataset.label || "导出全部 SSO";
    }
  }
}

// Fallback bindings when page scripts load outside bindSoftNav rebind path.
on("acc-page-prev", "onclick", () => { if (accountsPage > 1 && !accountsLoading) { accountsPage--; loadAccountsPage(); } });
on("acc-page-next", "onclick", () => { if (!accountsLoading && accountsPage < (accountsTotalPages || 1)) { accountsPage++; loadAccountsPage(); } });
on("acc-page-size", "onchange", () => {
  accountsPageSize = parseInt(($("acc-page-size") && $("acc-page-size").value) || "25", 10) || 25;
  accountsPage = 1;
  loadAccountsPage({ reset: true });
});
if ($("acc-sort") && !$("acc-sort").onchange) {
  try {
    const saved = localStorage.getItem("g2a_accounts_sort");
    if (saved) { accountsSort = saved; $("acc-sort").value = saved; }
  } catch (_) {}
  $("acc-sort").onchange = () => {
    accountsSort = ($("acc-sort").value || "newest");
    try { localStorage.setItem("g2a_accounts_sort", accountsSort); } catch (_) {}
    accountsPage = 1;
    loadAccountsPage({ reset: true });
  };
}

if ($("acc-filter-sso") && !$("acc-filter-sso").onchange) {
  try {
    const savedSso = localStorage.getItem("g2a_accounts_sso_filter");
    if (savedSso === "1" || savedSso === "0" || savedSso === "") {
      accountsSsoFilter = savedSso || "";
      $("acc-filter-sso").value = accountsSsoFilter;
    }
  } catch (_) {}
  $("acc-filter-sso").onchange = () => {
    accountsSsoFilter = ($("acc-filter-sso").value || "");
    try { localStorage.setItem("g2a_accounts_sso_filter", accountsSsoFilter); } catch (_) {}
    accountsPage = 1;
    loadAccountsPage({ reset: true });
  };
}
if ($("btn-acc-export-sso-selected") && !$("btn-acc-export-sso-selected").onclick) {
  $("btn-acc-export-sso-selected").onclick = () => exportSelectedAccountsSso();
}
if ($("btn-acc-export-sso-all") && !$("btn-acc-export-sso-all").onclick) {
  $("btn-acc-export-sso-all").onclick = () => exportAllAccountsSso();
}

if ($("btn-acc-search")) $("btn-acc-search").onclick = () => applyAccountSearch(true);
if ($("btn-acc-search-clear")) $("btn-acc-search-clear").onclick = () => {
  if ($("acc-search")) $("acc-search").value = "";
  applyAccountSearch(true);
};
if ($("acc-search")) {
  $("acc-search").addEventListener("keydown", (e) => {
    if (e.key === "Enter") applyAccountSearch(true);
  });
}
if ($("btn-acc-select-page")) $("btn-acc-select-page").onclick = () => setPageSelection(true);
if ($("btn-acc-select-all-filtered")) $("btn-acc-select-all-filtered").onclick = () => { selectAllFilteredAccounts(); };
if ($("btn-acc-select-none")) $("btn-acc-select-none").onclick = () => {
  selectedAccountIds.clear();
  renderAccountsPage();
};
if ($("btn-acc-delete-selected")) $("btn-acc-delete-selected").onclick = () => deleteSelectedAccounts();
if ($("btn-acc-renew-selected")) $("btn-acc-renew-selected").onclick = () => renewAccounts(Array.from(selectedAccountIds));
  if ($("btn-acc-probe-selected")) $("btn-acc-probe-selected").onclick = () => probeAccounts(Array.from(selectedAccountIds));
if ($("btn-acc-export-selected")) $("btn-acc-export-selected").onclick = () => exportSelectedAccounts();
if ($("acc-check-page")) {
  on("acc-check-page", "onchange", (e) => setPageSelection(!!e.target.checked));
}


function poolStatusLabel(a, p) {
  const enabled = p.enabled !== false;
  const stackLen = Array.isArray(p.status_stack) ? p.status_stack.length : 0;
  const cdCount = Number(p.cooldown_count || stackLen || 0) || 0;
  const cooling = !!(p.in_cooldown || cdCount > 0 || stackLen > 0 || p.pool_status === "cooldown");
  const quotaOff = !!p.disabled_for_quota || p.pool_status === "quota_disabled";
  const blockedIds = Array.isArray(p.blocked_model_ids)
    ? p.blocked_model_ids
    : (p.blocked_models && typeof p.blocked_models === "object" ? Object.keys(p.blocked_models) : []);
  const modelBlocked = blockedIds.length > 0 || p.pool_status === "model_blocked";
  const expired = !!(
    p.pool_status === "expired"
    || a.expired
    || p.token_expired_at
    || ["failed","expired","sso_failed","no_sso_removed","no_sso_deleted","sso_attempt"].includes(String(p.last_renew_status || ""))
  );
  const renewFails = Number(p.renew_fail_count || 0) || 0;
  const streak = Number(p.consecutive_fails || 0) || 0;
  const cdCode = p.cooldown_code || "";
  const cdModel = p.cooldown_model || "";
  const cdTok = (p.cooldown_tokens_actual != null && p.cooldown_tokens_limit != null)
    ? `${p.cooldown_tokens_actual}/${p.cooldown_tokens_limit}` : "";
  let poolLabel;
  if (quotaOff) {
    const tip = [p.disabled_reason || "额度耗尽，已移出轮询", p.quota_source ? `source=${p.quota_source}` : ""].filter(Boolean).join(" · ");
    poolLabel = `<span class="g2a-tag bad" title="${esc(tip)}">额度禁用</span>`;
  } else if (expired) {
    const tip = [
      "已过期，已移出轮询",
      renewFails ? `续期失败×${renewFails}` : "",
      p.last_renew_error || p.token_expired_reason || p.last_error || "",
      p.last_renew_status === "no_sso_removed" || p.last_renew_status === "no_sso_deleted" ? "无 SSO，续不上 AT 已删除" : "",
      p.last_renew_status === "sso_failed" ? "SSO 重登失败" : "",
    ].filter(Boolean).join(" · ");
    poolLabel = `<span class="g2a-tag bad" title="${esc(tip)}">过期</span>`;
  } else if (!enabled || p.pool_status === "disabled") {
    const tip = p.disabled_reason || p.last_error || "已禁用，不参与轮询";
    poolLabel = `<span class="g2a-tag bad" title="${esc(tip)}">已禁用</span>`;
  } else if (cooling) {
    const n = cdCount > 0 ? cdCount : 1;
    const tip = [
      "冷却中",
      n > 1 ? `叠加×${n}` : "",
      cdCode ? `code=${cdCode}` : "",
      cdModel ? `model=${cdModel}` : "",
      cdTok ? `tokens ${cdTok}` : "",
      "单次测活成功即恢复正常",
    ].filter(Boolean).join(" · ");
    poolLabel = `<span class="g2a-tag warn" title="${esc(tip)}">冷却中</span>`;
  } else if (modelBlocked) {
    const tip = [
      "模型封禁（账号仍可轮询其他模型）",
      blockedIds.length ? blockedIds.join(", ") : "",
      p.last_error || "",
    ].filter(Boolean).join(" · ");
    const short = blockedIds.length <= 2
      ? blockedIds.join(",")
      : `${blockedIds.slice(0, 2).join(",")}…+${blockedIds.length - 2}`;
    poolLabel = `<span class="g2a-tag warn" title="${esc(tip)}">模型封禁${short ? " · " + esc(short) : ""}</span>`;
  } else if (streak >= 2) {
    poolLabel = `<span class="g2a-tag warn">轮询中 · 连败${streak}</span>`;
  } else {
    poolLabel = '<span class="g2a-tag ok">轮询中</span>';
  }
  return poolLabel;
}

function fmtProbeCell(lastProbe, lastError, blockedIds) {
  const ids = Array.isArray(blockedIds) ? blockedIds.filter(Boolean) : [];
  const blocked = ids.length
    ? `<div class="g2a-tag warn" title="${esc("模型封禁: " + ids.join(", "))}" style="margin-top:4px">屏蔽 ${esc(ids.length <= 2 ? ids.join(", ") : ids.slice(0, 2).join(", ") + "…")}</div>`
    : "";
  const lp = lastProbe || null;
  if (!lp) {
    if (blocked) {
      return blocked + (lastError
        ? `<div class="g2a-muted" title="${esc(lastError)}" style="max-width:180px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${esc(String(lastError).slice(0, 48))}</div>`
        : "");
    }
    const err = lastError
      ? `<div class="g2a-muted" title="${esc(lastError)}" style="max-width:180px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${esc(String(lastError).slice(0, 48))}</div>`
      : '<span class="g2a-muted">未探测</span>';
    return err;
  }
  const ok = lp.available || lp.ok;
  const pill = ok ? '<span class="g2a-tag ok">正常</span>' : '<span class="g2a-tag bad">报错</span>';
  const model = lp.model ? `<span class="mono">${esc(lp.model)}</span>` : "";
  const when = lp.probed_at ? fmtTime(lp.probed_at) : "";
  const err = (!ok && lp.error)
    ? `<div class="g2a-muted" title="${esc(lp.error)}" style="max-width:200px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${esc(String(lp.error).slice(0, 60))}</div>`
    : "";
  return `${pill} ${model}<div class="g2a-muted">${when}</div>${err}${blocked}`;
}

async function refreshAllQuota(force = true) {
  // force=false => read DB cache only; force=true => live query and persist.
  const btnIds = ["btn-refresh-quota", "btn-refresh-quota-2"];
  btnIds.forEach((id) => { const el = $(id); if (el) { el.disabled = true; if (!el.dataset.label) el.dataset.label = el.textContent; el.textContent = force ? "查询中…" : "读取缓存…"; } });
  try {
    const path = force ? "/accounts/quota?refresh=1" : "/accounts/quota?cached=1";
    const data = await api(path);
    const rows = data.results || data.accounts || data.quotas || [];
    // keep existing cache, update with returned rows
    rows.forEach((q) => {
      if (q && q.account_id) quotaCache[q.account_id] = q;
    });
    // also reflect onto current page rows
    (accountsList || []).forEach((a) => {
      const q = quotaCache[a.id];
      if (!q) return;
      a._pool = a._pool || { id: a.id };
      a._pool.last_quota = q;
      if (q.auto_disabled || q.exhausted) {
        a._pool.disabled_for_quota = true;
        a._pool.enabled = false;
      }
    });
    try { renderAccountsPage(); } catch (_) {}
    const qs = $("quota-summary");
    if (qs) {
      const total = data.count ?? rows.length;
      const exhausted = data.exhausted_count ?? rows.filter(x => x.exhausted || x.auto_disabled).length;
      const cached = data.cached ? "缓存" : "实时";
      qs.textContent = `额度(${cached})：${total} 个账号 · 耗尽 ${exhausted}`;
    }
    const ok = (data.ok_count != null ? data.ok_count : rows.filter(x => x.ok && !x.exhausted).length);
    toast(force ? `额度已刷新：可用 ${ok}/${data.count ?? rows.length}` : `已加载缓存额度：${rows.length} 条`, true);
  } catch (e) {
    toast(e.message || "额度查询失败", false);
  } finally {
    btnIds.forEach((id) => { const el = $(id); if (el) { el.disabled = false; el.textContent = el.dataset.label || "查询全部额度"; } });
  }
}


async function loadModels() {
  // Prefer lightweight admin catalog over stale/empty dashCache.models.
  // /dashboard is skipped on the models page for performance with large pools.
  let list = [];
  try {
    const r = await api("/models");
    list = (r && (r.data || r.models)) || [];
    if (!Array.isArray(list)) list = [];
    dashCache = dashCache || {};
    // Always replace — never keep a stale single-model dashboard snapshot.
    dashCache.models = list.slice();
    if (r && r.default_model) dashCache.default_model = r.default_model;
    if (r && r.storage) dashCache.models_storage = r.storage;
    if (r && r.meta) dashCache.models_meta = r.meta;
  } catch (e) {
    console.warn("loadModels failed", e);
    try { toast((e && e.message) || "加载模型列表失败", false); } catch (_) {}
  }
  renderModels();
  return (dashCache && dashCache.models) || list || [];
}

function renderModels() {
  const models = (dashCache && Array.isArray(dashCache.models) ? dashCache.models : []) || [];
  const tbody = $("models-tbody");
  if (!tbody) return;
  // Subtitle count if present
  try {
    const head = tbody.closest(".g2a-card");
    const sub = head && head.querySelector(".g2a-card-head p");
    if (sub) {
      sub.textContent = models.length
        ? `已从上游同步到数据库，共 ${models.length} 个模型。可重新同步并查看探测结果。`
        : "模型目录为空。请点「同步上游模型」拉取并写入数据库。";
    }
  } catch (_) {}
  tbody.innerHTML = models.map(m => `
    <tr>
      <td class="mono">${esc(m.id || "")}</td>
      <td>${esc(m.name || m.id || "—")}</td>
      <td class="g2a-muted">${m.context_window ? Number(m.context_window).toLocaleString() : "—"}</td>
      <td class="g2a-muted">${m.supports_reasoning_effort ? "是" : "—"}</td>
    </tr>
  `).join("") || `<tr><td colspan="4" class="g2a-muted">暂无模型。请点「同步上游模型」或刷新；默认可用 grok-4.5 / grok-build</td></tr>`;
}

function renderModelHealthInfo() {
  const el = $("model-health-info");
  const pill = $("model-health-pill");
  const mh = (dashCache && dashCache.model_health)
    || (statusCache && statusCache.model_health)
    || {};
  const settings = (dashCache && dashCache.settings) || (statusCache && statusCache.settings) || {};
  const enabled = mh.enabled !== false && settings.model_health_enabled !== false;
  const chk = $("chk-model-health");
  if (chk && document.activeElement !== chk) chk.checked = !!enabled;
  if (pill) {
    if (!enabled) {
      pill.className = "g2a-tag warn";
      pill.textContent = "● 已关闭";
    } else if (mh.running || mh.cluster_running || mh.leader_running) {
      pill.className = "g2a-tag ok";
      pill.textContent = "● 探测运行中";
    } else if (mh.enabled) {
      pill.className = "g2a-tag ok";
      pill.textContent = "● 已启用（后台）";
    } else {
      pill.className = "g2a-tag bad";
      pill.textContent = "● 未运行";
    }
  }
  if (!el) return;
  if (!enabled) {
    el.textContent = "模型探测：已关闭（可在下方开关重新开启）";
    return;
  }
  const last = mh.last || null;
  const sweep = mh.sweep || (last && last.sweep) || null;
  let lastTxt = "尚未跑过周期探测";
  if (last && (last.at || last.probed_at || last.count != null)) {
    const parts = [
      `上次 ${fmtTime(last.at || last.probed_at)}`,
      `可用 ${last.available_count ?? "—"}/${last.count ?? "—"}`,
      `自动处理 ${last.auto_action_count ?? 0}`,
    ];
    if (last.kick_cooldown || last.kick_disabled) {
      parts.push(`踢出 冷却${last.kick_cooldown || 0}/硬${last.kick_disabled || 0}`);
    }
    if (last.deferred != null) parts.push(`延后 ${last.deferred}`);
    if (last.budget_hit) parts.push("周期预算截断");
    const lastModels = last.models || last.models_configured;
    if (Array.isArray(lastModels) && lastModels.length) {
      parts.push(`本轮模型 ${lastModels.join(",")}`);
    }
    lastTxt = parts.join(" · ");
  }
  let sweepTxt = "";
  if (sweep && (sweep.covered != null || sweep.generation)) {
    const live = sweep.live ?? sweep.sweep_live;
    const left = sweep.remaining ?? sweep.sweep_remaining;
    const mode = sweep.mode || mh.selection || "priority_sweep";
    sweepTxt = ` · 扫池(${mode}) ${sweep.covered ?? 0}${live != null ? "/" + live : ""}${left != null ? " 剩余" + left : ""}`;
    const pr = (sweep.priority && (sweep.priority.batch || sweep.priority.remaining)) || null;
    if (pr) {
      sweepTxt += ` · 优先 冷却${pr.cooldown_due || 0}/未测${pr.never_probed || 0}/失败${pr.fail_streak || 0}/正常${pr.healthy || 0}`;
    }
    if (sweep.re_admit) sweepTxt += ` · 复检${sweep.re_admit}`;
    if (sweep.held_recoverable) sweepTxt += ` · 限流待恢复${sweep.held_recoverable}`;
  }
  let etaTxt = "";
  if (mh.full_pool_eta_sec != null && Number(mh.full_pool_eta_sec) > 0) {
    const sec = Number(mh.full_pool_eta_sec);
    if (sec < 3600) etaTxt = ` · 全池约 ${Math.ceil(sec / 60)} 分钟`;
    else etaTxt = ` · 全池约 ${(sec / 3600).toFixed(1)} 小时`;
  }
  const modelsTxt = (mh.probe_models || []).join(", ") || "—";
  const rotateTxt = (mh.probe_models || []).length > 1
    ? `（后台每轮轮转 1 个，共 ${(mh.probe_models || []).length} 个）`
    : "";
  el.textContent =
    `模型探测：后台每 ${mh.interval_sec ?? "?"}s 检查 · 每批 ${mh.probe_batch ?? "?"} · 模型 ${modelsTxt}${rotateTxt} · ${lastTxt}${sweepTxt}${etaTxt}`;
}

async function runAccountProbe(accountId, model) {
  setLogPanel("probe-result", `探测中… account=${accountId}`, { forceShow: true });
  setRowBusy(accountId, true, "探测中");
  try {
    const body = { auto_disable: true };
    if (model) body.model = model;
    const r = await api("/accounts/" + encodeURIComponent(accountId) + "/probe", {
      method: "POST",
      body: JSON.stringify(body),
    });
    const res = r.result || r;
    const ok = !!(r.ok || res.available);
    const pool = r.pool || {};
    const recovered = !!(res.auto_action && res.auto_action.recovered)
      || !!(pool.pool_status === "normal" && ok)
      || !!(r.pool_status === "normal" && ok);
    const cooling = !!(pool.in_cooldown || r.in_cooldown || pool.pool_status === "cooldown");
    const lines = [
      ok ? "✓ 探测成功" : "✗ 探测失败",
      `账号: ${r.email || res.email || accountId}`,
      `模型: ${res.model || pool.cooldown_model || "—"}`,
      res.latency_ms != null ? `耗时: ${res.latency_ms} ms` : null,
      res.status_code != null ? `HTTP: ${res.status_code}` : null,
      res.error ? `错误: ${res.error}` : null,
      cooling ? "状态：冷却中（已写库）" : null,
      ok && recovered ? "状态：冷却中 → 正常（已写库）" : null,
      pool.cooldown_code ? `code: ${pool.cooldown_code}` : null,
      res.auto_disabled ? "已自动屏蔽模型 / 移出轮询" : null,
    ].filter(Boolean);
    setLogPanel("probe-result", lines.join("\n"), { forceShow: true });
    toast(
      ok
        ? (recovered ? "测活成功，已恢复为正常" : "账号模型探测成功")
        : (cooling ? "测活失败，已进入冷却中" : (res.error || r.error || "探测失败")),
      ok
    );
    const poolPatch = poolPatchFromProbeResponse(r);
    applyAccountLivePatch(accountId, {
      email: r.email || res.email,
      _pool: poolPatch,
    });
  } catch (e) {
    setLogPanel("probe-result", "✗ " + e.message, { forceShow: true });
    toast(e.message, false);
  } finally {
    setRowBusy(accountId, false);
  }
}

async function probeAccounts(ids, { confirmMany = true } = {}) {
  const list = Array.from(new Set((ids || []).map((x) => String(x || "").trim()).filter(Boolean)));
  if (!list.length) {
    toast("请先选择要测试的账号", false);
    return;
  }
  if (confirmMany && list.length > 1) {
    if (!confirm(`确认对选中的 ${list.length} 个账号执行模型测试？状态会实时更新。`)) return;
  }
  if (list.length === 1) {
    await runAccountProbe(list[0]);
    return;
  }
  list.forEach((id) => setRowBusy(id, true, "探测中"));
  setLogPanel("probe-result", `批量探测中… ×${list.length}`, { forceShow: true });
  try {
    const r = await api("/accounts/probe-batch", {
      method: "POST",
      body: JSON.stringify({ ids: list, auto_disable: true }),
    });
    const results = Array.isArray(r.results) ? r.results : [];
    let okN = 0, badN = 0, coolN = 0;
    const lines = [`批量模型测试完成：${results.length}/${list.length}`];
    results.forEach((item) => {
      const id = item.account_id || item.id;
      if (!id) return;
      const res = item.result || item;
      const ok = !!(item.ok || res.available);
      const pool = item.pool || {};
      if (ok) okN += 1; else badN += 1;
      if (pool.in_cooldown || item.in_cooldown || pool.pool_status === "cooldown") coolN += 1;
      applyAccountLivePatch(id, {
        email: item.email || res.email,
        _pool: poolPatchFromProbeResponse(item),
      });
      setRowBusy(id, false);
      lines.push(
        `${ok ? "✓" : "✗"} ${item.email || id}` +
          (pool.pool_status ? ` · ${pool.pool_status}` : "") +
          ((pool.in_cooldown || pool.pool_status === "cooldown") ? " · 冷却中" : "") +
          (res.error ? ` · ${String(res.error).slice(0, 80)}` : "")
      );
    });
    // any ids missing from response
    list.forEach((id) => setRowBusy(id, false));
    lines.splice(1, 0, `成功 ${okN} · 失败 ${badN} · 冷却 ${coolN}`);
    setLogPanel("probe-result", lines.join("\n"), { forceShow: true });
    toast(`批量测活：成功 ${okN} · 失败 ${badN} · 冷却 ${coolN}`, badN === 0);
  } catch (e) {
    list.forEach((id) => setRowBusy(id, false));
    setLogPanel("probe-result", "✗ " + e.message, { forceShow: true });
    toast(e.message, false);
  }
}

async function runProbeAll() {
  const btns = ["btn-probe-all", "btn-probe-all-2"].map((id) => $(id)).filter(Boolean);
  const startedAt = Date.now();
  btns.forEach((b) => {
    try {
      b.disabled = true;
      b.dataset._oldText = b.textContent;
      b.textContent = "探测中…";
    } catch (_) {}
  });
  toast("已开始全部账号模型探测，请稍候…");
  setLogPanel(
    "probe-result",
    "正在探测全部账号模型…\n（多波次后台执行，完成后自动刷新列表）",
    { forceShow: true }
  );
  try {
    // Async multi-wave job: returns immediately, then we poll /model-health.
    const start = await api("/accounts/probe-all", { method: "POST", body: "{}" });
    let r = start;
    const pollDeadline = Date.now() + 35 * 60 * 1000;
    while (true) {
      const job = (r && r.running != null) ? r : ((r && r.job) || r);
      const running = !!(job && job.running);
      const wave = job && (job.wave || job.waves) || 0;
      const probed = job && (job.probed || job.count) || 0;
      const avail = job && (job.available_count ?? job.available) || 0;
      const failed = job && (job.unavailable_count ?? job.failed) || 0;
      const rem = job && job.sweep && job.sweep.remaining;
      const elapsed = Math.max(1, Math.round((Date.now() - startedAt) / 1000));
      const progressLines = [
        running ? `全部账号模型探测进行中（${elapsed}s）` : `全部账号模型探测完成（${elapsed}s）`,
        wave ? `波次 ${wave}` : null,
        `已探测 ${probed}` + (rem != null ? ` · 剩余 ${rem}` : (job && job.deferred ? ` · 延后 ${job.deferred}` : "")),
        `可用 ${avail}` + (failed ? ` · 不可用 ${failed}` : ""),
        job && job.models ? `模型 ${(job.models || []).join(", ")}` : null,
      ].filter(Boolean);
      setLogPanel("probe-result", progressLines.join("\n"), { forceShow: true });
      if (!running) {
        r = job || r;
        break;
      }
      if (Date.now() > pollDeadline) {
        throw new Error("探测超时（>35min），请查看 model-health 状态");
      }
      await new Promise((res) => setTimeout(res, 2000));
      try {
        const st = await api("/model-health");
        r = (st && st.job) ? st.job : (st && st.last) ? st.last : st;
        if (st && st.sweep && r && !r.sweep) r.sweep = st.sweep;
      } catch (_) {
        // keep looping on transient errors
      }
    }
    const elapsed = Math.max(1, Math.round((Date.now() - startedAt) / 1000));
    const lines = [
      `全部账号模型探测完成（${elapsed}s）`,
      `探测 ${r.probed ?? r.count ?? 0}` + (r.deferred ? ` · 延后 ${r.deferred}` : ""),
      `可用 ${r.available_count ?? r.available ?? 0}/${r.count ?? r.probed ?? 0}`,
      `不可用 ${r.unavailable_count ?? r.failed ?? 0}`,
      `自动处理 ${r.auto_action_count ?? 0}` + (r.kick_cooldown ? ` · 进入冷却 ${r.kick_cooldown}` : ""),
      r.waves ? `波次 ${r.waves}` : null,
      `模型 ${((r.models || []).join(", ") || "—")}`,
    ].filter(Boolean);
    const bad = (r.failed_sample || r.results || []).filter((x) => x && !x.available);
    bad.slice(0, 8).forEach((x) => {
      let err = String(x.error || "error");
      if (/free-usage-exhausted|free usage/i.test(err)) {
        err = "临时额度耗尽，已冷却，等待下次测活成功";
      } else if (err.startsWith("{") && err.length > 120) {
        err = err.slice(0, 120) + "…";
      }
      lines.push(`- ${x.email || x.account_id}: ${err}`);
    });
    setLogPanel("probe-result", lines.join("\n"), { forceShow: true });
    toast(`探测完成：${r.available_count ?? r.available ?? 0}/${r.count ?? r.probed ?? 0} 可用`);
    statusCache = statusCache || {};
    dashCache = dashCache || {};
    const mh = Object.assign({}, statusCache.model_health || {}, {
      last: Object.assign({}, r, { at: Date.now() / 1000, probed_at: r.probed_at || Date.now() / 1000 }),
    });
    if (r.sweep) mh.sweep = r.sweep;
    statusCache.model_health = mh;
    dashCache.model_health = mh;
    try { renderModelHealthInfo(); } catch (_) {}
    try { renderMaintainer(); } catch (_) {}
    try { renderStats(); } catch (_) {}
    try { await refreshOverviewStatus({ force: true, render: true }); } catch (_) {}
    try { await loadAccountsPage({ reset: false }); } catch (_) {}
  } catch (e) {
    setLogPanel("probe-result", "✗ " + (e.message || e), { forceShow: true });
    toast(e.message || "全部探测失败", false);
  } finally {
    btns.forEach((b) => {
      try {
        b.disabled = false;
        if (b.dataset._oldText) b.textContent = b.dataset._oldText;
      } catch (_) {}
    });
  }
}

let lastAutoTokenRefreshAt = 0;
let _autoRefreshInFlight = false;
function startAutoUiRefresh() {
  if (uiRefreshTimer) return;
  uiRefreshTimer = setInterval(async () => {
    try {
      const page = document.body.dataset.page || pageFromPath(location.pathname) || "overview";
      if (page !== "overview") return;
      if (document.hidden) return;
      const chk = $("chk-auto-refresh-ui");
      if (chk && !chk.checked) return;
      if (_autoRefreshInFlight) return;
      _autoRefreshInFlight = true;
      try {
        const now = Date.now();
        if (!statusCache || (now - (_statusFetchedAt || 0)) > 5000) {
          statusCache = await api("/status");
          _statusFetchedAt = Date.now();
          if (window.G2A && G2A.state) G2A.state.status = statusCache;
        }
        try { renderStats(); } catch (_) {}
        try { renderMaintainer(); } catch (_) {}
        try { renderModelHealthInfo(); } catch (_) {}
        try { renderStoreConn("overview-conn"); } catch (_) {}
        const tm = (statusCache && statusCache.token_maintainer) || {};
        const rem = tm.min_remaining_sec;
        if (rem != null && rem < 15 * 60 && Date.now() - lastAutoTokenRefreshAt > 5 * 60 * 1000) {
          lastAutoTokenRefreshAt = Date.now();
          try { await api("/accounts/refresh", { method: "POST", body: JSON.stringify({ force: false }) }); } catch (_) {}
        }
      } finally {
        _autoRefreshInFlight = false;
      }
    } catch (e) {
      _autoRefreshInFlight = false;
      if (e && e.status === 401) return;
      // Ignore transient network errors on overview polling.
      if (e && (e.network || e.status === 0)) {
        console.warn("auto refresh network", e.message || e);
        return;
      }
      console.warn("auto refresh", e);
    }
  }, 20000);
}


function renderGuide() {
  const pageOrigin = currentOrigin();
  let base = (dashCache && dashCache.api_base) || (statusCache && statusCache.api_base) || "";
  // Prefer current browser origin on public deployments so guides never show 127.0.0.1
  // when the page itself was opened via domain/public IP.
  if (pageOrigin && (!base || /127\.0\.0\.1|localhost/i.test(base))) {
    base = pageOrigin.replace(/\/$/, "") + "/v1";
  }
  if (!base) base = "<your-host>/v1";
  let origin = base.replace(/\/v1\/?$/, "");
  if (!origin) origin = pageOrigin || "<your-host>";
  const model = (dashCache && dashCache.default_model) || (statusCache && statusCache.default_model) || "grok-4.5";
  $("guide-base").textContent = base;
  $("guide-model").textContent = model;
  $("guide-curl").textContent = `curl ${base}/chat/completions \\
  -H "Authorization: Bearer sk-g2a-YOUR_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{"model":"${model}","messages":[{"role":"user","content":"你好"}],"stream":false}'`;
  $("guide-py").textContent = `from openai import OpenAI
client = OpenAI(base_url="${base}", api_key="sk-g2a-YOUR_KEY")
r = client.chat.completions.create(
    model="${model}",
    messages=[{"role": "user", "content": "Hello"}],
)
print(r.choices[0].message.content)

# Tools / Function Calling 示例
tools = [{
  "type": "function",
  "function": {
    "name": "get_weather",
    "description": "Get weather",
    "parameters": {
      "type": "object",
      "properties": {"city": {"type": "string"}},
      "required": ["city"],
    },
  },
}]
r = client.chat.completions.create(
    model="${model}",
    messages=[{"role": "user", "content": "北京天气？"}],
    tools=tools,
    tool_choice="auto",
)
print(r.choices[0].message.tool_calls or r.choices[0].message.content)`;
  if ($("guide-anthropic")) {
    $("guide-anthropic").textContent = `# Anthropic Messages API
# Base URL 填网关根地址（或带 /v1）；鉴权用 x-api-key
curl ${origin}/v1/messages \\
  -H "x-api-key: sk-g2a-YOUR_KEY" \\
  -H "anthropic-version: 2023-06-01" \\
  -H "Content-Type: application/json" \\
  -d '{"model":"${model}","max_tokens":1024,"messages":[{"role":"user","content":"你好"}]}'

# Python anthropic SDK
from anthropic import Anthropic
client = Anthropic(base_url="${origin}", api_key="sk-g2a-YOUR_KEY")
msg = client.messages.create(
    model="${model}",
    max_tokens=1024,
    messages=[{"role": "user", "content": "Hello"}],
)
print(msg.content[0].text)

# Claude Code / 其他工具：API Base = ${origin}  或  ${origin}/v1
# 模型名可用 claude-* 别名，会自动映射到默认 Grok 模型`;
  }
  $("guide-linux").textContent = `# Linux 服务器部署
cp .env.example .env
# 编辑 .env（GROK2API_ADMIN_PASSWORD 等）
# 默认 GROK2API_REASONING_COMPAT=off
pip install -r requirements.txt
./start.sh
# 或后台：
# nohup ./start.sh > grok2api.log 2>&1 &
# 授权：管理台 → 设备码登录（无需 grok CLI）`;
}

/* ── Email registration ─────────────────────────────── */
const REG_CONFIG_KEY = "g2a_registration_config_v1";

function clearRegTrack() {
  try { sessionStorage.removeItem(REG_TRACK_KEY); } catch (_) {}
}

function resetRegProgressForNewTask() {
  // Hard-reset UI/state before every new registration so the progress card and
  // task-log view never concatenate the previous finished/stopped run.
  try { clearInterval(regPollTimer); } catch (_) {}
  regPollTimer = null;
  regBatchId = null;
  regSessionId = null;
  regSessionIds = [];
  regFinishedNotified = false;
  regStopping = false;
  regPollInFlight = false;
  regLastLogText = "";
  regLastStatusText = "";
  regLastEmailText = "";
  regProbedIds = new Set();
  regProbeRunning = false;
  clearRegTrack();
  setLogPanel("reg-log", "", { forceShow: false });
  setRegStatusText("starting");
  setRegEmailText("—");
}

function saveRegTrack() {
  try {
    if (!regBatchId && !regSessionId && !(regSessionIds && regSessionIds.length)) {
      clearRegTrack();
      return;
    }
    sessionStorage.setItem(
      REG_TRACK_KEY,
      JSON.stringify({
        batch_id: regBatchId || null,
        session_id: regSessionId || null,
        session_ids: Array.isArray(regSessionIds) ? regSessionIds.slice(0, 200) : [],
        finished: !!regFinishedNotified,
        saved_at: Date.now(),
      })
    );
  } catch (_) {}
}

function loadRegTrack() {
  try {
    const raw = sessionStorage.getItem(REG_TRACK_KEY);
    if (!raw) return null;
    const obj = JSON.parse(raw);
    if (!obj || typeof obj !== "object") return null;
    // Drop stale tracks (> 12h) so we don't keep resurrecting ancient cards.
    const age = Date.now() - Number(obj.saved_at || 0);
    if (age > 12 * 3600 * 1000) {
      clearRegTrack();
      return null;
    }
    return obj;
  } catch (_) {
    return null;
  }
}

function applyRegTrack(track) {
  if (!track || typeof track !== "object") return false;
  const batchId = track.batch_id || null;
  const ids = Array.isArray(track.session_ids)
    ? track.session_ids.map((x) => String(x || "").trim()).filter(Boolean)
    : [];
  const sid = track.session_id || ids[0] || null;
  if (!batchId && !sid && !ids.length) return false;
  regBatchId = batchId;
  regSessionIds = ids.length ? ids.slice() : (sid ? [sid] : []);
  regSessionId = regSessionIds[0] || sid || null;
  // Always rehydrate finished flag from storage; never leave stale true/false from
  // a previous soft-nav session.
  regFinishedNotified = !!track.finished;
  return hasTrackedRegTask();
}

function dismissRegProgressCard() {
  // Close only the UI card. Backend registration keeps running unless user hits stop.
  try { clearInterval(regPollTimer); } catch (_) {}
  regPollTimer = null;
  regBatchId = null;
  regSessionId = null;
  regSessionIds = [];
  regFinishedNotified = false;
  regStopping = false;
  regPollInFlight = false;
  regLastLogText = "";
  regLastStatusText = "";
  regLastEmailText = "";
  regProbedIds = new Set();
  regProbeRunning = false;
  clearRegTrack();
  hidePanel("reg-session-box");
  setLogPanel("reg-log", "", { forceShow: false });
  setRegStatusText("idle");
  setRegEmailText("—");
}

function stopRegPolling() {
  try { clearInterval(regPollTimer); } catch (_) {}
  try { clearTimeout(regPollTimer); } catch (_) {}
  regPollTimer = null;
  regPollInFlight = false;
  regPollPending = false;
}

function isNotFoundError(err) {
  if (!err) return false;
  if (Number(err.status) === 404) return true;
  const msg = String(err.message || err.detail || "");
  return /not found|registration (batch|session) not found/i.test(msg);
}

function markTrackedRegistrationMissing(reason) {
  // Stale browser track after worker restart / TTL expiry: stop hammering 404s.
  stopRegPolling();
  regFinishedNotified = true;
  regStopping = false;
  const lines = [
    "[恢复] 后端已找不到该注册任务（可能已完成并过期，或服务重启后进度未镜像）",
    regBatchId ? `batch_id: ${regBatchId}` : "",
    regSessionId ? `session_id: ${regSessionId}` : "",
    reason ? `detail: ${reason}` : "",
    "已停止轮询。可点「关闭」收起进度卡片，或重新开始注册",
  ].filter(Boolean);
  setRegStatusText("not found");
  setRegEmailText(regBatchId ? `batch ${regBatchId}` : (regSessionId || "—"));
  setLogPanel("reg-log", lines.join("\n"), { forceShow: true });
  showPanel("reg-session-box");
  // Drop ids so refresh / soft-nav won't resurrect 404 polling.
  regBatchId = null;
  regSessionId = null;
  regSessionIds = [];
  clearRegTrack();
}

function startRegPolling({ immediate = true, intervalMs = 1000 } = {}) {
  try { clearInterval(regPollTimer); } catch (_) {}
  try { clearTimeout(regPollTimer); } catch (_) {}
  // Active registration needs ~1s freshness so waiting_email / captcha lines
  // do not sit stale. Stopping uses a gentler cadence.
  const ms = Math.max(regStopping ? 1500 : 800, Number(intervalMs) || 1000);
  regPollTimer = setInterval(() => {
    pollRegSession().catch(() => {});
  }, ms);
  // Immediate first poll — do not wait a full interval (or the old 300ms delay).
  if (immediate) {
    setTimeout(() => { pollRegSession().catch(() => {}); }, 0);
  }
}

async function stopRegistration() {
  const hasTrack = !!(regBatchId || regSessionId || (regSessionIds && regSessionIds.length));
  if (!hasTrack) {
    // Still allow stop-all for leftover server sessions.
    if (!confirm("停止全部进行中的注册会话？")) return;
  } else if (!confirm(regBatchId ? `停止批次 ${regBatchId} 的全部注册？` : "停止当前注册会话？")) {
    return;
  }
  try {
    if ($("btn-stop-reg")) $("btn-stop-reg").disabled = true;
    if ($("btn-stop-reg-inline")) $("btn-stop-reg-inline").disabled = true;
    // Mark stopping before network round-trip so poll cannot flip UI back to "running".
    regStopping = true;
    regFinishedNotified = false;
    setRegStatusText("stopping");
    let r = null;
    let missing = false;
    if (regBatchId) {
      try {
        r = await api("/accounts/register-email/batches/" + encodeURIComponent(regBatchId) + "/stop", {
          method: "POST",
          body: "{}",
        });
      } catch (e) {
        if (isNotFoundError(e)) {
          missing = true;
          // Fall back to stop-all so any still-running workers exit.
          try {
            r = await api("/accounts/register-email/stop", { method: "POST", body: "{}" });
          } catch (_) {
            r = { ok: true, message: "批次已不存在，已停止轮询" };
          }
        } else {
          throw e;
        }
      }
    } else if (regSessionId) {
      try {
        r = await api("/accounts/register-email/sessions/" + encodeURIComponent(regSessionId) + "/stop", {
          method: "POST",
          body: "{}",
        });
      } catch (e) {
        if (isNotFoundError(e)) {
          missing = true;
          try {
            r = await api("/accounts/register-email/stop", { method: "POST", body: "{}" });
          } catch (_) {
            r = { ok: true, message: "会话已不存在，已停止轮询" };
          }
        } else {
          throw e;
        }
      }
    } else if (regSessionIds && regSessionIds.length) {
      // No batch id — stop each known session, then stop-all as a safety net.
      const results = [];
      let anyOk = false;
      for (const sid of regSessionIds) {
        try {
          results.push(
            await api("/accounts/register-email/sessions/" + encodeURIComponent(sid) + "/stop", {
              method: "POST",
              body: "{}",
            })
          );
          anyOk = true;
        } catch (e) {
          if (isNotFoundError(e)) missing = true;
          results.push({ ok: false, id: sid, error: (e && e.message) || String(e) });
        }
      }
      try {
        r = await api("/accounts/register-email/stop", { method: "POST", body: "{}" });
      } catch (_) {
        r = {
          ok: true,
          message: anyOk ? "已请求停止已知会话" : "会话已不存在，已停止轮询",
          results,
        };
      }
    } else {
      r = await api("/accounts/register-email/stop", { method: "POST", body: "{}" });
    }
    if (missing && !(r && r.ok === false)) {
      markTrackedRegistrationMissing((r && r.message) || "stop target not found");
      toast(r && r.message ? r.message : "注册任务已不存在", true);
      return;
    }
    toast(r && r.message ? r.message : "已请求停止注册", !!(r && r.ok !== false));
    setRegStatusText("stopping");
    // Append one stable stop note; do not wipe existing progress log.
    const prev = regLastLogText && regLastLogText !== "—" ? regLastLogText : "";
    const stopNote = [
      "",
      "[stop] 已请求停止注册，等待进行中的任务退出…",
      regBatchId ? `batch_id: ${regBatchId}` : "",
      regSessionId ? `session_id: ${regSessionId}` : "",
      r && r.message ? `message: ${r.message}` : "",
    ].filter(Boolean).join("\n");
    setLogPanel(
      "reg-log",
      (prev ? prev + "\n" : "") + stopNote,
      { forceShow: true }
    );
    showPanel("reg-session-box");
    saveRegTrack();
    // Keep polling until cancelled/stopped, but avoid aggressive 1.2s thrash.
    startRegPolling({ immediate: true, intervalMs: 1000 });
  } catch (e) {
    regStopping = false;
    toast((e && e.message) || "停止失败", false);
  } finally {
    if ($("btn-stop-reg")) $("btn-stop-reg").disabled = false;
    if ($("btn-stop-reg-inline")) $("btn-stop-reg-inline").disabled = false;
  }
}

let regConfigCache = null;
let regConfigLoadedAt = 0;

function syncRegCaptchaProviderUI() {
  const provider = $("reg-captcha-provider")
    ? ($("reg-captcha-provider").value || "local").trim().toLowerCase()
    : "local";
  const isLocal = provider !== "yescaptcha";
  // Local captcha is always inline in main container; never expose URL field.
  if ($("reg-local-solver-wrap")) {
    $("reg-local-solver-wrap").style.display = "none";
  }
  if ($("reg-yescaptcha-wrap")) {
    $("reg-yescaptcha-wrap").style.display = isLocal ? "none" : "";
  }
}

// Per-provider mail keys/domains kept in memory so switching the dropdown
// does not overwrite another provider's values before save.
const REG_MAIL_KEY_SLOTS = {
  moemail: "moemail_api_key",
  yyds: "yyds_api_key",
  gptmail: "gptmail_api_key",
  cfmail: "cfmail_api_key",
};
const REG_MAIL_DOMAIN_SLOTS = {
  moemail: "moemail_domain",
  yyds: "yyds_domain",
  gptmail: "gptmail_domain",
  cfmail: "cfmail_domain",
};
let regMailKeys = { moemail: "", yyds: "", gptmail: "", cfmail: "" };
let regMailDomains = { moemail: "", yyds: "", gptmail: "", cfmail: "" };
// Self-hosted hosts kept per provider so MoeMail / CF never overwrite each other.
let regMailBaseUrls = { moemail: "", cfmail: "" };
let regMailProviderPrev = "moemail";

function currentRegMailProvider() {
  const mail = $("reg-mail-provider")
    ? ($("reg-mail-provider").value || "moemail").trim().toLowerCase()
    : "moemail";
  if (mail === "yyds") return "yyds";
  if (mail === "gptmail") return "gptmail";
  if (mail === "cfmail") return "cfmail";
  return "moemail";
}

function stashRegMailFieldsFromInput() {
  const mail = regMailProviderPrev || currentRegMailProvider();
  if ($("reg-api-key")) {
    regMailKeys[mail] = $("reg-api-key").value || "";
  }
  if ($("reg-domain")) {
    regMailDomains[mail] = $("reg-domain").value || "";
  }
  if ($("reg-base-url") && (mail === "moemail" || mail === "cfmail")) {
    regMailBaseUrls[mail] = $("reg-base-url").value || "";
  }
}

// Back-compat alias used by older event wiring if any.
function stashRegMailKeyFromInput() {
  stashRegMailFieldsFromInput();
}

function syncRegMailProviderUI() {
  const mail = currentRegMailProvider();
  // Persist key/domain typed for the previous provider before swapping inputs.
  if (mail !== regMailProviderPrev) {
    stashRegMailFieldsFromInput();
    regMailProviderPrev = mail;
  }
  const isYyds = mail === "yyds";
  const isGpt = mail === "gptmail";
  const isCf = mail === "cfmail";
  const isMoe = mail === "moemail";
  const isTemp24h = isYyds || isGpt;

  // YYDS / GPTMail use fixed official hosts — hide URL field entirely.
  // MoeMail / CF Temp Email are self-hosted — show URL field with per-provider value.
  if ($("reg-base-url-wrap")) {
    $("reg-base-url-wrap").style.display = (isMoe || isCf) ? "" : "none";
  }
  if ($("reg-base-url-label")) {
    $("reg-base-url-label").textContent = isCf
      ? "CF Temp Email Base URL"
      : "MoeMail Base URL";
  }
  if ($("reg-base-url")) {
    if (isMoe || isCf) {
      $("reg-base-url").placeholder = isCf
        ? "https://your-worker.example.workers.dev"
        : "https://moemail.example.com";
      // Restore this provider's host only — never the other self-hosted host.
      $("reg-base-url").value = regMailBaseUrls[mail] || "";
    } else {
      $("reg-base-url").value = "";
    }
  }

  if ($("reg-api-key-label")) {
    $("reg-api-key-label").textContent = isYyds
      ? "YYDS API Key"
      : isGpt
        ? "GPTMail API Key"
        : isCf
          ? "CF Temp Email Admin 密码"
          : "MoeMail API Key";
  }
  if ($("reg-api-key")) {
    $("reg-api-key").placeholder = isYyds
      ? "AC-..."
      : isGpt
        ? "sk-...（自有 Key）"
        : isCf
          ? "管理后台密码（x-admin-auth）"
          : "mk_...";
    // Show the key stored for this provider only.
    $("reg-api-key").value = regMailKeys[mail] || "";
  }
  if ($("reg-domain-label")) {
    $("reg-domain-label").textContent = isYyds
      ? "YYDS 邮箱域名"
      : isGpt
        ? "GPTMail 邮箱域名"
        : isCf
          ? "CF Temp Email 域名"
          : "MoeMail 邮箱域名";
  }
  if ($("reg-domain")) {
    $("reg-domain").placeholder = isYyds
      ? "留空则自动随机获取公开域名"
      : isGpt
        ? "可选；留空由 GPTMail 随机分配"
        : isCf
          ? "可选；留空从 /open_api/settings 自动选"
          : "example.com";
    // Show the domain stored for this provider only.
    $("reg-domain").value = regMailDomains[mail] || "";
  }
  // YYDS / GPTMail temp mail is ~24h — hide permanent / 3d options for clarity.
  if ($("reg-expiry-ms")) {
    const opts = $("reg-expiry-ms").options || [];
    for (let i = 0; i < opts.length; i++) {
      const v = String(opts[i].value || "");
      if (v === "0" || v === "259200000") {
        opts[i].hidden = isTemp24h;
        opts[i].disabled = isTemp24h;
      }
    }
    if (isTemp24h) {
      const curExp = String($("reg-expiry-ms").value || "");
      if (curExp === "0" || curExp === "259200000") {
        $("reg-expiry-ms").value = "86400000";
      }
    }
  }
}

function readRegConfig() {
  const provider = $("reg-captcha-provider")
    ? ($("reg-captcha-provider").value || "local").trim().toLowerCase()
    : "local";
  const isLocal = provider !== "yescaptcha";
  const mailProvider = currentRegMailProvider();
  // Capture currently visible key/domain into the selected provider slots.
  stashRegMailFieldsFromInput();
  regMailProviderPrev = mailProvider;
  const activeKey = regMailKeys[mailProvider] || "";
  const activeDomain = regMailDomains[mailProvider] || "";
  // Keep self-hosted hosts in dedicated slots so save never mixes them.
  if (mailProvider === "moemail" || mailProvider === "cfmail") {
    regMailBaseUrls[mailProvider] = $("reg-base-url") ? ($("reg-base-url").value || "").trim() : (regMailBaseUrls[mailProvider] || "");
  }
  const activeBase =
    mailProvider === "moemail" || mailProvider === "cfmail"
      ? (regMailBaseUrls[mailProvider] || "")
      : "";
  return {
    mail_provider: mailProvider,
    // Active host mirrors the selected self-hosted provider.
    base_url: activeBase,
    moemail_base_url: regMailBaseUrls.moemail || "",
    cfmail_base_url: regMailBaseUrls.cfmail || "",
    domain: activeDomain,
    moemail_domain: regMailDomains.moemail || "",
    yyds_domain: regMailDomains.yyds || "",
    gptmail_domain: regMailDomains.gptmail || "",
    cfmail_domain: regMailDomains.cfmail || "",
    expiry_ms: $("reg-expiry-ms") ? $("reg-expiry-ms").value.trim() : "",
    // Active key + all per-provider keys (empty keeps previous secret server-side).
    api_key: activeKey,
    moemail_api_key: regMailKeys.moemail || "",
    yyds_api_key: regMailKeys.yyds || "",
    gptmail_api_key: regMailKeys.gptmail || "",
    cfmail_api_key: regMailKeys.cfmail || "",
    captcha_provider: isLocal ? "local" : "yescaptcha",
    // Inline local solver is fixed; do not accept/show custom URL.
    local_solver_url: isLocal ? "http://127.0.0.1:5072" : "",
    yescaptcha_key: isLocal
      ? ""
      : ($("reg-yescaptcha-key") ? $("reg-yescaptcha-key").value.trim() : ""),
    proxy: $("reg-proxy") ? $("reg-proxy").value.trim() : "",
    proxy_username: $("reg-proxy-username") ? $("reg-proxy-username").value.trim() : "",
    proxy_password: $("reg-proxy-password") ? $("reg-proxy-password").value.trim() : "",
    proxy_strategy: $("reg-proxy-strategy") ? $("reg-proxy-strategy").value.trim() : "round_robin",
    count: $("reg-count") ? $("reg-count").value.trim() : "1",
    concurrency: $("reg-concurrency") ? $("reg-concurrency").value.trim() : "5",
    stagger_ms: $("reg-stagger-ms") ? $("reg-stagger-ms").value.trim() : "300",
    probe_delay_sec: $("reg-probe-delay-sec")
      ? $("reg-probe-delay-sec").value.trim()
      : "30",
  };
}
// MoeMail official EXPIRY_OPTIONS only:
// 1h / 24h / 3d / permanent (0). See beilunyang/moemail app/types/email.ts
const MOEMAIL_EXPIRY_PRESETS = [3600000, 86400000, 259200000, 0];

function normalizeRegExpiryMs(value) {
  const raw = value == null ? "" : String(value).trim();
  if (raw === "" || raw == null) return "3600000"; // default 1 hour
  if (raw === "0") return "0";
  const n = Number.parseInt(raw, 10);
  if (!Number.isFinite(n)) return "3600000";
  if (MOEMAIL_EXPIRY_PRESETS.includes(n)) return String(n);
  // Map legacy free-form ms to the nearest official preset (exclude permanent=0).
  const timed = [3600000, 86400000, 259200000];
  let best = timed[0];
  let bestDiff = Math.abs(n - best);
  for (const p of timed) {
    const d = Math.abs(n - p);
    if (d < bestDiff) {
      best = p;
      bestDiff = d;
    }
  }
  return String(best);
}

function applyRegConfig(cfg) {
  if (!cfg || typeof cfg !== "object") return;
  const mail = String(cfg.mail_provider || cfg.provider || "moemail").trim().toLowerCase();
  const mailProv = mail === "yyds" ? "yyds" : mail === "gptmail" ? "gptmail" : mail === "cfmail" ? "cfmail" : "moemail";
  if ($("reg-mail-provider")) {
    $("reg-mail-provider").value = mailProv;
  }
  // Hydrate per-provider key/domain caches.
  // Prefer dedicated fields; only fall back to active field for the *current*
  // provider. Never invent values for other providers.
  const activeKey = cfg.api_key == null ? "" : String(cfg.api_key);
  const activeDomain = cfg.domain == null ? "" : String(cfg.domain);
  regMailKeys = {
    moemail:
      cfg.moemail_api_key != null && cfg.moemail_api_key !== ""
        ? String(cfg.moemail_api_key)
        : (mailProv === "moemail" ? activeKey : (regMailKeys.moemail || "")),
    yyds:
      cfg.yyds_api_key != null && cfg.yyds_api_key !== ""
        ? String(cfg.yyds_api_key)
        : (mailProv === "yyds" ? activeKey : (regMailKeys.yyds || "")),
    gptmail:
      cfg.gptmail_api_key != null && cfg.gptmail_api_key !== ""
        ? String(cfg.gptmail_api_key)
        : (mailProv === "gptmail" ? activeKey : (regMailKeys.gptmail || "")),
    cfmail:
      cfg.cfmail_api_key != null && cfg.cfmail_api_key !== ""
        ? String(cfg.cfmail_api_key)
        : (mailProv === "cfmail" ? activeKey : (regMailKeys.cfmail || "")),
  };
  // Domain: if the dedicated field is present (including empty string from server),
  // honor it. Empty means cleared — do not restore from cache/localStorage.
  const pickDomain = (slotKey, isActive) => {
    if (Object.prototype.hasOwnProperty.call(cfg, slotKey)) {
      return cfg[slotKey] == null ? "" : String(cfg[slotKey]);
    }
    if (isActive && Object.prototype.hasOwnProperty.call(cfg, "domain")) {
      return activeDomain;
    }
    return regMailDomains[slotKey.replace("_domain", "")] || "";
  };
  regMailDomains = {
    moemail: pickDomain("moemail_domain", mailProv === "moemail"),
    yyds: pickDomain("yyds_domain", mailProv === "yyds"),
    gptmail: pickDomain("gptmail_domain", mailProv === "gptmail"),
    cfmail: pickDomain("cfmail_domain", mailProv === "cfmail"),
  };
  // If server returned empty dedicated slot for active provider, force empty.
  if (mailProv === "yyds" && Object.prototype.hasOwnProperty.call(cfg, "yyds_domain")) {
    regMailDomains.yyds = cfg.yyds_domain == null ? "" : String(cfg.yyds_domain);
  }
  if (mailProv === "gptmail" && Object.prototype.hasOwnProperty.call(cfg, "gptmail_domain")) {
    regMailDomains.gptmail = cfg.gptmail_domain == null ? "" : String(cfg.gptmail_domain);
  }
  if (mailProv === "cfmail" && Object.prototype.hasOwnProperty.call(cfg, "cfmail_domain")) {
    regMailDomains.cfmail = cfg.cfmail_domain == null ? "" : String(cfg.cfmail_domain);
  }
  if (mailProv === "moemail" && Object.prototype.hasOwnProperty.call(cfg, "moemail_domain")) {
    regMailDomains.moemail = cfg.moemail_domain == null ? "" : String(cfg.moemail_domain);
  }
  regMailProviderPrev = mailProv;
  // Hydrate per-provider hosts independently.
  const pickBase = (slotKey, isActive) => {
    if (Object.prototype.hasOwnProperty.call(cfg, slotKey)) {
      return cfg[slotKey] == null ? "" : String(cfg[slotKey]);
    }
    if (isActive && Object.prototype.hasOwnProperty.call(cfg, "base_url")) {
      return cfg.base_url == null ? "" : String(cfg.base_url);
    }
    return regMailBaseUrls[slotKey.replace("_base_url", "")] || "";
  };
  regMailBaseUrls = {
    moemail: pickBase("moemail_base_url", mailProv === "moemail"),
    cfmail: pickBase("cfmail_base_url", mailProv === "cfmail"),
  };
  if (mailProv === "moemail" && Object.prototype.hasOwnProperty.call(cfg, "moemail_base_url")) {
    regMailBaseUrls.moemail = cfg.moemail_base_url == null ? "" : String(cfg.moemail_base_url);
  }
  if (mailProv === "cfmail" && Object.prototype.hasOwnProperty.call(cfg, "cfmail_base_url")) {
    regMailBaseUrls.cfmail = cfg.cfmail_base_url == null ? "" : String(cfg.cfmail_base_url);
  }
  if ($("reg-base-url")) {
    $("reg-base-url").value = (mailProv === "moemail" || mailProv === "cfmail")
      ? (regMailBaseUrls[mailProv] || "")
      : "";
  }
  if ($("reg-domain")) {
    $("reg-domain").value = regMailDomains[mailProv] || "";
    try { $("reg-domain").setAttribute("autocomplete", "off"); } catch (_) {}
  }
  if ($("reg-expiry-ms")) {
    const exp = normalizeRegExpiryMs(cfg.expiry_ms);
    $("reg-expiry-ms").value = exp;
    // Keep select valid if browser rejected an unexpected value.
    if ($("reg-expiry-ms").value !== exp) $("reg-expiry-ms").value = "3600000";
  }
  if ($("reg-api-key")) {
    $("reg-api-key").value = regMailKeys[mailProv] || "";
    try { $("reg-api-key").setAttribute("autocomplete", "off"); } catch (_) {}
  }
  if ($("reg-captcha-provider")) {
    const provider = String(cfg.captcha_provider || "local").trim().toLowerCase();
    $("reg-captcha-provider").value = provider === "yescaptcha" ? "yescaptcha" : "local";
  }
  // Local solver URL is not user-facing (always inline 127.0.0.1:5072).
  if ($("reg-yescaptcha-key")) $("reg-yescaptcha-key").value = cfg.yescaptcha_key || "";
  if ($("reg-proxy")) $("reg-proxy").value = cfg.proxy || "";
  if ($("reg-proxy-username")) $("reg-proxy-username").value = cfg.proxy_username || "";
  if ($("reg-proxy-password")) $("reg-proxy-password").value = cfg.proxy_password || "";
  if ($("reg-proxy-strategy")) {
    const strat = String(cfg.proxy_strategy || "round_robin").trim().toLowerCase();
    $("reg-proxy-strategy").value =
      strat === "random" ? "random" : strat === "sticky" ? "sticky" : "round_robin";
  }
  updateRegProxyHint(cfg);
  if ($("reg-count")) $("reg-count").value = cfg.count != null ? String(cfg.count) : "1";
  if ($("reg-concurrency")) $("reg-concurrency").value = cfg.concurrency != null ? String(cfg.concurrency) : "5";
  if ($("reg-stagger-ms")) $("reg-stagger-ms").value = cfg.stagger_ms != null ? String(cfg.stagger_ms) : "300";
  if ($("reg-probe-delay-sec")) {
    const pd = cfg.probe_delay_sec != null ? Number(cfg.probe_delay_sec) : 30;
    $("reg-probe-delay-sec").value = String(
      Number.isFinite(pd) ? Math.max(0, Math.min(600, Math.floor(pd))) : 30
    );
  }
  syncRegCaptchaProviderUI();
  syncRegMailProviderUI();
  regConfigCache = Object.assign({}, cfg);
}

function cacheRegConfigLocal(cfg) {
  try {
    localStorage.setItem(REG_CONFIG_KEY, JSON.stringify(cfg || readRegConfig()));
  } catch (_) {}
}

async function saveRegConfig() {
  const cfg = readRegConfig();
  // Do NOT cache the pre-save form first — if the user cleared domain/key,
  // caching here would let a later loadRegConfigLocal() restore the old value
  // before the server response lands.
  try {
    const r = await api("/accounts/register-email/config", {
      method: "PUT",
      body: JSON.stringify(cfg),
    });
    const saved = (r && r.config) || cfg;
    // Force active provider domain/key from what we just submitted when server
    // omits empty strings, so UI stays cleared.
    const mail = String(saved.mail_provider || cfg.mail_provider || "moemail").toLowerCase();
    if (mail === "yyds") {
      if (!Object.prototype.hasOwnProperty.call(saved, "yyds_domain")) saved.yyds_domain = cfg.yyds_domain || "";
      if (!Object.prototype.hasOwnProperty.call(saved, "domain")) saved.domain = cfg.domain || "";
      if (!Object.prototype.hasOwnProperty.call(saved, "yyds_api_key")) saved.yyds_api_key = cfg.yyds_api_key || "";
      if (!Object.prototype.hasOwnProperty.call(saved, "api_key")) saved.api_key = cfg.api_key || "";
    } else if (mail === "gptmail") {
      if (!Object.prototype.hasOwnProperty.call(saved, "gptmail_domain")) saved.gptmail_domain = cfg.gptmail_domain || "";
      if (!Object.prototype.hasOwnProperty.call(saved, "domain")) saved.domain = cfg.domain || "";
      if (!Object.prototype.hasOwnProperty.call(saved, "gptmail_api_key")) saved.gptmail_api_key = cfg.gptmail_api_key || "";
      if (!Object.prototype.hasOwnProperty.call(saved, "api_key")) saved.api_key = cfg.api_key || "";
    } else if (mail === "cfmail") {
      if (!Object.prototype.hasOwnProperty.call(saved, "cfmail_domain")) saved.cfmail_domain = cfg.cfmail_domain || "";
      if (!Object.prototype.hasOwnProperty.call(saved, "domain")) saved.domain = cfg.domain || "";
      if (!Object.prototype.hasOwnProperty.call(saved, "cfmail_api_key")) saved.cfmail_api_key = cfg.cfmail_api_key || "";
      if (!Object.prototype.hasOwnProperty.call(saved, "api_key")) saved.api_key = cfg.api_key || "";
      if (!Object.prototype.hasOwnProperty.call(saved, "base_url")) saved.base_url = cfg.base_url || "";
      if (!Object.prototype.hasOwnProperty.call(saved, "cfmail_base_url")) saved.cfmail_base_url = cfg.cfmail_base_url || cfg.base_url || "";
      // Never lose the other self-hosted host when saving CF.
      if (!Object.prototype.hasOwnProperty.call(saved, "moemail_base_url")) saved.moemail_base_url = cfg.moemail_base_url || "";
    } else {
      if (!Object.prototype.hasOwnProperty.call(saved, "moemail_domain")) saved.moemail_domain = cfg.moemail_domain || "";
      if (!Object.prototype.hasOwnProperty.call(saved, "domain")) saved.domain = cfg.domain || "";
      if (!Object.prototype.hasOwnProperty.call(saved, "base_url")) saved.base_url = cfg.base_url || "";
      if (!Object.prototype.hasOwnProperty.call(saved, "moemail_base_url")) saved.moemail_base_url = cfg.moemail_base_url || cfg.base_url || "";
      // Never lose CF host when saving MoeMail.
      if (!Object.prototype.hasOwnProperty.call(saved, "cfmail_base_url")) saved.cfmail_base_url = cfg.cfmail_base_url || "";
    }
    applyRegConfig(saved);
    cacheRegConfigLocal(saved);
    regConfigLoadedAt = Date.now();
    toast(r.message || "注册配置已保存到数据库");
    return saved;
  } catch (e) {
    // Only cache on failure so a retry still has the typed values.
    cacheRegConfigLocal(cfg);
    toast((e && e.message) || "保存失败（已写本地缓存）", false);
    throw e;
  }
}

function loadRegConfigLocal() {
  try {
    applyRegConfig(JSON.parse(localStorage.getItem(REG_CONFIG_KEY) || "null"));
  } catch (_) {}
}

async function loadRegConfig(force) {
  // Prefer server/DB truth. Local cache is only a first-paint fallback when we
  // have nothing yet — never let it overwrite a just-cleared domain/key.
  // Always re-fetch when forced, or when cache is older than 2s (multi-worker
  // saves must not stick on a stale browser/page session forever).
  if (!force && !regConfigCache) loadRegConfigLocal();
  const now = Date.now();
  if (!force && regConfigCache && now - regConfigLoadedAt < 2000) {
    applyRegConfig(regConfigCache);
    return regConfigCache;
  }
  try {
    const r = await api("/accounts/register-email/config");
    const cfg = (r && r.config) || null;
    if (cfg) {
      applyRegConfig(cfg);
      cacheRegConfigLocal(cfg);
      regConfigLoadedAt = Date.now();
      // Expose source for debugging in console when not from database.
      if (r && r.source && r.source !== "database") {
        console.info("registration_config source=", r.source);
      }
      return cfg;
    }
  } catch (e) {
    console.warn("loadRegConfig", e);
  }
  // Fallback: settings payload may already include registration_config
  try {
    const s = (statusCache && statusCache.settings) || (dashCache && dashCache.settings) || null;
    if (s && s.registration_config) {
      applyRegConfig(s.registration_config);
      cacheRegConfigLocal(s.registration_config);
      regConfigLoadedAt = Date.now();
      return s.registration_config;
    }
  } catch (_) {}
  if (regConfigCache) applyRegConfig(regConfigCache);
  else loadRegConfigLocal();
  return regConfigCache;
}
function buildRegBody(config) {
  const body = {};
  const mailProvider = String(config.mail_provider || "moemail").trim().toLowerCase();
  body.mail_provider =
    mailProvider === "yyds"
      ? "yyds"
      : mailProvider === "gptmail"
        ? "gptmail"
        : mailProvider === "cfmail"
          ? "cfmail"
          : "moemail";
  // Keep legacy field for older backends.
  body.provider = body.mail_provider;
  // MoeMail + CF Temp Email need base_url; YYDS/GPTMail use fixed hosts.
  // Always send dedicated host slots (including empty) so saves stay isolated.
  body.moemail_base_url = config.moemail_base_url == null ? "" : String(config.moemail_base_url);
  body.cfmail_base_url = config.cfmail_base_url == null ? "" : String(config.cfmail_base_url);
  if (body.mail_provider === "moemail") {
    body.base_url = body.moemail_base_url || (config.base_url == null ? "" : String(config.base_url));
    body.moemail_base_url = body.base_url;
  } else if (body.mail_provider === "cfmail") {
    body.base_url = body.cfmail_base_url || (config.base_url == null ? "" : String(config.base_url));
    body.cfmail_base_url = body.base_url;
  }
  // Always send domain for the active provider (empty clears/auto).
  body.domain = config.domain == null ? "" : String(config.domain);
  // Always send an official MoeMail preset (including permanent=0).
  // YYDS / GPTMail are ~24h; still send 1d when selected.
  body.expiry_ms = Number.parseInt(normalizeRegExpiryMs(config.expiry_ms), 10);
  if (
    (body.mail_provider === "yyds" || body.mail_provider === "gptmail") &&
    (body.expiry_ms === 0 || body.expiry_ms === 259200000)
  ) {
    body.expiry_ms = 86400000;
  }
  // Always send active key/domain, including empty string, so "delete + save"
  // clears DB instead of restoring the previous value.
  body.api_key = config.api_key == null ? "" : String(config.api_key);
  if (body.mail_provider === "moemail") {
    body.moemail_api_key = config.moemail_api_key == null ? body.api_key : String(config.moemail_api_key);
    body.moemail_domain = config.moemail_domain == null ? body.domain : String(config.moemail_domain);
  } else if (body.mail_provider === "yyds") {
    body.yyds_api_key = config.yyds_api_key == null ? body.api_key : String(config.yyds_api_key);
    body.yyds_domain = config.yyds_domain == null ? body.domain : String(config.yyds_domain);
  } else if (body.mail_provider === "gptmail") {
    body.gptmail_api_key = config.gptmail_api_key == null ? body.api_key : String(config.gptmail_api_key);
    body.gptmail_domain = config.gptmail_domain == null ? body.domain : String(config.gptmail_domain);
  } else if (body.mail_provider === "cfmail") {
    body.cfmail_api_key = config.cfmail_api_key == null ? body.api_key : String(config.cfmail_api_key);
    body.cfmail_domain = config.cfmail_domain == null ? body.domain : String(config.cfmail_domain);
  }
  const provider = String(config.captcha_provider || "local").trim().toLowerCase();
  body.captcha_provider = provider === "yescaptcha" ? "yescaptcha" : "local";
  // Local mode: always inline; never send custom URL.
  if (body.captcha_provider === "local") {
    body.local_solver_url = "http://127.0.0.1:5072";
  } else if (config.yescaptcha_key) {
    body.yescaptcha_key = config.yescaptcha_key;
  }
  if (config.proxy) body.proxy = config.proxy;
  if (config.proxy_username) body.proxy_username = config.proxy_username;
  if (config.proxy_password) body.proxy_password = config.proxy_password;
  if (config.proxy_strategy) body.proxy_strategy = config.proxy_strategy;
  const count = Number.parseInt(config.count || "1", 10);
  const concurrency = Number.parseInt(config.concurrency || "5", 10);
  const stagger = Number.parseInt(config.stagger_ms || "300", 10);
  const probeDelay = Number.parseInt(config.probe_delay_sec || "30", 10);
  if (Number.isFinite(count) && count > 0) body.count = Math.floor(count);
  // threads / concurrency: real in-flight registration cap (3 => 3 at a time)
  if (Number.isFinite(concurrency) && concurrency > 0) body.concurrency = Math.min(10, Math.max(1, Math.floor(concurrency)));
  if (Number.isFinite(stagger) && stagger >= 0) body.stagger_ms = Math.min(10000, Math.floor(stagger));
  if (Number.isFinite(probeDelay) && probeDelay >= 0) {
    body.probe_delay_sec = Math.min(600, Math.max(0, Math.floor(probeDelay)));
  }
  return body;
}

function getRegProbeDelaySec() {
  // Prefer current form value, then cached config, then 30s default.
  const raw = $("reg-probe-delay-sec")
    ? $("reg-probe-delay-sec").value
    : (regConfigCache && regConfigCache.probe_delay_sec != null
      ? regConfigCache.probe_delay_sec
      : 30);
  const n = Number.parseInt(String(raw ?? "30"), 10);
  if (!Number.isFinite(n) || n < 0) return 30;
  return Math.min(600, Math.max(0, n));
}
function buildProxyTestBody(config) {
  const body = {};
  if (config.proxy) body.proxy = config.proxy;
  if (config.proxy_username) body.proxy_username = config.proxy_username;
  if (config.proxy_password) body.proxy_password = config.proxy_password;
  if (config.proxy_strategy) body.proxy_strategy = config.proxy_strategy;
  // Multi-proxy lists: smoke-test up to 5 entries so pool health is visible.
  const lines = String(config.proxy || "")
    .split(/\r?\n|;|,/)
    .map((s) => s.trim())
    .filter((s) => s && !s.startsWith("#"));
  if (lines.length > 1) {
    body.test_all = true;
    body.max_test = Math.min(5, lines.length);
  }
  return body;
}

function countProxyLines(text) {
  return String(text || "")
    .split(/\r?\n|;/)
    .map((s) => s.trim())
    .filter((s) => s && !s.startsWith("#"))
    .length;
}

function updateRegProxyHint(cfg) {
  const el = $("reg-proxy-hint");
  if (!el) return;
  const text = $("reg-proxy") ? $("reg-proxy").value : (cfg && cfg.proxy) || "";
  const n = countProxyLines(text);
  const strat = $("reg-proxy-strategy")
    ? $("reg-proxy-strategy").value
    : (cfg && cfg.proxy_strategy) || "round_robin";
  const stratLabel =
    strat === "random" ? "随机" : strat === "sticky" ? "固定首个" : "轮询";
  if (n <= 0) {
    el.textContent = "未配置代理（直连）。可粘贴多行代理池；批量注册时按策略轮换。";
  } else if (n === 1) {
    el.textContent = `已配置 1 个代理（策略：${stratLabel}）。支持多行池。`;
  } else {
    el.textContent = `代理池 ${n} 个 · 策略：${stratLabel}。批量注册时每个任务取一个代理。`;
  }
}
const REG_TERMINAL_OK = new Set(["success", "completed", "imported"]);
const REG_TERMINAL_BAD = new Set([
  "error",
  "failed",
  "expired",
  "protocol_error",
  "protocol_blocked",
  "cancelled",
  "stopped",
]);

function isRegTerminalStatus(status) {
  const st = String(status || "").toLowerCase();
  return REG_TERMINAL_OK.has(st) || REG_TERMINAL_BAD.has(st);
}

function hasTrackedRegTask() {
  return !!(regBatchId || regSessionId || (regSessionIds && regSessionIds.length));
}

function adoptRegSessions(sessions, { batch = null, continuePolling = true } = {}) {
  const list = Array.isArray(sessions) ? sessions.filter(Boolean) : [];
  const batchObj = batch && typeof batch === "object" ? batch : null;
  const batchId =
    (batchObj && (batchObj.batch_id || batchObj.id)) ||
    (list.find((s) => s && s.batch_id)?.batch_id) ||
    null;
  const ids = [];
  for (const s of list) {
    const id = regSessionKey(s);
    if (id && !ids.includes(id)) ids.push(id);
  }
  if (batchObj && Array.isArray(batchObj.session_ids)) {
    for (const id of batchObj.session_ids) {
      if (id && !ids.includes(id)) ids.push(String(id));
    }
  }
  if (!ids.length && !batchId) return false;

  // Preserve "already finished" across restore so we never re-toast completion.
  const wasFinished = !!regFinishedNotified;

  regBatchId = batchId || regBatchId || null;
  regSessionIds = ids.length ? ids.slice() : (regSessionId ? [regSessionId] : []);
  regSessionId = regSessionIds[0] || regSessionId || null;
  regStopping = false;
  regPollInFlight = false;
  regLastLogText = "";
  regLastStatusText = "";
  regLastEmailText = "";

  const batchStatus = String(
    (batchObj && (batchObj.batch_status || batchObj.status)) || ""
  ).toLowerCase();
  const batchRunning = Number((batchObj && batchObj.running) || 0) > 0;
  const batchDone =
    !!batchObj &&
    (batchStatus === "done" ||
      batchStatus === "partial" ||
      batchStatus === "error" ||
      batchStatus === "cancelled" ||
      batchStatus === "stopped" ||
      batchStatus === "failed" ||
      (Number(batchObj.done || 0) > 0 &&
        Number(batchObj.done || 0) >= Number(batchObj.total || batchObj.count || 0) &&
        !batchRunning) ||
      (Number((batchObj.imported || 0) + (batchObj.error || 0) + (batchObj.cancelled || 0)) > 0 &&
        Number((batchObj.imported || 0) + (batchObj.error || 0) + (batchObj.cancelled || 0)) >=
          Number(batchObj.total || batchObj.count || 0) &&
        !batchRunning));
  const allTerminal =
    list.length > 0 &&
    list.every((s) => isRegTerminalStatus(regStatusOf(s)));
  const finished = !!wasFinished || !!batchDone || (allTerminal && !batchRunning);

  // Placeholder sessions for UI only when real session objects are missing.
  // Never fake "running" for an already-finished batch — that freezes the card.
  const placeholderStatus = finished
    ? (batchStatus === "error" || batchStatus === "failed"
        ? "error"
        : batchStatus === "cancelled" || batchStatus === "stopped"
          ? "cancelled"
          : "done")
    : "running";
  const placeholderSessions = regSessionIds.map((id) => ({
    id,
    status: placeholderStatus,
    batch_id: regBatchId,
  }));

  if (list.length <= 1 && !regBatchId) {
    showRegSession(
      list[0] ||
        batchObj ||
        { id: regSessionId, status: placeholderStatus, batch_id: regBatchId },
      { batch: batchObj }
    );
  } else {
    showRegSessionGroup(list.length ? list : placeholderSessions, { batch: batchObj });
  }

  if (finished) {
    // Already terminal: paint final card once, never re-toast via forced re-poll.
    regFinishedNotified = true;
    stopRegPolling();
    // Ensure status text is not left as "restoring…"
    try {
      const total = Number((batchObj && (batchObj.total || batchObj.count)) || regSessionIds.length || 0);
      const done = Number((batchObj && batchObj.done) || total || 0);
      const ok = Number((batchObj && (batchObj.imported || batchObj.success)) || 0);
      const fail = Number((batchObj && (batchObj.error || batchObj.failed)) || 0);
      setRegStatusText(
        total > 0
          ? `已结束 · ${done}/${total}` + (ok || fail ? ` · 成功 ${ok} / 失败 ${fail}` : "")
          : "已结束"
      );
      setRegEmailText(regBatchId ? `batch ${regBatchId}` : (regSessionId || "—"));
    } catch (_) {}
    saveRegTrack();
    return true;
  }

  // Live task only.
  regFinishedNotified = false;
  if (continuePolling) {
    startRegPolling({ immediate: true, intervalMs: 1000 });
  }
  saveRegTrack();
  return true;
}

async function restoreTrackedRegistration({ toastIfEmpty = false } = {}) {
  // Prefer the last browser-tracked batch/session ids (hard refresh safe).
  const track = loadRegTrack();
  if (!track) return false;
  if (!applyRegTrack(track)) return false;

  const alreadyFinished = !!(track.finished || regFinishedNotified);
  showPanel("reg-session-box");
  setRegStatusText(alreadyFinished ? "已结束 · 恢复中…" : "restoring…");
  setRegEmailText(regBatchId ? `batch ${regBatchId}` : (regSessionId || "—"));
  setLogPanel(
    "reg-log",
    [
      "[恢复] 正在从后端恢复注册进度…",
      regBatchId ? `batch_id: ${regBatchId}` : "",
      regSessionId ? `session_id: ${regSessionId}` : "",
      regSessionIds.length > 1 ? `session_ids: ${regSessionIds.slice(0, 12).join(", ")}${regSessionIds.length > 12 ? "…" : ""}` : "",
    ].filter(Boolean).join("\n"),
    { forceShow: true }
  );

  // Fetch tracked batch/session even if list endpoint is empty on this worker.
  try {
    let batch = null;
    let sessions = [];
    let batchMissing = false;
    if (regBatchId) {
      try {
        batch = await api("/accounts/register-email/batches/" + encodeURIComponent(regBatchId));
      } catch (e) {
        batch = null;
        batchMissing = isNotFoundError(e);
      }
    }
    if (batch) {
      if (Array.isArray(batch.session_ids) && batch.session_ids.length) {
        for (const id of batch.session_ids) {
          if (id && !regSessionIds.includes(id)) regSessionIds.push(id);
        }
        regSessionId = regSessionIds[0] || regSessionId;
      }
      if (Array.isArray(batch.sessions) && batch.sessions.length) {
        sessions = batch.sessions.slice();
      }
    }
    if (!sessions.length) {
      const ids = regSessionIds.length ? regSessionIds : (regSessionId ? [regSessionId] : []);
      let foundAny = false;
      let missingAll = ids.length > 0;
      for (const id of ids.slice(0, 40)) {
        try {
          const s = await api("/accounts/register-email/sessions/" + encodeURIComponent(id));
          if (s) {
            sessions.push(s);
            foundAny = true;
            missingAll = false;
          }
        } catch (e) {
          if (!isNotFoundError(e)) missingAll = false;
        }
      }
      // Only treat as fully missing when every looked-up id 404'd.
      if (!foundAny && missingAll && (batchMissing || !regBatchId)) {
        markTrackedRegistrationMissing("tracked batch/session 404");
        if (toastIfEmpty) toast("未找到进行中的注册任务", false);
        return true;
      }
    }
    if (sessions.length || batch) {
      // Preserve finished flag from sessionStorage so re-adopt does not re-toast.
      const preserveFinished = alreadyFinished || regFinishedNotified;
      const ok = adoptRegSessions(sessions, {
        batch: batch || (regBatchId ? { id: regBatchId, batch_id: regBatchId, session_ids: regSessionIds } : null),
        continuePolling: !preserveFinished,
      });
      if (ok) {
        if (preserveFinished) {
          regFinishedNotified = true;
          stopRegPolling();
          saveRegTrack();
        }
        return true;
      }
    }
    // Track exists but backend no longer has it (TTL expired / finished ages ago).
    markTrackedRegistrationMissing(batchMissing ? "batch not found" : "session not found");
    if (toastIfEmpty) toast("未找到进行中的注册任务", false);
    return true;
  } catch (e) {
    if (toastIfEmpty) toast((e && e.message) || "恢复注册进度失败", false);
    return hasTrackedRegTask();
  }
}

async function restoreActiveRegistration({ force = false, toastIfEmpty = false } = {}) {
  // Hard refresh / soft-nav re-entry loses in-memory session ids. Rebuild from backend.
  if (!hasTrackedRegTask()) {
    // Rehydrate from sessionStorage first (hard refresh path).
    applyRegTrack(loadRegTrack());
  }
  // Already showing a finished card — don't re-adopt/re-toast on soft refresh.
  if (!force && hasTrackedRegTask() && regFinishedNotified) {
    showPanel("reg-session-box");
    return true;
  }
  // Always re-validate tracked ids against backend. Blindly resuming poll from a
  // stale sessionStorage track is what spammed console 404s after restarts.
  try {
    // 1) Prefer explicitly tracked ids (survives refresh even when list is empty).
    if (hasTrackedRegTask() || loadRegTrack()) {
      const tracked = await restoreTrackedRegistration({ toastIfEmpty: false });
      if (tracked) return true;
    }

    const all = await api("/accounts/register-email/sessions");
    const sessions = Array.isArray(all && all.sessions) ? all.sessions : [];
    const batches = Array.isArray(all && all.batches) ? all.batches : [];

    const activeBatches = batches
      .filter((b) => {
        if (!b) return false;
        const st = String((b.batch_status || b.status) || "").toLowerCase();
        const running = Number(b.running || 0);
        const total = Number(b.total || b.count || b.spawned || 0);
        const done = Number(b.done || 0);
        // Explicit live work.
        if (running > 0) return true;
        // Terminal statuses are never "active".
        if (
          st === "done" ||
          st === "partial" ||
          st === "error" ||
          st === "cancelled" ||
          st === "stopped" ||
          st === "failed"
        ) {
          return false;
        }
        // done >= total with no running → finished even if status lagging.
        if (total > 0 && done >= total && running <= 0) return false;
        // Only treat as active when status is clearly non-terminal / in-flight.
        if (st === "running" || st === "starting" || st === "stopping" || st === "queued") {
          return true;
        }
        // Unknown / empty status with no running workers is a ghost — ignore.
        return false;
      })
      .sort(
        (a, b) =>
          Number((b && (b.updated_at || b.created_at)) || 0) -
          Number((a && (a.updated_at || a.created_at)) || 0)
      );

    if (activeBatches.length) {
      const batch = activeBatches[0];
      const bid = batch.batch_id || batch.id;
      let full = batch;
      if (bid) {
        try {
          full = await api("/accounts/register-email/batches/" + encodeURIComponent(bid));
        } catch (_) {
          full = batch;
        }
      }
      const sess =
        (full && Array.isArray(full.sessions) && full.sessions.length
          ? full.sessions
          : sessions.filter((s) => s && s.batch_id && s.batch_id === bid)) || [];
      const ok = adoptRegSessions(sess, { batch: full || batch, continuePolling: true });
      if (ok) return true;
    }

    const activeSessions = sessions
      .filter((s) => !isRegTerminalStatus(regStatusOf(s)))
      .sort(
        (a, b) =>
          Number((b && (b.updated_at || b.created_at)) || 0) -
          Number((a && (a.updated_at || a.created_at)) || 0)
      );
    if (activeSessions.length) {
      // Prefer the newest active batch cluster when sessions share batch_id.
      const top = activeSessions[0];
      const bid = top && top.batch_id;
      if (bid) {
        const cluster = activeSessions.filter((s) => s && s.batch_id === bid);
        const batchMeta = batches.find((b) => (b.id || b.batch_id) === bid) || {
          id: bid,
          batch_id: bid,
          session_ids: cluster.map(regSessionKey).filter(Boolean),
          running: cluster.length,
          status: "running",
        };
        const ok = adoptRegSessions(cluster, { batch: batchMeta, continuePolling: true });
        if (ok) return true;
      }
      const ok = adoptRegSessions([top], { continuePolling: true });
      if (ok) return true;
    }

    // No live task: if we were already tracking something, keep the last card.
    // Otherwise leave the card hidden — user can start a new registration.
    if (toastIfEmpty) toast("当前没有进行中的注册", false);
    return false;
  } catch (e) {
    if (toastIfEmpty) toast((e && e.message) || "刷新注册进度失败", false);
    return false;
  }
}

async function refreshRegistrationProgress({ toastIfEmpty = true } = {}) {
  // Finished card: keep it visible, do not re-fire completion toast.
  if (hasTrackedRegTask() && regFinishedNotified) {
    showPanel("reg-session-box");
    if (toastIfEmpty) toast("注册已结束（可点关闭收起进度卡片）", true);
    return true;
  }
  if (hasTrackedRegTask()) {
    showPanel("reg-session-box");
    await pollRegSession();
    return true;
  }
  const track = loadRegTrack();
  if (track && track.finished && applyRegTrack(track)) {
    regFinishedNotified = true;
    showPanel("reg-session-box");
    setRegStatusText("已结束");
    setRegEmailText(regBatchId ? `batch ${regBatchId}` : (regSessionId || "—"));
    if (toastIfEmpty) toast("注册已结束（可点关闭收起进度卡片）", true);
    return true;
  }
  return restoreActiveRegistration({ force: true, toastIfEmpty });
}

function regSessionKey(s) {
  return (s && (s.id || s.session_id)) || "";
}

function regStatusOf(s) {
  return String((s && s.status) || "").toLowerCase();
}

function summarizeRegSessions(sessions) {
  const list = Array.isArray(sessions) ? sessions : [];
  let success = 0;
  let fail = 0;
  let probing = 0;
  let running = 0;
  for (const s of list) {
    const st = regStatusOf(s);
    if (REG_TERMINAL_OK.has(st)) success += 1;
    else if (REG_TERMINAL_BAD.has(st)) fail += 1;
    else if (st === "probing") probing += 1;
    else running += 1;
  }
  return {
    total: list.length,
    success,
    fail,
    probing,
    running,
    done: success + fail,
  };
}

function collectImportedAccountIds(sessions) {
  const out = [];
  const seen = new Set();
  for (const s of sessions || []) {
    const ids = Array.isArray(s.imported_account_ids) ? s.imported_account_ids : [];
    for (const id of ids) {
      const k = String(id || "").trim();
      if (!k || seen.has(k)) continue;
      seen.add(k);
      out.push(k);
    }
    const accounts = Array.isArray(s.imported_accounts) ? s.imported_accounts : [];
    for (const a of accounts) {
      const k = String((a && a.id) || "").trim();
      if (!k || seen.has(k)) continue;
      seen.add(k);
      out.push(k);
    }
  }
  return out;
}

function formatRegSessionLine(s, idx) {
  const st = regStatusOf(s) || "—";
  const email = (s && s.email) || "—";
  const id = regSessionKey(s) || `#${idx + 1}`;
  const msg = (s && (s.message || s.error)) || "";
  const probe = s && s.probe;
  let probeTxt = "";
  if (probe && typeof probe === "object") {
    probeTxt = ` | 测活 ok=${probe.ok ?? 0} fail=${probe.fail ?? 0}`;
  } else if (st === "probing") {
    probeTxt = " | 测活中…";
  }
  const shortMsg = msg ? ` | ${String(msg).slice(0, 120)}` : "";
  return `[${idx + 1}] ${st.padEnd(10)} ${email} (${id})${probeTxt}${shortMsg}`;
}

function buildRegLogText(sessions, { batch = null, extraLines = [] } = {}) {
  const stats = summarizeRegSessions(sessions);
  const success = Math.max(stats.success, Number((batch && batch.imported) || 0) || 0);
  const fail = Math.max(stats.fail, Number((batch && batch.error) || 0) || 0);
  const cancelled = Math.max(
    (sessions || []).filter((s) => {
      const st = regStatusOf(s);
      return st === "cancelled" || st === "stopped";
    }).length,
    Number((batch && batch.cancelled) || 0) || 0
  );
  const running = Math.max(
    stats.running + stats.probing,
    Number((batch && batch.running) || 0) || 0
  );
  const total = Math.max(
    stats.total,
    Number((batch && (batch.total || batch.count || batch.spawned)) || 0) || 0,
    Array.isArray(batch && batch.session_ids) ? batch.session_ids.length : 0
  );
  const lines = [];
  lines.push("======== 协议注册进度 ========");
  if (batch && (batch.batch_id || batch.id)) {
    lines.push(`batch_id: ${batch.batch_id || batch.id}`);
  } else if (regBatchId) {
    lines.push(`batch_id: ${regBatchId}`);
  }
  lines.push(
    `统计: 总数 ${total || stats.total} · 成功 ${success} · 失败 ${fail}` +
      (cancelled ? ` · 已停止 ${cancelled}` : "") +
      (stats.probing ? ` · 测活中 ${stats.probing}` : "") +
      (running ? ` · 进行中 ${running}` : "")
  );
  if (batch && batch.message) lines.push(`batch: ${batch.message}`);
  lines.push("-------- 会话明细 --------");
  (sessions || []).forEach((s, i) => lines.push(formatRegSessionLine(s, i)));
  // Probe details from backend auto-probe
  const probeRows = [];
  for (const s of sessions || []) {
    const results = s && s.probe && Array.isArray(s.probe.results) ? s.probe.results : [];
    for (const p of results) {
      probeRows.push(
        `  · ${p.ok ? "✓" : "✗"} ${p.account_id || "?"} model=${p.model || "—"}` +
          (p.latency_ms != null ? ` ${p.latency_ms}ms` : "") +
          (p.error ? ` err=${String(p.error).slice(0, 100)}` : "")
      );
    }
  }
  if (probeRows.length) {
    lines.push("-------- 入池测活结果 --------");
    lines.push(...probeRows);
  }
  for (const x of extraLines || []) {
    if (x) lines.push(String(x));
  }
  lines.push("==============================");
  return lines.join("\n");
}

function showRegSession(s, opts = {}) {
  showPanel("reg-session-box");
  const rawSt = String((s && (s.status || s.message)) || "—");
  const stLow = rawSt.toLowerCase();
  // While user requested stop, keep a stable "stopping" label to avoid flicker
  // between server "stopping" and temporary "queued/running" snapshots.
  const st =
    regStopping && !REG_TERMINAL_OK.has(stLow) && !REG_TERMINAL_BAD.has(stLow)
      ? "stopping"
      : rawSt;
  setRegStatusText(st);
  setRegEmailText((s && (s.email || s.id || s.session_id)) || "—");
  const stats = summarizeRegSessions(s ? [s] : []);
  const head = `成功 ${stats.success} · 失败 ${stats.fail}` +
    (stats.probing || stats.running ? ` · 进行中 ${stats.probing + stats.running}` : "");
  const log = buildRegLogText(s ? [s] : [], {
    batch: opts.batch || null,
    extraLines: [
      s && (s.output_tail || s.log) ? String(s.output_tail || s.log).slice(0, 800) : "",
      head,
      regStopping ? "[stop] 停止中…" : "",
    ].filter(Boolean),
  });
  setLogPanel("reg-log", log, { forceShow: true });
}

function showRegSessionGroup(sessions, opts = {}) {
  showPanel("reg-session-box");
  const stats = summarizeRegSessions(sessions);
  const batch = opts.batch || null;
  // Prefer batch-level counters for large jobs (UI may only hold a compact session window).
  const success = Math.max(stats.success, Number((batch && batch.imported) || 0) || 0);
  const fail = Math.max(stats.fail, Number((batch && batch.error) || 0) || 0);
  const cancelled = Math.max(
    sessions.filter((s) => {
      const st = regStatusOf(s);
      return st === "cancelled" || st === "stopped";
    }).length,
    Number((batch && batch.cancelled) || 0) || 0
  );
  const running = Math.max(
    stats.running + stats.probing,
    Number((batch && batch.running) || 0) || 0
  );
  const total = Math.max(
    stats.total,
    Number((batch && (batch.total || batch.count || batch.spawned)) || 0) || 0,
    Array.isArray(batch && batch.session_ids) ? batch.session_ids.length : 0,
    regSessionIds.length || 0
  );
  setRegEmailText(
    `${total || stats.total} 个注册会话` + (regBatchId ? ` · ${regBatchId}` : "")
  );
  // Prefer stable stop status while stop is in flight and work remains.
  if (regStopping && running > 0) {
    setRegStatusText(
      `停止中 · 成功 ${success} · 失败 ${fail}` +
        (cancelled ? ` · 已停 ${cancelled}` : "") +
        (total ? ` / ${total}` : "")
    );
  } else {
    setRegStatusText(
      `成功 ${success} · 失败 ${fail}` +
        (cancelled ? ` · 停止 ${cancelled}` : "") +
        (running ? ` · 运行 ${running}` : "") +
        (total ? ` / ${total}` : "")
    );
  }
  setLogPanel(
    "reg-log",
    buildRegLogText(sessions, {
      batch,
      extraLines: [
        total > stats.total
          ? `[注] 明细仅展示最近 ${stats.total} 条会话；上方统计按批次总数 ${total}`
          : "",
        regStopping && running > 0 ? "[stop] 停止中，等待进行中的任务退出…" : "",
      ].filter(Boolean),
    }),
    { forceShow: true }
  );
}

async function probeImportedAccounts(accountIds, { sessions = [], delaySec = 30 } = {}) {
  const ids = (accountIds || []).filter((id) => id && !regProbedIds.has(id));
  if (!ids.length || regProbeRunning) return null;
  regProbeRunning = true;
  const wait = Math.max(0, Number(delaySec) || 0);
  const lines = [];
  if (wait > 0) {
    lines.push(`[测活] 新号入池后等待 ${wait}s 再检测 ×${ids.length}…`);
    try {
      setLogPanel(
        "reg-log",
        buildRegLogText(sessions, { extraLines: lines }),
        { forceShow: true }
      );
    } catch (_) {}
    await new Promise((resolve) => setTimeout(resolve, wait * 1000));
  }
  lines.push(`[测活] 开始检测新入池账号 ×${ids.length}…`);
  try {
    setLogPanel(
      "reg-log",
      buildRegLogText(sessions, { extraLines: lines }),
      { forceShow: true }
    );
  } catch (_) {}
  const results = [];
  for (const id of ids) {
    regProbedIds.add(id);
    try {
      const r = await api("/accounts/" + encodeURIComponent(id) + "/probe", {
        method: "POST",
        body: JSON.stringify({ auto_disable: true }),
      });
      const detail = (r && r.result) || r || {};
      const ok = !!(r && r.ok);
      results.push({
        account_id: id,
        ok,
        model: detail.model || r.model,
        error: detail.error || r.error || null,
        latency_ms: detail.latency_ms || detail.elapsed_ms || null,
      });
      lines.push(
        `  ${ok ? "✓" : "✗"} ${id}` +
          (detail.model ? ` model=${detail.model}` : "") +
          (detail.error ? ` err=${String(detail.error).slice(0, 100)}` : "")
      );
    } catch (e) {
      results.push({ account_id: id, ok: false, error: (e && e.message) || String(e) });
      lines.push(`  ✗ ${id} err=${(e && e.message) || e}`);
    }
  }
  const okN = results.filter((x) => x.ok).length;
  const failN = results.length - okN;
  lines.push(`[测活] 完成：成功 ${okN} · 失败 ${failN}`);
  try {
    setLogPanel(
      "reg-log",
      buildRegLogText(sessions, { extraLines: lines }),
      { forceShow: true }
    );
  } catch (_) {}
  regProbeRunning = false;
  return { ok: okN, fail: failN, results, lines };
}

async function pollRegSession() {
  // Trailing-edge: never silently drop a tick while a previous poll is still
  // in flight — that freezes the progress log until the next free interval.
  if (regPollInFlight) {
    regPollPending = true;
    return;
  }
  regPollInFlight = true;
  regPollPending = false;
  try {
  // Prefer batch endpoint when available for accurate total/success/fail.
  // Batch embeds compact sessions (status/message/updated_at), so one request
  // is enough for timely log updates in the common path.
  let batch = null;
  let batchMissing = false;
  if (regBatchId) {
    try {
      batch = await api("/accounts/register-email/batches/" + encodeURIComponent(regBatchId));
      if (batch && Array.isArray(batch.session_ids)) {
        for (const id of batch.session_ids) {
          if (id && !regSessionIds.includes(id)) regSessionIds.push(id);
        }
      }
    } catch (e) {
      batchMissing = isNotFoundError(e);
    }
  }

  const ids = regSessionIds.length ? regSessionIds : (regSessionId ? [regSessionId] : []);
  if (!ids.length && !batch) {
    if (batchMissing || regBatchId) {
      markTrackedRegistrationMissing(batchMissing ? "batch not found while polling" : "no sessions");
    }
    return;
  }

  try {
    let sessions = [];
    let sessionHits = 0;
    let sessionMisses = 0;
    if (batch && Array.isArray(batch.sessions) && batch.sessions.length) {
      sessions = batch.sessions.slice();
      sessionHits = sessions.length;
    } else {
      // Parallel fetches: serial awaits on N sessions made each tick multi-second
      // and caused the log panel to lag far behind real progress.
      const results = await Promise.all(
        ids.map(async (id) => {
          try {
            return { ok: true, data: await api("/accounts/register-email/sessions/" + encodeURIComponent(id)) };
          } catch (e) {
            return { ok: false, missing: isNotFoundError(e) };
          }
        })
      );
      for (const r of results) {
        if (r && r.ok && r.data) {
          sessions.push(r.data);
          sessionHits += 1;
        } else if (r && r.missing) {
          sessionMisses += 1;
        }
      }
    }

    // Pull all sessions so late-spawned batch workers appear.
    // Strict filter: only the currently tracked batch / session ids — never
    // absorb leftover sessions from a previous finished/stopped registration.
    // Skip this extra list call when the batch payload already has sessions —
    // it was the main source of 2–4s poll lag under multi-worker load.
    let listHasTrackedBatch = false;
    const needListSweep =
      !batch ||
      !Array.isArray(batch.sessions) ||
      !batch.sessions.length ||
      (Array.isArray(batch.session_ids) && batch.sessions.length < batch.session_ids.length);
    if (needListSweep) try {
      const all = await api("/accounts/register-email/sessions");
      if (all && Array.isArray(all.sessions)) {
        const trackedIds = new Set(
          (regSessionIds && regSessionIds.length
            ? regSessionIds
            : (regSessionId ? [regSessionId] : [])
          ).map((x) => String(x || "").trim()).filter(Boolean)
        );
        const known = new Set(sessions.map(regSessionKey).filter(Boolean));
        for (const s of all.sessions) {
          const id = regSessionKey(s);
          if (!id) continue;
          const sameBatch = !!(regBatchId && s.batch_id && s.batch_id === regBatchId);
          const tracked = trackedIds.has(id);
          // Without a batch id, only accept explicitly tracked session ids.
          if (!(sameBatch || tracked)) continue;
          if (!regSessionIds.includes(id)) regSessionIds.push(id);
          if (!known.has(id)) {
            sessions.push(s);
            known.add(id);
            sessionHits += 1;
          } else {
            // Prefer the fresher message/status by updated_at when merging.
            const idx = sessions.findIndex((x) => regSessionKey(x) === id);
            if (idx >= 0) {
              const cur = sessions[idx] || {};
              const curTs = Number(cur.updated_at || 0) || 0;
              const nextTs = Number(s.updated_at || 0) || 0;
              if (nextTs >= curTs) sessions[idx] = s;
            }
          }
        }
        // Prefer batch stats from list endpoint when present.
        if (regBatchId && Array.isArray(all.batches)) {
          const listed = all.batches.find((b) => (b.id || b.batch_id) === regBatchId) || null;
          if (listed) {
            listHasTrackedBatch = true;
            if (!batch) batch = listed;
          }
        }
      }
    } catch (_) {}

    if (!sessions.length && !batch) {
      // All known ids 404'd and list has no matching batch → drop stale track.
      if (
        batchMissing ||
        (ids.length > 0 && sessionMisses >= ids.length && sessionHits === 0 && !listHasTrackedBatch)
      ) {
        markTrackedRegistrationMissing(
          batchMissing
            ? "batch not found while polling"
            : `sessions not found (${sessionMisses}/${ids.length})`
        );
      }
      return;
    }

    // Merge batch-level counters into status when session list still spawning.
    if (batch && (!sessions.length || (batch.count && sessions.length < batch.count))) {
      // keep showing partial list
    }

    if (sessions.length <= 1 && !regBatchId) showRegSession(sessions[0] || batch, { batch });
    else showRegSessionGroup(sessions, { batch });
    // Keep browser track in sync as late-spawned sessions appear.
    saveRegTrack();

    const stats = summarizeRegSessions(sessions);
    // Use batch totals if spawner hasn't emitted all sessions yet.
    const targetTotal = Math.max(
      stats.total,
      Number((batch && (batch.total || batch.count || batch.spawned)) || 0) || 0,
      regSessionIds.length
    );
    const batchStatus = String(
      (batch && (batch.batch_status || batch.status)) || ""
    ).toLowerCase();
    const batchRunningNow = Number((batch && batch.running) || 0) > 0;
    const batchDone =
      batch &&
      (batchStatus === "done" ||
        batchStatus === "partial" ||
        batchStatus === "error" ||
        batchStatus === "cancelled" ||
        batchStatus === "stopped" ||
        batchStatus === "failed" ||
        // Counters say complete and nothing is still running.
        (Number(batch.done || 0) > 0 &&
          Number(batch.done || 0) >= Number(batch.total || batch.count || 0) &&
          !batchRunningNow) ||
        // Spawner counters already match target with no live workers (status lag).
        (Number((batch.imported || 0) + (batch.error || 0) + (batch.cancelled || 0)) > 0 &&
          Number((batch.imported || 0) + (batch.error || 0) + (batch.cancelled || 0)) >=
            Number(batch.total || batch.count || 0) &&
          !batchRunningNow));
    const batchStopping =
      !!regStopping ||
      batchStatus === "stopping" ||
      !!(batch && batch.cancel_requested);

    const allTerminal =
      sessions.length > 0 &&
      sessions.every((s) => REG_TERMINAL_OK.has(regStatusOf(s)) || REG_TERMINAL_BAD.has(regStatusOf(s)));
    // Prefer batch-level completion: large batches may only keep a compact session window in UI.
    // Also finish when every observed session is terminal and batch reports no running workers,
    // even if the UI only holds a compact window of sessions.
    const finished =
      !!batchDone ||
      (allTerminal &&
        !batchRunningNow &&
        (targetTotal <= 0 ||
          sessions.length >= targetTotal ||
          !regBatchId ||
          batchStopping ||
          Number((batch && batch.done) || 0) >= targetTotal));

    // Fallback client-side probe for imported accounts missing backend probe.
    // Skip while stopping — no need to thrash the card with new probe lines mid-stop.
    const importedIds = collectImportedAccountIds(sessions);
    const needProbe = importedIds.filter((id) => !regProbedIds.has(id));
    const backendProbed = sessions.some(
      (s) => s && s.probe && (s.probe.count > 0 || (Array.isArray(s.probe.results) && s.probe.results.length))
    );
    if (!regStopping && needProbe.length && !backendProbed && !regProbeRunning) {
      // Fire and continue polling; probe results append to log.
      // New registrations: wait probe_delay_sec before first health probe.
      probeImportedAccounts(needProbe, {
        sessions,
        delaySec: getRegProbeDelaySec(),
      }).catch(() => {});
    } else if (backendProbed) {
      for (const id of importedIds) regProbedIds.add(id);
    }

    if (!finished) return;
    if (regFinishedNotified) {
      try { clearInterval(regPollTimer); } catch (_) {}
      regPollTimer = null;
      return;
    }

    regFinishedNotified = true;
    regStopping = false;
    // Keep track after finish so refresh can still reopen the final card.
    saveRegTrack();
    const success = Math.max(
      stats.success,
      Number((batch && batch.imported) || 0) || 0
    );
    const fail = Math.max(
      stats.fail,
      Number((batch && batch.error) || 0) || 0
    );
    const cancelled = Math.max(
      sessions.filter((s) => {
        const st = regStatusOf(s);
        return st === "cancelled" || st === "stopped";
      }).length,
      Number((batch && batch.cancelled) || 0) || 0
    );
    const summary =
      (cancelled > 0 && success === 0 && fail === 0
        ? `注册已停止`
        : `注册完成：成功 ${success} · 失败 ${fail}`) +
      (cancelled ? ` · 已停止 ${cancelled}` : "") +
      (targetTotal ? ` / 共 ${Math.max(targetTotal, success + fail + cancelled)}` : "");

    // Ensure final log includes summary line.
    setLogPanel(
      "reg-log",
      buildRegLogText(sessions, {
        batch,
        extraLines: [
          `[结束] ${summary}`,
          backendProbed
            ? "[测活] 后端已在入池后自动测活（见上方结果）"
            : (importedIds.length ? "[测活] 已触发/完成新号入池测活" : "[测活] 无成功导入账号"),
          "[提示] 可点「关闭」收起进度卡片",
        ],
      }),
      { forceShow: true }
    );
    setRegStatusText(
      cancelled > 0 && success === 0 && fail === 0
        ? "stopped"
        : `成功 ${success} · 失败 ${fail}` +
            (cancelled ? ` · 停止 ${cancelled}` : "") +
            (targetTotal ? ` / ${Math.max(targetTotal, success + fail + cancelled)}` : "")
    );

    if (success > 0 && fail === 0 && cancelled === 0) toast(summary);
    else if (success > 0 || cancelled > 0) toast(summary, true);
    else toast(summary + "，请查看下方日志", false);

    try { clearInterval(regPollTimer); } catch (_) {}
    regPollTimer = null;
    try {
      // Force status/pool totals refresh after registration imports land in DB.
      _statusFetchedAt = 0;
      statusCache = await api("/status");
      _statusFetchedAt = Date.now();
      if (statusCache && statusCache.pool) {
        dashCache = dashCache || {};
        dashCache.pool = Object.assign({}, dashCache.pool || {}, statusCache.pool);
        if (statusCache.accounts) {
          dashCache.accounts = Object.assign({}, dashCache.accounts || {}, statusCache.accounts);
        }
      }
      await loadDashboard();
      // Accounts page total comes from /accounts — refresh like manual import does.
      try { await loadAccountsPage({ reset: true }); } catch (_) {}
    } catch (_) {}
  } catch (_) {}
  } finally {
    regPollInFlight = false;
    // Drain trailing-edge request so a tick that arrived mid-flight still runs.
    if (regPollPending && (regBatchId || regSessionId || (regSessionIds && regSessionIds.length))) {
      regPollPending = false;
      setTimeout(() => { pollRegSession().catch(() => {}); }, 50);
    }
  }
}


function bindSoftNav() {
  document.addEventListener("click", (e) => {
    const a = e.target && e.target.closest ? e.target.closest("a[href]") : null;
    if (!a) return;
    if (e.defaultPrevented) return;
    if (e.button !== 0) return;
    if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    if (a.target && a.target !== "" && a.target !== "_self") return;
    let href = a.getAttribute("href") || "";
    if (!href) return;
    try {
      const u = new URL(href, location.origin);
      if (u.origin !== location.origin) return;
      href = u.pathname + u.search + u.hash;
    } catch (_) {
      return;
    }
    if (!href.startsWith("/admin")) return;
    if (href.startsWith("/admin/login") || href.startsWith("/admin/api")) return;
    const page = pageFromPath((href.split("?")[0] || "").replace(/\/$/, ""));
    if (!(page in PAGE_HREF) && page !== "overview") return;
    e.preventDefault();
    softNavigate(page);
  }, true);

  window.addEventListener("popstate", () => {
    const page = pageFromPath(location.pathname);
    if (page === "login") return;
    softNavigate(page, { replace: true, force: true });
  });
}
bindSoftNav();

// Bind once DOM is ready (top-level on() may run before elements exist).
document.addEventListener("DOMContentLoaded", () => {
  try { rebindPageControls(); } catch (e) { console.warn(e); }
});
if (document.readyState !== "loading") {
  try { rebindPageControls(); } catch (e) { console.warn(e); }
}


/* ── Events ─────────────────────────────────────────── */
loadRegConfig().then(() => {
  try { syncRegCaptchaProviderUI(); } catch (_) {}
  try { syncRegMailProviderUI(); } catch (_) {}
}).catch(() => {
  try { syncRegCaptchaProviderUI(); } catch (_) {}
  try { syncRegMailProviderUI(); } catch (_) {}
});

document.querySelectorAll(".sidebar .nav-btn").forEach(btn => {
  btn.onclick = () => switchTab(btn.dataset.tab);
});
document.querySelectorAll("[data-jump]").forEach(btn => {
  btn.onclick = () => switchTab(btn.dataset.jump);
});
buildMobileNav();

on("auth-submit", "onclick", async () => {  const password = $("password").value;
  if (!password) return toast("请输入密码", false);
  try {
    const setup = statusCache && statusCache.setup_needed;
    const data = setup
      ? await api("/setup", { method: "POST", body: JSON.stringify({ password }) })
      : await api("/login", { method: "POST", body: JSON.stringify({ password }) });
    token = data.token;
    localStorage.setItem(TOKEN_KEY, token);
    if ($("password")) $("password").value = "";
    statusCache = await api("/status");
    await loadDashboard();
    showMain();
    toast(setup ? "初始化成功" : "登录成功");
  } catch (e) {
    toast(e.message, false);
  }
});
if ($("password")) $("password").addEventListener("keydown", e => { if (e.key === "Enter") $("auth-submit")?.click(); });
on("auth-refresh", "onclick", () => bootstrap());
on("btn-logout", "onclick", async () => {  try { await api("/logout", { method: "POST" }); } catch {}
  token = "";
  localStorage.removeItem(TOKEN_KEY);
  showAuth(false);
});
on("btn-refresh-all", "onclick", async () => {  try {
    statusCache = await api("/status");
    await loadDashboard();
    toast("已刷新");
  } catch (e) { toast(e.message, false); }
});

on("btn-create-key", "onclick", async () => {
  try {
    const name = ($("key-name") && $("key-name").value) || "default";
    const note = ($("key-note") && $("key-note").value) || "";
    const data = await api("/keys", { method: "POST", body: JSON.stringify({ name, note }) });
    const rec = data.key || data;
    const full = (rec && (rec.key || rec.secret)) || data.secret || "";
    const box = $("new-key-box");
    if (box) {
      box.classList.remove("hidden");
      box.innerHTML = `<div style="font-weight:600;margin-bottom:6px;color:var(--ok)">✓ Key 已创建 — 列表中可随时再复制</div>
      <div class="mono" id="new-key-value" style="user-select:all;word-break:break-all;cursor:pointer" title="点击复制">${esc(full)}</div>
      <div style="margin-top:8px;display:flex;gap:8px;flex-wrap:wrap">
        <button class="g2a-btn g2a-btn-primary g2a-btn-sm" id="copy-key">复制 Key</button>
        <button class="g2a-btn g2a-btn-default g2a-btn-sm" id="dismiss-key">收起</button>
      </div>`;
      const doCopy = async () => {
        if (!full) { toast("Key 为空", false); return; }
        const ok = await copyText(full);
        toast(ok ? "已复制 API Key" : "复制失败，请手动选中复制", ok);
      };
      on("copy-key", "onclick", doCopy);
      on("new-key-value", "onclick", doCopy);
      on("dismiss-key", "onclick", () => box.classList.add("hidden"));
    }
    if (full) {
      const ok = await copyText(full);
      if (ok) toast("已创建并自动复制到剪贴板");
    }
    if ($("key-name")) $("key-name").value = "";
    if ($("key-note")) $("key-note").value = "";
    statusCache = await api("/status");
    await loadDashboard();
  } catch (e) { toast(e.message, false); }
});

on("keys-tbody", "onclick", async (e) => {  const btn = e.target.closest("button");
  if (!btn) return;
  const id = btn.dataset.id;
  try {
    if (btn.dataset.act === "copy") {
      const k = keysCache[id] || {};
      let full = k.secret || k.key || "";
      let regenerated = false;
      if (!full) {
        if (!confirm("该 Key 未保存完整值，无法直接复制。是否重新生成一个新 Key？旧 Key 会立即失效。")) return;
        const data = await api("/keys/" + id + "/regenerate", { method: "POST" });
        const rec = data.key || data;
        full = (rec && (rec.key || rec.secret)) || data.secret || "";
        if (!full) {
          toast("Key 已重建，但接口未返回完整值，请刷新后再试", false);
          await loadDashboard();
          return;
        }
        keysCache[id] = rec;
        regenerated = true;
      }
      const ok = await copyText(full);
      toast(ok ? (regenerated ? "已重建并复制 API Key" : "已复制 API Key") : "复制失败，请手动选中复制", ok);
      if (regenerated) await loadDashboard();
      return;
    }
    if (btn.dataset.act === "del") {
      if (!confirm("确定删除此 Key？")) return;
      await api("/keys/" + id, { method: "DELETE" });
      toast("已删除");
    } else if (btn.dataset.act === "toggle") {
      await api("/keys/" + id, {
        method: "PATCH",
        body: JSON.stringify({ enabled: btn.dataset.on === "1" }),
      });
      toast("已更新");
    }
    statusCache = await api("/status");
    await loadDashboard();
  } catch (err) { toast(err.message, false); }
});

on("accounts-tbody", "onclick", async (e) => {  // checkbox selection
  const chk = e.target.closest(".acc-check-one");
  if (chk) {
    const id = chk.dataset.id;
    if (!id) return;
    if (chk.checked) selectedAccountIds.add(id);
    else selectedAccountIds.delete(id);
    updateAccountSelectionInfo(getFilteredAccounts().length, document.querySelectorAll(".acc-check-one").length);
    return;
  }

  const btn = e.target.closest("button");
  if (!btn) return;
  const id = btn.dataset.id;
  try {
    if (btn.dataset.act === "renew-one") {
      await renewAccounts([id], { confirmMany: false });
      return;
    }
    if (btn.dataset.act === "probe-one") {
      await runAccountProbe(id);
      return;
    }
    if (btn.dataset.act === "quota-one") {
      setRowBusy(id, true, "查询中");
      try {
        const q = await api("/accounts/" + encodeURIComponent(id) + "/quota");
        quotaCache[id] = q;
        if (q.auto_disabled) toast("该账号额度已耗尽，已移出轮询", false);
        else if (q.ok) toast((q.display && q.display.summary) || "额度已更新");
        else toast(q.error || "额度查询失败", false);
        upsertAccountInList({
          id,
          _pool: {
            last_quota: q,
            disabled_for_quota: !!q.auto_disabled || !!q.exhausted,
          },
        });
        refreshOneAccountLocal(id);
      } finally {
        setRowBusy(id, false);
      }
      return;
    }
    if (btn.dataset.act === "toggle-acc") {
      setRowBusy(id, true, "处理中");
      try {
        const en = btn.dataset.on === "1";
        await api("/accounts/" + encodeURIComponent(id) + "/enabled", {
          method: "PATCH",
          body: JSON.stringify({ enabled: en }),
        });
        toast(en ? "已启用（重新加入轮询）" : "已禁用");
        upsertAccountInList({ id, _pool: { enabled: en } });
        refreshOneAccountLocal(id);
      } finally {
        setRowBusy(id, false);
      }
      return;
    }
    if (btn.dataset.act === "rm-acc") {
      if (!confirm("确定移除此账号？将从数据库与本地镜像同步删除。")) return;
      setRowBusy(id, true, "移除中");
      try {
        await api("/accounts/" + encodeURIComponent(id), { method: "DELETE" });
        selectedAccountIds.delete(id);
        accountsList = (accountsList || []).filter((a) => a.id !== id);
        accountsTotal = Math.max(0, (accountsTotal || 1) - 1);
        const row = document.querySelector(`tr[data-acc-id="${CSS.escape(String(id))}"]`);
        if (row) row.remove();
        if ($("acc-page-info")) {
          $("acc-page-info").textContent = `${accountsPage} / ${Math.max(1, accountsTotalPages || 1)} (本页 ${document.querySelectorAll("#accounts-tbody tr[data-acc-id]").length} / 共 ${accountsTotal || 0} 个)`;
        }
        toast("已移除");
      } finally {
        setRowBusy(id, false);
      }
      return;
    }
  } catch (err) { toast(err.message, false); }
});

const bindQuota = (id) => { const el = $(id); if (el) el.onclick = () => refreshAllQuota(true); };
bindQuota("btn-refresh-quota");
bindQuota("btn-refresh-quota-2");
const bindProbe = (id) => { const el = $(id); if (el) el.onclick = () => runProbeAll(); };
bindProbe("btn-probe-all");
bindProbe("btn-probe-all-2");

on("btn-save-mode", "onclick", async () => {  try {
    const mode = $("account-mode").value;
    await api("/settings/account-mode", {
      method: "PUT",
      body: JSON.stringify({ mode }),
    });
    toast("轮询策略已保存: " + mode);
    statusCache = await api("/status");
    await loadDashboard();
  } catch (e) { toast(e.message, false); }
});



function showJsonIoProgress(show) {
  const wrap = $("json-io-progress-wrap");
  if (!wrap) return;
  if (show) {
    wrap.classList.remove("hidden", "is-done", "is-error");
    wrap.hidden = false;
  } else {
    wrap.classList.add("hidden");
    wrap.hidden = true;
  }
}

function setJsonIoProgress({
  percent = 0,
  label = "",
  detail = "",
  done = null,
  total = null,
  success = null,
  fail = null,
  status = "",
} = {}) {
  const pct = Math.max(0, Math.min(100, Math.round(Number(percent) || 0)));
  const fill = $("json-io-progress-fill");
  const bar = $("json-io-progress-bar");
  const pctEl = $("json-io-progress-pct");
  const labelEl = $("json-io-progress-label");
  const detailEl = $("json-io-progress-detail");
  const wrap = $("json-io-progress-wrap");
  if (fill) fill.style.width = pct + "%";
  if (bar) bar.setAttribute("aria-valuenow", String(pct));
  if (pctEl) pctEl.textContent = pct + "%";
  if (labelEl) labelEl.textContent = label || "处理中…";
  if (detailEl) {
    const parts = [];
    if (detail) parts.push(String(detail));
    if (total != null) {
      parts.push(
        `进度 ${done != null ? done : 0}/${total}` +
          (success != null || fail != null
            ? ` · 成功 ${success || 0} · 失败 ${fail || 0}`
            : "")
      );
    }
    detailEl.textContent = parts.filter(Boolean).join(" · ") || "—";
  }
  if (wrap) {
    wrap.classList.toggle("is-done", status === "done" || status === "partial");
    wrap.classList.toggle("is-error", status === "error");
  }
}

async function pollJsonIoJob(jobId, { kind = "import", totalHint = 0 } = {}) {
  const path =
    kind === "export"
      ? "/accounts/export/jobs/" + encodeURIComponent(jobId)
      : "/accounts/import-files/jobs/" + encodeURIComponent(jobId);
  const job = await api(path);
  const status = String(job.status || "");
  const total = Number(job.total || totalHint || 0) || 0;
  const done = Number(job.done || 0) || 0;
  const success = Number(job.success != null ? job.success : job.count || 0) || 0;
  const fail = Number(job.fail || job.parse_errors || 0) || 0;
  const percent = Number(
    job.percent != null ? job.percent : total ? (100 * done) / total : 0
  );
  setJsonIoProgress({
    percent,
    label: job.message || (status === "done" ? "完成" : "处理中…"),
    detail: job.phase ? `阶段: ${job.phase}` : "",
    done,
    total,
    success,
    fail,
    status,
  });
  const meta = (job.file_meta || [])
    .map((x) => {
      if (!x) return "";
      return `${x.ok === false ? "❌" : "✅"} ${x.filename || "file"}${x.error ? " · " + x.error : ""}`;
    })
    .filter(Boolean);
  setLogPanel(
    "json-io-result",
    [job.message || "", meta.length ? meta.join("\n") : ""].filter(Boolean).join("\n") || "—",
    { forceShow: true }
  );
  return job;
}

async function waitJsonIoJob(jobId, { kind = "import", totalHint = 0, maxWaitMs = 300000 } = {}) {
  const startedAt = Date.now();
  let finalJob = null;
  while (Date.now() - startedAt < maxWaitMs) {
    try {
      finalJob = await pollJsonIoJob(jobId, { kind, totalHint });
    } catch (e) {
      setLogPanel(
        "json-io-result",
        `进度查询暂时失败: ${(e && e.message) || e}\n将继续重试…`,
        { forceShow: true }
      );
    }
    const st = String((finalJob && finalJob.status) || "");
    if (st === "done" || st === "partial" || st === "error") break;
    await new Promise((resolve) => setTimeout(resolve, 700));
  }
  return finalJob;
}

async function downloadExportJob(jobId, fallbackName) {
  const res = await fetch(
    "/admin/api/accounts/export/jobs/" + encodeURIComponent(jobId) + "/download",
    { credentials: "same-origin", headers: headers(false) }
  );
  if (!res.ok) {
    let msg = res.statusText;
    try {
      const d = await res.json();
      msg = d.detail || d.error || msg;
    } catch (_) {}
    throw new Error(typeof msg === "string" ? msg : JSON.stringify(msg));
  }
  const blob = await res.blob();
  const cd = res.headers.get("Content-Disposition") || "";
  let filename = fallbackName || "grok2api-auth-export.json";
  const m = /filename=\"?([^\";]+)\"?/.exec(cd);
  if (m) filename = m[1];
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(url);
  return filename;
}

async function runJsonExportJob({ mode = "all", ids = [], buttonId = "btn-export" } = {}) {
  const btn = $(buttonId);
  const selectedN = Array.isArray(ids) ? ids.length : 0;
  if (btn) {
    btn.disabled = true;
    if (!btn.dataset.label) btn.dataset.label = btn.textContent;
    btn.textContent = mode === "selected" ? `导出中 0/${selectedN}` : "导出中…";
  }
  showJsonIoProgress(true);
  setJsonIoProgress({
    percent: 0,
    label: mode === "selected" ? `开始导出选中 ${selectedN} 个账号…` : "开始导出全部账号…",
    detail: "提交任务中",
    done: 0,
    total: mode === "selected" ? selectedN : 1,
    success: 0,
    fail: 0,
    status: "queued",
  });
  setLogPanel("json-io-result", "提交导出任务…", { forceShow: true });
  try {
    let started;
    if (mode === "selected") {
      started = await api("/accounts/export-batch?async_job=1", {
        method: "POST",
        body: JSON.stringify({ ids, include_secrets: true }),
      });
    } else {
      started = await api("/accounts/export?async_job=1");
    }
    // Sync fallback for older servers: if payload returned directly, download it.
    if (started && started.auth && !started.job_id) {
      const blob = new Blob([JSON.stringify(started, null, 2)], { type: "application/json" });
      const a = document.createElement("a");
      a.href = URL.createObjectURL(blob);
      a.download = mode === "selected"
        ? `grok2api-auth-export-selected-${selectedN}.json`
        : "grok2api-auth-export.json";
      document.body.appendChild(a); a.click(); a.remove();
      setTimeout(() => URL.revokeObjectURL(a.href), 1000);
      setJsonIoProgress({ percent: 100, label: "导出完成", detail: a.download, done: started.count || selectedN || 0, total: started.count || selectedN || 0, success: started.count || 0, fail: 0, status: "done" });
      toast(`已导出 ${started.count || selectedN || ""} 个账号`);
      return;
    }
    const jobId = started && started.job_id;
    if (!jobId) throw new Error("未返回 job_id，无法跟踪导出进度");
    setJsonIoProgress({
      percent: 5,
      label: started.message || "任务已启动",
      detail: `job_id: ${jobId}`,
      done: 0,
      total: started.total || (mode === "selected" ? selectedN : 1),
      status: "queued",
    });
    const finalJob = await waitJsonIoJob(jobId, {
      kind: "export",
      totalHint: started.total || selectedN || 1,
      maxWaitMs: Math.max(120000, (selectedN || 50) * 2000),
    });
    if (!finalJob || (finalJob.status !== "done" && finalJob.status !== "partial")) {
      throw new Error((finalJob && (finalJob.error || finalJob.message)) || "导出超时或失败");
    }
    if (finalJob.status === "error") {
      throw new Error(finalJob.error || finalJob.message || "导出失败");
    }
    const filename = await downloadExportJob(
      jobId,
      finalJob.filename ||
        (mode === "selected"
          ? `grok2api-auth-export-selected-${selectedN}.json`
          : "grok2api-auth-export.json")
    );
    setJsonIoProgress({
      percent: 100,
      label: finalJob.message || "导出完成",
      detail: filename,
      done: finalJob.count || selectedN || 0,
      total: finalJob.count || selectedN || 0,
      success: finalJob.count || 0,
      fail: 0,
      status: "done",
    });
    toast(finalJob.message || `已导出 ${finalJob.count || selectedN || ""} 个账号`);
  } catch (e) {
    setJsonIoProgress({
      percent: 100,
      label: "导出失败",
      detail: (e && e.message) || String(e),
      status: "error",
    });
    toast((e && e.message) || "导出失败", false);
  } finally {
    if (btn) {
      btn.disabled = false;
      btn.textContent = btn.dataset.label || (mode === "selected" ? "导出选中" : "导出全部");
    }
  }
}

async function exportAllAccounts() {
  return runJsonExportJob({ mode: "all", buttonId: "btn-export" });
}

async function importJsonFiles() {
  return importAccountJsonFiles({
    inputId: "import-file",
    buttonId: "btn-import",
    nameLabelId: "import-file-name",
    label: "JSON",
    emptyMsg: "请先选择 JSON 文件",
  });
}

/** Import CLIProxyAPI auth files (same backend as JSON import; CPA auto-detected). */
async function importCliproxyapiFiles() {
  return importAccountJsonFiles({
    inputId: "import-cliproxyapi-file",
    buttonId: "btn-acc-import-cliproxyapi",
    nameLabelId: null,
    label: "CLIProxyAPI",
    emptyMsg: "请选择 CLIProxyAPI 的 auth JSON（xai-*.json / type=xai|codex / bundle）",
    forceMerge: true,
  });
}

/**
 * Shared multi-file import against /accounts/import-files.
 * Used by generic JSON import and the dedicated CLIProxyAPI button.
 */
async function importAccountJsonFiles({
  inputId = "import-file",
  buttonId = "btn-import",
  nameLabelId = "import-file-name",
  label = "JSON",
  emptyMsg = "请先选择文件",
  forceMerge = null,
} = {}) {
  const input = $(inputId);
  const files = input && input.files;
  if (!files || !files.length) return toast(emptyMsg, false);
  let merge;
  if (forceMerge === true) merge = "true";
  else if (forceMerge === false) merge = "false";
  else merge = ($("import-merge") && $("import-merge").checked) ? "true" : "false";
  const btn = $(buttonId);
  if (btn) {
    btn.disabled = true;
    if (!btn.dataset.label) btn.dataset.label = btn.textContent;
    btn.textContent = files.length > 1 ? `导入中 0/${files.length}` : "导入中…";
  }
  showJsonIoProgress(true);
  setJsonIoProgress({
    percent: 0,
    label: `开始导入 ${files.length} 个 ${label} 文件…`,
    detail: "提交任务中",
    done: 0,
    total: files.length,
    success: 0,
    fail: 0,
    status: "queued",
  });
  setLogPanel(
    "json-io-result",
    `开始导入 ${files.length} 个 ${label} 文件…\n提交后台任务…`,
    { forceShow: true }
  );
  try {
    const fd = new FormData();
    for (let i = 0; i < files.length; i++) fd.append("files", files[i]);
    fd.append("merge", merge);
    let started;
    try {
      started = await api("/accounts/import-files", { method: "POST", body: fd });
    } catch (e) {
      // Fallback: sequential single-file jobs (older backend without bulk async).
      let totalImported = 0, totalFailed = 0, lastMessage = "";
      for (let i = 0; i < files.length; i++) {
        if (btn) btn.textContent = `导入中 ${i + 1}/${files.length}`;
        setJsonIoProgress({
          percent: Math.round((100 * i) / files.length),
          label: `导入中 ${i + 1}/${files.length}`,
          detail: files[i].name,
          done: i,
          total: files.length,
          success: totalImported,
          fail: totalFailed,
          status: "running",
        });
        const f = files[i];
        try {
          const one = new FormData();
          one.append("file", f);
          one.append("merge", merge);
          const rr = await api("/accounts/import-file", { method: "POST", body: one });
          if (rr && rr.job_id) {
            const job = await waitJsonIoJob(rr.job_id, {
              kind: "import",
              totalHint: 1,
              maxWaitMs: 180000,
            });
            totalImported += Number((job && job.count) || 0);
            if (job && (job.status === "error" || job.ok === false)) totalFailed++;
            lastMessage = (job && job.message) || lastMessage;
          } else {
            totalImported += rr.imported?.length || rr.count || 0;
            lastMessage = rr.message || `已导入 ${rr.imported?.length || 0} 个账号`;
          }
        } catch (err) {
          totalFailed++;
          toast(`${f.name}: ${err.message}`, false);
        }
      }
      setJsonIoProgress({
        percent: 100,
        label: totalFailed ? "导入完成（有失败）" : "导入完成",
        done: files.length,
        total: files.length,
        success: totalImported,
        fail: totalFailed,
        status: totalFailed ? "partial" : "done",
      });
      toast(
        files.length > 1
          ? `${label} 导入完成：${totalImported} 账号，${totalFailed} 文件失败`
          : (lastMessage || `已导入 ${totalImported} 个账号`),
        totalFailed === 0
      );
      if (input) input.value = "";
      if (nameLabelId && $(nameLabelId)) $(nameLabelId).textContent = "未选择文件";
      try { await loadAccountsPage({ reset: true }); } catch (_) { await loadDashboard(); }
      return;
    }

    // Sync response (old backend): no job_id.
    if (!started.job_id) {
      const count = started.count || started.imported?.length || 0;
      const parseErrors = started.parse_errors || 0;
      setJsonIoProgress({
        percent: 100,
        label: started.message || "导入完成",
        done: files.length,
        total: files.length,
        success: count,
        fail: parseErrors,
        status: parseErrors ? "partial" : "done",
      });
      toast(
        started.message || `导入完成：${count} 个账号` + (parseErrors ? `，${parseErrors} 个文件失败` : ""),
        parseErrors === 0
      );
      if (input) input.value = "";
      if (nameLabelId && $(nameLabelId)) $(nameLabelId).textContent = "未选择文件";
      try { await loadAccountsPage({ reset: true }); } catch (_) { await loadDashboard(); }
      return;
    }

    const jobId = started.job_id;
    if (btn) btn.textContent = `导入中 0/${started.total || files.length}`;
    setJsonIoProgress({
      percent: 0,
      label: started.message || "任务已启动",
      detail: `job_id: ${jobId}`,
      done: 0,
      total: started.total || files.length,
      success: 0,
      fail: 0,
      status: "queued",
    });
    const finalJob = await waitJsonIoJob(jobId, {
      kind: "import",
      totalHint: started.total || files.length,
      maxWaitMs: Math.max(120000, files.length * 30000),
    });
    if (!finalJob) throw new Error("导入超时，未拿到任务结果");
    const st = String(finalJob.status || "");
    if (st === "error") {
      throw new Error(finalJob.error || finalJob.message || "导入失败");
    }
    if (btn) btn.textContent = `导入中 ${finalJob.done || files.length}/${finalJob.total || files.length}`;
    toast(
      finalJob.message || `${label} 导入完成：${finalJob.count || 0} 个账号`,
      st !== "error" && !(finalJob.fail > 0 && !(finalJob.count > 0))
    );
    if (input) input.value = "";
    if (nameLabelId && $(nameLabelId)) $(nameLabelId).textContent = "未选择文件";
    try { await loadAccountsPage({ reset: true }); } catch (_) { await loadDashboard(); }
  } catch (e) {
    setJsonIoProgress({
      percent: 100,
      label: "导入失败",
      detail: (e && e.message) || String(e),
      status: "error",
    });
    toast(e.message || "导入失败", false);
  } finally {
    if (btn) {
      btn.disabled = false;
      btn.textContent = btn.dataset.label || (buttonId === "btn-acc-import-cliproxyapi" ? "导入 CLIProxyAPI" : "导入文件");
    }
  }
}

let ssoImportPollTimer = null;
let ssoImportJobId = null;

function showSsoProgress(show) {
  const wrap = $("sso-progress-wrap");
  if (!wrap) return;
  if (show) {
    wrap.classList.remove("hidden", "is-done", "is-error");
    wrap.hidden = false;
  } else {
    wrap.classList.add("hidden");
    wrap.hidden = true;
  }
}

function setSsoProgress({
  percent = 0,
  label = "",
  detail = "",
  done = null,
  total = null,
  success = null,
  fail = null,
  status = "",
} = {}) {
  const pct = Math.max(0, Math.min(100, Math.round(Number(percent) || 0)));
  const fill = $("sso-progress-fill");
  const bar = $("sso-progress-bar");
  const pctEl = $("sso-progress-pct");
  const labelEl = $("sso-progress-label");
  const detailEl = $("sso-progress-detail");
  const wrap = $("sso-progress-wrap");
  if (fill) fill.style.width = pct + "%";
  if (bar) bar.setAttribute("aria-valuenow", String(pct));
  if (pctEl) pctEl.textContent = pct + "%";
  if (labelEl) labelEl.textContent = label || "SSO 导入中…";
  if (detailEl) {
    const parts = [];
    if (detail) parts.push(String(detail));
    if (total != null) {
      parts.push(
        `进度 ${done != null ? done : 0}/${total}` +
          (success != null || fail != null
            ? ` · 成功 ${success || 0} · 失败 ${fail || 0}`
            : "")
      );
    }
    detailEl.textContent = parts.filter(Boolean).join(" · ") || "—";
  }
  if (wrap) {
    wrap.classList.toggle("is-done", status === "done");
    wrap.classList.toggle("is-error", status === "error");
  }
}

function formatSsoResultRows(results) {
  return (results || []).map((x) => {
    const st = String(x.status || "");
    const ok = st === "ok";
    const converted = st === "converted";
    const icon = ok ? "✅" : converted ? "🔄" : "❌";
    const meta = ok
      ? `${x.email || x.user_id || ""} ${x.has_refresh_token ? "+refresh" : ""}`.trim()
      : converted
        ? `${x.email || ""} 已转换，等待入库`.trim()
        : (x.error || st || "");
    return `[${x.index ?? "?"}] ${icon} ${x.sso_hint || ""} ${meta}`.trim();
  });
}

function stopSsoImportPolling() {
  try { clearInterval(ssoImportPollTimer); } catch (_) {}
  ssoImportPollTimer = null;
}

async function pollSsoImportJob(jobId, { totalHint = 0 } = {}) {
  if (!jobId) return null;
  const job = await api("/accounts/import-sso/jobs/" + encodeURIComponent(jobId));
  const status = String(job.status || "");
  const total = Number(job.total || totalHint || 0) || 0;
  const done = Number(job.done || 0) || 0;
  const success = Number(job.success || 0) || 0;
  const fail = Number(job.fail || 0) || 0;
  const percent = Number(job.percent != null ? job.percent : (total ? (100 * done) / total : 0));
  setSsoProgress({
    percent,
    label: job.message || (status === "done" ? "SSO 导入完成" : "SSO 导入中…"),
    detail: job.phase ? `阶段: ${job.phase}` : "",
    done,
    total,
    success,
    fail,
    status,
  });
  const btn = $("btn-import-sso");
  if (btn && status !== "done" && status !== "error") {
    btn.textContent = total ? `导入中 ${done}/${total}` : "导入中…";
  }
  const rows = formatSsoResultRows(job.results || []);
  const head = job.message || `SSO 导入 ${done}/${total || "?"}`;
  setLogPanel(
    "sso-result",
    [head, rows.length ? rows.join("\n") : "（等待转换结果…）"].join("\n"),
    { forceShow: true }
  );
  return job;
}


async function exportRegistrationSso() {
  const fmt = (($("sso-export-format") && $("sso-export-format").value) || "sso").trim();
  const batch = (($("sso-export-batch") && $("sso-export-batch").value) || "").trim();
  const importedOnly = !($("sso-export-imported-only") && !$("sso-export-imported-only").checked);
  const includePassword = !!( $("sso-export-password") && $("sso-export-password").checked );
  const btn = $("btn-export-sso");
  if (btn) {
    btn.disabled = true;
    if (!btn.dataset.label) btn.dataset.label = btn.textContent;
    btn.textContent = "导出中…";
  }
  try {
    const body = {
      batch_id: batch || null,
      status: importedOnly ? ["imported"] : [],
      include_password: includePassword || fmt === "email_password_sso",
      format: fmt,
      download: false,
    };
    const res = await api("/accounts/register-email/export-sso", {
      method: "POST",
      body: JSON.stringify(body),
    });
    if (!res || res.ok === false) {
      throw new Error((res && (res.detail || res.error || res.message)) || "导出失败");
    }
    let content = "";
    let filename = `grok2api-sso-export-${Date.now()}`;
    let mime = "text/plain;charset=utf-8";
    if (fmt === "json") {
      content = JSON.stringify(res, null, 2);
      filename += ".json";
      mime = "application/json;charset=utf-8";
    } else {
      content = res.text || "";
      if (!content && Array.isArray(res.items)) {
        content = res.items.map((it) => it.sso || "").filter(Boolean).join("\n") + "\n";
      }
      filename += ".txt";
    }
    if (!content || !String(content).trim()) {
      throw new Error("没有可导出的 SSO（注册会话可能已清空，需重新注册）");
    }
    // Prefer browser download
    try {
      const blob = new Blob([content], { type: mime });
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = filename;
      document.body.appendChild(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(url), 2000);
    } catch (_) {
      // fallback: fill textarea for copy
      if ($("sso-cookies")) $("sso-cookies").value = content;
    }
    setLogPanel(
      "sso-result",
      `导出 SSO 完成：${res.count || 0} 条` + (batch ? `（batch=${batch}）` : "") + `\n格式=${fmt}`,
      { forceShow: true }
    );
    toast(`已导出 ${res.count || 0} 条 SSO`, true);
  } catch (e) {
    const msg = (e && e.message) || String(e);
    setLogPanel("sso-result", "导出 SSO 失败：\n" + msg, { forceShow: true });
    toast("导出 SSO 失败: " + msg, false);
  } finally {
    if (btn) {
      btn.disabled = false;
      btn.textContent = btn.dataset.label || "导出 SSO";
    }
  }
}

async function importSsoCookies() {
  const ta = $("sso-cookies");
  const fileInput = $("sso-file");
  let raw = ta && ta.value.trim();
  if (!raw && fileInput && fileInput.files && fileInput.files[0]) {
    try { raw = await fileInput.files[0].text(); }
    catch (e) { return toast("读取文件失败: " + e.message, false); }
  }
  if (!raw) return toast("请粘贴 SSO cookie 或选择文件", false);
  const lines = raw.split("\n").map(s => s.trim()).filter(Boolean);
  if (!lines.length) return toast("请粘贴 SSO cookie 或选择文件", false);
  const delay = parseInt(($("sso-delay") && $("sso-delay").value) || "0", 10) || 0;
  const merge = !!($("sso-merge") && $("sso-merge").checked);
  const btn = $("btn-import-sso");
  stopSsoImportPolling();
  ssoImportJobId = null;
  if (btn) {
    btn.disabled = true;
    if (!btn.dataset.label) btn.dataset.label = btn.textContent;
    btn.textContent = `导入中 0/${lines.length}`;
  }
  showSsoProgress(true);
  setSsoProgress({
    percent: 0,
    label: `开始导入 ${lines.length} 条 SSO…`,
    detail: "提交任务中",
    done: 0,
    total: lines.length,
    success: 0,
    fail: 0,
    status: "queued",
  });
  setLogPanel("sso-result", `开始导入 ${lines.length} 条 SSO…\n提交后台任务…`, { forceShow: true });
  try {
    // Async job + progress polling (backend caps workers).
    const started = await api("/accounts/import-sso", {
      method: "POST",
      body: JSON.stringify({
        sso_cookies: lines,
        merge,
        delay,
        // Higher concurrency; backend still caps via GROK2API_SSO_IMPORT_WORKERS.
        max_workers: delay >= 5 ? 4 : 12,
      }),
    });

    // Backward-compat: old sync response already has results.
    if (!started.job_id && Array.isArray(started.results)) {
      const rows = formatSsoResultRows(started.results || []);
      setSsoProgress({
        percent: 100,
        label: started.message || "SSO 导入完成",
        done: started.total || lines.length,
        total: started.total || lines.length,
        success: started.success || 0,
        fail: started.fail || 0,
        status: started.ok === false ? "error" : "done",
      });
      setLogPanel("sso-result", `${started.message || ""}\n${rows.join("\n")}`, { forceShow: true });
      toast(started.message || `SSO 导入完成：${started.success || 0}/${started.total || lines.length}`, !!started.ok);
      if (ta) ta.value = "";
      if (fileInput) fileInput.value = "";
      if ($("sso-file-name")) $("sso-file-name").textContent = "未选择文件";
      try { await loadAccountsPage({ reset: true }); } catch (_) { await loadDashboard(); }
      return;
    }

    const jobId = started.job_id;
    if (!jobId) throw new Error("未返回 job_id，无法跟踪进度");
    ssoImportJobId = jobId;
    setSsoProgress({
      percent: 0,
      label: started.message || "任务已启动",
      detail: `job_id: ${jobId}`,
      done: 0,
      total: started.total || lines.length,
      success: 0,
      fail: 0,
      status: "queued",
    });

    // Poll until terminal. Use timeout so a hung job doesn't lock the button forever.
    const startedAt = Date.now();
    const maxWaitMs = Math.max(120000, lines.length * 45000);
    let finalJob = null;
    while (Date.now() - startedAt < maxWaitMs) {
      try {
        finalJob = await pollSsoImportJob(jobId, { totalHint: lines.length });
      } catch (e) {
        // Transient poll errors: keep trying briefly.
        setLogPanel(
          "sso-result",
          `进度查询暂时失败: ${(e && e.message) || e}\n将继续重试…`,
          { forceShow: true }
        );
      }
      const st = String((finalJob && finalJob.status) || "");
      if (st === "done" || st === "error") break;
      await new Promise((resolve) => setTimeout(resolve, 900));
    }

    if (!finalJob || (finalJob.status !== "done" && finalJob.status !== "error")) {
      // One last fetch
      try { finalJob = await pollSsoImportJob(jobId, { totalHint: lines.length }); } catch (_) {}
    }

    const st = String((finalJob && finalJob.status) || "");
    if (st !== "done" && st !== "error") {
      throw new Error("SSO 导入超时，请稍后刷新账号列表确认是否已部分入库");
    }

    const rows = formatSsoResultRows((finalJob && finalJob.results) || []);
    const msg =
      (finalJob && finalJob.message) ||
      `SSO 导入完成：${finalJob.success || 0} 成功, ${finalJob.fail || 0} 失败`;
    setSsoProgress({
      percent: 100,
      label: msg,
      detail: finalJob.job_id ? `job_id: ${finalJob.job_id}` : "",
      done: finalJob.total || lines.length,
      total: finalJob.total || lines.length,
      success: finalJob.success || 0,
      fail: finalJob.fail || 0,
      status: st === "error" ? "error" : "done",
    });
    setLogPanel("sso-result", `${msg}\n${rows.join("\n")}`, { forceShow: true });
    toast(msg, st === "done" && !(finalJob.fail > 0 && finalJob.success === 0));
    if (st === "done") {
      if (ta) ta.value = "";
      if (fileInput) fileInput.value = "";
      if ($("sso-file-name")) $("sso-file-name").textContent = "未选择文件";
      try { await loadAccountsPage({ reset: true }); } catch (_) { await loadDashboard(); }
    }
  } catch (e) {
    showSsoProgress(true);
    setSsoProgress({
      percent: 0,
      label: "SSO 导入失败",
      detail: (e && e.message) || String(e),
      status: "error",
    });
    setLogPanel("sso-result", "导入失败: " + (e.message || e), { forceShow: true });
    toast(e.message || "SSO 导入失败", false);
  } finally {
    stopSsoImportPolling();
    ssoImportJobId = null;
    if (btn) {
      btn.disabled = false;
      btn.textContent = btn.dataset.label || "导入 SSO";
    }
  }
}


async function startDeviceLogin() {
  const btn = $("btn-login-device");
  if (btn) {
    btn.disabled = true;
    if (!btn.dataset.label) btn.dataset.label = btn.textContent;
    btn.textContent = "启动中…";
  }
  try {
    const r = await api("/accounts/login", {
      method: "POST",
      body: JSON.stringify({ mode: "device", capture: true }),
    });
    // some backends return ok=false with error; others just return session fields
    if (r && r.ok === false) {
      toast(r.error || r.message || "启动失败", false);
      setDeviceLoginIdle(true);
      return;
    }
    if (!(r.session_id || r.id || r.user_code || r.device_code)) {
      toast(r.error || r.message || "启动失败：未返回设备码会话", false);
      setDeviceLoginIdle(true);
      return;
    }
    showDeviceSession(r);
    clearInterval(devicePollTimer);
    devicePollTimer = setInterval(pollDeviceSession, 2500);
    setTimeout(pollDeviceSession, 800);
    setTimeout(pollDeviceSession, 2500);
    const code = r.user_code || r.device_code || r.code;
    toast(code ? ("设备码已生成: " + code) : (r.message || "已启动设备码登录"));
  } catch (e) {
    toast(e.message || "启动设备码登录失败", false);
    setDeviceLoginIdle(true);
  } finally {
    if (btn) {
      btn.disabled = false;
      btn.textContent = btn.dataset.label || "开始设备码登录";
    }
  }
}

async function copyDeviceCode() {
  const code = (($("device-code") && $("device-code").textContent) || "").trim();
  if (!code || code === "—" || code === "····") {
    toast("暂无设备码，请先开始设备码登录", false);
    return;
  }
  const ok = await copyText(code);
  toast(ok ? "已复制设备码" : code, ok);
}

function setDeviceLoginIdle(idle) {
  const box = $("device-session");
  if (box) box.dataset.deviceIdle = idle ? "1" : "0";
  const poll = $("btn-poll-device");
  const copy = $("btn-copy-device");
  const hint = $("device-idle-hint");
  const result = $("device-result");
  if (poll) {
    poll.disabled = !!idle;
    poll.title = idle ? "请先开始设备码登录" : "刷新当前设备码会话";
  }
  if (copy) {
    copy.disabled = !!idle;
    copy.title = idle ? "请先开始设备码登录" : "复制设备码";
  }
  if (idle) {
    if (result) { result.classList.add("hidden"); result.hidden = true; }
    if (hint) { hint.classList.remove("hidden"); hint.hidden = false; }
    if ($("device-status")) $("device-status").textContent = "未开始";
    if ($("device-code")) $("device-code").textContent = "—";
    if ($("device-url")) { $("device-url").textContent = "—"; $("device-url").removeAttribute("href"); }
    setLogPanel("device-log", "", { forceShow: false });
  } else {
    if (result) { result.classList.remove("hidden"); result.hidden = false; }
    if (hint) { hint.classList.add("hidden"); hint.hidden = true; }
  }
}

function showDeviceSession(r) {
  setDeviceLoginIdle(false);
  showPanel("device-session");
  loginSessionId = (r && (r.session_id || r.id)) || loginSessionId;
  const code = r && (r.user_code || r.device_code || r.code);
  if (code && $("device-code")) $("device-code").textContent = code;
  const url = r && (r.verification_url || r.verification_uri || r.url);
  if (url && $("device-url")) {
    $("device-url").textContent = url;
    $("device-url").href = url;
  }
  const st = ((r && (r.status || r.state)) || "running");
  const msg = (r && (r.message || r.error)) || "";
  if ($("device-status")) $("device-status").textContent = msg ? (st + " · " + msg) : st;
  if (r && r.output_tail) setLogPanel("device-log", r.output_tail, { forceShow: true });
  else if (msg) setLogPanel("device-log", msg, { forceShow: true });
  // enable copy only when code exists
  const copy = $("btn-copy-device");
  if (copy) copy.disabled = !(code && String(code).trim() && String(code).trim() !== "—");
  const poll = $("btn-poll-device");
  if (poll) poll.disabled = !loginSessionId;
}


async function pollDeviceSession() {
  if (!loginSessionId) {
    toast("请先点击“开始设备码登录”", false);
    setDeviceLoginIdle(true);
    return;
  }
  const pollBtn = $("btn-poll-device");
  if (pollBtn) {
    pollBtn.disabled = true;
    if (!pollBtn.dataset.label) pollBtn.dataset.label = pollBtn.textContent;
    pollBtn.textContent = "刷新中…";
  }
  try {
    const s = await api("/accounts/login/sessions/" + encodeURIComponent(loginSessionId));
    showDeviceSession(s);
    if (s.status === "success" || s.status === "completed" || s.status === "imported") {
      toast("登录成功，账号已入库");
      clearInterval(devicePollTimer);
      devicePollTimer = null;
      // only refresh accounts list page, not whole dashboard if possible
      try { await loadAccountsPage({ reset: false }); } catch (_) { try { await loadDashboard(); } catch(__){} }
    } else if (s.status === "error" || s.status === "failed" || s.status === "expired") {
      toast(s.error || s.message || "登录失败", false);
      clearInterval(devicePollTimer);
      devicePollTimer = null;
    }
  } catch (e) {
    toast((e && e.message) || "刷新设备码状态失败", false);
  } finally {
    if (pollBtn) {
      pollBtn.textContent = pollBtn.dataset.label || "刷新状态";
      pollBtn.disabled = !loginSessionId;
    }
  }
}


on("btn-login-device", "onclick", () => startDeviceLogin());
on("btn-poll-device", "onclick", () => pollDeviceSession());
on("btn-copy-device", "onclick", () => copyDeviceCode());

if ($("import-file")) {
  on("import-file", "onchange", () => {    const files = $("import-file").files;
    const label = $("import-file-name");
    if (label) {
      if (!files || !files.length) {
        label.textContent = "未选择文件";
      } else if (files.length === 1) {
        label.textContent = `已选择：${files[0].name}（${(files[0].size / 1024).toFixed(1)} KB）`;
      } else {
        const totalKb = Array.from(files).reduce((s, f) => s + f.size, 0) / 1024;
        label.textContent = `已选择 ${files.length} 个文件（共 ${totalKb.toFixed(1)} KB）`;
      }
    }
  });
}
on("btn-import", "onclick", () => importJsonFiles());
on("btn-import-sso", "onclick", () => importSsoCookies());
on("btn-export-sso", "onclick", () => exportRegistrationSso());
if ($("sso-file")) {
  on("sso-file", "onchange", () => {    const f = $("sso-file").files && $("sso-file").files[0];
    const label = $("sso-file-name");
    if (label) {
      label.textContent = f
        ? `已选择：${f.name}（${(f.size / 1024).toFixed(1)} KB）`
        : "未选择文件";
    }
  });
}
if ($("btn-export")) {
  on("btn-export", "onclick", () => exportAllAccounts());
}

on("btn-refresh-acc", "onclick", async () => {  try {
    statusCache = await api("/status");
    await loadDashboard();
    if (loginSessionId) await pollDeviceSession();
    toast("已刷新");
  } catch (e) { toast(e.message, false); }
});
on("btn-logout-cli", "onclick", async () => {  if (!confirm("注销全部 Grok 账号？（将清空数据库账号池与本地镜像）")) return;
  try {
    const r = await api("/accounts/logout", { method: "POST" });
    toast(r.message || "完成", !!r.ok);
    statusCache = await api("/status");
    await loadDashboard();
  } catch (e) { toast(e.message, false); }
});




/* ── sub2api push ───────────────────────────────────── */
function fillSub2apiForm(cfg) {
  cfg = cfg || {};
  if ($("set-sub2api-enabled")) $("set-sub2api-enabled").checked = !!cfg.enabled;
  if ($("set-sub2api-url")) $("set-sub2api-url").value = cfg.base_url || "";
  if ($("set-sub2api-email")) $("set-sub2api-email").value = cfg.email || "";
  // never echo password; placeholder indicates saved state
  if ($("set-sub2api-password")) {
    $("set-sub2api-password").value = "";
    $("set-sub2api-password").placeholder = cfg.has_password ? "已保存，留空不改" : "登录密码";
  }
  if ($("set-sub2api-group-id")) {
    $("set-sub2api-group-id").value = cfg.group_id != null && cfg.group_id !== "" ? cfg.group_id : "";
  }
  if ($("set-sub2api-group-name")) $("set-sub2api-group-name").value = cfg.group_name || "";
  if ($("set-sub2api-auto-group")) $("set-sub2api-auto-group").checked = cfg.auto_create_group !== false;
  if ($("set-sub2api-auto-push-register")) {
    $("set-sub2api-auto-push-register").checked = !!cfg.auto_push_on_register;
  }
  if ($("set-sub2api-concurrency")) $("set-sub2api-concurrency").value = cfg.concurrency != null ? cfg.concurrency : 4;
  if ($("set-sub2api-account-concurrency")) {
    const ac = cfg.account_concurrency != null ? cfg.account_concurrency : (cfg.account_capacity != null ? cfg.account_capacity : 3);
    $("set-sub2api-account-concurrency").value = ac;
  }
  if ($("set-sub2api-account-priority")) {
    $("set-sub2api-account-priority").value = cfg.account_priority != null ? cfg.account_priority : 50;
  }
  if ($("set-sub2api-account-rate")) {
    $("set-sub2api-account-rate").value = cfg.account_rate_multiplier != null ? cfg.account_rate_multiplier : 1;
  }
  if ($("set-sub2api-notes")) $("set-sub2api-notes").value = cfg.notes_prefix || "grokcli-2api";
  const pill = $("sub2api-pill");
  if (pill) {
    if (cfg.base_url && cfg.has_password) {
      pill.textContent = "已配置";
      pill.className = "g2a-tag g2a-tag-ok";
    } else if (cfg.base_url) {
      pill.textContent = "缺密码";
      pill.className = "g2a-tag g2a-tag-warn";
    } else {
      pill.textContent = "未配置";
      pill.className = "g2a-tag";
    }
  }
}

function fillCliproxyapiForm(cfg) {
  cfg = cfg || {};
  if ($("set-cliproxyapi-enabled")) $("set-cliproxyapi-enabled").checked = !!cfg.enabled;
  if ($("set-cliproxyapi-url")) $("set-cliproxyapi-url").value = cfg.base_url || "";
  if ($("set-cliproxyapi-key")) {
    $("set-cliproxyapi-key").value = "";
    $("set-cliproxyapi-key").placeholder = cfg.has_management_key ? "已保存，留空不改" : "Management Key";
  }
  if ($("set-cliproxyapi-auto-push-register")) {
    $("set-cliproxyapi-auto-push-register").checked = !!cfg.auto_push_on_register;
  }
  if ($("set-cliproxyapi-concurrency")) {
    $("set-cliproxyapi-concurrency").value = cfg.concurrency != null ? cfg.concurrency : 4;
  }
  if ($("set-cliproxyapi-auth-type")) {
    $("set-cliproxyapi-auth-type").value = cfg.auth_type || "xai";
  }
  if ($("set-cliproxyapi-base-upstream")) {
    $("set-cliproxyapi-base-upstream").value =
      cfg.base_upstream || "https://cli-chat-proxy.grok.com/v1";
  }
  if ($("set-cliproxyapi-notes")) {
    $("set-cliproxyapi-notes").value = cfg.notes_prefix || "grokcli-2api";
  }
  const pill = $("cliproxyapi-pill");
  if (pill) {
    if (cfg.base_url && cfg.has_management_key) {
      pill.textContent = "已配置";
      pill.className = "g2a-tag g2a-tag-ok";
    } else if (cfg.base_url) {
      pill.textContent = "缺 Key";
      pill.className = "g2a-tag g2a-tag-warn";
    } else {
      pill.textContent = "未配置";
      pill.className = "g2a-tag";
    }
  }
}

function collectCliproxyapiPatch() {
  if (!$("set-cliproxyapi-url") && !$("set-cliproxyapi-key")) return null;
  const patch = {
    enabled: !!( $("set-cliproxyapi-enabled") && $("set-cliproxyapi-enabled").checked ),
    base_url: $("set-cliproxyapi-url") ? ($("set-cliproxyapi-url").value || "").trim() : "",
    auto_push_on_register: !!(
      $("set-cliproxyapi-auto-push-register") && $("set-cliproxyapi-auto-push-register").checked
    ),
    notes_prefix: $("set-cliproxyapi-notes")
      ? (($("set-cliproxyapi-notes").value || "").trim() || "grokcli-2api")
      : "grokcli-2api",
    auth_type: $("set-cliproxyapi-auth-type")
      ? ($("set-cliproxyapi-auth-type").value || "xai")
      : "xai",
    base_upstream: $("set-cliproxyapi-base-upstream")
      ? (($("set-cliproxyapi-base-upstream").value || "").trim() ||
          "https://cli-chat-proxy.grok.com/v1")
      : "https://cli-chat-proxy.grok.com/v1",
  };
  const conc = $("set-cliproxyapi-concurrency")
    ? ($("set-cliproxyapi-concurrency").value || "").trim()
    : "";
  if (conc !== "") patch.concurrency = Number(conc);
  const key = $("set-cliproxyapi-key") ? ($("set-cliproxyapi-key").value || "") : "";
  if (key) patch.management_key = key;
  return patch;
}

async function saveCliproxyapiConfig(opts) {
  opts = opts || {};
  const patch = collectCliproxyapiPatch() || {};
  if (opts.test) patch.test = true;
  const r = await api("/settings/cliproxyapi", {
    method: "PUT",
    body: JSON.stringify(patch),
  });
  if (r && r.config) fillCliproxyapiForm(r.config);
  if (r && r.ok === false) {
    throw new Error((r.test && r.test.error) || r.error || "CLIProxyAPI 配置保存失败");
  }
  return r;
}

async function testCliproxyapiConnection() {
  const pre = $("cliproxyapi-test-result");
  if (pre) {
    pre.style.display = "block";
    pre.textContent = "测试中…";
  }
  try {
    await saveCliproxyapiConfig({});
    const r = await api("/settings/cliproxyapi/test", { method: "POST", body: "{}" });
    if (pre) pre.textContent = JSON.stringify(r, null, 2);
    const ok = !!(r && (r.ok || (r.test && r.test.ok)));
    const msg = ok
      ? (r.test && r.test.message) || r.message || "连接成功"
      : (r && r.test && r.test.error) || (r && r.error) || "失败";
    toast(msg, !ok);
    return r;
  } catch (e) {
    if (pre) pre.textContent = String(e.message || e);
    toast(e.message || String(e), true);
    throw e;
  }
}

async function pushAccountsToCliproxyapi({ all = false } = {}) {
  let body;
  if (all) {
    if (!confirm("确认将【全部账号】同步导入到 CLIProxyAPI？")) return;
    body = { all: true };
  } else {
    const ids = Array.from(selectedAccountIds || []);
    if (!ids.length) {
      toast("请先勾选要导入的账号", false);
      return;
    }
    if (!confirm(`确认将选中的 ${ids.length} 个账号同步导入到 CLIProxyAPI？`)) return;
    body = { account_ids: ids };
  }
  toast(all ? "正在同步全部账号到 CLIProxyAPI…" : "正在同步选中账号到 CLIProxyAPI…");
  try {
    const r = await api("/accounts/push-cliproxyapi", {
      method: "POST",
      body: JSON.stringify(body),
    });
    const ok = r && r.success != null ? r.success : 0;
    const fail = r && r.failed != null ? r.failed : 0;
    const total = r && r.total != null ? r.total : ok + fail;
    toast(
      r.message || `CLIProxyAPI 导入完成：成功 ${ok} / 失败 ${fail} / 共 ${total}`,
      fail !== 0
    );
    if (fail && r && Array.isArray(r.results)) {
      const firstErr = r.results.find((x) => x && !x.ok);
      if (firstErr) console.warn("cliproxyapi push sample error", firstErr);
    }
    return r;
  } catch (e) {
    toast(e.message || String(e), false);
    throw e;
  }
}

function collectSub2apiPatch() {
  if (!$("set-sub2api-url") && !$("set-sub2api-email")) return null;
  const patch = {
    enabled: !!( $("set-sub2api-enabled") && $("set-sub2api-enabled").checked ),
    base_url: $("set-sub2api-url") ? ($("set-sub2api-url").value || "").trim() : "",
    email: $("set-sub2api-email") ? ($("set-sub2api-email").value || "").trim() : "",
    group_name: $("set-sub2api-group-name") ? ($("set-sub2api-group-name").value || "").trim() : "",
    auto_create_group: !!( $("set-sub2api-auto-group") && $("set-sub2api-auto-group").checked ),
    auto_push_on_register: !!(
      $("set-sub2api-auto-push-register") && $("set-sub2api-auto-push-register").checked
    ),
    notes_prefix: $("set-sub2api-notes") ? (($("set-sub2api-notes").value || "").trim() || "grokcli-2api") : "grokcli-2api",
  };
  const gid = $("set-sub2api-group-id") ? ($("set-sub2api-group-id").value || "").trim() : "";
  if (gid !== "") patch.group_id = Number(gid);
  else patch.group_id = null;
  const conc = $("set-sub2api-concurrency") ? ($("set-sub2api-concurrency").value || "").trim() : "";
  if (conc !== "") patch.concurrency = Number(conc);
  const accConc = $("set-sub2api-account-concurrency") ? ($("set-sub2api-account-concurrency").value || "").trim() : "";
  if (accConc !== "") patch.account_concurrency = Number(accConc);
  const accPrio = $("set-sub2api-account-priority") ? ($("set-sub2api-account-priority").value || "").trim() : "";
  if (accPrio !== "") patch.account_priority = Number(accPrio);
  const accRate = $("set-sub2api-account-rate") ? ($("set-sub2api-account-rate").value || "").trim() : "";
  if (accRate !== "") patch.account_rate_multiplier = Number(accRate);
  const pw = $("set-sub2api-password") ? ($("set-sub2api-password").value || "") : "";
  if (pw) patch.password = pw;
  return patch;
}

function renderSub2apiGroups(groups) {
  const sel = $("set-sub2api-group-select");
  if (!sel) return;
  const cur = $("set-sub2api-group-id") ? String($("set-sub2api-group-id").value || "") : "";
  const items = Array.isArray(groups) ? groups : [];
  sel.innerHTML = '<option value="">— 选择已有分组 —</option>' + items.map((g) => {
    const id = g && g.id != null ? String(g.id) : "";
    const name = (g && (g.name || g.title)) || id;
    const plat = g && g.platform ? ` [${g.platform}]` : "";
    const selected = id && id === cur ? " selected" : "";
    return `<option value="${esc(id)}"${selected}>#${esc(id)} ${esc(name)}${esc(plat)}</option>`;
  }).join("");
}

async function saveSub2apiConfig(opts) {
  opts = opts || {};
  const patch = collectSub2apiPatch() || {};
  if (opts.test) patch.test = true;
  // Always persist via dedicated endpoint so secrets land even if main
  // "保存设置" path is skipped or soft-nav form is partial.
  const r = await api("/settings/sub2api", { method: "PUT", body: JSON.stringify(patch) });
  if (r && r.config) fillSub2apiForm(r.config);
  if (r && r.test && Array.isArray(r.test.groups)) renderSub2apiGroups(r.test.groups);
  if (r && r.ok === false) {
    throw new Error((r.test && r.test.error) || r.error || "sub2api 配置保存失败");
  }
  return r;
}

async function testSub2apiConnection() {
  const pre = $("sub2api-test-result");
  if (pre) { pre.style.display = "block"; pre.textContent = "测试中…"; }
  try {
    // Save current form first (password optional if already stored)
    await saveSub2apiConfig({});
    const r = await api("/settings/sub2api/test", { method: "POST", body: "{}" });
    if (pre) pre.textContent = JSON.stringify(r, null, 2);
    if (r && Array.isArray(r.groups)) renderSub2apiGroups(r.groups);
    toast(r && r.ok ? `连接成功，${r.group_count || 0} 个分组` : (r && r.error) || "失败", !(r && r.ok));
    return r;
  } catch (e) {
    if (pre) pre.textContent = String(e.message || e);
    toast(e.message || String(e), true);
    throw e;
  }
}

async function loadSub2apiGroups() {
  const pre = $("sub2api-test-result");
  if (pre) { pre.style.display = "block"; pre.textContent = "刷新分组中…"; }
  try {
    try {
      await saveSub2apiConfig({});
    } catch (e) {
      // still try list with previously saved config
      console.warn("save before groups failed", e);
    }
    const r = await api("/settings/sub2api/groups");
    renderSub2apiGroups((r && r.groups) || []);
    if (pre) pre.textContent = JSON.stringify(r, null, 2);
    toast(`已加载 ${(r && r.count) || 0} 个分组`);
    return r;
  } catch (e) {
    if (pre) pre.textContent = String(e.message || e);
    toast(e.message || String(e), true);
    throw e;
  }
}

async function createSub2apiGroup() {
  const name = prompt("新分组名称", ($("set-sub2api-group-name") && $("set-sub2api-group-name").value) || "grokcli-2api");
  if (!name) return;
  try { await saveSub2apiConfig({}); } catch (_) {}
  const r = await api("/settings/sub2api/groups", {
    method: "POST",
    body: JSON.stringify({ name, platform: "grok", set_default: true }),
  });
  if (r && r.config) fillSub2apiForm(r.config);
  toast(r && r.ok ? `分组已创建 #${(r.group && r.group.id) || "?"}` : "创建失败", !(r && r.ok));
  try { await loadSub2apiGroups(); } catch (_) {}
  return r;
}

async function pushAccountsToSub2api({ all = false } = {}) {
  let body;
  if (all) {
    if (!confirm("确认将【全部账号】导入到 sub2api？")) return;
    body = { all: true };
  } else {
    const ids = Array.from(selectedAccountIds || []);
    if (!ids.length) {
      toast("请先勾选要导入的账号", false);
      return;
    }
    if (!confirm(`确认将选中的 ${ids.length} 个账号导入到 sub2api？`)) return;
    body = { account_ids: ids };
  }
  // optional override from settings form if present
  const gid = $("set-sub2api-group-id") ? ($("set-sub2api-group-id").value || "").trim() : "";
  if (gid) body.group_id = Number(gid);
  toast(all ? "正在导入全部账号到 sub2api…" : "正在导入选中账号到 sub2api…");
  try {
    const r = await api("/accounts/push-sub2api", {
      method: "POST",
      body: JSON.stringify(body),
    });
    const ok = r && r.success != null ? r.success : 0;
    const fail = r && r.failed != null ? r.failed : 0;
    const total = r && r.total != null ? r.total : ok + fail;
    toast(`sub2api 导入完成：成功 ${ok} / 失败 ${fail} / 共 ${total}`, fail !== 0);
    if (fail && r && Array.isArray(r.results)) {
      const firstErr = r.results.find((x) => x && !x.ok);
      if (firstErr) console.warn("sub2api push sample error", firstErr);
    }
    return r;
  } catch (e) {
    toast(e.message || String(e), false);
    throw e;
  }
}

async function exportSub2apiFormat() {
  const ids = Array.from(selectedAccountIds || []);
  const body = ids.length ? { account_ids: ids } : { all: true };
  if (!ids.length && !confirm("未选择账号，将导出全部账号为 sub2api 数据备份 JSON（type=sub2api-data）。继续？")) return;
  try {
    const r = await api("/accounts/export-sub2api-format", {
      method: "POST",
      body: JSON.stringify(body),
    });
    // Backend now returns pure DataPayload {type,version,proxies,accounts}.
    // Fall back if an older server wrapped it.
    let payload = r;
    if (r && r.accounts && r.type !== "sub2api-data" && r.type !== "sub2api-bundle") {
      // legacy CreateAccountRequest[] wrapper → convert client-side
      const rows = Array.isArray(r.accounts) ? r.accounts : [];
      payload = {
        type: "sub2api-data",
        version: 1,
        exported_at: new Date().toISOString(),
        proxies: [],
        accounts: rows.map((row) => ({
          name: row.name || row.email || "grok-account",
          notes: row.notes || null,
          platform: row.platform || "grok",
          type: row.type || "oauth",
          credentials: row.credentials || {},
          extra: row.extra || {},
          concurrency: row.concurrency != null ? row.concurrency : 3,
          priority: row.priority != null ? row.priority : 50,
          rate_multiplier: row.rate_multiplier != null ? row.rate_multiplier : 1.0,
        })),
      };
    }
    if (!payload || !Array.isArray(payload.accounts) || !Array.isArray(payload.proxies)) {
      throw new Error("导出结果不是 sub2api-data 格式");
    }
    if (!payload.type) payload.type = "sub2api-data";
    if (!payload.version) payload.version = 1;
    if (!payload.exported_at) payload.exported_at = new Date().toISOString();
    const count = payload.accounts.length;
    const blob = new Blob([JSON.stringify(payload, null, 2)], { type: "application/json" });
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    // Name matches sub2api's own export convention so users recognize it.
    a.download = `sub2api-data-${Date.now()}.json`;
    document.body.appendChild(a);
    a.click();
    setTimeout(() => { URL.revokeObjectURL(a.href); a.remove(); }, 1000);
    toast(`已导出 sub2api-data：${count} 个账号（可在 sub2api「导入数据」中使用）`);
  } catch (e) {
    toast(e.message || String(e), true);
  }
}

async function exportCliproxyapiFormat() {
  const ids = Array.from(selectedAccountIds || []);
  const body = ids.length ? { account_ids: ids } : { all: true };
  if (
    !ids.length &&
    !confirm(
      "未选择账号，将导出全部账号为 CLIProxyAPI auth 包（type=cliproxyapi-auth-bundle）。继续？"
    )
  ) {
    return;
  }
  try {
    const r = await api("/accounts/export-cliproxyapi-format", {
      method: "POST",
      body: JSON.stringify(body),
    });
    let payload = r;
    if (!payload || payload.type !== "cliproxyapi-auth-bundle") {
      // tolerate accidental wrappers
      if (r && Array.isArray(r.accounts)) {
        payload = {
          type: "cliproxyapi-auth-bundle",
          version: 1,
          exported_at: new Date().toISOString(),
          source: "grokcli-2api",
          accounts: r.accounts,
        };
      }
    }
    if (!payload || !Array.isArray(payload.accounts)) {
      throw new Error("导出结果不是 cliproxyapi-auth-bundle 格式");
    }
    if (!payload.type) payload.type = "cliproxyapi-auth-bundle";
    if (!payload.version) payload.version = 1;
    if (!payload.exported_at) payload.exported_at = new Date().toISOString();
    const count = payload.accounts.length;
    const blob = new Blob([JSON.stringify(payload, null, 2)], {
      type: "application/json",
    });
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = `cliproxyapi-auth-bundle-${Date.now()}.json`;
    document.body.appendChild(a);
    a.click();
    setTimeout(() => {
      URL.revokeObjectURL(a.href);
      a.remove();
    }, 1000);
    toast(
      `已导出 CLIProxyAPI：${count} 个账号（可再「导入文件」回本系统，或拆成单文件放进 CPA auth 目录）`
    );
  } catch (e) {
    toast(e.message || String(e), true);
  }
}

function bindSub2apiUi() {
  on("btn-sub2api-test", "onclick", () => { testSub2apiConnection().catch(() => {}); });
  on("btn-sub2api-load-groups", "onclick", () => { loadSub2apiGroups().catch((e) => toast(e.message || String(e), true)); });
  on("btn-sub2api-create-group", "onclick", () => { createSub2apiGroup().catch((e) => toast(e.message || String(e), true)); });
  on("set-sub2api-group-select", "onchange", () => {
    const sel = $("set-sub2api-group-select");
    if (!sel || !sel.value) return;
    if ($("set-sub2api-group-id")) $("set-sub2api-group-id").value = sel.value;
    const opt = sel.options[sel.selectedIndex];
    if (opt && $("set-sub2api-group-name")) {
      // option text: #id name [platform]
      const t = (opt.textContent || "").replace(/^#\S+\s*/, "").replace(/\s*\[.*\]\s*$/, "").trim();
      if (t) $("set-sub2api-group-name").value = t;
    }
  });
  on("btn-acc-push-sub2api-selected", "onclick", () => { pushAccountsToSub2api({ all: false }).catch(() => {}); });
  on("btn-acc-push-sub2api-all", "onclick", () => { pushAccountsToSub2api({ all: true }).catch(() => {}); });
  on("btn-acc-export-sub2api-format", "onclick", () => { exportSub2apiFormat().catch(() => {}); });
  on("btn-acc-export-cliproxyapi-format", "onclick", () => { exportCliproxyapiFormat().catch(() => {}); });
  on("btn-acc-push-cliproxyapi-selected", "onclick", () => { pushAccountsToCliproxyapi({ all: false }).catch(() => {}); });
  on("btn-acc-push-cliproxyapi-all", "onclick", () => { pushAccountsToCliproxyapi({ all: true }).catch(() => {}); });
  on("btn-cliproxyapi-test", "onclick", () => { testCliproxyapiConnection().catch(() => {}); });
}
// bind once when DOM ready (core.js loads at end of body)
try { bindSub2apiUi(); } catch (_) {}

/* ── System settings page ───────────────────────────── */
function fillSystemSettingsForm(s) {
  s = s || {};
  if ($("set-account-mode") && s.account_mode) $("set-account-mode").value = s.account_mode;
  if ($("set-default-model")) $("set-default-model").value = s.default_model || "";
  if ($("set-token-maintain")) $("set-token-maintain").checked = s.token_maintain_enabled !== false;
  if ($("set-model-health")) $("set-model-health").checked = s.model_health_enabled !== false;
  if ($("set-model-health-auto-disable")) {
    $("set-model-health-auto-disable").checked = s.model_health_auto_disable !== false;
  }
  if ($("set-affinity")) $("set-affinity").checked = s.conversation_affinity_enabled !== false;
  if ($("set-token-maintain-interval") && s.token_maintain_interval_sec != null) {
    $("set-token-maintain-interval").value = s.token_maintain_interval_sec;
  }
  if ($("set-token-refresh-skew") && s.token_refresh_skew_sec != null) {
    $("set-token-refresh-skew").value = s.token_refresh_skew_sec;
  }
  if ($("set-model-health-interval") && s.model_health_interval_sec != null) {
    $("set-model-health-interval").value = s.model_health_interval_sec;
  }
  if ($("set-affinity-ttl") && s.conversation_affinity_ttl_sec != null) {
    $("set-affinity-ttl").value = s.conversation_affinity_ttl_sec;
  }
  if ($("set-probe-models")) {
    const pm = s.probe_models;
    $("set-probe-models").value = Array.isArray(pm) ? pm.join(", ") : (pm || "");
  }
  if ($("set-reasoning") && s.reasoning_compat) $("set-reasoning").value = s.reasoning_compat;
  if ($("set-max-tools")) $("set-max-tools").value = (s.outbound_max_tools != null ? s.outbound_max_tools : 1);
  if ($("set-max-tools-openai")) {
    $("set-max-tools-openai").value = (s.outbound_max_tools_openai != null ? s.outbound_max_tools_openai : 0);
  }
  if ($("set-tool-gap")) $("set-tool-gap").value = (s.outbound_tool_gap_sec != null ? s.outbound_tool_gap_sec : 0.08);
  if ($("set-sse-keepalive")) $("set-sse-keepalive").value = (s.sse_keepalive != null ? s.sse_keepalive : 8);
  if ($("set-history-compact")) $("set-history-compact").checked = !!s.history_compact_enabled;
  if ($("set-history-auto-chars") && s.history_compact_auto_chars != null) {
    $("set-history-auto-chars").value = s.history_compact_auto_chars;
  }
  if ($("set-history-keep-rounds") && s.history_keep_tool_rounds != null) {
    $("set-history-keep-rounds").value = s.history_keep_tool_rounds;
  }
  if ($("set-history-tool-max") && s.history_max_tool_result_chars != null) {
    $("set-history-tool-max").value = s.history_max_tool_result_chars;
  }
  const pol = s.pool_policy || s;
  if ($("set-cd-default") && pol.cooldown_default_sec != null) $("set-cd-default").value = pol.cooldown_default_sec;
  if ($("set-cd-auth") && pol.cooldown_auth_sec != null) $("set-cd-auth").value = pol.cooldown_auth_sec;
  if ($("set-cd-429") && pol.cooldown_rate_limit_sec != null) $("set-cd-429").value = pol.cooldown_rate_limit_sec;
  if ($("set-cd-5xx") && pol.cooldown_server_error_sec != null) $("set-cd-5xx").value = pol.cooldown_server_error_sec;
  if ($("set-cd-max") && pol.cooldown_max_sec != null) $("set-cd-max").value = pol.cooldown_max_sec;
  if ($("set-soft-ttl") && pol.soft_model_block_ttl_sec != null) $("set-soft-ttl").value = pol.soft_model_block_ttl_sec;
  if ($("set-durable-ttl") && pol.durable_model_block_ttl_sec != null) $("set-durable-ttl").value = pol.durable_model_block_ttl_sec;
  if ($("set-probe-kick-streak") && pol.probe_fail_kick_streak != null) $("set-probe-kick-streak").value = pol.probe_fail_kick_streak;
  if ($("set-probe-disable-streak") && pol.probe_fail_disable_streak != null) $("set-probe-disable-streak").value = pol.probe_fail_disable_streak;
  if ($("set-probe-kick-cd") && pol.probe_kick_cooldown_sec != null) $("set-probe-kick-cd").value = pol.probe_kick_cooldown_sec;
  if ($("set-max-failover") && pol.max_failover_attempts != null) $("set-max-failover").value = pol.max_failover_attempts;
  // Outbound proxy pool (account chat / probe / refresh)
  const ob = s.outbound_proxy_config || s.outbound_proxy || {};
  if ($("set-outbound-proxy-enabled")) {
    $("set-outbound-proxy-enabled").checked = ob.enabled !== false;
  }
  if ($("set-outbound-proxy")) $("set-outbound-proxy").value = ob.proxy || "";
  if ($("set-outbound-proxy-username")) $("set-outbound-proxy-username").value = ob.proxy_username || "";
  if ($("set-outbound-proxy-password")) $("set-outbound-proxy-password").value = ob.proxy_password || "";
  if ($("set-outbound-proxy-strategy")) {
    const st = String(ob.proxy_strategy || "round_robin").toLowerCase();
    $("set-outbound-proxy-strategy").value =
      st === "random" ? "random" : st === "sticky" ? "sticky" : "round_robin";
  }
  try { updateOutboundProxyHint(s); } catch (_) {}
  // sub2api push config
  try { fillSub2apiForm(s && s.sub2api_config); } catch (_) {}
  try { fillCliproxyapiForm(s && s.cliproxyapi_config); } catch (_) {}
  const pill = $("pwd-env-pill");
  if (pill) {
    if (s.admin_password_in_store || (s.has_admin_password && !s.admin_password_from_env)) {
      pill.textContent = "数据库密码";
      pill.className = "g2a-tag";
    } else if (s.admin_password_from_env) {
      // First-boot only: env still the only source before seed/setup.
      pill.textContent = "待写入数据库";
      pill.className = "g2a-tag g2a-tag-warn";
    } else {
      pill.textContent = s.has_admin_password ? "已设置密码" : "未设置";
      pill.className = "g2a-tag";
    }
  }
  const up = $("settings-updated-at");
  if (up) {
    up.textContent = s.updated_at
      ? ("上次更新：" + (typeof fmtTime === "function" ? fmtTime(s.updated_at) : new Date(s.updated_at * 1000).toLocaleString()))
      : "尚未通过管理台保存过设置";
  }
}

async function loadSystemSettings(force) {
  // Prefer dedicated endpoint; fall back to dashboard cache.
  let s = null;
  try {
    const r = await api("/settings");
    s = (r && r.settings) || r || null;
  } catch (e) {
    if (force) throw e;
    s = (dashCache && dashCache.settings) || (statusCache && statusCache.settings) || null;
  }
  if (!s) return null;
  if (dashCache) dashCache.settings = Object.assign({}, dashCache.settings || {}, s);
  if (statusCache) statusCache.settings = Object.assign({}, statusCache.settings || {}, s);
  fillSystemSettingsForm(s);
  return s;
}

function collectSystemSettingsPatch() {
  const patch = {};
  if ($("set-account-mode")) patch.account_mode = $("set-account-mode").value;
  if ($("set-default-model")) patch.default_model = ($("set-default-model").value || "").trim();
  if ($("set-token-maintain")) patch.token_maintain_enabled = !!$("set-token-maintain").checked;
  if ($("set-model-health")) patch.model_health_enabled = !!$("set-model-health").checked;
  if ($("set-model-health-auto-disable")) {
    patch.model_health_auto_disable = !!$("set-model-health-auto-disable").checked;
  }
  if ($("set-affinity")) patch.conversation_affinity_enabled = !!$("set-affinity").checked;
  if ($("set-token-maintain-interval") && $("set-token-maintain-interval").value !== "") {
    patch.token_maintain_interval_sec = Number($("set-token-maintain-interval").value);
  }
  if ($("set-token-refresh-skew") && $("set-token-refresh-skew").value !== "") {
    patch.token_refresh_skew_sec = Number($("set-token-refresh-skew").value);
  }
  if ($("set-model-health-interval") && $("set-model-health-interval").value !== "") {
    patch.model_health_interval_sec = Number($("set-model-health-interval").value);
  }
  if ($("set-affinity-ttl") && $("set-affinity-ttl").value !== "") {
    patch.conversation_affinity_ttl_sec = Number($("set-affinity-ttl").value);
  }
  if ($("set-probe-models")) {
    patch.probe_models = ($("set-probe-models").value || "").trim();
  }
  if ($("set-reasoning")) patch.reasoning_compat = $("set-reasoning").value;
  if ($("set-max-tools") && $("set-max-tools").value !== "") {
    patch.outbound_max_tools = Number($("set-max-tools").value);
  }
  if ($("set-max-tools-openai") && $("set-max-tools-openai").value !== "") {
    patch.outbound_max_tools_openai = Number($("set-max-tools-openai").value);
  }
  if ($("set-tool-gap") && $("set-tool-gap").value !== "") {
    patch.outbound_tool_gap_sec = Number($("set-tool-gap").value);
  }
  if ($("set-sse-keepalive") && $("set-sse-keepalive").value !== "") {
    patch.sse_keepalive = Number($("set-sse-keepalive").value);
  }
  if ($("set-history-compact")) patch.history_compact_enabled = !!$("set-history-compact").checked;
  if ($("set-history-auto-chars") && $("set-history-auto-chars").value !== "") {
    patch.history_compact_auto_chars = Number($("set-history-auto-chars").value);
  }
  if ($("set-history-keep-rounds") && $("set-history-keep-rounds").value !== "") {
    patch.history_keep_tool_rounds = Number($("set-history-keep-rounds").value);
  }
  if ($("set-history-tool-max") && $("set-history-tool-max").value !== "") {
    patch.history_max_tool_result_chars = Number($("set-history-tool-max").value);
  }
  if ($("set-cd-default") && $("set-cd-default").value !== "") patch.cooldown_default_sec = Number($("set-cd-default").value);
  if ($("set-cd-auth") && $("set-cd-auth").value !== "") patch.cooldown_auth_sec = Number($("set-cd-auth").value);
  if ($("set-cd-429") && $("set-cd-429").value !== "") patch.cooldown_rate_limit_sec = Number($("set-cd-429").value);
  if ($("set-cd-5xx") && $("set-cd-5xx").value !== "") patch.cooldown_server_error_sec = Number($("set-cd-5xx").value);
  if ($("set-cd-max") && $("set-cd-max").value !== "") patch.cooldown_max_sec = Number($("set-cd-max").value);
  if ($("set-soft-ttl") && $("set-soft-ttl").value !== "") patch.soft_model_block_ttl_sec = Number($("set-soft-ttl").value);
  if ($("set-durable-ttl") && $("set-durable-ttl").value !== "") patch.durable_model_block_ttl_sec = Number($("set-durable-ttl").value);
  if ($("set-probe-kick-streak") && $("set-probe-kick-streak").value !== "") patch.probe_fail_kick_streak = Number($("set-probe-kick-streak").value);
  if ($("set-probe-disable-streak") && $("set-probe-disable-streak").value !== "") patch.probe_fail_disable_streak = Number($("set-probe-disable-streak").value);
  if ($("set-probe-kick-cd") && $("set-probe-kick-cd").value !== "") patch.probe_kick_cooldown_sec = Number($("set-probe-kick-cd").value);
  if ($("set-max-failover") && $("set-max-failover").value !== "") patch.max_failover_attempts = Number($("set-max-failover").value);
  // Outbound proxy pool
  if ($("set-outbound-proxy-enabled")) patch.outbound_proxy_enabled = !!$("set-outbound-proxy-enabled").checked;
  if ($("set-outbound-proxy")) patch.outbound_proxy = $("set-outbound-proxy").value || "";
  if ($("set-outbound-proxy-username")) patch.outbound_proxy_username = $("set-outbound-proxy-username").value || "";
  if ($("set-outbound-proxy-password")) {
    const pw = $("set-outbound-proxy-password").value || "";
    // Empty keeps previous secret server-side unless user cleared proxy list.
    if (pw) patch.outbound_proxy_password = pw;
  }
  if ($("set-outbound-proxy-strategy")) patch.outbound_proxy_strategy = $("set-outbound-proxy-strategy").value || "round_robin";
  // sub2api
  try {
    const s2 = collectSub2apiPatch();
    if (s2) patch.sub2api_config = s2;
  } catch (_) {}
  // CLIProxyAPI
  try {
    const cpa = collectCliproxyapiPatch();
    if (cpa) patch.cliproxyapi_config = cpa;
  } catch (_) {}
  return patch;
}

function countOutboundProxyLines(text) {
  return String(text || "")
    .split(/\r?\n|;|,/)
    .map((s) => s.trim())
    .filter((s) => s && !s.startsWith("#"))
    .length;
}

function updateOutboundProxyHint(s) {
  const hint = $("set-outbound-proxy-hint");
  const pill = $("outbound-proxy-pill");
  const enabled = $("set-outbound-proxy-enabled")
    ? !!$("set-outbound-proxy-enabled").checked
    : true;
  const text = $("set-outbound-proxy")
    ? $("set-outbound-proxy").value
    : ((s && s.outbound_proxy_config && s.outbound_proxy_config.proxy) || "");
  const n = countOutboundProxyLines(text);
  const strat = $("set-outbound-proxy-strategy")
    ? $("set-outbound-proxy-strategy").value
    : ((s && s.outbound_proxy_config && s.outbound_proxy_config.proxy_strategy) || "round_robin");
  const stratLabel =
    strat === "random" ? "随机" : strat === "sticky" ? "固定首个" : "粘性哈希";
  const summary = (s && s.outbound_proxy_pool) || {};
  const src = summary.source || (n > 0 ? "settings" : "none");
  if (hint) {
    if (!enabled) {
      hint.textContent = "已关闭出站代理，账号请求直连上游。";
    } else if (n <= 0) {
      hint.textContent = "未配置代理（直连）。可粘贴多行代理池；账号聊天/测活/续期共用。";
    } else {
      hint.textContent = `代理池 ${n} 个 · 策略：${stratLabel}。同一账号固定出口（会话粘性更稳）。来源：${src}`;
    }
  }
  if (pill) {
    if (!enabled) {
      pill.textContent = "已关闭";
      pill.className = "g2a-tag";
    } else if (n <= 0) {
      pill.textContent = "直连";
      pill.className = "g2a-tag";
    } else {
      pill.textContent = `代理池 ${n}`;
      pill.className = "g2a-tag g2a-tag-ok";
    }
  }
}

async function saveSystemSettings() {
  const btn = $("btn-save-settings");
  if (btn) btn.disabled = true;
  try {
    const patch = collectSystemSettingsPatch();
    if (patch.outbound_max_tools != null && (Number.isNaN(patch.outbound_max_tools) || patch.outbound_max_tools < 0)) {
      throw new Error("每轮工具数无效");
    }
    if (patch.outbound_tool_gap_sec != null && (Number.isNaN(patch.outbound_tool_gap_sec) || patch.outbound_tool_gap_sec < 0)) {
      throw new Error("工具间隔无效");
    }
    // Persist sub2api via dedicated endpoint FIRST so password is never dropped
    // by the redacted public settings response from PUT /settings.
    let s2err = null;
    let cpaErr = null;
    try {
      if ($("set-sub2api-url") || $("set-sub2api-email")) {
        await saveSub2apiConfig({});
      }
    } catch (e) {
      s2err = e;
      console.warn("sub2api save failed", e);
    }
    try {
      if ($("set-cliproxyapi-url") || $("set-cliproxyapi-key")) {
        await saveCliproxyapiConfig({});
      }
    } catch (e) {
      cpaErr = e;
      console.warn("cliproxyapi save failed", e);
    }
    // Avoid double-writing / redacting secrets through general settings path.
    if (patch.sub2api_config) delete patch.sub2api_config;
    if (patch.cliproxyapi_config) delete patch.cliproxyapi_config;
    const r = await api("/settings", { method: "PUT", body: JSON.stringify(patch) });
    const s = (r && r.settings) || patch;
    if (dashCache) dashCache.settings = Object.assign({}, dashCache.settings || {}, s);
    if (statusCache) statusCache.settings = Object.assign({}, statusCache.settings || {}, s);
    fillSystemSettingsForm(s);
    // Re-fill sub2api from dedicated GET so has_password/url stay accurate.
    try {
      const s2 = await api("/settings/sub2api");
      if (s2 && s2.config) fillSub2apiForm(s2.config);
    } catch (_) {}
    try {
      const cpa = await api("/settings/cliproxyapi");
      if (cpa && cpa.config) fillCliproxyapiForm(cpa.config);
    } catch (_) {}
    try { await refreshOverviewStatus({ force: true, render: true }); } catch (_) {}
    if (s2err || cpaErr) {
      const parts = [];
      if (s2err) parts.push("sub2api: " + (s2err.message || s2err));
      if (cpaErr) parts.push("CLIProxyAPI: " + (cpaErr.message || cpaErr));
      toast("其它设置已保存，但 " + parts.join("；"), true);
    } else {
      toast("设置已保存");
    }
    try { await loadDashboard(); } catch (_) {}
    return s;
  } finally {
    if (btn) btn.disabled = false;
  }
}

async function changeAdminPassword() {
  const cur = ($("set-cur-password") && $("set-cur-password").value) || "";
  const nw = ($("set-new-password") && $("set-new-password").value) || "";
  const cf = ($("set-confirm-password") && $("set-confirm-password").value) || "";
  if (!cur) throw new Error("请输入当前密码");
  if (!nw || nw.length < 4) throw new Error("新密码至少 4 位");
  if (nw !== cf) throw new Error("两次输入的新密码不一致");
  const btn = $("btn-change-password");
  if (btn) btn.disabled = true;
  try {
    const r = await api("/settings/password", {
      method: "PUT",
      body: JSON.stringify({
        current_password: cur,
        new_password: nw,
        confirm_password: cf,
      }),
    });
    if ($("set-cur-password")) $("set-cur-password").value = "";
    if ($("set-new-password")) $("set-new-password").value = "";
    if ($("set-confirm-password")) $("set-confirm-password").value = "";
    if (r && r.settings) fillSystemSettingsForm(r.settings);
    toast(r.message || "密码已更新");
  } finally {
    if (btn) btn.disabled = false;
  }
}


async function setFeatureToggle(path, enabled, label) {
  try {
    const r = await api(path, {
      method: "PUT",
      body: JSON.stringify({ enabled: !!enabled }),
    });
    toast((label || "设置") + (enabled ? " 已开启" : " 已关闭"));
    statusCache = statusCache || {};
    dashCache = dashCache || {};
    statusCache.settings = statusCache.settings || {};
    dashCache.settings = dashCache.settings || {};
    if (path.indexOf("token-maintain") >= 0) {
      statusCache.settings.token_maintain_enabled = !!enabled;
      dashCache.settings.token_maintain_enabled = !!enabled;
    }
    if (path.indexOf("model-health") >= 0) {
      statusCache.settings.model_health_enabled = !!enabled;
      dashCache.settings.model_health_enabled = !!enabled;
    }
    // Prefer fields returned by toggle API immediately.
    if (r && r.maintainer) {
      statusCache.token_maintainer = r.maintainer;
      dashCache.token_maintainer = r.maintainer;
    }
    if (r && r.model_health) {
      statusCache.model_health = r.model_health;
      dashCache.model_health = r.model_health;
    }
    if (r && r.settings) {
      statusCache.settings = Object.assign({}, statusCache.settings, r.settings);
      dashCache.settings = Object.assign({}, dashCache.settings, r.settings);
    }
    try { renderMaintainer(); } catch (_) {}
    try { renderModelHealthInfo(); } catch (_) {}
    try { await refreshOverviewStatus({ force: true, render: true }); } catch (_) {}
  } catch (e) {
    toast(e.message || "切换失败", false);
    try { await refreshOverviewStatus({ force: true, render: true }); } catch (_) {
      try { renderMaintainer(); } catch (_) {}
      try { renderModelHealthInfo(); } catch (_) {}
    }
  }
}

if ($("chk-token-maintain")) {
  $("chk-token-maintain").onchange = () => setFeatureToggle(
    "/settings/token-maintain",
    !!$("chk-token-maintain").checked,
    "Token 自动续期"
  );
}
if ($("chk-model-health")) {
  $("chk-model-health").onchange = () => setFeatureToggle(
    "/settings/model-health",
    !!$("chk-model-health").checked,
    "自动健康探测"
  );
}

on("btn-refresh-tokens", "onclick", async () => {  try {
    if ($("btn-refresh-tokens")) $("btn-refresh-tokens").disabled = true;
    const r = await api("/accounts/refresh", {
      method: "POST",
      body: JSON.stringify({ force: true }),
    });
    const n = r.refreshed ?? (r.results || []).filter(x => x.ok && !x.skipped).length;
    const lines = (r.results || [])
      .filter(x => x.ok && !x.skipped)
      .map(x => `${x.email || x.id}: 新过期 ${fmtTime(x.expires_at)} (剩余 ${fmtRemaining(x.expires_at)})`);
    toast(`Token 已刷新：${n} 个账号` + (lines.length ? " · " + lines[0] : ""));
    statusCache = await api("/status");
    await loadDashboard();
  } catch (e) { toast(e.message, false); }
  finally { if ($("btn-refresh-tokens")) $("btn-refresh-tokens").disabled = false; }
});
on("btn-normalize-keys", "onclick", async () => {  try {
    const r = await api("/accounts/normalize", { method: "POST" });
    toast(`多账号键规范化：变更 ${r.changed ?? 0}，共 ${r.total ?? 0} 个`);
    statusCache = await api("/status");
    await loadDashboard();
  } catch (e) { toast(e.message, false); }
});

if ($("btn-sync-models")) {
  on("btn-sync-models", "onclick", async () => {    try {
      const r = await api("/models/sync", { method: "POST" });
      statusCache = await api("/status");
      const list = await loadModels();
      toast(`已同步 ${r.count || (list || []).length || 0} 个模型`);
    } catch (e) { toast(e.message, false); }
  });
}

// Fallback top-level bindings (first paint / non soft-nav). Soft-nav rebinds via rebindPageControls.
if ($("btn-start-reg")) {
  // Prefer the rebind path; only attach if not already bound by rebindPageControls.
  if (!$("btn-start-reg").onclick) {
    on("btn-start-reg", "onclick", async () => {
      try {
        const config = readRegConfig();
        cacheRegConfigLocal(config);
        if ($("btn-start-reg")) $("btn-start-reg").disabled = true;
        // New task must not inherit previous run's log / track / poll state.
        resetRegProgressForNewTask();
        const r = await api("/accounts/register-email", {
          method: "POST",
          body: JSON.stringify(buildRegBody(config)),
        });
        regBatchId = r.batch_id || null;
        if (r.batch || (Array.isArray(r.session_ids) && r.session_ids.length > 1) || (Array.isArray(r.sessions) && r.sessions.length > 1)) {
          regSessionIds = Array.isArray(r.session_ids) && r.session_ids.length
            ? r.session_ids.slice()
            : (Array.isArray(r.sessions) ? r.sessions.map(s => s.id || s.session_id).filter(Boolean) : []);
          regSessionId = regSessionIds[0] || r.id || r.session_id || null;
          if (Array.isArray(r.sessions) && r.sessions.length) showRegSessionGroup(r.sessions, { batch: r });
          else showRegSessionGroup(regSessionIds.map(id => ({ id, status: "starting" })), { batch: r });
          toast(`已启动批量注册：${r.count || regSessionIds.length} 个 / 并发 ${r.concurrency || "?"}`);
        } else {
          regSessionId = r.id || r.session_id || null;
          regSessionIds = regSessionId ? [regSessionId] : [];
          showRegSession(r);
          toast(r.email ? ("已启动: " + r.email) : "已启动邮箱注册");
        }
        saveRegTrack();
        setTimeout(() => { loadRegConfig(true).catch(() => {}); }, 300);
        startRegPolling({ immediate: true, intervalMs: 1000 });
        if (r.batch_id) {
          setTimeout(async () => {
            try {
              // Ignore late batch snapshot if user already started another task.
              if (regBatchId && regBatchId !== r.batch_id) return;
              const b = await api("/accounts/register-email/batches/" + encodeURIComponent(r.batch_id));
              if (Array.isArray(b.session_ids) && b.session_ids.length) {
                regSessionIds = b.session_ids.slice();
                regSessionId = regSessionIds[0];
              }
              if (Array.isArray(b.sessions) && b.sessions.length) {
                showRegSessionGroup(b.sessions, { batch: b });
              }
              saveRegTrack();
            } catch (_) {}
          }, 1500);
        }
      } catch (e) {
        toast(e.message, false);
      } finally {
        if ($("btn-start-reg")) $("btn-start-reg").disabled = false;
      }
    });
  }
}
if ($("btn-test-reg-proxy") && !$("btn-test-reg-proxy").onclick) {
  on("btn-test-reg-proxy", "onclick", async () => {
    try {
      if ($("btn-test-reg-proxy")) $("btn-test-reg-proxy").disabled = true;
      const r = await api("/register-email/test-proxy", {
        method: "POST",
        body: JSON.stringify(buildProxyTestBody(readRegConfig())),
      });
      showPanel("reg-session-box");
      setRegEmailText("xAI 代理测试");
      const poolN = r.proxy_pool && r.proxy_pool.count != null ? Number(r.proxy_pool.count) : 0;
      let status = r.ok ? "代理可用" : "代理不可用";
      if (poolN > 1) {
        if (Array.isArray(r.results)) {
          status = r.ok
            ? `代理池 ${r.ok_count || 0}/${r.tested || r.results.length} 可用`
            : `代理池测试失败 (${r.ok_count || 0}/${r.tested || r.results.length})`;
        } else {
          status = r.ok ? `代理可用 (池 ${poolN})` : `代理不可用 (池 ${poolN})`;
        }
      }
      setRegStatusText(status);
      setLogPanel("reg-log", JSON.stringify(r, null, 2), { forceShow: true });
      toast(r.ok ? status : (status + (r.error ? ": " + r.error : "")), !!r.ok);
    } catch (e) {
      toast(e.message, false);
    } finally {
      if ($("btn-test-reg-proxy")) $("btn-test-reg-proxy").disabled = false;
    }
  });
}
if ($("btn-save-reg") && !$("btn-save-reg").onclick) {
  on("btn-save-reg", "onclick", () => { saveRegConfig().catch(() => {}); });
}
if ($("btn-refresh-reg") && !$("btn-refresh-reg").onclick) {
  on("btn-refresh-reg", "onclick", () => {
    refreshRegistrationProgress({ toastIfEmpty: true }).catch(() => {});
  });
}
if ($("btn-stop-reg") && !$("btn-stop-reg").onclick) {
  on("btn-stop-reg", "onclick", () => { stopRegistration().catch(() => {}); });
}
if ($("btn-stop-reg-inline") && !$("btn-stop-reg-inline").onclick) {
  on("btn-stop-reg-inline", "onclick", () => { stopRegistration().catch(() => {}); });
}
if ($("btn-refresh-reg-inline") && !$("btn-refresh-reg-inline").onclick) {
  on("btn-refresh-reg-inline", "onclick", () => {
    refreshRegistrationProgress({ toastIfEmpty: true }).catch(() => {});
  });
}
if ($("btn-close-reg-inline") && !$("btn-close-reg-inline").onclick) {
  on("btn-close-reg-inline", "onclick", () => {
    dismissRegProgressCard();
    toast("已关闭进度卡片（后台注册不受影响）");
  });
}
if ($("reg-captcha-provider")) {
  on("reg-captcha-provider", "onchange", () => {
    syncRegCaptchaProviderUI();
  });
  syncRegCaptchaProviderUI();
}
if ($("reg-mail-provider")) {
  on("reg-mail-provider", "onchange", () => {
    syncRegMailProviderUI();
  });
  syncRegMailProviderUI();
}
if ($("reg-proxy")) {
  on("reg-proxy", "oninput", () => { try { updateRegProxyHint(); } catch (_) {} });
}
if ($("reg-proxy-strategy")) {
  on("reg-proxy-strategy", "onchange", () => { try { updateRegProxyHint(); } catch (_) {} });
}
try { updateRegProxyHint(); } catch (_) {}

  window.addEventListener("pagehide", () => {
    try { if (devicePollTimer) clearInterval(devicePollTimer); } catch(_){}
    try { if (regPollTimer) clearInterval(regPollTimer); } catch(_){}
    try { if (uiRefreshTimer) clearInterval(uiRefreshTimer); } catch(_){}
  });
  [["btn-probe-all","btn-probe-all-2"],["btn-refresh-quota","btn-refresh-quota-2"]].forEach(([main,alt]) => {
    if (!$(main) && $(alt)) { try { $(alt).id = main; } catch(_){ } }
  });
  
/* ── Usage / token stats ───────────────────────────── */
let usageDays = 7;
let usageLoading = false;
let usageEventsPage = 1;
let usageEventsPageSize = 50;
let usageEventsTotalPages = 1;
let usageEventsLoading = false;
let usageEventsLoadSeq = 0;

function bindUsageControls() {
  on("btn-usage-reload", "onclick", () => loadUsage());
  const days = $("usage-days");
  if (days && !days._usageBound) {
    days._usageBound = true;
    days.addEventListener("change", () => {
      usageDays = Number(days.value || 7) || 7;
      loadUsage();
    });
  }
  bindUsageEventsControls();
}

function bindUsageEventsControls() {
  on("btn-usage-events-reload", "onclick", () => loadUsageEvents({ reset: false }));
  on("btn-usage-events-search", "onclick", () => loadUsageEvents({ reset: true }));
  on("usage-events-page-prev", "onclick", () => {
    if (usageEventsPage > 1 && !usageEventsLoading) {
      usageEventsPage -= 1;
      loadUsageEvents();
    }
  });
  on("usage-events-page-next", "onclick", () => {
    if (!usageEventsLoading && usageEventsPage < usageEventsTotalPages) {
      usageEventsPage += 1;
      loadUsageEvents();
    }
  });
  const q = $("usage-events-q");
  if (q && !q._usageEventsBound) {
    q._usageEventsBound = true;
    q.addEventListener("keydown", (e) => {
      if (e.key === "Enter") loadUsageEvents({ reset: true });
    });
  }
  ["usage-events-protocol", "usage-events-ok", "usage-events-page-size"].forEach((id) => {
    const el = $(id);
    if (el && !el._usageEventsBound) {
      el._usageEventsBound = true;
      el.addEventListener("change", () => loadUsageEvents({ reset: true }));
    }
  });
  const tb = $("usage-events-tbody");
  if (tb && !tb._usageEventsBound) {
    tb._usageEventsBound = true;
    tb.addEventListener("click", (e) => {
      const tr = e.target.closest("tr[data-usage-detail]");
      if (!tr) return;
      try {
        const detail = JSON.parse(tr.getAttribute("data-usage-detail") || "{}");
        const panel = $("usage-events-detail");
        if (!panel) return;
        panel.hidden = false;
        panel.classList.remove("hidden", "is-empty");
        panel.textContent = JSON.stringify(detail, null, 2);
      } catch (_) {}
    });
  }
}

async function loadUsageEvents({ reset = false } = {}) {
  if (!$("usage-events-tbody")) return;
  bindUsageEventsControls();
  if (reset) usageEventsPage = 1;
  usageEventsLoading = true;
  const seq = ++usageEventsLoadSeq;
  const q = ($("usage-events-q") && $("usage-events-q").value || "").trim();
  const protocol = ($("usage-events-protocol") && $("usage-events-protocol").value) || "all";
  const ok = ($("usage-events-ok") && $("usage-events-ok").value) || "all";
  usageEventsPageSize = parseInt(($("usage-events-page-size") && $("usage-events-page-size").value) || "50", 10) || 50;
  $("usage-events-tbody").innerHTML = `<tr><td colspan="14" class="g2a-muted">加载明细中…</td></tr>`;
  if ($("usage-events-info")) $("usage-events-info").textContent = "查询中…";
  try {
    // Backend stores chat as openai_chat; keep UI label "openai".
    const protocolFilter = protocol === "openai" ? "openai_chat" : protocol;
    const params = new URLSearchParams({
      page: String(usageEventsPage),
      page_size: String(usageEventsPageSize),
      q,
      protocol: protocolFilter,
      ok,
    });
    const data = await api("/usage/events?" + params.toString());
    if (seq !== usageEventsLoadSeq) return;
    const items = (data && data.items) || [];
    usageEventsPage = Number(data.page || usageEventsPage) || 1;
    usageEventsTotalPages = Number(data.total_pages || 1) || 1;
    if ($("usage-events-info")) {
      $("usage-events-info").textContent =
        `共 ${fmtNum(data.total || 0)} 条 · 源 ${(data.store_source || "none")}` +
        (q ? ` · 关键词 “${q}”` : "");
    }
    if ($("usage-events-page-info")) {
      $("usage-events-page-info").textContent =
        `第 ${usageEventsPage} / ${usageEventsTotalPages} 页`;
    }
    if (!items.length) {
      $("usage-events-tbody").innerHTML =
        `<tr><td colspan="14" class="g2a-muted">暂无请求明细（新请求完成后会出现在这里）</td></tr>`;
      return;
    }
    const fmtLatency = (ms) => {
      if (ms == null || ms === "" || Number.isNaN(Number(ms))) return "—";
      const n = Number(ms);
      if (n < 1000) return `${Math.round(n)} ms`;
      return `${(n / 1000).toFixed(n >= 10000 ? 1 : 2)} s`;
    };
    $("usage-events-tbody").innerHTML = items.map((it) => {
      const keyLabel = it.api_key_name
        ? `${it.api_key_name}${it.api_key_prefix ? " · " + it.api_key_prefix : ""}`
        : (it.api_key_prefix || it.api_key_id || "—");
      const protoPath = `${it.protocol || "—"}${it.stream ? " · stream" : ""}\n${it.path || ""}`;
      const cacheRead = Number(it.cache_read_tokens || 0);
      const cacheCreate = Number(it.cache_creation_tokens || 0);
      const promptTok = Number(it.prompt_tokens || 0);
      const cacheTokens = cacheRead + cacheCreate;
      const hitPct = promptTok > 0 && cacheRead > 0
        ? Math.min(100, Math.round((cacheRead / promptTok) * 1000) / 10)
        : null;
      const cacheParts = [];
      if (cacheRead > 0) cacheParts.push(`读 ${fmtNum(cacheRead)}`);
      if (cacheCreate > 0) cacheParts.push(`写 ${fmtNum(cacheCreate)}`);
      if (hitPct != null) cacheParts.push(`命中 ${hitPct}%`);
      const cacheSub = cacheParts.join(" / ");
      const reasoningTokens = Number(it.reasoning_tokens || 0);
      const effortRaw = (
        it.reasoning_effort
        || (it.detail && (it.detail.reasoning_effort || it.detail.thinking_intensity || it.detail.thinking_effort))
        || ""
      );
      // Show canonical English labels (low / medium / high / xhigh).
      const effort = String(effortRaw || "").trim().toLowerCase();
      let effortPill;
      if (!effort) {
        effortPill = '<span class="g2a-muted">—</span>';
      } else if (effort === "low") {
        effortPill = `<span class="g2a-tag" title="reasoning_effort: ${esc(effort)}">${esc(effort)}</span>`;
      } else if (effort === "medium") {
        effortPill = `<span class="g2a-tag warn" title="reasoning_effort: ${esc(effort)}">${esc(effort)}</span>`;
      } else if (effort === "high" || effort === "xhigh") {
        effortPill = `<span class="g2a-tag bad" title="reasoning_effort: ${esc(effort)}">${esc(effort)}</span>`;
      } else {
        effortPill = `<span class="g2a-tag" title="reasoning_effort: ${esc(effort)}">${esc(effort)}</span>`;
      }
      const ttftMs = it.ttft_ms != null
        ? it.ttft_ms
        : (it.detail && it.detail.ttft_ms != null ? it.detail.ttft_ms : null);
      const doneMs = it.latency_ms != null
        ? it.latency_ms
        : (it.detail && it.detail.latency_ms != null ? it.detail.latency_ms : null);
      const okPill = it.ok
        ? '<span class="g2a-tag ok">成功</span>'
        : '<span class="g2a-tag bad">失败</span>';
      const detail = {
        id: it.id,
        created_at: it.created_at,
        api_key_id: it.api_key_id,
        api_key_name: it.api_key_name,
        api_key_prefix: it.api_key_prefix,
        account_id: it.account_id,
        account_email: it.account_email,
        model: it.model,
        protocol: it.protocol,
        path: it.path,
        stream: it.stream,
        ok: it.ok,
        prompt_tokens: it.prompt_tokens,
        completion_tokens: it.completion_tokens,
        total_tokens: it.total_tokens,
        cache_read_tokens: it.cache_read_tokens,
        cache_creation_tokens: it.cache_creation_tokens,
        reasoning_tokens: it.reasoning_tokens,
        reasoning_effort: effort || "",
        thinking_intensity: effort || "",
        client_ip: it.client_ip,
        user_agent: it.user_agent,
        status_code: it.status_code,
        ttft_ms: ttftMs,
        latency_ms: doneMs,
        error: it.error,
        detail: it.detail || {},
      };
      const detailAttr = esc(JSON.stringify(detail)).replace(/'/g, "&#39;");
      return `<tr data-usage-detail='${detailAttr}' style="cursor:pointer" title="点击查看完整明细">
        <td class="mono" style="font-size:12px">${esc(fmtTime(it.created_at))}</td>
        <td style="font-size:12px;white-space:pre-line">${esc(protoPath)}</td>
        <td class="mono" style="font-size:12px">${esc(keyLabel)}<div class="g2a-muted" style="font-size:11px">${esc(it.api_key_id || "")}</div></td>
        <td class="mono" style="font-size:12px">${esc(it.client_ip || "—")}</td>
        <td class="mono" style="font-size:12px">${esc(it.model || "—")}<div class="g2a-muted" style="font-size:11px">${esc(it.account_email || it.account_id || "")}</div></td>
        <td class="mono">${fmtNum(it.prompt_tokens)}</td>
        <td class="mono">${fmtNum(it.completion_tokens)}</td>
        <td class="mono">${fmtNum(it.total_tokens)}</td>
        <td class="mono" style="font-size:12px">${cacheTokens > 0 ? fmtNum(cacheTokens) : "—"}${cacheSub ? `<div class="g2a-muted" style="font-size:11px">${esc(cacheSub)}</div>` : ""}</td>
        <td class="mono">${reasoningTokens > 0 ? fmtNum(reasoningTokens) : "—"}</td>
        <td style="font-size:12px;text-align:center">${effortPill}</td>
        <td class="mono" style="font-size:12px" title="首字延迟 TTFT">${esc(fmtLatency(ttftMs))}</td>
        <td class="mono" style="font-size:12px" title="请求完成总耗时">${esc(fmtLatency(doneMs))}</td>
        <td>${okPill}</td>
      </tr>`;
    }).join("");
  } catch (e) {
    if (seq !== usageEventsLoadSeq) return;
    console.warn("loadUsageEvents", e);
    $("usage-events-tbody").innerHTML =
      `<tr><td colspan="14" class="g2a-muted">加载失败：${esc((e && e.message) || e)}</td></tr>`;
    if ($("usage-events-info")) $("usage-events-info").textContent = "加载失败";
    toast((e && e.message) || "加载使用明细失败", false);
  } finally {
    if (seq === usageEventsLoadSeq) usageEventsLoading = false;
  }
}

function renderUsageBars(series) {
  const host = $("usage-series");
  if (!host) return;
  const rows = Array.isArray(series) ? series : [];
  if (!rows.length) {
    host.innerHTML = '<div class="g2a-muted">暂无数据</div>';
    return;
  }
  const maxTok = Math.max(1, ...rows.map((r) => Number(r.total_tokens || 0)));
  host.innerHTML = rows.map((r) => {
    const tok = Number(r.total_tokens || 0);
    const req = Number(r.requests || 0);
    const h = Math.max(4, Math.round((tok / maxTok) * 100));
    return `<div class="g2a-usage-bar" title="${esc(r.day)} · ${fmtNum(tok)} tok · ${req} 请求">
      <div class="g2a-usage-bar-fill" style="height:${h}%"></div>
      <div class="g2a-usage-bar-label">${esc(String(r.day || "").slice(5))}</div>
      <div class="g2a-usage-bar-val">${fmtNum(tok)}</div>
    </div>`;
  }).join("");
}

function renderUsageTable(tbodyId, items, kind) {
  const tb = $(tbodyId);
  if (!tb) return;
  const list = Array.isArray(items) ? items : [];
  if (!list.length) {
    tb.innerHTML = `<tr><td colspan="${kind === "account" ? 6 : 4}" class="g2a-muted">暂无数据</td></tr>`;
    return;
  }
  if (kind === "account") {
    tb.innerHTML = list.map((it) => {
      const label = it.email || it.id || "—";
      const rate = it.success_rate != null ? (it.success_rate + "%") : "—";
      return `<tr>
        <td><div class="mono">${esc(label)}</div><div class="g2a-muted" style="font-size:11px">${esc(it.id || "")}</div></td>
        <td>${fmtNum(it.requests)}</td>
        <td>${fmtNum(it.success)}</td>
        <td>${fmtNum(it.fail)}</td>
        <td class="mono">${fmtNum(it.total_tokens)}</td>
        <td>${esc(rate)}</td>
      </tr>`;
    }).join("");
    return;
  }
  tb.innerHTML = list.map((it) => {
    const label = kind === "key"
      ? ((it.name || it.prefix || it.id || "—") + (it.prefix ? ` · ${it.prefix}` : ""))
      : (it.id || "—");
    const rate = it.success_rate != null ? (it.success_rate + "%") : "—";
    return `<tr>
      <td class="mono">${esc(label)}</td>
      <td>${fmtNum(it.requests)}</td>
      <td class="mono">${fmtNum(it.total_tokens)}</td>
      <td>${esc(rate)}</td>
    </tr>`;
  }).join("");
}

async function loadUsage() {
  if (usageLoading) return;
  usageLoading = true;
  try {
    const daysEl = $("usage-days");
    if (daysEl) usageDays = Number(daysEl.value || usageDays) || 7;
    const [sum, byKey, byModel, byAcc] = await Promise.all([
      api("/usage/summary?days=" + encodeURIComponent(usageDays)),
      api("/usage/by-key?days=" + encodeURIComponent(usageDays) + "&limit=30"),
      api("/usage/by-model?days=" + encodeURIComponent(usageDays) + "&limit=30"),
      api("/usage/by-account?days=" + encodeURIComponent(usageDays) + "&limit=30"),
    ]);
    const today = (sum && sum.today) || {};
    const window = (sum && sum.window) || {};
    const life = (sum && sum.lifetime) || {};
    const cache = (sum && sum.cache) || {};
    const cacheToday = cache.today || {};
    const cacheWin = cache.window || {};
    const cacheLife = cache.lifetime || {};
    const fmtRatio = (v) => (v == null || v === "" ? "—" : `${v}%`);
    const grid = $("usage-stats-grid");
    if (grid) {
      grid.innerHTML = `
        <div class="stat"><div class="label">今日请求</div><div class="value">${fmtNum(today.requests)}</div>
          <div class="sub">成功 ${fmtNum(today.success)} · 失败 ${fmtNum(today.fail)}${today.success_rate != null ? ` · ${today.success_rate}%` : ""}</div></div>
        <div class="stat"><div class="label">今日 token</div><div class="value mono">${fmtNum(today.total_tokens)}</div>
          <div class="sub">输入 ${fmtNum(today.prompt_tokens)} · 输出 ${fmtNum(today.completion_tokens)}</div></div>
        <div class="stat"><div class="label">今日缓存命中</div><div class="value mono">${fmtRatio(cacheToday.token_hit_ratio)}</div>
          <div class="sub">读 ${fmtNum(cacheToday.cache_read_tokens || 0)} / 输入 ${fmtNum(cacheToday.prompt_tokens || 0)} · 请求命中 ${fmtRatio(cacheToday.request_hit_ratio)}</div></div>
        <div class="stat"><div class="label">近 ${usageDays} 天 token</div><div class="value mono">${fmtNum(window.total_tokens)}</div>
          <div class="sub">请求 ${fmtNum(window.requests)}${window.success_rate != null ? ` · 成功率 ${window.success_rate}%` : ""}</div></div>
        <div class="stat"><div class="label">近 ${usageDays} 天缓存命中</div><div class="value mono">${fmtRatio(cacheWin.token_hit_ratio)}</div>
          <div class="sub">读 ${fmtNum(cacheWin.cache_read_tokens || 0)} / 输入 ${fmtNum(cacheWin.prompt_tokens || 0)} · 请求命中 ${fmtRatio(cacheWin.request_hit_ratio)}</div></div>
        <div class="stat"><div class="label">累计 token</div><div class="value mono">${fmtNum(life.total_tokens)}</div>
          <div class="sub">请求 ${fmtNum(life.requests)} · 累计缓存读 ${fmtNum(cacheLife.cache_read_tokens || 0)} · 源 ${esc((sum && sum.source) || "—")}</div></div>
      `;
    }
    if ($("usage-source")) {
      $("usage-source").textContent = "数据源: " + ((sum && sum.source) || "none") +
        " · 缓存命中来自 usage_events（token 命中率 = cache_read / prompt）" +
        " · UTC 日切 · 失败请求不计 token" +
        (cache.source ? ` · cache源 ${cache.source}` : "");
    }
    renderUsageBars((sum && sum.series) || []);
    renderUsageTable("usage-by-key-tbody", (byKey && byKey.items) || [], "key");
    renderUsageTable("usage-by-model-tbody", (byModel && byModel.items) || [], "model");
    renderUsageTable("usage-by-account-tbody", (byAcc && byAcc.items) || [], "account");
    try { await loadUsageEvents({ reset: true }); } catch (_) {}
  } catch (e) {
    console.warn("loadUsage", e);
    toast((e && e.message) || "加载用量失败", false);
  } finally {
    usageLoading = false;
  }
}

/* ── Admin task logs ────────────────────────────────── */
let logsPage = 1;
let logsPageSize = 50;
let logsTotalPages = 1;
let logsLoading = false;
let logsLoadSeq = 0;
// Keep selected row detail across soft-nav / refresh so "refresh" doesn't blank the panel.
let logsSelectedId = null;
let logsDetailCache = Object.create(null);

function taskStatusTag(status, ok) {
  const st = String(status || "").toLowerCase();
  if (st === "error" || st === "failed" || ok === false) {
    return '<span class="g2a-tag bad">失败</span>';
  }
  if (st === "partial") return '<span class="g2a-tag warn">部分</span>';
  if (st === "cancelled" || st === "stopped") return '<span class="g2a-tag">取消</span>';
  if (st === "running" || st === "queued") return '<span class="g2a-tag">进行中</span>';
  return '<span class="g2a-tag ok">成功</span>';
}

function taskProgressText(it) {
  const done = Number(it.progress_done || 0) || 0;
  const total = Number(it.progress_total || 0) || 0;
  if (total > 0) return `${done}/${total}`;
  if (done > 0) return String(done);
  return "—";
}

function bindLogsControls() {
  on("btn-logs-search", "onclick", () => loadAdminLogs({ reset: true }));
  on("btn-logs-reload", "onclick", () => loadAdminLogs({ reset: false }));
  on("logs-page-prev", "onclick", () => {
    if (logsPage > 1 && !logsLoading) { logsPage -= 1; loadAdminLogs(); }
  });
  on("logs-page-next", "onclick", () => {
    if (!logsLoading && logsPage < logsTotalPages) { logsPage += 1; loadAdminLogs(); }
  });
  const q = $("logs-q");
  if (q && !q._logsBound) {
    q._logsBound = true;
    q.addEventListener("keydown", (e) => {
      if (e.key === "Enter") loadAdminLogs({ reset: true });
    });
  }
  const act = $("logs-action");
  if (act && !act._logsBound) {
    act._logsBound = true;
    act.addEventListener("change", () => loadAdminLogs({ reset: true }));
  }
  const st = $("logs-status");
  if (st && !st._logsBound) {
    st._logsBound = true;
    st.addEventListener("change", () => loadAdminLogs({ reset: true }));
  }
  const ps = $("logs-page-size");
  if (ps && !ps._logsBound) {
    ps._logsBound = true;
    ps.addEventListener("change", () => loadAdminLogs({ reset: true }));
  }
  const tb = $("logs-tbody");
  if (tb && !tb._logsBound) {
    tb._logsBound = true;
    tb.addEventListener("click", (e) => {
      const tr = e.target.closest("tr[data-log-id], tr[data-log-detail]");
      if (!tr) return;
      // Prefer in-memory cache (survives soft-nav/rebind). Attribute is fallback.
      let detail = null;
      const id = tr.getAttribute("data-log-id");
      if (id && logsDetailCache && Object.prototype.hasOwnProperty.call(logsDetailCache, id)) {
        detail = logsDetailCache[id];
      } else {
        try {
          detail = JSON.parse(tr.getAttribute("data-log-detail") || "{}");
        } catch (_) {
          detail = { raw: tr.getAttribute("data-log-detail") || "" };
        }
      }
      try {
        setLogPanel("logs-detail", JSON.stringify(detail || {}, null, 2), { forceShow: true });
        // Remember selected row so refresh can re-show the same detail.
        logsSelectedId = id || null;
        if (logsSelectedId) {
          try { sessionStorage.setItem("g2a_logs_selected_id", String(logsSelectedId)); } catch (_) {}
        }
      } catch (_) {}
    });
  }
}

async function ensureLogActions() {
  const sel = $("logs-action");
  if (!sel || sel.options.length > 1) return;
  try {
    const r = await api("/logs/actions");
    const actions = (r && (r.kinds || r.actions)) || [];
    actions.forEach((a) => {
      const opt = document.createElement("option");
      opt.value = a;
      opt.textContent = a;
      sel.appendChild(opt);
    });
  } catch (_) {}
}

async function loadAdminLogs({ reset = false } = {}) {
  if (!$("logs-tbody")) return;
  bindLogsControls();
  await ensureLogActions();
  if (reset) logsPage = 1;
  // Restore last selected row after hard refresh / soft-nav.
  if (!logsSelectedId) {
    try { logsSelectedId = sessionStorage.getItem("g2a_logs_selected_id") || null; } catch (_) {}
  }
  logsLoading = true;
  const seq = ++logsLoadSeq;
  const q = ($("logs-q") && $("logs-q").value || "").trim();
  const action = ($("logs-action") && $("logs-action").value) || "all";
  const status = ($("logs-status") && $("logs-status").value) || "all";
  logsPageSize = parseInt(($("logs-page-size") && $("logs-page-size").value) || "50", 10) || 50;
  $("logs-tbody").innerHTML = `<tr><td colspan="6" class="g2a-muted">加载任务日志中…</td></tr>`;
  if ($("logs-info")) $("logs-info").textContent = "查询中…";
  try {
    const data = await api(
      `/logs?page=${encodeURIComponent(logsPage)}&page_size=${encodeURIComponent(logsPageSize)}&q=${encodeURIComponent(q)}&kind=${encodeURIComponent(action)}&action=${encodeURIComponent(action)}&status=${encodeURIComponent(status)}`
    );
    if (seq !== logsLoadSeq) return;
    const items = (data && data.items) || [];
    logsTotalPages = Number(data.total_pages || 1) || 1;
    logsPage = Number(data.page || logsPage) || 1;
    if ($("logs-info")) {
      $("logs-info").textContent = `共 ${data.total ?? items.length} 条任务 · 数据源 ${data.store_source || "postgres"} · 点击行查看详情`;
    }
    if ($("logs-page-info")) {
      $("logs-page-info").textContent = `${logsPage} / ${logsTotalPages}`;
    }
    if ($("logs-page-prev")) $("logs-page-prev").disabled = logsPage <= 1;
    if ($("logs-page-next")) $("logs-page-next").disabled = logsPage >= logsTotalPages;
    // Rebuild detail cache for this page so clicks never depend on fragile HTML attrs.
    logsDetailCache = Object.create(null);
    if (!items.length) {
      $("logs-tbody").innerHTML = `<tr><td colspan="6" class="g2a-muted">暂无任务日志</td></tr>`;
      // Keep previously shown detail if user only filtered to empty page.
    } else {
      $("logs-tbody").innerHTML = items.map((it, idx) => {
        const rowId = String(it.id != null ? it.id : (it.task_id || `row-${idx}`));
        const payload = {
          id: it.id != null ? it.id : null,
          created_at: it.created_at || null,
          updated_at: it.updated_at || null,
          finished_at: it.finished_at || null,
          task_id: it.task_id || null,
          kind: it.kind || it.action || null,
          status: it.status || null,
          summary: it.summary || null,
          ok: it.ok,
          progress_done: it.progress_done,
          progress_total: it.progress_total,
          detail: it.detail || {},
        };
        logsDetailCache[rowId] = payload;
        const kind = it.kind || it.action || "—";
        const st = it.status || "—";
        const selected = logsSelectedId && String(logsSelectedId) === rowId;
        return `<tr data-log-id="${esc(rowId)}" style="cursor:pointer${selected ? ";outline:1px solid var(--g2a-primary, #4f8cff)" : ""}">
          <td class="g2a-muted">${esc(fmtTime(it.created_at))}</td>
          <td class="mono">${esc(kind)}</td>
          <td class="mono g2a-muted">${esc(st)}</td>
          <td>${esc(it.summary || "—")}</td>
          <td class="mono g2a-muted">${esc(taskProgressText(it))}</td>
          <td>${taskStatusTag(st, it.ok)}</td>
        </tr>`;
      }).join("");
      // Re-show selected detail after refresh so the panel doesn't go blank.
      if (logsSelectedId && logsDetailCache[logsSelectedId]) {
        setLogPanel(
          "logs-detail",
          JSON.stringify(logsDetailCache[logsSelectedId], null, 2),
          { forceShow: true }
        );
      }
    }
  } catch (e) {
    if (seq !== logsLoadSeq) return;
    $("logs-tbody").innerHTML = `<tr><td colspan="6" class="g2a-muted">加载失败：${esc(e.message || e)}</td></tr>`;
    toast(e.message || "加载任务日志失败", false);
  } finally {
    if (seq === logsLoadSeq) logsLoading = false;
  }
}


window.G2AAdmin = { bootstrap, loadDashboard, api, $, toast, PAGE_META, renderAccounts, renderKeys };
  if (document.body && document.body.dataset.page) {
    const _boot = () => {
      try { if (document.body.dataset.page === "accounts") renderAccountStatusChips(); } catch (_) {}
      bootstrap();
    };
    if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", _boot);
    else _boot();
  }
})();
/* g2a-cache-bust-20260715-reg-restore-fix */

