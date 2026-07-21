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
  // Adaptive registration poll cadence + round-robin live session refresh.
  let regPollIntervalMs = 180;
  let regPollLiveCursor = 0;
  let regPollLastDurationMs = 0;
  // Hard abort per admin→Go request. Keep under Go→sidecar budget so ticks stay snappy.
  // Shared Go client timeout is ~750ms; browser abort slightly above that.
  const REG_POLL_TIMEOUT_MS = 900;
  // Deep session GETs are expensive; batch embed already carries log_lines.
  // Only re-fetch 1 live session when embed has no timeline yet.
  const REG_POLL_LIVE_REFRESH = 1;
  // Survive hard refresh: remember which batch/sessions the UI was tracking.
  const REG_TRACK_KEY = "g2a_reg_track_v1";
  let keysCache = {};
  let quotaCache = {};
let quotaLiveTimer = null;
let quotaLiveInFlight = false;
// Auto quota: prefer DB hydrate; live-probe only missing / very-stale rows.
// After last_quota is durable, aggressive re-probe just burns upstream connections.
const QUOTA_LIVE_INTERVAL_MS = 180000; // 3 min between ticks (was 45s)
const QUOTA_LIVE_MAX_PER_TICK = 2;     // at most 2 accounts per tick (was 4)
const QUOTA_STALE_SEC = 900;           // re-probe only if older than 15 min (was 3 min)
const QUOTA_MIN_REQUERY_SEC = 600;     // hard floor: never re-hit same id within 10 min (anti-duplicate)
const QUOTA_MISSING_BOOST = 200;       // priority score for accounts without quota
const QUOTA_FULL_POOL_ON_BUTTON = false; // 查全部额度默认只刷本页，避免 7k 连接风暴
// Per-account last live-probe wall time (sec). Survives within the page session.
const quotaLiveProbedAt = Object.create(null);
const QUOTA_CACHE_LS_KEY = "g2a_quota_cache_v1";
const QUOTA_CACHE_LS_MAX = 400; // bound localStorage size

function loadQuotaCacheFromStorage() {
  try {
    const raw = localStorage.getItem(QUOTA_CACHE_LS_KEY);
    if (!raw) return;
    const obj = JSON.parse(raw);
    if (!obj || typeof obj !== "object") return;
    let n = 0;
    for (const [k, v] of Object.entries(obj)) {
      if (!v || typeof v !== "object") continue;
      if (typeof hasQuotaInfo === "function" && !hasQuotaInfo(v)) continue;
      const id = accountIdKey(k);
      if (!id) continue;
      // Only seed if memory empty — live/API still wins later.
      if (!quotaCache[id] || !hasQuotaInfo(quotaCache[id])) {
        quotaCache[id] = { ...v, account_id: id, cached: true, from_storage: true };
        n++;
      }
    }
    if (n) console.info("[quota] restored", n, "snaps from localStorage");
  } catch (e) {
    console.warn("loadQuotaCacheFromStorage", e);
  }
}

function saveQuotaCacheToStorage() {
  try {
    const entries = [];
    for (const [k, v] of Object.entries(quotaCache || {})) {
      if (!v || typeof v !== "object" || v.probing) continue;
      if (typeof hasQuotaInfo === "function" && !hasQuotaInfo(v)) continue;
      const id = accountIdKey(k);
      if (!id) continue;
      const ts = quotaSnapTs(v) || 0;
      entries.push([id, v, ts]);
    }
    // Keep most recently fetched.
    entries.sort((a, b) => b[2] - a[2]);
    const out = {};
    for (const [id, v] of entries.slice(0, QUOTA_CACHE_LS_MAX)) {
      // Compact: drop huge nested noise.
      const slim = {
        ok: v.ok,
        account_id: id,
        account_type: v.account_type || v.plan,
        plan: v.plan || v.account_type,
        plan_label: v.plan_label,
        free_tokens: v.free_tokens,
        tokens_limit: v.tokens_limit,
        tokens_used: v.tokens_used,
        tokens_remaining: v.tokens_remaining,
        tokens_actual: v.tokens_actual,
        tokens_usage_percent: v.tokens_usage_percent,
        monthly_limit: v.monthly_limit,
        used: v.used,
        remaining: v.remaining,
        usage_percent: v.usage_percent,
        weekly_limit: v.weekly_limit,
        weekly_used: v.weekly_used,
        weekly_remaining: v.weekly_remaining,
        summary: v.summary,
        display: v.display,
        exhausted: v.exhausted,
        auto_disabled: v.auto_disabled,
        source: v.source,
        fetched_at: v.fetched_at,
      };
      out[id] = slim;
    }
    localStorage.setItem(QUOTA_CACHE_LS_KEY, JSON.stringify(out));
  } catch (e) {
    // Quota exceeded etc. — ignore.
  }
}

// Seed early (functions below are hoisted for function decls; accountIdKey/hasQuotaInfo exist later).
// Actual load deferred until first accounts paint via ensureQuotaCacheHydrated().
let _quotaStorageLoaded = false;
function ensureQuotaCacheHydrated() {
  if (_quotaStorageLoaded) return;
  _quotaStorageLoaded = true;
  try { loadQuotaCacheFromStorage(); } catch (_) {}
}
  let uiRefreshTimer = null;
  let accountsList = [];
  let accountsPage = 1;
  let accountsTotal = 0;
  let accountsTotalPages = 1;
  let accountsLoading = false;
  let accountsLoadSeq = 0;
  // Wall-clock when the current full list load began — used by the stuck-loading watchdog.
  let accountsLoadingSince = 0;
  let accountsPageSize = 25;
  let accountsSearchQuery = "";
  let accountsSort = "newest";
  // "" | "1" | "0" — server-side has_sso filter
  let accountsSsoFilter = "";
  // "" | live|cooldown|disabled|quota_disabled|model_blocked|expired
  // Restore last filter so "冷却中" view survives soft-nav / refresh.
  let accountsStatusFilter = "";
  try {
    const savedSt = localStorage.getItem("g2a_accounts_status_filter");
    if (savedSt === "live" || savedSt === "cooldown" || savedSt === "model_blocked"
      || savedSt === "quota_disabled" || savedSt === "disabled" || savedSt === "expired"
      || savedSt === "") {
      accountsStatusFilter = savedSt || "";
    }
  } catch (_) {}
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
  // Avoid rewriting identical registration logs (stop/poll re-render flicker).
  // Still scroll to bottom when the panel is open so new lines stay in view.
  if (id === "reg-log") {
    if (next === regLastLogText && !el.classList.contains("hidden")) {
      try { el.scrollTop = el.scrollHeight; } catch (_) {}
      return;
    }
    regLastLogText = next;
  }
  if (el.textContent === next && !el.classList.contains("hidden")) {
    el.classList.remove("is-empty", "hidden");
    el.hidden = false;
    if (id === "reg-log") {
      try { el.scrollTop = el.scrollHeight; } catch (_) {}
    }
    return;
  }
  el.textContent = next;
  if (id === "reg-log") {
    try { el.scrollTop = el.scrollHeight; } catch (_) {}
  }
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
  usage: { title: "用量", sub: "Token 消耗与请求使用情况（今日按上海时间日切 / 近 N 天 / 累计）" },
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
    try {
      if (opts.hash) setPageSection(opts.hash, { replace: !!opts.replace });
      else initPageSubnav({ replace: true });
    } catch (_) {}
    return true;
  }
  // Keep shell painted. NEVER full-document navigate for admin pages (that causes black flash).
  if (_softNavBusy) {
    // A previous nav is stuck/in-flight: don't queue forever; unlock stale locks.
    // Mobile: 8s was too long — a hung /usage fetch left the chip bar looking "dead".
    if (_softNavBusySince && (Date.now() - _softNavBusySince) > 3500) {
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
  // Mobile networks hang more often; 5s keeps 用量/日志 chips recoverable.
  const busyTimer = setTimeout(() => {
    if (my === _softNavToken) {
      try { clearSoftNavBusy("timeout"); } catch (_) {}
      try { toast("页面切换超时，已恢复界面", false); } catch (_) {}
    }
  }, 5000);
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
    try { buildMobileNav(); } catch (_) {}
    applyPageMeta(page);
    const hashFromOpts = opts.hash ? ("#" + normalizeSectionHash(opts.hash).replace(/^#/, "")) : "";
    const nextUrl = href + (hashFromOpts || "");
    if (!opts.replace) history.pushState({ g2aPage: page }, "", nextUrl);
    else history.replaceState({ g2aPage: page }, "", nextUrl);

    try {
      if (typeof rebindPageControls === "function") rebindPageControls();
      try { if (window.G2A && G2A.bindThemeToggle) G2A.bindThemeToggle(document); } catch(_){}
      // Non-blocking data load so menu clicks feel instant.
      // Models page: load dedicated catalog first (do not rely on /dashboard).
      if (page === "models" && typeof loadModels === "function") {
        Promise.resolve(loadModels()).then(() => {
          try { return refreshModelHealthStatus(); } catch (_) {}
        }).then(() => {
          try { if (typeof startUpstreamMonitor === "function") startUpstreamMonitor({ force: true }); } catch (_) {}
        }).catch((e) => console.warn("soft nav loadModels", e));
      } else if (page === "accounts") {
        // Single list load only — never also call loadDashboard here (double /accounts
        // races accountsLoadSeq and can leave the table stuck on "加载账号中…").
        // After soft-nav DOM swap the tbody is empty even if accountsList still has
        // rows from a prior visit — paint cache first, then silent re-sync.
        if (typeof loadAccountsPage === "function") {
          try {
            if (accountsList && accountsList.length && typeof renderAccountsPage === "function") {
              renderAccountsPage();
            }
          } catch (_) {}
          const tbody = $("accounts-tbody");
          const painted = !!(tbody && tbody.querySelector("tr[data-acc-id]"));
          Promise.resolve(loadAccountsPage({ reset: false, silent: painted }))
            .then(() => { try { bindAccountsPagerControls(); } catch (_) {} })
            .catch((e) => console.warn("soft nav loadAccountsPage", e));
        }
        // Registration form is inside .g2a-content — rebind + repaint email panels
        // so switching MoeMail/YYDS/GPTMail/CF updates fields without hard refresh.
        try { bindRegMailFormControls(); } catch (_) {}
        try {
          if (typeof loadRegConfig === "function") {
            // Soft-nav: do not hard-force if we just saved (avoids flash-restore).
            const recent = regConfigCache && (Date.now() - regConfigLoadedAt) < 5000;
            loadRegConfig(!recent).catch(() => {
              try { paintRegMailFieldsToInput(); syncRegMailProviderUI(); } catch (_) {}
            });
          } else {
            paintRegMailFieldsToInput();
            syncRegMailProviderUI();
          }
        } catch (_) {}
      } else if (typeof loadDashboard === "function") {
        Promise.resolve(loadDashboard()).catch((e) => console.warn("soft nav loadDashboard", e));
      }
    } catch (e) {
      console.warn("soft nav loadDashboard", e);
    }
    if (page === "overview" || page === "accounts") {
      try { startAutoUiRefresh(); } catch (_) {}
    } else {
      try { if (uiRefreshTimer) { clearInterval(uiRefreshTimer); uiRefreshTimer = null; } } catch (_) {}
    }
    // Task logs auto-poll only while that page is visible.
    if (page === "logs") {
      try { if (typeof startLogsAutoRefresh === "function") startLogsAutoRefresh(); } catch (_) {}
    } else {
      try { if (typeof stopLogsAutoRefresh === "function") stopLogsAutoRefresh(); } catch (_) {}
    }
    // Upstream live monitor only while models page is visible.
    if (page === "models") {
      try { if (typeof startUpstreamMonitor === "function") startUpstreamMonitor({ force: true }); } catch (_) {}
    } else {
      try { if (typeof stopUpstreamMonitor === "function") stopUpstreamMonitor(); } catch (_) {}
    try {
      if (page !== "accounts" && typeof stopQuotaLiveRefresh === "function") stopQuotaLiveRefresh();
      if (page === "accounts" && typeof startQuotaLiveRefresh === "function") startQuotaLiveRefresh({ immediate: false });
    } catch (_) {}
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
      // accounts: list load kicked off above (single path). Do not start a second
      // /accounts fetch here — seq race left the table stuck on "加载中".
      if (page === "keys" && typeof renderKeys === "function") renderKeys();
      if (page === "logs" && typeof loadAdminLogs === "function") {
        const hasRows = !!($("logs-tbody") && $("logs-tbody").querySelector("tr[data-log-id]"));
        loadAdminLogs({ reset: false, soft: hasRows });
      }
      if (page === "usage" && typeof loadUsage === "function") {
        // Debounce: soft-nav + showMain both used to fire loadUsage → table double-flash.
        const now = Date.now();
        if (!window.__g2aUsageLoadAt || now - window.__g2aUsageLoadAt > 1500) {
          window.__g2aUsageLoadAt = now;
          loadUsage();
        }
      }
      // models: loadModels already kicked off above — only paint if cache is warm.
      if (page === "models" && typeof renderModels === "function") {
        try { renderModels(); } catch (_) {}
        try { renderModelHealthInfo(); } catch (_) {}
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


function syncAccountsPagerControls() {
  const totalPages = Math.max(1, Number(accountsTotalPages) || 1);
  const page = Math.max(1, Math.min(Number(accountsPage) || 1, totalPages));
  accountsPage = page;
  accountsTotalPages = totalPages;
  if ($("acc-page-prev")) {
    $("acc-page-prev").disabled = page <= 1;
  }
  if ($("acc-page-next")) {
    $("acc-page-next").disabled = page >= totalPages;
  }
  if ($("acc-page-size") && String($("acc-page-size").value) !== String(accountsPageSize || 25)) {
    try { $("acc-page-size").value = String(accountsPageSize || 25); } catch (_) {}
  }
  if ($("acc-page-info")) {
    const n = (accountsList && accountsList.length) || 0;
    const src = (window.__g2aAccountsStore && window.__g2aAccountsStore.source) || "";
    const srcTxt = src === "postgres" ? " · 数据库" : (src ? ` · ${src}` : "");
    $("acc-page-info").textContent =
      `${page} / ${totalPages} (本页 ${n} / 共 ${accountsTotal || 0} 个${srcTxt})`;
  }
}

/** Go to accounts list page. Always works even if a previous load is in-flight. */
function goAccountsPage(nextPage, { reset = false } = {}) {
  const totalPages = Math.max(1, Number(accountsTotalPages) || 1);
  let p = Number(nextPage);
  if (!Number.isFinite(p) || p < 1) p = 1;
  if (p > totalPages) p = totalPages;
  // Invalidate any in-flight load so the new page request wins accountsLoadSeq.
  if (accountsLoading) {
    try { accountsLoadSeq++; } catch (_) {}
    accountsLoading = false;
    accountsLoadingSince = 0;
  }
  accountsPage = p;
  return loadAccountsPage({ reset: !!reset, silent: false });
}

function bindAccountsPagerControls() {
  // Event delegation survives soft-nav DOM swaps for the pager region.
  // Also bind property handlers for first paint.
  const bindOne = () => {
    on("acc-page-prev", "onclick", (e) => {
      try { if (e && e.preventDefault) e.preventDefault(); } catch (_) {}
      if (accountsPage <= 1) return;
      goAccountsPage(accountsPage - 1);
    });
    on("acc-page-next", "onclick", (e) => {
      try { if (e && e.preventDefault) e.preventDefault(); } catch (_) {}
      const totalPages = Math.max(1, Number(accountsTotalPages) || 1);
      if (accountsPage >= totalPages) return;
      goAccountsPage(accountsPage + 1);
    });
    on("acc-page-size", "onchange", () => {
      accountsPageSize = parseInt(($("acc-page-size") && $("acc-page-size").value) || "25", 10) || 25;
      try { localStorage.setItem("g2a_accounts_page_size", String(accountsPageSize)); } catch (_) {}
      goAccountsPage(1, { reset: true });
    });
    // Restore page size preference.
    try {
      const saved = localStorage.getItem("g2a_accounts_page_size");
      if (saved && $("acc-page-size")) {
        const n = parseInt(saved, 10);
        if (n === 10 || n === 25 || n === 50 || n === 100) {
          accountsPageSize = n;
          $("acc-page-size").value = String(n);
        }
      }
    } catch (_) {}
    syncAccountsPagerControls();
  };
  bindOne();
  // Document-level backup: soft-nav may replace buttons before rebind runs.
  if (!document._g2aAccPagerBound) {
    document._g2aAccPagerBound = true;
    document.addEventListener("click", (e) => {
      const t = e && e.target;
      if (!t || !t.closest) return;
      const prev = t.closest("#acc-page-prev");
      const next = t.closest("#acc-page-next");
      if (!prev && !next) return;
      // Only on accounts page
      const page = (document.body && document.body.dataset.page) || "";
      if (page !== "accounts") return;
      e.preventDefault();
      if (prev) {
        if (accountsPage <= 1) return;
        goAccountsPage(accountsPage - 1);
      } else if (next) {
        const totalPages = Math.max(1, Number(accountsTotalPages) || 1);
        if (accountsPage >= totalPages) return;
        goAccountsPage(accountsPage + 1);
      }
    }, true);
    document.addEventListener("change", (e) => {
      const t = e && e.target;
      if (!t || t.id !== "acc-page-size") return;
      const page = (document.body && document.body.dataset.page) || "";
      if (page !== "accounts") return;
      accountsPageSize = parseInt(t.value || "25", 10) || 25;
      try { localStorage.setItem("g2a_accounts_page_size", String(accountsPageSize)); } catch (_) {}
      goAccountsPage(1, { reset: true });
    }, true);
  }
}


/* ── Page sub-navigation (settings / accounts) ───────── */
function normalizeSectionHash(raw) {
  return String(raw || "").replace(/^#/, "").trim().toLowerCase();
}

function pageSubnavRoot() {
  return document.querySelector(".g2a-sublayout[data-page-subnav]");
}

function listPageSections(root) {
  root = root || pageSubnavRoot();
  if (!root) return [];
  return Array.from(root.querySelectorAll(".g2a-subpanel[data-section]"))
    .map((el) => el.getAttribute("data-section") || "")
    .filter(Boolean);
}

function resolvePageSection(section, root) {
  root = root || pageSubnavRoot();
  if (!root) return "";
  const sections = listPageSections(root);
  if (!sections.length) return "";
  const fallback = root.getAttribute("data-default-section") || sections[0];
  const key = normalizeSectionHash(section);
  if (key && sections.includes(key)) return key;
  return fallback;
}

function setPageSection(section, opts) {
  opts = opts || {};
  const root = pageSubnavRoot();
  if (!root) return "";
  const key = resolvePageSection(section, root);
  if (!key) return "";

  root.querySelectorAll(".g2a-subpanel[data-section]").forEach((panel) => {
    const on = (panel.getAttribute("data-section") || "") === key;
    panel.classList.toggle("is-active", on);
    if (on) panel.removeAttribute("hidden");
    else panel.setAttribute("hidden", "");
  });
  root.querySelectorAll(".g2a-subnav-item[data-section]").forEach((btn) => {
    const on = (btn.getAttribute("data-section") || "") === key;
    btn.classList.toggle("is-active", on);
    if (on) btn.setAttribute("aria-current", "page");
    else btn.removeAttribute("aria-current");
  });

  if (!opts.skipHistory) {
    const url = location.pathname + location.search + "#" + key;
    try {
      if (opts.replace) history.replaceState(history.state || { g2aPage: document.body.dataset.page }, "", url);
      else if (normalizeSectionHash(location.hash) !== key) history.pushState(history.state || { g2aPage: document.body.dataset.page }, "", url);
      else history.replaceState(history.state || { g2aPage: document.body.dataset.page }, "", url);
    } catch (_) {}
  }
  root.dataset.activeSection = key;
  return key;
}

function initPageSubnav(opts) {
  opts = opts || {};
  const root = pageSubnavRoot();
  if (!root) return "";
  if (!root.dataset.subnavBound) {
    root.dataset.subnavBound = "1";
    root.addEventListener("click", (e) => {
      const btn = e.target && e.target.closest ? e.target.closest(".g2a-subnav-item[data-section]") : null;
      if (!btn || !root.contains(btn)) return;
      e.preventDefault();
      setPageSection(btn.getAttribute("data-section") || "", { replace: false });
    });
  }
  const fromHash = normalizeSectionHash(location.hash);
  return setPageSection(fromHash, { replace: true, skipHistory: !fromHash });
}

function rebindPageControls() {
  try { initPageSubnav({ replace: true }); } catch (_) {}
  try { bindKeysControls(); } catch (_) {}
  try { bindVersionSettingsUI(); } catch (_) {}
  try { bindModelsControls(); } catch (_) {}
  try { bindUpstreamMonitorControls(); } catch (_) {}
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
      try { if (typeof startQuotaLiveRefresh === "function") startQuotaLiveRefresh({ immediate: true }); } catch (_) {}
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
        try { await refreshModelHealthStatus(); } catch (_) {}
        try { await refreshUpstreamStatus({ force: true }); } catch (_) {}
        toast(`已刷新模型列表（${(list || []).length} 个）`);
      } else if (page === "accounts" && typeof refreshAccountsListUI === "function") {
        await refreshAccountsListUI({ toastOk: "已刷新账号列表", force: true });
      } else if (page === "logs" && typeof loadAdminLogs === "function") {
        // Manual header refresh: keep rows visible while revalidating.
        const hasRows = !!($("logs-tbody") && $("logs-tbody").querySelector("tr[data-log-id]"));
        await loadAdminLogs({ reset: false, soft: hasRows });
        toast("已刷新任务日志");
      } else if (page === "usage" && typeof loadUsage === "function") {
        await loadUsage();
        toast("已刷新用量");
      } else if (page === "keys" && typeof renderKeys === "function") {
        await renderKeys();
        toast("已刷新 API Keys");
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
      const page = document.body.dataset.page || pageFromPath(location.pathname) || "";
      // Keys page reuses #btn-refresh-all for list reload — do not replace with dashboard load.
      if (page === "keys" && typeof renderKeys === "function") {
        await renderKeys();
        toast("已刷新 Key 列表");
        return;
      }
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
  if ($("chk-quota-live") && !$("chk-quota-live")._g2aBound) {
    $("chk-quota-live")._g2aBound = true;
    try {
      const saved = localStorage.getItem("g2a_quota_live");
      if (saved === "0") $("chk-quota-live").checked = false;
      else $("chk-quota-live").checked = true;
    } catch (_) {}
    $("chk-quota-live").onchange = () => {
      try { localStorage.setItem("g2a_quota_live", $("chk-quota-live").checked ? "1" : "0"); } catch (_) {}
      if ($("chk-quota-live").checked) startQuotaLiveRefresh({ immediate: true });
      else stopQuotaLiveRefresh();
    };
  }
  bindProbe("btn-probe-all");
  bindProbe("btn-probe-all-2");
  bindProbe("btn-probe-all-models");
  try { bindModelsControls(); } catch (_) {}
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

  // Keys (create / copy / toggle / delete)
  try { bindKeysControls(); } catch (_) {}

  // Models / accounts common
  try { bindModelsControls(); } catch (_) {}
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
  // Per-card save / reset (only that section).
  on("btn-save-settings-pool", "onclick", async () => {
    try { await saveSettingsCard("pool", "btn-save-settings-pool", "轮询与维护"); }
    catch (e) { toast(e.message || "保存失败", false); }
  });
  on("btn-reset-settings-pool", "onclick", async () => {
    try { await resetSettingsCard("pool", "btn-reset-settings-pool", "轮询与维护"); }
    catch (e) { toast(e.message || "重置失败", false); }
  });
  on("btn-save-settings-proxy", "onclick", async () => {
    try { await saveSettingsCard("proxy", "btn-save-settings-proxy", "出站代理池"); }
    catch (e) { toast(e.message || "保存失败", false); }
  });
  on("btn-reset-settings-proxy", "onclick", async () => {
    try { await resetSettingsCard("proxy", "btn-reset-settings-proxy", "出站代理池"); }
    catch (e) { toast(e.message || "重置失败", false); }
  });
  on("btn-save-settings-relay", "onclick", async () => {
    try { await saveSettingsCard("relay", "btn-save-settings-relay", "Relay 参数"); }
    catch (e) { toast(e.message || "保存失败", false); }
  });
  on("btn-reset-settings-relay", "onclick", async () => {
    try { await resetSettingsCard("relay", "btn-reset-settings-relay", "Relay 参数"); }
    catch (e) { toast(e.message || "重置失败", false); }
  });
  on("btn-save-settings-cooldown", "onclick", async () => {
    try { await saveSettingsCard("cooldown", "btn-save-settings-cooldown", "冷却策略"); }
    catch (e) { toast(e.message || "保存失败", false); }
  });
  on("btn-reset-settings-cooldown", "onclick", async () => {
    try { await resetSettingsCard("cooldown", "btn-reset-settings-cooldown", "冷却策略"); }
    catch (e) { toast(e.message || "重置失败", false); }
  });
  on("btn-save-settings-sub2api", "onclick", async () => {
    try { await saveSettingsCard("sub2api", "btn-save-settings-sub2api", "sub2api 导入"); }
    catch (e) { toast(e.message || "保存失败", false); }
  });
  on("btn-reset-settings-sub2api", "onclick", async () => {
    try { await resetSettingsCard("sub2api", "btn-reset-settings-sub2api", "sub2api 导入"); }
    catch (e) { toast(e.message || "重置失败", false); }
  });
  on("btn-save-settings-cliproxyapi", "onclick", async () => {
    try { await saveSettingsCard("cliproxyapi", "btn-save-settings-cliproxyapi", "CLIProxyAPI 导入"); }
    catch (e) { toast(e.message || "保存失败", false); }
  });
  on("btn-reset-settings-cliproxyapi", "onclick", async () => {
    try { await resetSettingsCard("cliproxyapi", "btn-reset-settings-cliproxyapi", "CLIProxyAPI 导入"); }
    catch (e) { toast(e.message || "重置失败", false); }
  });
  // Legacy global save if still present in old HTML cache.
  if ($("btn-save-settings")) {
    on("btn-save-settings", "onclick", async () => {
      try { await saveSystemSettings({ label: "全部设置" }); } catch (e) { toast(e.message || "保存失败", false); }
    });
  }
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
      await refreshAccountsListUI({ toastOk: "已热更新", force: true });
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
      if (saved && saved !== "cooldown_first" && saved !== "disabled_first") {
        accountsSort = saved;
        $("acc-sort").value = saved;
      } else {
        accountsSort = $("acc-sort").value || "newest";
      }
    } catch (_) {
      accountsSort = $("acc-sort").value || "newest";
    }
    $("acc-sort").onchange = () => {
      if (!accountsSortAllowed()) {
        try { syncAccountSortControl(); } catch (_) {}
        return;
      }
      accountsSort = ($("acc-sort").value || "newest");
      try { localStorage.setItem("g2a_accounts_sort", accountsSort); } catch (_) {}
      accountsPage = 1;
      loadAccountsPage({ reset: true });
    };
    try { syncAccountSortControl(); } catch (_) {}
  }
  // Restore status chips highlight from accountsStatusFilter (localStorage).
  try { renderAccountStatusChips(); } catch (_) {}
  if ($("acc-filter-status")) {
    try { $("acc-filter-status").value = accountsStatusFilter || ""; } catch (_) {}
  }
  if ($("acc-filter-sso")) {
    try {
      // Status chips match full-pool counters. Restoring SSO on top of a status
      // filter silently shrinks the list (冷却中 8 + 有SSO → 2). Prefer status.
      if (accountsStatusFilter) {
        accountsSsoFilter = "";
        $("acc-filter-sso").value = "";
        try { localStorage.setItem("g2a_accounts_sso_filter", ""); } catch (_) {}
      } else {
        const savedSso = localStorage.getItem("g2a_accounts_sso_filter");
        if (savedSso === "1" || savedSso === "0" || savedSso === "") {
          accountsSsoFilter = savedSso || "";
          $("acc-filter-sso").value = accountsSsoFilter;
        } else {
          accountsSsoFilter = $("acc-filter-sso").value || "";
        }
      }
    } catch (_) {
      if (accountsStatusFilter) accountsSsoFilter = "";
      else accountsSsoFilter = $("acc-filter-sso").value || "";
    }
    $("acc-filter-sso").onchange = () => {
      accountsSsoFilter = ($("acc-filter-sso").value || "");
      try { localStorage.setItem("g2a_accounts_sso_filter", accountsSsoFilter); } catch (_) {}
      accountsPage = 1;
      if (accountsSsoFilter && accountsStatusFilter) {
        try {
          toast("已叠加「" + accountStatusFilterLabel(accountsStatusFilter) + "」+ " +
            (accountsSsoFilter === "1" ? "有SSO" : "无SSO") + " 筛选；数量会少于顶部统计", false);
        } catch (_) {}
      }
      try { syncAccountSortControl(); } catch (_) {}
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
  // Table header "select all" checkbox (top-left). Soft-nav recreates this node — rebind every time.
  on("acc-check-page", "onchange", (e) => setPageSelection(!!(e && e.target && e.target.checked)));
  // Also bind click for browsers that only fire click on some label/th layouts.
  on("acc-check-page", "onclick", (e) => {
    const el = e && e.target;
    if (!el) return;
    // Defer to after the checkbox toggles its own checked state.
    setTimeout(() => setPageSelection(!!el.checked), 0);
  });
  try { bindAccountsPagerControls(); } catch (e) { console.warn("bindAccountsPagerControls", e); }

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
  // Soft-nav swaps #main-content; rebind file pickers so name labels update again.
  on("import-file", "onchange", () => {
    const files = $("import-file") && $("import-file").files;
    const label = $("import-file-name");
    if (!label) return;
    if (!files || !files.length) label.textContent = "未选择文件";
    else if (files.length === 1) label.textContent = `已选择：${files[0].name}（${(files[0].size / 1024).toFixed(1)} KB）`;
    else {
      const totalKb = Array.from(files).reduce((s, f) => s + f.size, 0) / 1024;
      label.textContent = `已选择 ${files.length} 个文件（共 ${totalKb.toFixed(1)} KB）`;
    }
  });
  on("sso-file", "onchange", () => {
    const f = $("sso-file") && $("sso-file").files && $("sso-file").files[0];
    const label = $("sso-file-name");
    if (!label) return;
    label.textContent = f ? `已选择：${f.name}（${(f.size / 1024).toFixed(1)} KB）` : "未选择文件";
  });
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
      startRegPolling({ immediate: true, intervalMs: 180 });
    } catch (e) { toast(e.message, false); }
    finally { if ($("btn-start-reg")) $("btn-start-reg").disabled = false; }
  });
  if ($("btn-save-reg")) on("btn-save-reg", "onclick", () => { saveRegConfig().catch(() => {}); });
  // Soft-nav swaps registration form DOM — rebind provider select + repaint panels.
  try { bindRegMailFormControls(); } catch (e) { console.warn("bindRegMailFormControls", e); }
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
  try { bindKeysControls(); } catch (_) {}

  if ($("accounts-tbody") && !$("accounts-tbody")._g2aBound) {
    $("accounts-tbody")._g2aBound = true;
    $("accounts-tbody").addEventListener("click", async (e) => {
      const chk = e.target.closest(".acc-check-one");
      if (chk) {
        const id = accountIdKey(chk.dataset.id);
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
            if (!q || typeof q !== "object") {
              toast("额度查询返回空结果", false);
              return;
            }
            quotaCache[id] = q;
            if (q.auto_disabled || q.exhausted || q.disabled_for_quota) toast("该账号额度已耗尽，已进入冷却池", false);
            else if (q.ok) toast((q.display && q.display.summary) || "额度已更新");
            else toast(q.error || "额度查询失败", false);
            // Prefer DB pool view returned after SaveQuotaSnapshot; fallback to local patch.
            let qPatch = {};
            if (q.pool && typeof q.pool === "object") {
              qPatch = (typeof poolPatchFromStatusAccount === "function")
                ? poolPatchFromStatusAccount({ pool: q.pool, _pool: q.pool })
                : { ...(q.pool || {}) };
              qPatch.last_quota = q;
            } else if (typeof poolPatchFromQuotaResult === "function") {
              qPatch = poolPatchFromQuotaResult(q) || {};
            } else {
              const dead = !!(q.auto_disabled || q.exhausted);
              qPatch = {
                last_quota: q,
                disabled_for_quota: false,
                enabled: true,
                disabled_reason: null,
                pool_status: dead ? "cooldown" : "normal",
                in_cooldown: !!dead,
                cooldown_reason: dead ? (q.exhaust_reason || q.error || "额度耗尽") : null,
                cooldown_code: dead
                  ? ((q.free_tokens || q.account_type === "free" || q.source === "free_tokens")
                      ? "subscription:free-usage-exhausted"
                      : "billing_quota")
                  : null,
              };
              if (dead) {
                const free = q.free_tokens || q.account_type === "free" || q.source === "free_tokens";
                qPatch.cooldown_until = Math.floor(Date.now() / 1000) + (free ? 2 * 3600 : 6 * 3600);
                if (q.tokens_used != null) qPatch.cooldown_tokens_actual = q.tokens_used;
                if (q.tokens_actual != null) qPatch.cooldown_tokens_actual = q.tokens_actual;
                if (q.tokens_limit != null) qPatch.cooldown_tokens_limit = q.tokens_limit;
              }
            }
            if (!qPatch.last_quota) qPatch.last_quota = q;
            applyAccountLivePatch(id, { _pool: qPatch });
            // Hot-update this row only — no full accounts reload (prevents scroll jump).
            Promise.resolve().then(() => softRefreshPoolChips({ stats: true }));

          } catch (err) {
            toast((err && err.message) || "额度查询失败", false);
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
            // Hot-update this row only — do not reload the whole account pool list.
            const enPool = en
              ? {
                  enabled: true,
                  disabled_for_quota: false,
                  disabled_reason: null,
                  quota_disabled_at: null,
                  quota_source: null,
                  pool_status: "normal",
                }
              : { enabled: false, pool_status: "disabled" };
            applyAccountLivePatch(id, { _pool: enPool });
            Promise.resolve().then(() => softRefreshPoolChips({ stats: true }));
          } catch (err) {
            toast((err && err.message) || "操作失败", false);
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
            selectedAccountIds.delete(accountIdKey(id));
            accountsList = (accountsList || []).filter((a) => accountIdKey(a.id) !== accountIdKey(id));
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
  // Must cover every PAGE_META entry (incl. logs / usage). A partial map left
  // href="undefined" (literal) → browser requests /admin/undefined → HTTP 404.
  const active = document.body.dataset.page || pageFromPath(location.pathname) || "overview";
  const order = ["overview", "keys", "accounts", "usage", "logs", "models", "settings", "guide"];
  const keys = order.filter((k) => PAGE_META[k]).concat(
    Object.keys(PAGE_META).filter((k) => !order.includes(k))
  );
  host.innerHTML = keys.map((k) => {
    let href = (PAGE_HREF && PAGE_HREF[k]) || "";
    // Never emit empty / "undefined" / relative garbage — that was the mobile 404.
    if (!href || href === "undefined" || href === "null") {
      href = k === "overview" ? "/admin" : ("/admin/" + k);
    }
    if (href.charAt(0) !== "/") href = "/" + href;
    const on = k === active ? "active is-active" : "";
    const title = (PAGE_META[k] && PAGE_META[k].title) || k;
    return `<a class="${on}" href="${href}" data-page="${k}">${title}</a>`;
  }).join("");
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
      // loadDashboard already called renderAccounts → loadAccountsPage once.
      // Only re-load if the first path failed to populate (network race / empty).
      if (!(accountsList && accountsList.length) && !accountsLoading) {
        try { loadAccountsPage({ reset: false, silent: true }); } catch (_) {}
      }
      try {
        restoreActiveRegistration({ force: !hasTrackedRegTask(), toastIfEmpty: false }).catch(() => {});
      } catch (_) {}
    }
    if (page === "keys") renderKeys();
    // btn-refresh / btn-logout already bound by rebindPageControls() above.
    // Do not re-bind here — a second handler used to drop the accounts force-refresh path.
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
    try { if (typeof startQuotaLiveRefresh === "function") startQuotaLiveRefresh({ immediate: true }); } catch (_) {}
  } else if (page === "usage") {
    try { await loadUsage(); } catch (e) { console.warn(e); }
  } else if (page === "logs") {
    // Soft: keep last table if any; first hard paint still happens when tbody empty.
    try {
      const hasRows = !!($("logs-tbody") && $("logs-tbody").querySelector("tr[data-log-id]"));
      await loadAdminLogs({ reset: false, soft: hasRows });
    } catch (e) { console.warn(e); }
  } else if (page === "models") {
    // Models page intentionally skips /dashboard (large). Load the dedicated
    // /models catalog so local extras like grok-build are visible.
    try { await loadModels(); } catch (e) { console.warn(e); }
    try { await refreshModelHealthStatus(); } catch (e) { try { renderModelHealthInfo(); } catch (_) {} }
    try { if (typeof startUpstreamMonitor === "function") startUpstreamMonitor({ force: true }); } catch (_) {}
  } else if (page === "guide") {
    try { renderGuide(); } catch (e) {}
  }

  try {
    const st = statusCache || {};
    const ver = st.version || (dashCache && dashCache.version) || "";
    if ($("app-version") && ver) {
      $("app-version").textContent = "v" + ver;
      try { window.__g2aCurrentVersion = String(ver).replace(/^v/, ""); } catch (_) {}
    }
    try { scheduleVersionBannerCheck(); } catch (_) {}
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
  const sign = v < 0 ? "-" : "";
  const a = Math.abs(v);
  // Always attach a unit from 1k so 累计 figures are readable (1.23k / 4.56M / 1.02B).
  if (a >= 1e12) return sign + (a / 1e12).toFixed(2) + "T";
  if (a >= 1e9) return sign + (a / 1e9).toFixed(2) + "B";
  if (a >= 1e6) return sign + (a / 1e6).toFixed(2) + "M";
  if (a >= 1e3) return sign + (a / 1e3).toFixed(a >= 1e4 ? 1 : 2) + "k";
  return sign + String(Math.round(a));
}

/** e.g. "1.23M token" */
function fmtTokens(n, unit = "token") {
  return `${fmtNum(n)}${unit ? " " + unit : ""}`;
}

/** Prefer API billed_tokens (total − cache_read); fall back to total_tokens. */
function usageBilled(obj) {
  if (!obj || typeof obj !== "object") return 0;
  if (obj.billed_tokens != null && obj.billed_tokens !== "") return Number(obj.billed_tokens) || 0;
  if (obj.total_tokens != null && obj.total_tokens !== "") return Number(obj.total_tokens) || 0;
  return 0;
}

function renderStats() {
  const s = statusCache || {};
  const d = dashCache || {};
  // Prefer live /status pool counters over possibly-stale /dashboard cache.
  const pool = Object.assign({}, d.pool || {}, s.pool || {});
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
      <div class="sub">模式 ${esc(d.account_mode || s.account_mode || "—")} · 冷却 ${pool.in_cooldown ?? 0} · 过期 ${pool.expired ?? 0} · 模型封禁 ${pool.model_blocked ?? 0} · 额度冷却 ${pool.quota_disabled ?? 0} · 禁用 ${pool.disabled ?? 0}</div></div>
    <div class="stat"><div class="label">API Keys</div><div class="value">${keys.enabled ?? 0} 启用 / ${keys.total ?? 0}</div>
      <div class="sub">请求累计 ${fmtNum(keys.total_requests ?? 0)} · 鉴权 ${keys.auth_required ? "开启" : "关闭"}</div></div>
    <div class="stat"><div class="label">今日用量</div><div class="value mono">${fmtTokens((d.usage || s.usage || {}).today_tokens || 0)}</div>
      <div class="sub">请求 ${fmtNum((d.usage || s.usage || {}).today_requests ?? 0)} · 累计 ${fmtTokens((d.usage || s.usage || {}).total_tokens ?? 0)}（已扣缓存）</div></div>
    <div class="stat"><div class="label">Token 自动续期</div><div class="value">${(tm.running || tm.cluster_running || tm.leader_running) ? "运行中" : (tm.enabled === false ? "已关闭" : (tm.enabled ? "已启用" : "未运行"))}</div>
      <div class="sub">最短剩余 ${esc(remLabel)} · 下次 ${nextWait ?? "—"}s${lastRef != null ? ` · 上次刷新 ${lastRef}` : ""}${lastTm.at ? ` · ${fmtTime(lastTm.at)}` : ""}</div></div>`;
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


// ── API Keys helpers ──────────────────────────────────
// Create endpoint returns a FLAT object:
//   { id, name, prefix, secret, key: <secret string>, ... }
// Never do `const rec = data.key || data` — data.key is the secret string.
function extractKeySecret(payload) {
  if (!payload) return "";
  if (typeof payload === "string") {
    const s = payload.trim();
    return s.startsWith("sk-") || s.startsWith("sk_") ? s : "";
  }
  if (typeof payload !== "object") return "";
  // Prefer explicit secret fields; avoid treating nested key-object as secret.
  for (const k of ["secret", "api_key", "raw_key", "full_key"]) {
    const v = payload[k];
    if (typeof v === "string" && v.trim()) return v.trim();
  }
  // `key` may be the plaintext secret (create/regenerate) OR a nested object.
  const keyField = payload.key;
  if (typeof keyField === "string" && keyField.trim()) return keyField.trim();
  if (keyField && typeof keyField === "object") {
    for (const k of ["secret", "key", "api_key"]) {
      const v = keyField[k];
      if (typeof v === "string" && v.trim()) return v.trim();
    }
  }
  return "";
}

function extractKeyRecord(payload) {
  if (!payload || typeof payload !== "object") return {};
  // Prefer nested record object when present (avoid treating plaintext key string as record).
  if (payload.record && typeof payload.record === "object" && !Array.isArray(payload.record)) {
    return { ...payload.record };
  }
  if (payload.key && typeof payload.key === "object" && !Array.isArray(payload.key)) {
    return { ...payload.key };
  }
  // Flat create/regenerate body.
  return { ...payload };
}

/** Normalize list payloads from GET /keys (array under keys/items/data, or single object). */
function normalizeKeyList(data) {
  if (!data) return [];
  if (Array.isArray(data)) return data.filter((k) => k && typeof k === "object" && k.id);
  if (typeof data !== "object") return [];
  const candidates = [data.keys, data.items, data.data, data.records, data.api_keys];
  for (const c of candidates) {
    if (Array.isArray(c)) return c.filter((k) => k && typeof k === "object" && k.id);
  }
  // Single-key create-shaped body mistakingly used as list.
  if (data.id && (data.prefix || data.name || data.secret || data.key)) return [data];
  return [];
}

function rememberKeySecret(id, secret) {
  if (!id || !secret) return;
  if (!keysCache || typeof keysCache !== "object" || Array.isArray(keysCache)) keysCache = {};
  const prev = keysCache[id] || { id };
  keysCache[id] = { ...prev, id, secret, key: secret, has_secret: true };
}

/** Paint one key row into the table immediately (create/regenerate optimistic UI). */
function paintKeyRowOptimistic(rec, secret) {
  if (!rec || !rec.id) return;
  const body = $("keys-tbody");
  if (!body) return;
  // Drop empty / loading placeholders so the new row is visible.
  const emptyRow = body.querySelector("tr:not([data-key-id])");
  if (emptyRow && body.querySelectorAll("tr[data-key-id]").length === 0) {
    body.innerHTML = "";
  }
  const id = String(rec.id);
  const existing = body.querySelector(`tr[data-key-id="${CSS && CSS.escape ? CSS.escape(id) : id.replace(/"/g, '\\"')}"]`);
  if (existing) existing.remove();
  const full = secret || extractKeySecret(rec) || "";
  if (full) rememberKeySecret(id, full);
  else {
    if (!keysCache || typeof keysCache !== "object" || Array.isArray(keysCache)) keysCache = {};
    keysCache[id] = { ...(keysCache[id] || {}), ...rec, id };
  }
  const cached = keysCache[id] || rec;
  const canCopy = !!(extractKeySecret(cached) || rec.has_secret || full);
  const lastUsed = rec.last_used_at != null ? rec.last_used_at : rec.created_at;
  const tr = document.createElement("tr");
  tr.setAttribute("data-key-id", id);
  tr.innerHTML = `
      <td>${esc(rec.name || "—")}<div class="g2a-muted" style="font-size:0.75rem">${esc(rec.note || "")}</div></td>
      <td class="mono" title="${canCopy ? "可复制完整 Key" : "库内无完整 Key，需重建"}">${esc(rec.prefix || "")}…</td>
      <td>${rec.enabled === false ? '<span class="g2a-tag bad">停用</span>' : '<span class="g2a-tag ok">启用</span>'}</td>
      <td class="mono">${fmtNum(rec.request_count || 0)}</td>
      <td class="g2a-muted" title="最后使用 / 创建时间">${esc(fmtTime(lastUsed))}</td>
      <td class="g2a-actions">
        <button type="button" class="g2a-btn g2a-btn-primary g2a-btn-sm" data-act="copy" data-id="${esc(id)}">${canCopy ? "复制" : "重建复制"}</button>
        <button type="button" class="g2a-btn g2a-btn-default g2a-btn-sm" data-act="toggle" data-id="${esc(id)}" data-on="${rec.enabled === false ? 1 : 0}">${rec.enabled === false ? "启用" : "停用"}</button>
        <button type="button" class="g2a-btn g2a-btn-danger g2a-btn-sm" data-act="del" data-id="${esc(id)}">删除</button>
      </td>`;
  body.insertBefore(tr, body.firstChild);
  try { bindKeysControls(); } catch (_) {}
}

function showNewKeyBox(secret, meta) {
  const box = $("new-key-box");
  if (!box) return;
  const full = String(secret || "").trim();
  const name = (meta && meta.name) || "";
  box.classList.remove("hidden");
  box.hidden = false;
  box.innerHTML = `
    <div style="font-weight:600;margin-bottom:6px;color:var(--ok)">✓ Key 已创建${name ? " · " + esc(name) : ""} — 请立即复制保存</div>
    <div class="g2a-muted" style="margin-bottom:6px;font-size:12px">完整密钥仅此时可见（或列表中点「复制」若库内已存 secret）。</div>
    <code id="new-key-value" class="g2a-code-inline mono" style="display:block;user-select:all;word-break:break-all;cursor:pointer;padding:8px 10px" title="点击复制">${esc(full || "（空）")}</code>
    <div class="g2a-actions" style="margin-top:10px;display:flex;gap:8px;flex-wrap:wrap">
      <button type="button" class="g2a-btn g2a-btn-primary g2a-btn-sm" id="copy-key">复制 Key</button>
      <button type="button" class="g2a-btn g2a-btn-default g2a-btn-sm" id="dismiss-key">关闭</button>
    </div>`;
  const doCopy = async () => {
    if (!full) { toast("Key 为空", false); return; }
    const ok = await copyText(full);
    toast(ok ? "已复制 API Key" : "复制失败，请手动选中复制", ok);
  };
  on("copy-key", "onclick", doCopy);
  on("new-key-value", "onclick", doCopy);
  on("dismiss-key", "onclick", () => {
    box.classList.add("hidden");
    box.hidden = true;
  });
}

async function createApiKeyFromForm() {
  const nameEl = $("key-name");
  const noteEl = $("key-note");
  const name = ((nameEl && nameEl.value) || "").trim() || "default";
  const note = ((noteEl && noteEl.value) || "").trim();
  const btn = $("btn-create-key");
  if (btn) {
    if (!btn.dataset.label) btn.dataset.label = btn.textContent;
    btn.disabled = true;
    btn.textContent = "创建中…";
  }
  try {
    const data = await api("/keys", {
      method: "POST",
      body: JSON.stringify({ name, note }),
    });
    const rec = extractKeyRecord(data) || {};
    // Flat create body uses id on the root; ensure id/name/prefix survive extract.
    if (!rec.id && data && data.id) Object.assign(rec, data);
    if (data && typeof data === "object") {
      if (!rec.name && data.name) rec.name = data.name;
      if (!rec.prefix && data.prefix) rec.prefix = data.prefix;
      if (rec.enabled == null && data.enabled != null) rec.enabled = data.enabled;
      if (!rec.note && data.note) rec.note = data.note;
      if (!rec.created_at && data.created_at != null) rec.created_at = data.created_at;
    }
    const full = extractKeySecret(data) || extractKeySecret(rec);
    if (rec && rec.id && full) rememberKeySecret(rec.id, full);
    else if (rec && rec.id) {
      if (!keysCache || typeof keysCache !== "object" || Array.isArray(keysCache)) keysCache = {};
      keysCache[rec.id] = { ...(keysCache[rec.id] || {}), ...rec };
    }
    // Paint immediately so the new row is visible even if list refresh races/fails.
    try { paintKeyRowOptimistic(rec, full); } catch (_) {}
    showNewKeyBox(full, rec);
    if (full) {
      const ok = await copyText(full);
      toast(ok ? "已创建并复制 API Key" : (full ? "已创建（自动复制失败，请手动复制）" : "已创建但未返回完整密钥"), !!full);
    } else {
      toast("已创建 Key，但响应未包含完整密钥，请用「重建复制」", false);
    }
    if (nameEl) nameEl.value = "";
    if (noteEl) noteEl.value = "";
    // Re-sync from server (newest-first). Keep optimistic row if list is empty/stale.
    try {
      await renderKeys({ preserveIds: rec && rec.id ? [rec.id] : [] });
    } catch (e) {
      console.warn("renderKeys after create", e);
    }
    return true;
  } catch (e) {
    toast((e && e.message) || "创建 Key 失败", false);
    return false;
  } finally {
    if (btn) {
      btn.disabled = false;
      btn.textContent = btn.dataset.label || "新建 Key";
    }
  }
}

async function handleKeyRowAction(btn) {
  if (!btn) return;
  const id = btn.dataset.id;
  const act = btn.dataset.act;
  if (!id || !act) return;
  try {
    if (act === "copy") {
      const k = (keysCache && keysCache[id]) || {};
      let full = extractKeySecret(k);
      let regenerated = false;
      if (!full) {
        if (!confirm("该 Key 未保存完整值，无法直接复制。是否重新生成？旧 Key 会立即失效。")) return;
        const data = await api("/keys/" + encodeURIComponent(id) + "/regenerate", { method: "POST" });
        const rec = extractKeyRecord(data);
        full = extractKeySecret(data) || extractKeySecret(rec);
        if (!full) {
          toast("重建后仍无完整值，请刷新后再试", false);
          await renderKeys();
          return;
        }
        rememberKeySecret(id, full);
        if (rec && typeof rec === "object") {
          keysCache[id] = { ...(keysCache[id] || {}), ...rec, secret: full, key: full };
          try { paintKeyRowOptimistic({ ...(keysCache[id] || {}), id, ...rec }, full); } catch (_) {}
        }
        regenerated = true;
        showNewKeyBox(full, rec);
      }
      const ok = await copyText(full);
      toast(ok ? (regenerated ? "已重建并复制 API Key" : "已复制 API Key") : "复制失败，请手动选中复制", ok);
      if (regenerated) await renderKeys();
      return;
    }
    if (act === "del") {
      if (!confirm("确定删除此 Key？删除后使用该密钥的客户端将立即失效。")) return;
      await api("/keys/" + encodeURIComponent(id), { method: "DELETE" });
      if (keysCache && keysCache[id]) delete keysCache[id];
      toast("已删除");
      await renderKeys();
      return;
    }
    if (act === "toggle") {
      const enable = btn.dataset.on === "1";
      await api("/keys/" + encodeURIComponent(id), {
        method: "PATCH",
        body: JSON.stringify({ enabled: enable }),
      });
      if (keysCache && keysCache[id]) keysCache[id].enabled = enable;
      toast(enable ? "已启用" : "已停用");
      await renderKeys();
      return;
    }
  } catch (err) {
    toast((err && err.message) || "操作失败", false);
  }
}

function bindKeysControls() {
  on("btn-create-key", "onclick", () => { createApiKeyFromForm(); });
  // Static new-key-box buttons (if template still has them before first create).
  on("copy-key", "onclick", async () => {
    const el = $("new-key-value");
    const full = (el && (el.textContent || el.innerText) || "").trim();
    if (!full || full === "（空）") { toast("没有可复制的 Key", false); return; }
    const ok = await copyText(full);
    toast(ok ? "已复制 API Key" : "复制失败", ok);
  });
  on("dismiss-key", "onclick", () => {
    const box = $("new-key-box");
    if (box) { box.classList.add("hidden"); box.hidden = true; }
  });
  const tb = $("keys-tbody");
  if (tb && !tb._g2aBound) {
    tb._g2aBound = true;
    tb.addEventListener("click", async (e) => {
      const btn = e.target && e.target.closest ? e.target.closest("button[data-act]") : null;
      if (!btn) return;
      e.preventDefault();
      await handleKeyRowAction(btn);
    });
  }
  on("btn-refresh-all", "onclick", async () => {
    try {
      await renderKeys();
      toast("已刷新 Key 列表");
    } catch (e) { toast(e.message || "刷新失败", false); }
  });
}

function renderKeys(opts) {
  opts = opts || {};
  const preserveIds = new Set((opts.preserveIds || []).map(String).filter(Boolean));
  bindKeysControls();
  const tbody = $("keys-tbody");
  const hasRows = !!(tbody && tbody.querySelector("tr[data-key-id]"));
  if (tbody && !hasRows) {
    tbody.innerHTML = `<tr><td colspan="6" class="g2a-muted">加载 API Keys…</td></tr>`;
  }
  return api("/keys").then((data) => {
    const body = $("keys-tbody");
    if (!body) return;
    let keys = normalizeKeyList(data);
    // Guard: never treat stats object {total, enabled, ...} as a key list.
    if (!Array.isArray(keys)) keys = [];
    const src = (data && (data.store_source || data.store_backend)) || "";
    window.__g2aKeysStore = { source: src };
    if ($("page-sub") && document.body && document.body.dataset.page === "keys") {
      $("page-sub").textContent = src === "postgres"
        ? "创建、复制、停用客户端访问密钥 · 数据源：数据库"
        : "创建、复制、停用客户端访问密钥";
    }
    // Merge into cache — keep any session-only secrets from create/regenerate.
    const next = {};
    keys.forEach((k) => {
      if (!k || !k.id) return;
      const prev = (keysCache && keysCache[k.id]) || {};
      const secret = extractKeySecret(k) || extractKeySecret(prev) || "";
      next[k.id] = {
        ...prev,
        ...k,
        secret: secret || undefined,
        key: secret || undefined,
        has_secret: !!(secret || k.has_secret),
      };
    });
    // If server list is empty/missing a just-created id, keep cache entry for optimistic paint.
    if (preserveIds.size && keysCache && typeof keysCache === "object") {
      preserveIds.forEach((id) => {
        if (!next[id] && keysCache[id]) next[id] = { ...keysCache[id] };
      });
    }
    keysCache = next;
    // Prefer server order (newest first). Append any preserve-only rows at top.
    const list = keys.slice();
    if (preserveIds.size) {
      const have = new Set(list.map((k) => String(k && k.id)));
      preserveIds.forEach((id) => {
        if (!have.has(id) && next[id]) list.unshift(next[id]);
      });
    }
    if (!list.length) {
      body.innerHTML = `<tr><td colspan="6" class="g2a-muted">暂无 Key。创建后客户端访问 /v1 将需要鉴权。</td></tr>`;
      return;
    }
    body.innerHTML = list.map((k) => {
      const cached = keysCache[k.id] || k;
      const canCopy = !!(extractKeySecret(cached) || k.has_secret);
      const lastUsed = k.last_used_at != null ? k.last_used_at : k.created_at;
      return `
      <tr data-key-id="${esc(k.id)}">
        <td>${esc(k.name || "—")}<div class="g2a-muted" style="font-size:0.75rem">${esc(k.note || "")}</div></td>
        <td class="mono" title="${canCopy ? "可复制完整 Key" : "库内无完整 Key，需重建"}">${esc(k.prefix || "")}…</td>
        <td>${k.enabled ? '<span class="g2a-tag ok">启用</span>' : '<span class="g2a-tag bad">停用</span>'}</td>
        <td class="mono">${fmtNum(k.request_count || 0)}</td>
        <td class="g2a-muted" title="最后使用 / 创建时间">${esc(fmtTime(lastUsed))}</td>
        <td class="g2a-actions">
          <button type="button" class="g2a-btn g2a-btn-primary g2a-btn-sm" data-act="copy" data-id="${esc(k.id)}">${canCopy ? "复制" : "重建复制"}</button>
          <button type="button" class="g2a-btn g2a-btn-default g2a-btn-sm" data-act="toggle" data-id="${esc(k.id)}" data-on="${k.enabled ? 0 : 1}">${k.enabled ? "停用" : "启用"}</button>
          <button type="button" class="g2a-btn g2a-btn-danger g2a-btn-sm" data-act="del" data-id="${esc(k.id)}">删除</button>
        </td>
      </tr>`;
    }).join("");
    // Re-bind in case soft-nav replaced tbody before this paint.
    try { bindKeysControls(); } catch (_) {}
  }).catch((e) => {
    const body = $("keys-tbody");
    // Keep optimistic rows if we already painted a create result.
    if (body && preserveIds.size && body.querySelector("tr[data-key-id]")) {
      toast(e.message || "刷新 Keys 失败（已保留新建项）", false);
      return;
    }
    if (body) body.innerHTML = `<tr><td colspan="6" class="g2a-muted">加载失败：${esc(e.message || e)}</td></tr>`;
    toast(e.message || "加载 Keys 失败", false);
  });
}


function resolveAccountQuota(p, liveQuota) {
  // Prefer live only when it actually carries type/usage. A probing-only
  // or error-only placeholder must not hide durable last_quota from the database.
  const poolQ = (p && p.last_quota && typeof p.last_quota === "object") ? p.last_quota : null;
  if (liveQuota && typeof liveQuota === "object" && hasQuotaInfo(liveQuota)) {
    // If both have info, prefer fresher; else live.
    if (poolQ && hasQuotaInfo(poolQ) && typeof mergeQuotaSnapClient === "function") {
      const lt = quotaSnapTs(liveQuota);
      const pt = quotaSnapTs(poolQ);
      if (pt > lt) return mergeQuotaSnapClient(liveQuota, poolQ);
      return mergeQuotaSnapClient(poolQ, liveQuota);
    }
    return liveQuota;
  }
  if (poolQ && hasQuotaInfo(poolQ)) return poolQ;
  if (poolQ) return poolQ;
  return liveQuota || null;
}

function resolveAccountPlan(q, p) {
  // Prefer quota snapshot, then durable row fields from DB list API.
  const src = q || {};
  const row = p || {};
  let plan = String(src.account_type || src.plan || row.account_type || row.plan || "").toLowerCase();
  if (plan === "free" || plan === "supergrok" || plan === "team") return plan;
  if (src.free_tokens || src.unlimited_or_free || row.free_tokens) return "free";
  if (Number(src.monthly_limit) > 0 || Number(src.on_demand_cap) > 0) return "supergrok";
  if (src.tokens_limit != null || src.tokens_remaining != null) return "free";
  // last_quota nested on pool when live cache empty after hard refresh
  const lq = row.last_quota || (row._pool && row._pool.last_quota) || null;
  if (lq && typeof lq === "object") {
    plan = String(lq.account_type || lq.plan || "").toLowerCase();
    if (plan === "free" || plan === "supergrok" || plan === "team") return plan;
    if (lq.free_tokens || lq.unlimited_or_free) return "free";
    if (Number(lq.monthly_limit) > 0) return "supergrok";
    if (lq.tokens_limit != null || lq.tokens_remaining != null) return "free";
  }
  return "";
}

function calcQuotaUsage(q) {
  // Returns { used, limit, remaining, pct, unit, text, weeklyText }
  if (!q || typeof q !== "object") {
    return { used: null, limit: null, remaining: null, pct: null, unit: "", text: "—", weeklyText: "" };
  }
  const plan = resolveAccountPlan(q);
  const isFree = plan === "free" || q.free_tokens || q.unlimited_or_free ||
    (q.tokens_limit != null && !(Number(q.monthly_limit) > 0) && !(Number(q.weekly_limit) > 0));

  if (isFree) {
    let limit = q.tokens_limit != null ? Number(q.tokens_limit) : null;
    let remaining = q.tokens_remaining != null ? Number(q.tokens_remaining) : null;
    let used = q.tokens_used != null ? Number(q.tokens_used)
      : (q.tokens_actual != null ? Number(q.tokens_actual) : null);
    // Auto-calc: used = limit - remaining
    if ((used == null || !Number.isFinite(used)) && limit != null && remaining != null) {
      used = Math.max(0, limit - remaining);
    }
    if ((remaining == null || !Number.isFinite(remaining)) && limit != null && used != null) {
      remaining = Math.max(0, limit - used);
    }
    let pct = q.tokens_usage_percent != null ? Number(q.tokens_usage_percent) : null;
    if ((pct == null || !Number.isFinite(pct)) && limit > 0 && used != null) pct = (used / limit) * 100;
    if (pct != null && Number.isFinite(pct)) pct = Math.max(0, Math.min(100, Math.round(pct)));
    else pct = null;
    const text = (limit != null && limit > 0)
      ? `${fmtNum(used || 0)} / ${fmtNum(limit)}` + (pct != null ? ` · ${pct}%` : "") +
        (remaining != null ? ` · 剩 ${fmtNum(remaining)}` : "")
      : (remaining != null ? `剩 ${fmtNum(remaining)}` : "—");
    return { used, limit, remaining, pct, unit: "token", text, weeklyText: "" };
  }

  // SuperGrok: monthly + weekly USD
  const fmtUsd = (v) => {
    if (v == null || !Number.isFinite(Number(v))) return "—";
    return "$" + Number(v).toFixed(2);
  };
  let limit = q.monthly_limit != null ? Number(q.monthly_limit) : null;
  let used = q.used != null ? Number(q.used) : null;
  let remaining = q.remaining != null ? Number(q.remaining) : null;
  if ((remaining == null || !Number.isFinite(remaining)) && limit != null && used != null) {
    remaining = Math.max(0, limit - used);
  }
  if ((used == null || !Number.isFinite(used)) && limit != null && remaining != null) {
    used = Math.max(0, limit - remaining);
  }
  let pct = q.usage_percent != null ? Number(q.usage_percent) : null;
  if ((pct == null || !Number.isFinite(pct)) && limit > 0 && used != null) pct = (used / limit) * 100;
  if (pct != null && Number.isFinite(pct)) pct = Math.max(0, Math.min(100, Math.round(pct)));
  else pct = null;

  let text = (limit != null)
    ? `月 ${fmtUsd(used || 0)} / ${fmtUsd(limit)}` + (pct != null ? ` · ${pct}%` : "") +
      (remaining != null ? ` · 剩 ${fmtUsd(remaining)}` : "")
    : "—";

  // Weekly
  let weeklyText = "";
  const wl = q.weekly_limit != null ? Number(q.weekly_limit) : null;
  const wu = q.weekly_used != null ? Number(q.weekly_used) : null;
  let wr = q.weekly_remaining != null ? Number(q.weekly_remaining) : null;
  if (wl != null && wl > 0) {
    if (wr == null && wu != null) wr = Math.max(0, wl - wu);
    let wp = q.weekly_usage_percent != null ? Number(q.weekly_usage_percent) : null;
    if ((wp == null || !Number.isFinite(wp)) && wu != null) wp = (wu / wl) * 100;
    if (wp != null && Number.isFinite(wp)) wp = Math.max(0, Math.min(100, Math.round(wp)));
    weeklyText = `周 ${fmtUsd(wu || 0)} / ${fmtUsd(wl)}` +
      (wp != null ? ` · ${wp}%` : "") +
      (wr != null ? ` · 剩 ${fmtUsd(wr)}` : "");
  }

  // On-demand secondary
  const odc = q.on_demand_cap != null ? Number(q.on_demand_cap) : null;
  const odu = q.on_demand_used != null ? Number(q.on_demand_used) : null;
  if (odc != null && odc > 0) {
    const odLine = `按需 ${fmtUsd(odu || 0)} / ${fmtUsd(odc)}`;
    text = text === "—" ? odLine : (text + " · " + odLine);
  }

  // Prefer higher of monthly/weekly pct for meter
  let meterPct = pct;
  if (weeklyText) {
    const wp = q.weekly_usage_percent != null ? Math.round(Number(q.weekly_usage_percent)) : null;
    if (wp != null && (meterPct == null || wp > meterPct)) meterPct = wp;
  }

  return { used, limit, remaining, pct: meterPct, unit: "usd", text, weeklyText };
}

function fmtAccountTypeCell(p, liveQuota) {
  const q = resolveAccountQuota(p, liveQuota);
  const plan = resolveAccountPlan(q, p);
  if (!plan) {
    return `<span class="g2a-muted">—</span>`;
  }
  if (plan === "supergrok") {
    return `<span class="g2a-tag blue" title="付费 SuperGrok">SuperGrok</span>`;
  }
  if (plan === "team") {
    return `<span class="g2a-tag" title="Team / Org">Team</span>`;
  }
  if (plan === "free") {
    return `<span class="g2a-tag ok" title="免费号 · 滚动 token 窗口">Free</span>`;
  }
  return `<span class="g2a-tag">${esc(plan)}</span>`;
}

function fmtQuotaUsageMeter(usage) {
  if (!usage || usage.pct == null || !Number.isFinite(usage.pct)) return "";
  const pct = Math.max(0, Math.min(100, usage.pct));
  let color = "var(--ant-color-success, #49aa19)";
  if (pct >= 90) color = "var(--ant-color-error, #dc4446)";
  else if (pct >= 70) color = "var(--ant-color-warning, #d89614)";
  return `<div class="g2a-quota-meter" title="已使用 ${pct}%">
    <div class="g2a-quota-meter-fill" style="width:${pct}%;background:${color}"></div>
  </div>`;
}

function fmtQuotaCell(p, liveQuota) {
  const q = resolveAccountQuota(p, liveQuota);
  const poolDisabled = p.enabled === false || p.disabled_for_quota || !!(liveQuota && liveQuota.pool_disabled);
  if (!q || (typeof hasQuotaInfo === "function" && !hasQuotaInfo(q) && !q.error)) {
    return `<span class="g2a-muted">未查询</span>
      <div style="margin-top:4px"><button class="g2a-btn g2a-btn-default g2a-btn-sm" data-act="quota-one" data-id="${esc(p.id || "")}">查询</button></div>`;
  }
  // Error-only shell (no type/usage): treat as 未查询, not scary red "查询失败".
  // Auto-refresh failures used to paint the whole page red after hard refresh.
  if (q.error && typeof hasQuotaInfo === "function" && !hasQuotaInfo(q)) {
    const errShort = String(q.error || "").slice(0, 80);
    return `<span class="g2a-muted">未查询</span>
      <div class="g2a-muted" style="font-size:0.70rem;margin-top:2px" title="${esc(errShort)}">上次失败，可重试</div>
      <div style="margin-top:4px"><button class="g2a-btn g2a-btn-default g2a-btn-sm" data-act="quota-one" data-id="${esc(p.id || "")}">查询</button></div>`;
  }
  const exhausted = !!(q && (q.exhausted || q.auto_disabled)) || !!p.disabled_for_quota;
  const usage = calcQuotaUsage(q);
  const plan = resolveAccountPlan(q, p);
  const isFree = plan === "free" || usage.unit === "token";

  let statusPill = "";
  if (exhausted) statusPill = '<span class="g2a-tag bad">已耗尽</span>';
  else if (poolDisabled) statusPill = '<span class="g2a-tag warn">禁用</span>';
  else if (usage.pct != null && usage.pct >= 90) statusPill = '<span class="g2a-tag warn">将尽</span>';
  else statusPill = '<span class="g2a-tag ok">可用</span>';

  const unitHint = isFree ? "token" : "USD";
  const age = q.fetched_at ? (Math.max(0, Math.floor(Date.now() / 1000 - Number(q.fetched_at)))) : null;
  const ageTxt = age != null && Number.isFinite(age) ? (age < 60 ? `${age}s前` : `${Math.floor(age / 60)}m前`) : "";
  const reason = exhausted ? (p.disabled_reason || q.exhaust_reason || "") : "";
  const weeklyLine = usage.weeklyText
    ? `<div class="g2a-muted" style="font-size:0.70rem;margin-top:2px" title="SuperGrok 周额度">${esc(usage.weeklyText)}</div>`
    : "";
  return `${statusPill}
    <div class="g2a-muted" style="font-size:0.72rem;margin-top:4px" title="${esc(unitHint)} 已用量 / 总量">${esc(usage.text)}</div>
    ${weeklyLine}
    ${fmtQuotaUsageMeter(usage)}
    ${reason ? `<div class="g2a-muted" style="font-size:0.68rem;margin-top:2px">${esc(String(reason).slice(0, 80))}</div>` : ""}
    ${ageTxt ? `<div class="g2a-muted" style="font-size:0.68rem;margin-top:2px">更新 ${esc(ageTxt)}</div>` : ""}`;
}





const ACCOUNT_STATUS_FILTERS = [
  { key: "", label: "全部", tone: "" },
  { key: "live", label: "轮询中", tone: "ok" },
  { key: "cooldown", label: "冷却中", tone: "warn" },
  { key: "model_blocked", label: "模型封禁", tone: "warn" },
  { key: "quota_disabled", label: "额度冷却", tone: "warn" },
  { key: "disabled", label: "已禁用", tone: "bad" },
  { key: "expired", label: "过期", tone: "bad" },
];

function accountStatusFilterLabel(key) {
  const hit = ACCOUNT_STATUS_FILTERS.find((x) => x.key === (key || ""));
  return hit ? hit.label : (key || "");
}

// Sort dropdown only applies when viewing the full list ("全部").
// Under status / SSO / search filters we force stable newest-by-create_time order
// so rows do not reshuffle on every probe/quota refresh.
function accountsSortAllowed() {
  return !accountsStatusFilter && !accountsSsoFilter && !String(accountsSearchQuery || "").trim();
}

function effectiveAccountsSort() {
  if (!accountsSortAllowed()) return "newest";
  const s = String(accountsSort || "newest").trim() || "newest";
  // Drop legacy filter-only sorts.
  if (s === "cooldown_first" || s === "disabled_first") return "newest";
  return s;
}

function syncAccountSortControl() {
  const sel = $("acc-sort");
  const wrap = $("acc-sort-wrap") || (sel && sel.closest && sel.closest(".g2a-sort-wrap"));
  const allowed = accountsSortAllowed();
  if (sel) {
    try {
      sel.disabled = !allowed;
      if (allowed) {
        // Restore user preference when re-entering 全部.
        const want = effectiveAccountsSort();
        if (sel.value !== want) sel.value = want;
        accountsSort = want;
      } else {
        // Show fixed order while filtered (not editable).
        if (sel.value !== "newest") sel.value = "newest";
      }
    } catch (_) {}
  }
  if (wrap) {
    try {
      wrap.classList.toggle("is-disabled", !allowed);
      wrap.title = allowed
        ? "账号排序（仅「全部」列表生效）"
        : "排序仅在「全部」列表可用；当前筛选下固定「最新加入」且不可改";
    } catch (_) {}
  }
}

function setAccountStatusFilter(key, { reload = true } = {}) {
  accountsStatusFilter = key || "";
  try { localStorage.setItem("g2a_accounts_status_filter", accountsStatusFilter); } catch (_) {}
  if ($("acc-filter-status")) {
    try { $("acc-filter-status").value = accountsStatusFilter; } catch (_) {}
  }
  // Status chips match /status pool counters (全库). Leftover SSO / keyword search
  // would silently shrink the list (e.g. 冷却中 8 → 搜索后只剩 2). Clear both when
  // using status chips so the table total equals the chip number.
  if (accountsSsoFilter) {
    accountsSsoFilter = "";
    try { localStorage.setItem("g2a_accounts_sso_filter", ""); } catch (_) {}
    if ($("acc-filter-sso")) {
      try { $("acc-filter-sso").value = ""; } catch (_) {}
    }
  }
  if (accountsSearchQuery) {
    accountsSearchQuery = "";
    try { localStorage.setItem("g2a_accounts_search", ""); } catch (_) {}
    if ($("acc-search")) {
      try { $("acc-search").value = ""; } catch (_) {}
    }
  }
  // Do NOT auto-switch sort under status filters — sort is disabled + fixed newest.
  try { syncAccountSortControl(); } catch (_) {}
  try { renderAccountStatusChips(); } catch (_) {}
  if (reload) loadAccountsPage({ reset: true });
}

function poolCountForStatus(key) {
  const s = statusCache || {};
  const d = dashCache || {};
  // Prefer /status pool (live DB summary) over dashboard cache.
  const pool = Object.assign({}, d.pool || {}, s.pool || {});
  if (!key) return pool.total != null ? Number(pool.total) : null;
  const map = {
    live: pool.live,
    // Account count only — NOT cooldown_stacks (叠加 depth is per-row tip).
    cooldown: pool.in_cooldown,
    model_blocked: pool.model_blocked,
    quota_disabled: pool.quota_disabled,
    disabled: pool.disabled,
    expired: pool.expired,
  };
  const v = map[key];
  return v == null ? null : Number(v);
}

function renderAccountStatusChips() {
  const el = $("acc-status-chips");
  if (!el) return;
  const cur = accountsStatusFilter || "";
  el.innerHTML = ACCOUNT_STATUS_FILTERS.map((f) => {
    const active = (f.key || "") === cur;
    const cls = ["g2a-btn", "g2a-btn-sm", active ? "g2a-btn-primary" : "g2a-btn-default"].join(" ");
    const n = poolCountForStatus(f.key);
    const countTxt = (n != null && Number.isFinite(n)) ? ` (${n})` : "";
    const title = f.key
      ? `只显示「${f.label}」账号（库内 ${n != null ? n : "—"} 个）；点「筛选全选」可选中该状态下全部账号`
      : `显示全部状态（总量 ${n != null ? n : "—"}）`;
    return `<button type="button" class="${cls}" data-acc-status="${esc(f.key)}" title="${esc(title)}">${esc(f.label)}${esc(countTxt)}</button>`;
  }).join("");
  el.querySelectorAll("[data-acc-status]").forEach((btn) => {
    btn.onclick = () => setAccountStatusFilter(btn.getAttribute("data-acc-status") || "");
  });
}

async function selectAllFilteredAccounts() {
  const btn = $("btn-acc-select-all-filtered");
  const q = (accountsSearchQuery || ($("acc-search") && $("acc-search").value) || "").trim();
  const sort = (typeof effectiveAccountsSort === "function") ? effectiveAccountsSort() : (accountsSort || "newest");
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
    const pageIds = Array.from(document.querySelectorAll(".acc-check-one")).map(x => accountIdKey(x.dataset.id)).filter(Boolean);
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
    tbody.innerHTML = `<tr><td colspan="10" class="g2a-muted">加载账号中…</td></tr>`;
  } else {
    tbody.innerHTML = pageItems.map((a) => {
      const p = { ...(a._pool || { id: a.id }) };
      // Hard refresh: pool.last_quota may be null while localStorage/memory still has snap.
      const liveQ = quotaCache[accountIdKey(a.id)] || quotaCache[a.id];
      if ((!p.last_quota || (typeof hasQuotaInfo === "function" && !hasQuotaInfo(p.last_quota)))
        && liveQ && typeof liveQ === "object" && hasQuotaInfo(liveQ)) {
        p.last_quota = liveQ;
      }
      const enabled = p.enabled !== false;
      const poolLabel = poolStatusLabel(a, p);
      const usage = `${p.success_count || 0}√ / ${p.fail_count || 0}× · 共 ${p.request_count || 0}`;
      const refreshPill = a.has_refresh_token
        ? '<span class="g2a-tag ok" title="可自动 refresh">可自动续期</span>'
        : '<span class="g2a-tag warn">无 refresh</span>';
      const ssoPill = a.has_sso
        ? '<span class="g2a-tag ok" title="账号库已保存 SSO cookie">有SSO</span>'
        : '<span class="g2a-tag" title="未保存 SSO cookie">无SSO</span>';
      const probeCell = fmtProbeCell(p.last_probe, p.last_error, p.blocked_model_ids);
      const checked = selectedAccountIds.has(accountIdKey(a.id)) ? "checked" : "";
      const expiryCell = fmtExpiry(a.expires_at);
      const typeCell = fmtAccountTypeCell({ ...p, id: a.id, account_type: a.account_type || p.account_type, plan: a.plan || p.plan, plan_label: a.plan_label || p.plan_label, last_quota: p.last_quota }, liveQ);
      return `
    <tr data-acc-id="${esc(a.id)}">
      <td><input type="checkbox" class="acc-check-one" data-id="${esc(a.id)}" ${checked} /></td>
      <td>${esc(a.email || "—")}<div class="muted mono" style="font-size:0.72rem">${esc(a.id)}</div></td>
      <td>${a.expired ? '<span class="g2a-tag bad">已过期</span>' : '<span class="g2a-tag ok">有效</span>'}</td>
      <td style="min-width:88px">${typeCell}</td>
      <td>${poolLabel}</td>
      <td class="g2a-muted" style="font-size:0.8rem">${usage}</td>
      <td style="font-size:0.82rem;min-width:150px">${fmtQuotaCell({ ...p, id: a.id }, liveQ)}</td>
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
    }).join("") || `<tr><td colspan="10" class="g2a-muted">${(accountsTotal || 0) ? "无匹配账号" : "无账号"}</td></tr>`;
  }
  if ($("acc-page-info")) {
    const src = (window.__g2aAccountsStore && window.__g2aAccountsStore.source) || "";
    const srcTxt = src === "postgres" ? " · 数据库" : (src ? ` · ${src}` : "");
    const filtParts = [];
    if (accountsStatusFilter) filtParts.push(`状态「${accountStatusFilterLabel(accountsStatusFilter)}」`);
    if (accountsSsoFilter === "1") filtParts.push("仅有SSO");
    if (accountsSsoFilter === "0") filtParts.push("仅无SSO");
    const filt = filtParts.length ? ` · 筛选: ${filtParts.join(" + ")}` : "";
    $("acc-page-info").textContent = `${accountsPage} / ${totalPages} (本页 ${pageItems.length} / 共 ${accountsTotal || 0} 个${filt}${srcTxt})`;
  }
  // Do NOT gate on accountsLoading — that left prev/next permanently disabled
  // when renderAccountsPage ran mid-load and finally never re-enabled them.
  try { syncAccountsPagerControls(); } catch (_) {
    if ($("acc-page-prev")) $("acc-page-prev").disabled = accountsPage <= 1;
    if ($("acc-page-next")) $("acc-page-next").disabled = accountsPage >= totalPages;
  }
  updateAccountSelectionInfo(accountsTotal || 0, pageItems.length);
}

function renderAccounts() {
  // Soft re-entry (menu / header refresh): keep table painted when we already have rows.
  const silent = !!(accountsList && accountsList.length);
  return loadAccountsPage({ reset: false, silent });
}

let _quotaCacheHydrated = false;

// Unix seconds from probe/quota timestamps (sec or ms). 0 when missing/invalid.
function accountUnixTs(v) {
  if (v == null || v === "") return 0;
  if (typeof v === "number" && Number.isFinite(v)) {
    return v > 1e12 ? Math.floor(v / 1000) : Math.floor(v);
  }
  if (typeof v === "string") {
    const s = v.trim();
    if (!s) return 0;
    if (/^\d+(\.\d+)?$/.test(s)) {
      const n = Number(s);
      if (!Number.isFinite(n)) return 0;
      return n > 1e12 ? Math.floor(n / 1000) : Math.floor(n);
    }
    const ms = Date.parse(s);
    return Number.isFinite(ms) ? Math.floor(ms / 1000) : 0;
  }
  return 0;
}

function probeSnapTs(lp) {
  if (!lp || typeof lp !== "object") return 0;
  return accountUnixTs(lp.probed_at) || accountUnixTs(lp.at) || accountUnixTs(lp.updated_at);
}

function quotaSnapTs(q) {
  if (!q || typeof q !== "object") return 0;
  return accountUnixTs(q.fetched_at) || accountUnixTs(q.at) || accountUnixTs(q.updated_at);
}

// Keep the fresher of two snapshots by timestamp. Equal timestamps prefer left (server).
function preferFresherSnap(serverSnap, localSnap, tsFn) {
  const empty = (s) => !s || typeof s !== "object" || !Object.keys(s).length;
  if (empty(localSnap)) return empty(serverSnap) ? null : serverSnap;
  if (empty(serverSnap)) return localSnap;
  const lt = tsFn(localSnap) || 0;
  const st = tsFn(serverSnap) || 0;
  // Local live patch without ts still beats empty server; with equal/older ts keep server.
  // If local has usable quota info and server has none, always keep local.
  if (typeof hasQuotaInfo === "function") {
    if (hasQuotaInfo(localSnap) && !hasQuotaInfo(serverSnap)) return localSnap;
    if (!hasQuotaInfo(localSnap) && hasQuotaInfo(serverSnap)) return serverSnap;
  }
  if (lt > st) return localSnap;
  return serverSnap;
}

// Merge server /accounts rows with in-memory list so a just-finished probe/quota
// is not clobbered by async SaveLastProbe / SaveQuotaSnapshot lag (or auto hot-refresh).
function mergeAccountsFromServer(rawAccounts) {
  const prevById = new Map((accountsList || []).map((a) => [accountIdKey(a.id), a]));
  return (Array.isArray(rawAccounts) ? rawAccounts : []).map((a) => {
    const id = accountIdKey(a && a.id);
    const prev = id ? prevById.get(id) : null;
    const serverPool = (a && a._pool) || { id: a && a.id };
    const next = { ...a, _pool: { ...serverPool } };
    if (!prev || !prev._pool) return next;
    const prevPool = prev._pool;
    const merged = { ...serverPool };

    const preferLocalProbe = preferFresherSnap(serverPool.last_probe, prevPool.last_probe, probeSnapTs);
    if (preferLocalProbe && preferLocalProbe === prevPool.last_probe) {
      merged.last_probe = prevPool.last_probe;
      if (prevPool.last_probe_status != null) merged.last_probe_status = prevPool.last_probe_status;
      // Keep status derived from that fresher probe when server row is still lagging.
      if (prevPool.pool_status != null && prevPool.pool_status !== serverPool.pool_status) {
        const st = String(prevPool.pool_status);
        if (st === "cooldown" || st === "normal" || st === "live" || st === "model_blocked") {
          merged.pool_status = prevPool.pool_status;
          if (prevPool.in_cooldown != null) merged.in_cooldown = prevPool.in_cooldown;
          if (prevPool.cooldown_until !== undefined) merged.cooldown_until = prevPool.cooldown_until;
          if (prevPool.cooldown_code !== undefined) merged.cooldown_code = prevPool.cooldown_code;
          if (prevPool.cooldown_reason !== undefined) merged.cooldown_reason = prevPool.cooldown_reason;
          if (prevPool.cooldown_model !== undefined) merged.cooldown_model = prevPool.cooldown_model;
          if (prevPool.cooldown_count !== undefined) merged.cooldown_count = prevPool.cooldown_count;
          if (prevPool.blocked_model_ids) merged.blocked_model_ids = prevPool.blocked_model_ids;
          if (prevPool.blocked_models) merged.blocked_models = prevPool.blocked_models;
          if (prevPool.last_error !== undefined) merged.last_error = prevPool.last_error;
        }
      }
    }

    // last_quota: never drop a good in-memory / just-queried snap when the server
    // row is null or error-stripped. Hard refresh still relies on DB; soft refresh
    // must keep paint. Merge when both present.
    {
      const serverQ = serverPool.last_quota;
      const localQ = prevPool.last_quota;
      const serverGood = serverQ && typeof serverQ === "object" && Object.keys(serverQ).length
        && (typeof hasQuotaInfo !== "function" || hasQuotaInfo(serverQ));
      const localGood = localQ && typeof localQ === "object" && Object.keys(localQ).length
        && (typeof hasQuotaInfo !== "function" || hasQuotaInfo(localQ));
      if (localGood && serverGood) {
        const picked = preferFresherSnap(serverQ, localQ, quotaSnapTs);
        merged.last_quota = (typeof mergeQuotaSnapClient === "function")
          ? (mergeQuotaSnapClient(
              picked === localQ ? serverQ : localQ,
              picked === localQ ? localQ : serverQ
            ) || picked)
          : picked;
      } else if (localGood) {
        // Server empty / stripped — keep what user already sees.
        merged.last_quota = localQ;
      } else if (serverQ && typeof serverQ === "object" && Object.keys(serverQ).length) {
        merged.last_quota = serverQ;
      } else {
        merged.last_quota = serverQ || localQ || null;
      }
      // Promote type onto account row for type column after soft refresh.
      const lq = merged.last_quota;
      if (lq && typeof lq === "object") {
        if (lq.account_type || lq.plan) {
          next.account_type = lq.account_type || lq.plan;
          next.plan = next.account_type;
        }
        if (lq.plan_label) next.plan_label = lq.plan_label;
      }
      if (localGood && (!serverGood || preferFresherSnap(serverQ, localQ, quotaSnapTs) === localQ)) {
        if (prevPool.disabled_for_quota != null) merged.disabled_for_quota = prevPool.disabled_for_quota;
        if (prevPool.disabled_reason !== undefined) merged.disabled_reason = prevPool.disabled_reason;
        if (prevPool.quota_source !== undefined) merged.quota_source = prevPool.quota_source;
        if (prevPool.quota_disabled_at !== undefined) merged.quota_disabled_at = prevPool.quota_disabled_at;
        if (prevPool.pool_status === "quota_disabled" || prevPool.disabled_for_quota) {
          merged.pool_status = "quota_disabled";
          merged.disabled_for_quota = true;
          if (prevPool.enabled != null) merged.enabled = prevPool.enabled;
        } else if (
          prevPool.disabled_for_quota === false
          && (serverPool.disabled_for_quota === true || serverPool.pool_status === "quota_disabled")
        ) {
          merged.disabled_for_quota = false;
          if (prevPool.enabled != null) merged.enabled = prevPool.enabled;
          if (merged.pool_status === "quota_disabled") {
            merged.pool_status = prevPool.pool_status || "normal";
          }
          merged.disabled_reason = prevPool.disabled_reason ?? null;
          merged.quota_source = prevPool.quota_source ?? null;
        }
      }
    }

    if (prevPool._clientBusy) merged._clientBusy = prevPool._clientBusy;
    next._pool = { ...merged, id: a.id };
    return next;
  });
}

// Project last_quota into quotaCache without overwriting a fresher live result.
// Called on every /accounts page load so hard refresh paints type + usage from DB.
function applyQuotaCacheFromAccounts(list) {
  try { ensureQuotaCacheHydrated(); } catch (_) {}
  (list || []).forEach((a) => {
    if (!a || a.id == null) return;
    const id = accountIdKey(a.id);
    if (!id) return;
    const lq = a._pool && a._pool.last_quota;
    if (!lq || typeof lq !== "object" || !Object.keys(lq).length) return;
    const prev = (quotaCache[id] && !quotaCache[id].probing) ? quotaCache[id] : null;
    // Keep fresher live only if it has real quota info (not probing shell).
    if (prev && typeof prev === "object" && hasQuotaInfo(prev) && quotaSnapTs(prev) > quotaSnapTs(lq)) {
      return;
    }
    // Merge DB + memory. Never let an error-only last_quota wipe Free/用量.
    let merged;
    if (typeof mergeQuotaSnapClient === "function") {
      merged = mergeQuotaSnapClient(prev || {}, lq) || lq;
    } else {
      merged = { ...(prev || {}), ...lq };
    }
    merged = { ...merged, account_id: id, cached: true };
    try { delete merged.probing; } catch (_) { merged.probing = false; }
    // Pure error shell with no type/usage: ignore for paint (show 未查询 instead of 查询失败).
    if (!hasQuotaInfo(merged)) {
      // Keep previous good cache if any; otherwise leave empty so cell shows 未查询.
      if (prev && hasQuotaInfo(prev)) {
        quotaCache[id] = {
          ...prev,
          account_id: id,
          cached: true,
          last_error: merged.error || prev.last_error,
          last_error_at: merged.fetched_at || prev.last_error_at,
        };
      }
      return;
    }
    quotaCache[id] = merged;
  });
  try {
    clearTimeout(window.__g2aQuotaStoreT);
    window.__g2aQuotaStoreT = setTimeout(() => { try { saveQuotaCacheToStorage(); } catch (_) {} }, 400);
  } catch (_) {}
}


async function hydrateQuotaCacheFromDB() {
  // Page rows already embed last_quota from DB — just project them into quotaCache.
  // Do NOT call /accounts/quota?cached=1 (that scans the whole pool and freezes UI).
  // Do NOT re-render the full table here — that jumps scroll right after a probe/quota click.
  try {
    applyQuotaCacheFromAccounts(accountsList);
    _quotaCacheHydrated = true;
  } catch (_) {
    // ignore
  }
}

function resolveAccountsListQuery() {
  // Shared by loadAccountsPage + hotRefreshAccountsPage so filters/page size never diverge.
  const q = (accountsSearchQuery || ($("acc-search") && $("acc-search").value) || "").trim();
  accountsSearchQuery = q;
  // Sort preference only when full list; filtered views always use stable newest.
  if (accountsSortAllowed() && $("acc-sort") && $("acc-sort").value) {
    accountsSort = $("acc-sort").value;
  }
  try { syncAccountSortControl(); } catch (_) {}
  // Prefer chips state (accountsStatusFilter). Only read select if present.
  if ($("acc-filter-status") && $("acc-filter-status").value) {
    accountsStatusFilter = $("acc-filter-status").value || accountsStatusFilter || "";
  }
  // SSO select: only apply when NO status chip is active. Status chips represent
  // full-pool counters; stacking has_sso silently drops rows (冷却 8 → 有SSO 2).
  if (accountsStatusFilter) {
    accountsSsoFilter = "";
    if ($("acc-filter-sso") && $("acc-filter-sso").value) {
      try { $("acc-filter-sso").value = ""; } catch (_) {}
    }
  } else if ($("acc-filter-sso")) {
    accountsSsoFilter = $("acc-filter-sso").value || "";
  }
  const sort = (typeof effectiveAccountsSort === "function") ? effectiveAccountsSort() : (accountsSort || "newest");
  // When filtering a small status bucket, use a page size large enough to show all matches
  // (e.g. 冷却中 only has a handful of rows — never look like "only 2" because of paging).
  let pageSize = accountsPageSize || 25;
  if (accountsStatusFilter && pageSize < 50) pageSize = 50;
  const page = accountsPage || 1;
  const ssoQs = (accountsSsoFilter === "1" || accountsSsoFilter === "0")
    ? `&has_sso=${encodeURIComponent(accountsSsoFilter === "1" ? "true" : "false")}`
    : "";
  const statusQs = accountsStatusFilter
    ? `&status=${encodeURIComponent(accountsStatusFilter)}`
    : "";
  const path =
    `/accounts?page=${encodeURIComponent(page)}&page_size=${encodeURIComponent(pageSize)}` +
    `&q=${encodeURIComponent(q)}&sort=${encodeURIComponent(sort)}${ssoQs}${statusQs}`;
  return { q, sort, page, pageSize, path, ssoQs, statusQs };
}

// Clear a stuck "加载账号中…" if a prior load never finished (tab freeze / hung fetch).
function accountsLoadWatchdog(maxMs = 45000) {
  if (!accountsLoading) return false;
  const started = accountsLoadingSince || 0;
  if (started && (Date.now() - started) < maxMs) return false;
  console.warn("[accounts] load watchdog: clearing stuck accountsLoading",
    "ageMs", started ? (Date.now() - started) : "unknown", "seq", accountsLoadSeq);
  accountsLoading = false;
  accountsLoadingSince = 0;
  // Bump seq so any still-pending older fetch is treated as stale.
  accountsLoadSeq++;
  return true;
}

async function loadAccountsPage({ reset = false, silent = false } = {}) {
  try { ensureQuotaCacheHydrated(); } catch (_) {}
  // Soft-nav can leave a hung previous load with accountsLoading=true; recover first.
  accountsLoadWatchdog(12000);
  // Capture tbody only for the initial "加载中" paint. Soft-nav may replace DOM
  // while we await /accounts — always re-query before writing error/empty states.
  let tbody = $("accounts-tbody");
  if (reset) accountsPage = 1;
  // Only flash "加载中" when the table is empty or caller wants a full reload.
  // Background re-sync / soft-nav re-entry keeps current rows visible until data lands.
  const hasRows = !!(accountsList && accountsList.length);
  if (tbody && (!silent || !hasRows)) {
    tbody.innerHTML = `<tr><td colspan="10" class="g2a-muted">加载账号中…</td></tr>`;
  }
  accountsLoading = true;
  accountsLoadingSince = Date.now();
  const seq = ++accountsLoadSeq;
  const { q, sort, page, pageSize, path } = resolveAccountsListQuery();
  try {
    const data = await api(path);
    if (seq !== accountsLoadSeq) return;
    const rawAccounts = Array.isArray(data && data.accounts) ? data.accounts : [];
    // Merge with prior rows so a just-finished probe/quota is not wiped by async PG lag.
    accountsList = mergeAccountsFromServer(rawAccounts);
    applyQuotaCacheFromAccounts(accountsList);
    // Auto-query accounts that still have no quota info (current page).
    try {
      // Debounce: page loads / soft-nav can fire many times; only fill missing 额度.
      clearTimeout(window.__g2aQuotaMissingT);
      window.__g2aQuotaMissingT = setTimeout(() => {
        if ((document.body && document.body.dataset.page) !== "accounts") return;
        const chk = $("chk-quota-live");
        if (chk && !chk.checked) return;
        // Only when something is still missing — skip if page already filled from DB.
        const miss = (accountsList || []).some((a) => a && a.id && !hasQuotaInfo(accountQuotaSnapshot(a.id)));
        if (!miss) return;
        refreshVisibleQuotaLive({ silent: true, forceMissing: true }).catch(() => {});
      }, 2500);
    } catch (_) {}
    accountsTotal = Number(data.total != null ? data.total : (data.account_count || accountsList.length)) || 0;
    accountsTotalPages = Number(data.total_pages || Math.max(1, Math.ceil((accountsTotal || 0) / pageSize))) || 1;
    accountsPage = Number(data.page || page) || 1;
    accountsPageSize = Number(data.page_size || pageSize) || pageSize;
    // Keep user-selected sort authoritative. Only adopt server sort when the
    // select is empty/unknown — never force "newest" over a pending choice.
    if (data.sort && typeof accountsSortAllowed === "function" && accountsSortAllowed()) {
      const s = String(data.sort || "").trim();
      const cur = String(accountsSort || ($("acc-sort") && $("acc-sort").value) || "newest").trim();
      if (s && s !== "cooldown_first" && s !== "disabled_first") {
        // Prefer explicit user selection (cur) when server echoes a different value
        // due to older normalize bugs or race; only fill when cur is empty.
        const prefer = cur || s;
        accountsSort = prefer;
        if ($("acc-sort") && $("acc-sort").value !== prefer) {
          try { $("acc-sort").value = prefer; } catch (_) {}
        }
      }
    }
    try { syncAccountSortControl(); } catch (_) {}
    if (data.pool && typeof data.pool === "object" && Object.keys(data.pool).length) {
      if (statusCache) statusCache.pool = Object.assign({}, statusCache.pool || {}, data.pool);
      if (dashCache) dashCache.pool = Object.assign({}, dashCache.pool || {}, data.pool);
      try { renderStats(); } catch (_) {}
    } else {
      // List response may omit pool on older builds — refresh /status for accurate counters.
      // Re-check seq after this nested await so a newer load owns paint rights.
      try {
        const st = await api("/status");
        if (seq !== accountsLoadSeq) return;
        statusCache = st || statusCache;
        if (st && st.pool) {
          if (statusCache) statusCache.pool = st.pool;
          if (dashCache) dashCache.pool = Object.assign({}, dashCache.pool || {}, st.pool);
        }
        try { renderStats(); } catch (_) {}
      } catch (_) {
        if (seq !== accountsLoadSeq) return;
      }
    }
    if (seq !== accountsLoadSeq) return;
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
      "statusFilter", accountsStatusFilter || "(all)",
      "ssoFilter", accountsSsoFilter || "(all)",
      "store", window.__g2aAccountsStore.source,
      "pool.in_cooldown", (data.pool && data.pool.in_cooldown)
    );
    if (accountsStatusFilter === "cooldown" && data.pool && data.pool.in_cooldown != null
      && Number(data.total) !== Number(data.pool.in_cooldown)) {
      console.warn("[accounts] cooldown filter total != pool.in_cooldown", data.total, data.pool.in_cooldown,
        "ssoFilter", accountsSsoFilter, "q", accountsSearchQuery);
      // Auto-heal: if SSO/search is the only reason for shrink, surface it.
      if (accountsSsoFilter || accountsSearchQuery) {
        try {
          toast(
            `冷却中全库 ${data.pool.in_cooldown} 个；当前额外筛选后只显示 ${data.total} 个` +
            (accountsSsoFilter === "1" ? "（仅有SSO）" : accountsSsoFilter === "0" ? "（仅无SSO）" : "") +
            (accountsSearchQuery ? `（关键词: ${accountsSearchQuery}）` : ""),
            false
          );
        } catch (_) {}
      }
    }
    withAccountsScrollStable(() => {
      try { renderAccountStatusChips(); } catch (_) {}
      renderAccountsPage();
    });
    hydrateQuotaCacheFromDB();
    _lastAccountsHotAt = Date.now();
  } catch (e) {
    if (seq !== accountsLoadSeq) return;
    console.error("[accounts] load failed", e);
    if (!silent) toast(e.message || "加载账号失败", false);
    // Soft-nav may have swapped DOM during the failed fetch — re-query tbody.
    tbody = $("accounts-tbody");
    // Keep previous rows if this was a silent refresh and we already had data.
    if (tbody && !(silent && accountsList && accountsList.length)) {
      tbody.innerHTML = `<tr><td colspan="10" class="g2a-muted">加载失败：${esc(e.message || e)}</td></tr>`;
    }
  } finally {
    // Stale seq must not clear a newer in-flight load (double-fetch race).
    if (seq === accountsLoadSeq) {
      accountsLoading = false;
      accountsLoadingSince = 0;
    }
    // Always re-enable pager buttons after any load attempt.
    try { syncAccountsPagerControls(); } catch (_) {}
  }
}


function applyAccountSearch(resetPage = true) {
  accountsSearchQuery = $("acc-search") ? $("acc-search").value.trim() : "";
  if (resetPage) accountsPage = 1;
  try { syncAccountSortControl(); } catch (_) {}
  loadAccountsPage({ reset: !!resetPage });
}


// Durable header select-all binding (survives soft-nav DOM swaps; no rebind race).
if (!document._g2aAccCheckPageBound) {
  document._g2aAccCheckPageBound = true;
  document.addEventListener("change", (e) => {
    const t = e && e.target;
    if (!t || t.id !== "acc-check-page") return;
    try { setPageSelection(!!t.checked); } catch (_) {}
  }, true);
}

function accountIdKey(id) {
  // Normalize ids so Set.has() works for both string data-id and numeric API ids.
  if (id == null || id === "") return "";
  return String(id);
}

function setPageSelection(checked) {
  const want = !!checked;
  document.querySelectorAll(".acc-check-one").forEach(el => {
    const id = accountIdKey(el.dataset.id);
    if (!id) return;
    el.checked = want;
    if (want) selectedAccountIds.add(id);
    else selectedAccountIds.delete(id);
  });
  // Keep header checkbox in sync (avoid stuck indeterminate after manual toggle).
  const pageCheck = $("acc-check-page");
  if (pageCheck) {
    pageCheck.indeterminate = false;
    pageCheck.checked = want && document.querySelectorAll(".acc-check-one").length > 0;
  }
  updateAccountSelectionInfo(getFilteredAccounts().length, document.querySelectorAll(".acc-check-one").length);
}

function setFilteredSelection(checked) {
  const list = getFilteredAccounts();
  list.forEach(a => {
    const id = accountIdKey(a && a.id);
    if (!id) return;
    if (checked) selectedAccountIds.add(id);
    else selectedAccountIds.delete(id);
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
  if (!partial || partial.id == null || partial.id === "") return null;
  const id = accountIdKey(partial.id);
  let updated = null;
  let found = false;
  accountsList = (accountsList || []).map((a) => {
    if (accountIdKey(a.id) !== id) return a;
    found = true;
    const next = { ...a, ...partial, id: a.id };
    // Deep-merge pool so probe/quota patches always refresh status cells.
    if (partial._pool || a._pool) {
      const base = { ...(a._pool || { id: a.id }) };
      const patch = { ...(partial._pool || {}) };
      // Prefer patch last_quota / last_probe entirely (do not half-merge maps).
      if (patch.last_quota) base.last_quota = patch.last_quota;
      if (patch.last_probe) base.last_probe = patch.last_probe;
      next._pool = { ...base, ...patch, id: a.id };
    }
    // keep expired flag coherent when expires_at changes
    if (partial.expires_at != null) {
      const exp = Number(partial.expires_at);
      if (Number.isFinite(exp) && exp < 1e12) next.expired = exp * 1000 <= Date.now();
      else if (Number.isFinite(exp)) next.expired = exp > 0 && exp <= Date.now();
    }
    // Sync top-level expired when pool says expired.
    if (next._pool && (next._pool.pool_status === "expired" || next._pool.token_expired_at)) {
      next.expired = true;
    }
    updated = next;
    return next;
  });
  if (!found) {
    const realId = partial.id;
    updated = { id: realId, ...partial, _pool: { id: realId, ...(partial._pool || {}) } };
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
  const liveQ = quotaCache[accountIdKey(a.id)] || quotaCache[a.id];
  const probeCell = fmtProbeCell(p.last_probe, p.last_error, p.blocked_model_ids);
  const checked = selectedAccountIds.has(accountIdKey(a.id)) ? "checked" : "";
  const expiryCell = fmtExpiry(a.expires_at);
  const typeCell = fmtAccountTypeCell({ ...p, id: a.id, account_type: a.account_type || p.account_type, plan: a.plan || p.plan, plan_label: a.plan_label || p.plan_label, last_quota: p.last_quota }, liveQ);
  return `
    <tr data-acc-id="${esc(a.id)}">
      <td><input type="checkbox" class="acc-check-one" data-id="${esc(a.id)}" ${checked} /></td>
      <td>${esc(a.email || "—")}<div class="muted mono" style="font-size:0.72rem">${esc(a.id)}</div></td>
      <td>${a.expired ? '<span class="g2a-tag bad">已过期</span>' : '<span class="g2a-tag ok">有效</span>'}</td>
      <td style="min-width:88px">${typeCell}</td>
      <td>${poolLabel}</td>
      <td class="g2a-muted" style="font-size:0.8rem">${usage}</td>
      <td style="font-size:0.82rem;min-width:150px">${fmtQuotaCell({ ...p, id: a.id }, liveQ)}</td>
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


// Keep window + table wrap scroll when we rewrite a single account row.
// Model test / quota / renew used to call loadAccountsPage and re-render the
// whole tbody, which jumped the page under the click.
function captureAccountsScroll() {
  const wrap = document.querySelector("#accounts-tbody")
    ? (document.querySelector("#accounts-tbody").closest(".g2a-table-wrap")
      || document.querySelector(".g2a-table-wrap"))
    : document.querySelector(".g2a-table-wrap");
  return {
    y: window.scrollY || document.documentElement.scrollTop || 0,
    x: window.scrollX || document.documentElement.scrollLeft || 0,
    wrap,
    wrapTop: wrap ? wrap.scrollTop : 0,
    wrapLeft: wrap ? wrap.scrollLeft : 0,
  };
}

function restoreAccountsScroll(snap) {
  if (!snap) return;
  const apply = () => {
    try {
      window.scrollTo(snap.x || 0, snap.y || 0);
      if (snap.wrap) {
        snap.wrap.scrollTop = snap.wrapTop || 0;
        snap.wrap.scrollLeft = snap.wrapLeft || 0;
      }
    } catch (_) {}
  };
  apply();
  // Second frame: layout after outerHTML/chip rewrite may settle late.
  try { requestAnimationFrame(apply); } catch (_) { setTimeout(apply, 0); }
}

function withAccountsScrollStable(fn) {
  const snap = captureAccountsScroll();
  try {
    return fn();
  } finally {
    restoreAccountsScroll(snap);
  }
}

// Soft-refresh only pool chips / overview stats (no accounts tbody rewrite).
async function softRefreshPoolChips({ stats = true } = {}) {
  try {
    const st = await api("/status");
    statusCache = st || statusCache;
    if (st && st.pool) {
      if (statusCache) statusCache.pool = st.pool;
      if (dashCache) dashCache.pool = Object.assign({}, dashCache.pool || {}, st.pool);
    }
    withAccountsScrollStable(() => {
      try { renderAccountStatusChips(); } catch (_) {}
      if (stats) {
        try { renderStats(); } catch (_) {}
      }
    });
  } catch (_) {}
}

function patchAccountRowById(id) {
  if (id == null || id === "") return;
  const key = accountIdKey(id);
  const acc = (accountsList || []).find((a) => accountIdKey(a.id) === key);
  if (!acc) {
    // Missing from in-memory page — do NOT full re-render (scroll jump). Leave as-is.
    return;
  }
  withAccountsScrollStable(() => {
    let row = null;
    try {
      row = document.querySelector(`tr[data-acc-id="${CSS.escape(String(id))}"]`);
    } catch (_) {
      row = document.querySelector(`tr[data-acc-id="${String(id).replace(/"/g, '\\"')}"]`);
    }
    const html = renderOneAccountRow(acc);
    if (row) {
      // Replace only this <tr>; keep the rest of the tbody intact so the page
      // does not jump under the user's click.
      const tmp = document.createElement("tbody");
      tmp.innerHTML = html.trim();
      const next = tmp.firstElementChild;
      if (next) row.replaceWith(next);
      else row.outerHTML = html;
    }
    // If the row is off-page / filtered out, memory is still updated via upsert.
  });
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
  // Live status cell hint while busy (temporary overlay on 状态 column).
  if (row) {
    const statusCell = row.children && row.children[3];
    if (statusCell) {
      if (busy && label) {
        if (!statusCell.dataset.prevHtml) statusCell.dataset.prevHtml = statusCell.innerHTML;
        statusCell.innerHTML = `<span class="g2a-tag warn">${esc(label)}</span>`;
      } else if (!busy) {
        // Drop busy overlay only. Caller (probe/quota/renew) already live-patches
        // the row with the real status — re-painting here double-jumps the page.
        if (statusCell.dataset.prevHtml) {
          // If no live patch replaced the row yet, restore previous status HTML.
          if (statusCell.innerHTML.includes(label || "中") || /探测中|查询中|续期中|处理中|移除中/.test(statusCell.textContent || "")) {
            try { statusCell.innerHTML = statusCell.dataset.prevHtml; } catch (_) {}
          }
          delete statusCell.dataset.prevHtml;
        }
      }
    }
  }
}

function poolPatchFromProbeResponse(r) {
  const res = (r && (r.result || r)) || {};
  const pool = (r && r.pool) || {};
  const ok = !!(r && (r.ok || res.available));
  const nowSec = Math.floor(Date.now() / 1000);
  // Prefer full DB pool view (after SaveLastProbe / Kick / ClearCooldown).
  let patch = {};
  if (pool && typeof pool === "object" && Object.keys(pool).length) {
    patch = poolPatchFromStatusAccount({ pool });
  }
  // Probe handlers now also fetch quota (type + usage) for new accounts.
  // Prefer response.quota / pool.last_quota so 类型/额度 cells paint without extra click.
  const quotaSnap = (r && r.quota && typeof r.quota === "object")
    ? r.quota
    : (pool && pool.last_quota && typeof pool.last_quota === "object" ? pool.last_quota : null);
  if (quotaSnap) {
    patch.last_quota = quotaSnap;
    if (quotaSnap.account_type) {
      patch.account_type = quotaSnap.account_type;
      patch.plan = quotaSnap.account_type;
    }
    if (quotaSnap.plan_label) patch.plan_label = quotaSnap.plan_label;
  }
  if (r && r.account_type && !patch.account_type) {
    patch.account_type = r.account_type;
    patch.plan = r.account_type;
  }
  if (r && r.plan_label && !patch.plan_label) patch.plan_label = r.plan_label;
  // Always build the live probe snapshot from this response (result wins).
  // Do NOT prefer pool.last_probe: probe-batch defers SaveLastProbe, so the DB
  // view is often one probe behind and the UI would keep showing the old cell.
  const liveProbe = {
    ...(pool && pool.last_probe && typeof pool.last_probe === "object" ? pool.last_probe : {}),
    available: ok,
    ok,
    model: res.model || (pool && pool.last_probe && pool.last_probe.model) || pool.cooldown_model || null,
    latency_ms: res.latency_ms != null ? res.latency_ms : (pool && pool.last_probe && pool.last_probe.latency_ms),
    status_code: res.status_code != null ? res.status_code : (pool && pool.last_probe && pool.last_probe.status_code),
    error: ok
      ? null
      : (res.error || (r && r.error) || (pool && pool.last_probe && pool.last_probe.error) || null),
    probed_at: res.probed_at || nowSec,
    source: res.source || (pool && pool.last_probe && pool.last_probe.source) || "manual",
    kicked_cooldown: !!(res.kicked_cooldown || (pool && pool.last_probe && pool.last_probe.kicked_cooldown)),
    model_blocked: !!(res.model_blocked || (pool && pool.last_probe && pool.last_probe.model_blocked)),
    recovered: !!(res.recovered || (pool && pool.last_probe && pool.last_probe.recovered)),
    unblocked_model: res.unblocked_model || (pool && pool.last_probe && pool.last_probe.unblocked_model) || null,
    blocked_model: res.blocked_model || (pool && pool.last_probe && pool.last_probe.blocked_model) || null,
    cooldown_code: res.cooldown_code || (pool && pool.cooldown_code) || null,
    failure_class: res.failure_class || null,
  };
  patch.last_probe = liveProbe;
  // Live result always wins for status; pool.last_probe_status may lag (async SaveLastProbe).
  patch.last_probe_status = ok ? "ok" : "fail";
  patch.last_error = ok
    ? null
    : (res.error || (r && r.error) || pool.last_error || null);
  if (res.model_blocked && res.blocked_model) {
    const ids = Array.isArray(patch.blocked_model_ids) ? patch.blocked_model_ids.slice() : [];
    if (!ids.includes(res.blocked_model)) ids.push(res.blocked_model);
    patch.blocked_model_ids = ids;
    patch.blocked_models = { ...(patch.blocked_models || {}), [res.blocked_model]: true };
    if (!patch.pool_status || patch.pool_status === "normal" || patch.pool_status === "live") {
      // Keep model_blocked visible when only one model is soft-blocked.
      if (!patch.in_cooldown) patch.pool_status = "model_blocked";
    }
  }
  // Kick flags from result OR pool (handler may only set one). Force cooldown state so
  // the status column shows 冷却中 immediately, not only a probe "报错" pill.
  const kicked = !!(res.kicked_cooldown || liveProbe.kicked_cooldown || pool.in_cooldown
    || pool.pool_status === "cooldown"
    || (Number(pool.cooldown_remaining_sec || 0) > 0));
  if (kicked && !ok) {
    patch.in_cooldown = true;
    patch.pool_status = "cooldown";
    if (res.cooldown_code) patch.cooldown_code = res.cooldown_code;
    else if (pool.cooldown_code) patch.cooldown_code = pool.cooldown_code;
    if (res.model) patch.cooldown_model = res.model;
    else if (pool.cooldown_model) patch.cooldown_model = pool.cooldown_model;
    patch.cooldown_count = Math.max(1, Number(patch.cooldown_count || pool.cooldown_count || 0) || 1);
    if (pool.cooldown_until != null) patch.cooldown_until = pool.cooldown_until;
    if (pool.cooldown_remaining_sec != null) {
      patch.cooldown_remaining_sec = pool.cooldown_remaining_sec;
    } else if (pool.cooldown_until != null) {
      const until = Number(pool.cooldown_until);
      if (Number.isFinite(until) && until > 0) {
        const untilSec = until > 1e12 ? Math.floor(until / 1000) : Math.floor(until);
        patch.cooldown_remaining_sec = Math.max(0, untilSec - nowSec);
      }
    }
  }
  if (ok && (res.recovered || (pool.pool_status === "normal" && !pool.in_cooldown) || !kicked)) {
    // Successful probe: leave cooldown only when kick did not re-apply.
    if (res.recovered || !kicked) {
      patch.in_cooldown = false;
      if (patch.pool_status === "cooldown") patch.pool_status = "normal";
    }
  }
  // Fallback when backend older / no pool payload.
  if (!r || !r.pool || !Object.keys(pool).length) {
    if (ok) {
      patch.in_cooldown = false;
      patch.cooldown_count = 0;
      patch.cooldown_until = null;
      patch.cooldown_remaining_sec = 0;
      patch.pool_status = patch.pool_status || "normal";
      patch.consecutive_fails = 0;
    } else if (
      res.kicked_cooldown
      || /free-usage-exhausted|free usage|subscription:free-usage|rate.?limit|429/i.test(
        String(res.error || (r && r.error) || "")
      )
    ) {
      patch.in_cooldown = true;
      patch.pool_status = "cooldown";
      patch.cooldown_count = Math.max(1, Number(patch.cooldown_count || 0) || 1);
      patch.cooldown_code = patch.cooldown_code || res.cooldown_code || "subscription:free-usage-exhausted";
    }
  }
  return patch;
}

function applyAccountLivePatch(id, partial) {
  if (id == null || id === "") return;
  const patch = partial || {};
  upsertAccountInList({ id, ...patch });
  // Always re-paint the row so status / 额度 / 模型测试 cells update immediately.
  try { patchAccountRowById(id); } catch (_) {
    try { refreshOneAccountLocal(id, patch); } catch (__) {}
  }
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
        {
          const row = (accountsList || []).find((a) => String(a.id) === String(id));
          const p = (row && row._pool) || {};
          let nextStatus = p.pool_status;
          if (p.disabled_for_quota || nextStatus === "quota_disabled") nextStatus = "quota_disabled";
          else if (p.enabled === false || nextStatus === "disabled") nextStatus = "disabled";
          else nextStatus = "normal";
          applyAccountLivePatch(id, {
            expires_at: x.expires_at,
            expired: false,
            has_refresh_token: x.has_refresh_token != null ? x.has_refresh_token : true,
            _pool: {
              last_error: null,
              last_renew_status: "ok",
              renew_fail_count: 0,
              token_expired_at: null,
              token_expired_reason: null,
              in_cooldown: false,
              cooldown_until: null,
              cooldown_count: 0,
              pool_status: nextStatus,
            },
          });
        }
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
    // Rows already live-patched; only soft-refresh pool chips (no full list reload).
    Promise.resolve().then(() => softRefreshPoolChips({ stats: true }));
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
try { bindAccountsPagerControls(); } catch (_) {}
if ($("acc-sort") && !$("acc-sort").onchange) {
  try {
    const saved = localStorage.getItem("g2a_accounts_sort");
    if (saved && saved !== "cooldown_first" && saved !== "disabled_first") {
      accountsSort = saved; $("acc-sort").value = saved;
    }
  } catch (_) {}
  $("acc-sort").onchange = () => {
    if (typeof accountsSortAllowed === "function" && !accountsSortAllowed()) {
      try { syncAccountSortControl(); } catch (_) {}
      return;
    }
    accountsSort = ($("acc-sort").value || "newest");
    try { localStorage.setItem("g2a_accounts_sort", accountsSort); } catch (_) {}
    accountsPage = 1;
    loadAccountsPage({ reset: true });
  };
  try { syncAccountSortControl(); } catch (_) {}
}

if ($("acc-filter-sso") && !$("acc-filter-sso").onchange) {
  try {
    if (accountsStatusFilter) {
      accountsSsoFilter = "";
      $("acc-filter-sso").value = "";
      try { localStorage.setItem("g2a_accounts_sso_filter", ""); } catch (_) {}
    } else {
      const savedSso = localStorage.getItem("g2a_accounts_sso_filter");
      if (savedSso === "1" || savedSso === "0" || savedSso === "") {
        accountsSsoFilter = savedSso || "";
        $("acc-filter-sso").value = accountsSsoFilter;
      }
    }
  } catch (_) {}
  $("acc-filter-sso").onchange = () => {
    accountsSsoFilter = ($("acc-filter-sso").value || "");
    try { localStorage.setItem("g2a_accounts_sso_filter", accountsSsoFilter); } catch (_) {}
    accountsPage = 1;
    if (accountsSsoFilter && accountsStatusFilter) {
      try {
        toast("已叠加「" + accountStatusFilterLabel(accountsStatusFilter) + "」+ " +
          (accountsSsoFilter === "1" ? "有SSO" : "无SSO") + " 筛选；数量会少于顶部统计", false);
      } catch (_) {}
    }
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
// Header checkbox is bound via document-level change listener + rebindPageControls.
if ($("acc-check-page")) {
  on("acc-check-page", "onchange", (e) => setPageSelection(!!(e && e.target && e.target.checked)));
}



function currentPoolStatusKey(a, p) {
  p = p || {};
  a = a || {};
  // Sticky cool only: in_cooldown / pool_status / live until.
  // cooldown_count is stack depth (叠加×N), NOT "still cooling".
  const remain = Number(p.cooldown_remaining_sec || 0) || 0;
  const cooling = !!(p.in_cooldown === true || p.pool_status === "cooldown" || remain > 0);
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
  if (quotaOff) return "quota_disabled";
  if (expired) return "expired";
  if (p.enabled === false || p.pool_status === "disabled") return "disabled";
  if (cooling) return "cooldown";
  if (modelBlocked) return "model_blocked";
  return "live";
}


function poolPatchFromStatusAccount(acc) {
  if (!acc || typeof acc !== "object") return {};
  // API returns full pool view as account/pool (DB write result).
  const p = acc._pool || acc.pool || acc;
  const blocked = p.blocked_models && typeof p.blocked_models === "object" ? p.blocked_models : {};
  const blockedIds = Array.isArray(p.blocked_model_ids)
    ? p.blocked_model_ids
    : Object.keys(blocked);
  const out = {
    enabled: p.enabled !== false && p.enabled !== 0,
    disabled_for_quota: !!p.disabled_for_quota,
    disabled_reason: p.disabled_reason ?? null,
    quota_disabled_at: p.quota_disabled_at ?? null,
    quota_source: p.quota_source ?? null,
    in_cooldown: !!p.in_cooldown,
    cooldown_until: p.cooldown_until ?? null,
    cooldown_remaining_sec: p.cooldown_remaining_sec ?? 0,
    cooldown_count: p.cooldown_count ?? 0,
    cooldown_reason: p.cooldown_reason ?? null,
    cooldown_code: p.cooldown_code ?? null,
    cooldown_model: p.cooldown_model ?? null,
    cooldown_tokens_actual: p.cooldown_tokens_actual ?? null,
    cooldown_tokens_limit: p.cooldown_tokens_limit ?? null,
    pool_status: p.pool_status || "normal",
    blocked_models: blocked,
    blocked_model_ids: blockedIds,
    last_error: p.last_error ?? null,
    last_probe: p.last_probe ?? null,
    last_probe_status: p.last_probe_status ?? null,
    last_quota: p.last_quota ?? null,
    status_stack: Array.isArray(p.status_stack) ? p.status_stack : undefined,
    consecutive_fails: p.consecutive_fails,
    probe_fail_streak: p.probe_fail_streak,
    token_expired_at: p.token_expired_at ?? null,
    token_expired_reason: p.token_expired_reason ?? null,
    request_count: p.request_count,
    success_count: p.success_count,
    fail_count: p.fail_count,
  };
  return out;
}


function poolStatusLabel(a, p) {
  const enabled = p.enabled !== false;
  // Sticky cool only — stack depth (cooldown_count / 叠加×N) is no longer shown.
  const remain = Number(p.cooldown_remaining_sec || 0) || 0;
  const cooling = !!(
    p.in_cooldown === true
    || p.pool_status === "cooldown"
    || remain > 0
  );
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
    poolLabel = `<span class="g2a-tag bad" title="${esc(tip)}">额度冷却</span>`;
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
    // Sticky cool: one state only — no stack-depth overlay (叠×N) on the pill.
    const tip = [
      "冷却中（粘性状态，测活/调用成功后解除）",
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

function poolPatchFromQuotaResult(q) {
  if (!q || typeof q !== "object") return null;
  const exhausted = !!(q.exhausted || q.auto_disabled);
  const ok = !!q.ok && !exhausted;
  const fromDB = {};
  fromDB.last_quota = q;
  if (exhausted) {
    // Free/paid quota exhaust → cooldown pool (not permanent 额度禁用).
    fromDB.disabled_for_quota = false;
    fromDB.enabled = true;
    fromDB.pool_status = "cooldown";
    fromDB.in_cooldown = true;
    fromDB.cooldown_reason = q.exhaust_reason || (q.display && q.display.summary) || q.summary || "额度已耗尽";
    const src = String(q.source || "");
    const free = q.free_tokens || q.account_type === "free" || src === "free_tokens" || src === "free";
    fromDB.cooldown_code = free ? "subscription:free-usage-exhausted" : "billing_quota";
    const coolSec = free ? 2 * 3600 : 6 * 3600;
    fromDB.cooldown_until = Math.floor(Date.now() / 1000) + coolSec;
    if (q.tokens_used != null) fromDB.cooldown_tokens_actual = q.tokens_used;
    else if (q.tokens_actual != null) fromDB.cooldown_tokens_actual = q.tokens_actual;
    if (q.tokens_limit != null) fromDB.cooldown_tokens_limit = q.tokens_limit;
    fromDB.disabled_reason = null;
    fromDB.quota_source = null;
    fromDB.quota_disabled_at = null;
  } else if (ok) {
    fromDB.disabled_for_quota = false;
    fromDB.enabled = true;
    if (q.in_cooldown) {
      fromDB.in_cooldown = true;
      fromDB.pool_status = q.pool_status || "cooldown";
    } else {
      fromDB.in_cooldown = false;
      fromDB.pool_status = q.pool_status || "normal";
      // Clear free-usage cool when healthy.
      if (String(q.pool_status || "") !== "cooldown") {
        fromDB.cooldown_until = null;
        fromDB.cooldown_code = null;
        fromDB.cooldown_reason = null;
      }
    }
    fromDB.disabled_reason = null;
    fromDB.quota_disabled_at = null;
    fromDB.quota_source = null;
  }
  return fromDB;
}



/** True when quota snapshot has displayable usage (token or dollar). */
function hasQuotaInfo(q) {
  if (!q || typeof q !== "object") return false;
  // Soft lock from live refresh must not count as durable quota.
  if (q.probing && !q.account_type && !q.plan && q.tokens_limit == null && q.monthly_limit == null && !q.summary) {
    return false;
  }
  if (q.tokens_limit != null || q.tokens_remaining != null || q.tokens_used != null || q.tokens_actual != null) return true;
  // monthly_limit may be 0 for free/$0 billing — still counts as "queried".
  if (q.monthly_limit != null || q.used != null || q.remaining != null) return true;
  if (Number(q.weekly_limit) > 0 || q.weekly_used != null) return true;
  if (q.account_type || q.plan || q.free_tokens) return true;
  if (q.display && q.display.summary && q.display.summary !== "—" && !/查 token|未查询/.test(String(q.display.summary))) return true;
  if (q.summary && String(q.summary) !== "—" && !/未查询|查 token/.test(String(q.summary))) return true;
  if (q.ok === true && !q.error) return true;
  return false;
}

function accountQuotaSnapshot(id) {
  const key = accountIdKey(id);
  if (!key) return null;
  // Prefer normalized cache key; also try raw id for legacy entries.
  const live = quotaCache[key] || quotaCache[id];
  if (live && typeof live === "object" && hasQuotaInfo(live) && !live.probing) return live;
  const row = (accountsList || []).find((a) => a && accountIdKey(a.id) === key);
  const lq = row && row._pool && row._pool.last_quota;
  if (lq && typeof lq === "object" && hasQuotaInfo(lq)) return lq;
  // Fall back to any live entry (may only have error / probing).
  if (live && typeof live === "object" && hasQuotaInfo(live)) return live;
  if (lq && typeof lq === "object") return lq;
  return (live && typeof live === "object") ? live : null;
}

function isQuotaStale(q, nowSec) {
  if (!hasQuotaInfo(q)) return true;
  const ts = Number(q.fetched_at || 0);
  if (!ts) return true;
  return (nowSec - ts) > QUOTA_STALE_SEC;
}

// mergeQuotaSnapClient: keep durable type/usage when a live probe is error-only.
// Mirrors server mergeQuotaSnapshots so the UI never paints "查询失败" over good data.
function mergeQuotaSnapClient(prev, next) {
  if (!next || typeof next !== "object") return prev || next || null;
  if (!prev || typeof prev !== "object" || !Object.keys(prev).length) return next;
  const out = { ...prev };
  for (const [k, v] of Object.entries(next)) {
    if (v == null) continue;
    if (typeof v === "string" && !v.trim()) continue;
    out[k] = v;
  }
  // Preserve plan/usage when next is sparse/failed.
  const keep = [
    "account_type", "plan", "plan_label", "plan_source",
    "monthly_limit", "used", "remaining", "usage_percent",
    "weekly_limit", "weekly_used", "weekly_remaining", "weekly_usage_percent",
    "on_demand_cap", "on_demand_used", "prepaid_balance",
    "free_tokens", "unlimited_or_free",
    "tokens_limit", "tokens_remaining", "tokens_used", "tokens_actual", "tokens_usage_percent",
    "requests_limit", "requests_remaining", "summary", "display",
  ];
  const empty = (v) => {
    if (v == null) return true;
    if (typeof v === "string") {
      const s = v.trim().toLowerCase();
      return !s || s === "unknown" || s === "—";
    }
    return false;
  };
  for (const k of keep) {
    if (empty(out[k]) && !empty(prev[k])) out[k] = prev[k];
  }
  // Failed next must not blank previously-ok usage.
  const nextOk = next.ok === true && !(next.exhausted || next.auto_disabled);
  const prevOk = prev.ok === true && !(prev.exhausted || prev.auto_disabled);
  const nextFailed = next.ok === false || (!!next.error && !nextOk);
  if (nextFailed && prevOk && !(next.exhausted || next.auto_disabled)) {
    out.ok = true;
    if (next.error) {
      out.error = next.error;
      out.last_error_at = next.fetched_at || Math.floor(Date.now() / 1000);
    }
    // Keep previous exhausted state unless next explicitly sets it.
    if (next.exhausted == null && prev.exhausted != null) out.exhausted = prev.exhausted;
  }
  // Never demote known plan to unknown.
  const plan = String(out.account_type || out.plan || "").toLowerCase();
  if (!plan || plan === "unknown") {
    const pp = String(prev.account_type || prev.plan || "").toLowerCase();
    if (pp && pp !== "unknown") {
      out.account_type = prev.account_type || prev.plan;
      out.plan = out.account_type;
      if (prev.plan_label) out.plan_label = prev.plan_label;
    }
  }
  if (!out.account_id) out.account_id = next.account_id || prev.account_id;
  return out;
}

function applyQuotaResultsToUI(rows) {
  const list = Array.isArray(rows) ? rows : [];
  list.forEach((q) => {
    if (!q || typeof q !== "object") return;
    const id = accountIdKey(q.account_id || q.id);
    if (!id) return;
    // Merge with existing cache / row last_quota so a failed re-probe never
    // wipes Free/SuperGrok + token usage from the painted cell.
    const prev =
      (quotaCache[id] && typeof quotaCache[id] === "object" && !quotaCache[id].probing)
        ? quotaCache[id]
        : (() => {
            const row = (accountsList || []).find((a) => a && accountIdKey(a.id) === id);
            return (row && row._pool && row._pool.last_quota) || null;
          })();
    const merged = mergeQuotaSnapClient(prev, q) || q;
    // Live result is never a soft-lock probing placeholder.
    try { delete merged.probing; } catch (_) { merged.probing = false; }
    merged.account_id = id;
    // Skip pure empty shells.
    if (!hasQuotaInfo(merged) && !merged.error) {
      return;
    }
    // Error-only result: keep previous good snapshot; only attach last_error.
    if (!hasQuotaInfo(merged) && merged.error) {
      if (prev && hasQuotaInfo(prev)) {
        quotaCache[id] = {
          ...prev,
          account_id: id,
          last_error: merged.error,
          last_error_at: merged.fetched_at || Math.floor(Date.now() / 1000),
        };
        // Still refresh row so last_error can show in title if needed — keep last_quota good.
        applyAccountLivePatch(id, {
          _pool: { last_quota: quotaCache[id] },
        });
      }
      // No previous data: do NOT paint 查询失败 for silent auto-refresh failures.
      return;
    }
    quotaCache[id] = merged;
    // Drop legacy raw-key duplicate if any.
    try { if (q.id != null && accountIdKey(q.id) === id && quotaCache[q.id] && q.id !== id) delete quotaCache[q.id]; } catch (_) {}
    const patch = (typeof poolPatchFromQuotaResult === "function")
      ? (poolPatchFromQuotaResult(merged) || {})
      : { last_quota: merged };
    if (!patch.last_quota) patch.last_quota = merged;
    // Promote type to row for type column after hard refresh paths.
    const top = { _pool: patch };
    if (merged.account_type) {
      top.account_type = merged.account_type;
      top.plan = merged.account_type;
    }
    if (merged.plan_label) top.plan_label = merged.plan_label;
    applyAccountLivePatch(id, top);
  });
  try {
    // Keep chips in sync when cool/exhaust counts change.
    softRefreshPoolChips({ stats: true });
  } catch (_) {}
  try {
    clearTimeout(window.__g2aQuotaStoreT);
    window.__g2aQuotaStoreT = setTimeout(() => { try { saveQuotaCacheToStorage(); } catch (_) {} }, 400);
  } catch (_) {}
}

function stopQuotaLiveRefresh() {
  if (quotaLiveTimer) {
    try { clearInterval(quotaLiveTimer); } catch (_) {}
    quotaLiveTimer = null;
  }
  quotaLiveInFlight = false;
}

function startQuotaLiveRefresh({ immediate = false } = {}) {
  const page = (document.body && document.body.dataset.page) || "";
  if (page !== "accounts") {
    stopQuotaLiveRefresh();
    return;
  }
  const chk = $("chk-quota-live");
  if (chk && !chk.checked) { stopQuotaLiveRefresh(); return; }
  stopQuotaLiveRefresh();
  // immediate: only fill rows still missing durable quota (DB hydrate already ran).
  // Do not re-probe every account on every soft-nav / page enter.
  if (immediate) {
    const hasMissing = (accountsList || []).some((a) => a && a.id && !hasQuotaInfo(accountQuotaSnapshot(a.id)));
    if (hasMissing) {
      refreshVisibleQuotaLive({ silent: true, forceMissing: true }).catch(() => {});
    }
  }
  quotaLiveTimer = setInterval(() => {
    try {
      if (document.hidden) return;
      const p = (document.body && document.body.dataset.page) || "";
      if (p !== "accounts") { stopQuotaLiveRefresh(); return; }
      // Timer ticks: missing first; at most one very-stale row if spare capacity.
      refreshVisibleQuotaLive({ silent: true, forceMissing: false }).catch(() => {});
    } catch (_) {}
  }, QUOTA_LIVE_INTERVAL_MS);
}

/** Live-refresh: missing quota first; round-robin so we never hammer the same ids. */
async function refreshVisibleQuotaLive({ silent = true, forceMissing = true } = {}) {
  if (quotaLiveInFlight) return;
  if (!$("accounts-tbody")) return;
  const page = (document.body && document.body.dataset.page) || "";
  if (page !== "accounts") return;
  // Normalize + dedupe ids (string keys) so probe cooldown / cache always match.
  const seenIds = new Set();
  const allIds = [];
  for (const a of (accountsList || [])) {
    const id = accountIdKey(a && a.id);
    if (!id || seenIds.has(id)) continue;
    seenIds.add(id);
    allIds.push(id);
  }
  if (!allIds.length) return;
  const now = Math.floor(Date.now() / 1000);

  const recentlyProbed = (id) => {
    const key = accountIdKey(id);
    const t = Number(quotaLiveProbedAt[key] || 0);
    return t > 0 && (now - t) < QUOTA_MIN_REQUERY_SEC;
  };

  const scored = allIds.map((id) => {
    const q = accountQuotaSnapshot(id) || {};
    // Soft-lock probing without real data still counts as missing, but
    // recentlyProbed should block re-hit until MIN_REQUERY elapses.
    const missing = !hasQuotaInfo(q) || !!(q.probing && !hasQuotaInfo({ ...q, probing: false }));
    const reallyMissing = !hasQuotaInfo(q);
    let s = 0;
    if (reallyMissing) s += QUOTA_MISSING_BOOST;
    else if (isQuotaStale(q, now)) s += 40;
    const plan = String(q.account_type || q.plan || "");
    if (!reallyMissing && (plan === "free" || q.free_tokens)) {
      const pct = Number(q.tokens_usage_percent != null ? q.tokens_usage_percent : q.usage_percent);
      if (Number.isFinite(pct) && pct >= 90) s += 15;
    }
    const age = q.fetched_at ? (now - Number(q.fetched_at)) : 99999;
    if (reallyMissing) s += Math.min(20, Math.floor(age / 60));
    return { id, s, missing: reallyMissing, stale: !reallyMissing && isQuotaStale(q, now) };
  });
  // Stable score sort, then rotate missing pool so the same 2 ids are not always first.
  scored.sort((a, b) => b.s - a.s || String(a.id).localeCompare(String(b.id)));

  let missing = scored.filter((x) => x.missing && !recentlyProbed(x.id)).map((x) => x.id);
  // Round-robin missing: rotate by session cursor so each tick advances.
  if (missing.length > QUOTA_LIVE_MAX_PER_TICK) {
    if (typeof window.__g2aQuotaRR !== "number" || window.__g2aQuotaRR < 0) window.__g2aQuotaRR = 0;
    const start = window.__g2aQuotaRR % missing.length;
    missing = missing.slice(start).concat(missing.slice(0, start));
    window.__g2aQuotaRR = (start + QUOTA_LIVE_MAX_PER_TICK) % Math.max(1, missing.length);
  }
  const stale = (!forceMissing)
    ? scored.filter((x) => x.stale && !recentlyProbed(x.id)).map((x) => x.id)
    : [];

  let picked = [];
  for (const id of missing) {
    if (picked.length >= QUOTA_LIVE_MAX_PER_TICK) break;
    if (!picked.includes(id)) picked.push(id);
  }
  // Timer path: at most 1 stale re-probe if capacity remains.
  if (!forceMissing && picked.length < QUOTA_LIVE_MAX_PER_TICK && stale.length) {
    // Prefer least-recently-probed stale.
    let best = null;
    let bestT = Infinity;
    for (const sid of stale) {
      if (picked.includes(sid)) continue;
      const t = Number(quotaLiveProbedAt[sid] || 0);
      if (t < bestT) { bestT = t; best = sid; }
    }
    if (best) picked.push(best);
  }
  if (!picked.length) return;

  // Soft-lock selected ids so overlapping timers don't re-queue the same set.
  const lockTs = Math.floor(Date.now() / 1000);
  picked.forEach((id) => {
    const key = accountIdKey(id);
    quotaLiveProbedAt[key] = lockTs;
    const prev = quotaCache[key] || {};
    if (!hasQuotaInfo(prev)) {
      quotaCache[key] = { ...prev, account_id: key, probing: true, fetched_at: lockTs };
    }
  });

  quotaLiveInFlight = true;
  try {
    let rows = [];
    try {
      const qs = picked.map(encodeURIComponent).join(",");
      const data = await api("/accounts/quota?refresh=1&ids=" + qs);
      rows = data.results || data.accounts || data.quotas || [];
    } catch (e) {
      console.warn("batch quota failed, sequential fallback", e);
      for (const id of picked.slice(0, 1)) {
        try {
          const q = await api("/accounts/" + encodeURIComponent(id) + "/quota");
          if (q) rows.push(q);
        } catch (_) {}
      }
    }
    // Always stamp attempted ids (even if server omitted them) so we do not
    // re-pick the same missing account every tick.
    const doneAt = Math.floor(Date.now() / 1000);
    const returned = new Set();
    (rows || []).forEach((q) => {
      const rid = accountIdKey(q && (q.account_id || q.id));
      if (rid) returned.add(rid);
    });
    picked.forEach((id) => {
      const key = accountIdKey(id);
      // Successful or attempted: block re-query for MIN window.
      // If still missing after apply, keep cooldown so we rotate to others first.
      quotaLiveProbedAt[key] = doneAt;
    });
    applyQuotaResultsToUI(rows);
    // After apply: if still missing, extend cooldown so RR can move on.
    picked.forEach((id) => {
      const key = accountIdKey(id);
      const q = accountQuotaSnapshot(key);
      if (!hasQuotaInfo(q)) {
        // Stick longer on failed/empty so we do not spin on the same 1–2 rows.
        quotaLiveProbedAt[key] = doneAt;
      } else {
        // Has durable info now — allow stale re-check only after STALE window.
        // Keep min requery floor via recentlyProbed.
        quotaLiveProbedAt[key] = doneAt;
        try {
          if (quotaCache[key]) delete quotaCache[key].probing;
        } catch (_) {}
      }
    });
    if (!silent) {
      const missN = missing.length;
      toast(`已刷新 ${rows.length} 个额度` + (missN ? `（补齐缺失 ${Math.min(missN, rows.length)}）` : ""));
    }
  } finally {
    quotaLiveInFlight = false;
  }
}

async function refreshAllQuota(force = true) {
  // force=false => DB cache only
  // force=true  => live probe:
  //   1) current page first (scoped ids) for immediate UI feedback
  //   2) then full pool in background when pool is large
  const btnIds = ["btn-refresh-quota", "btn-refresh-quota-2"];
  btnIds.forEach((id) => {
    const el = $(id);
    if (el) {
      el.disabled = true;
      if (!el.dataset.label) el.dataset.label = el.textContent;
      el.textContent = force ? "查询中…" : "读取缓存…";
    }
  });
  try {
    if (!force) {
      const data = await api("/accounts/quota?cached=1");
      const rows = data.results || data.accounts || data.quotas || [];
      applyQuotaResultsToUI(rows);
      const qs = $("quota-summary");
      if (qs) {
        const total = data.count ?? rows.length;
        const exhausted = data.exhausted_count ?? rows.filter((x) => x && (x.exhausted || x.auto_disabled)).length;
        qs.textContent = `额度(缓存)：${total} 个 · 耗尽 ${exhausted}`;
      }
      toast(`已加载缓存额度：${rows.length} 条`, true);
      return data;
    }

    const pageIds = (accountsList || []).map((a) => a && a.id).filter(Boolean);
    let pageRows = [];
    // Phase 1: current page (fast path) — always scoped so 7k pool doesn't block UI.
    if (pageIds.length) {
      try {
        const qs = pageIds.map(encodeURIComponent).join(",");
        const pageData = await api("/accounts/quota?refresh=1&ids=" + qs);
        pageRows = pageData.results || pageData.accounts || pageData.quotas || [];
        applyQuotaResultsToUI(pageRows);
        const qsEl = $("quota-summary");
        if (qsEl) {
          const freeN = pageRows.filter((x) => x && (x.account_type === "free" || x.plan === "free" || x.free_tokens)).length;
          const superN = pageRows.filter((x) => x && (x.account_type === "supergrok" || x.plan === "supergrok")).length;
          const exhausted = pageRows.filter((x) => x && (x.exhausted || x.auto_disabled)).length;
          qsEl.textContent = `额度(本页实时)：${pageRows.length} 个 · Free ${freeN} · SuperGrok ${superN} · 耗尽 ${exhausted} · 全库后台刷新中…`;
        }
        toast(`本页额度已更新 ${pageRows.length} 个，全库后台刷新中…`, true);
      } catch (e) {
        // fall through to full
        console.warn("page quota refresh failed", e);
      }
    }

    // Phase 2 (optional): full-pool live probe. Default OFF — 7k accounts × multi
    // upstream calls floods privoxy / cli-chat-proxy and trips provider retries.
    btnIds.forEach((id) => {
      const el = $(id);
      if (el) {
        el.disabled = false;
        el.textContent = el.dataset.label || "查询全部额度";
      }
    });

    if (QUOTA_FULL_POOL_ON_BUTTON) {
      (async () => {
        try {
          const data = await api("/accounts/quota?refresh=1");
          const rows = data.results || data.accounts || data.quotas || [];
          const visible = new Set((accountsList || []).map((a) => a && a.id).filter(Boolean));
          rows.forEach((q) => {
            if (!q || typeof q !== "object") return;
            const id = q.account_id || q.id;
            if (!id) return;
            quotaCache[id] = q;
            if (visible.has(id)) {
              const patch = (typeof poolPatchFromQuotaResult === "function")
                ? (poolPatchFromQuotaResult(q) || {})
                : { last_quota: q };
              if (!patch.last_quota) patch.last_quota = q;
              applyAccountLivePatch(id, { _pool: patch });
            }
          });
          const qsEl = $("quota-summary");
          if (qsEl) {
            const total = data.count ?? rows.length;
            const exhausted = data.exhausted_count ?? rows.filter((x) => x && (x.exhausted || x.auto_disabled)).length;
            const ok = data.ok_count != null ? data.ok_count : rows.filter((x) => x && x.ok && !x.exhausted).length;
            qsEl.textContent = `额度(全库实时)：${total} 个 · 可用 ${ok} · 耗尽 ${exhausted}`;
          }
          try { softRefreshPoolChips({ stats: true }); } catch (_) {}
          toast(`全库额度已刷新：可用 ${(data.ok_count != null ? data.ok_count : "—")}/${data.count ?? rows.length}`, true);
        } catch (e) {
          console.warn("full quota refresh failed", e);
        }
      })();
    } else {
      const qsEl = $("quota-summary");
      if (qsEl && pageRows.length) {
        const freeN = pageRows.filter((x) => x && (x.account_type === "free" || x.free_tokens)).length;
        const superN = pageRows.filter((x) => x && (x.account_type === "supergrok" || x.plan === "supergrok")).length;
        const exhausted = pageRows.filter((x) => x && (x.exhausted || x.auto_disabled)).length;
        qsEl.textContent = `额度(本页实时)：${pageRows.length} 个 · Free ${freeN} · SuperGrok ${superN} · 耗尽 ${exhausted}（自动刷新仅本页，避免连接过多）`;
      }
      toast(`本页额度已更新 ${pageRows.length} 个（为避免连接过多，不再自动扫全库）`, true);
    }

    return { ok: true, page_count: pageRows.length };
  } catch (e) {
    toast((e && e.message) || "额度查询失败", false);
    throw e;
  } finally {
    btnIds.forEach((id) => {
      const el = $(id);
      if (el) {
        el.disabled = false;
        el.textContent = el.dataset.label || "查询全部额度";
      }
    });
  }
}


let modelsFilterQ = "";
let modelsFilterReason = "all";
let modelsLoadSeq = 0;

function applyModelsListToUI(list, meta) {
  const m = meta || {};
  dashCache = dashCache || {};
  dashCache.models = Array.isArray(list) ? list.slice() : [];
  if (m.default_model) dashCache.default_model = m.default_model;
  if (m.storage) dashCache.models_storage = m.storage;
  if (m.meta) dashCache.models_meta = m.meta;
  renderModels();
}

function bindModelsControls() {
  on("btn-sync-models", "onclick", async () => {
    const btn = $("btn-sync-models");
    if (btn) {
      if (!btn.dataset.label) btn.dataset.label = btn.textContent;
      btn.disabled = true;
      btn.textContent = "同步中…";
    }
    try {
      const r = await api("/models/sync", { method: "POST" });
      if (r && r.ok === false) throw new Error(r.error || r.detail || "同步失败");
      if (r && (Array.isArray(r.data) || Array.isArray(r.models))) {
        applyModelsListToUI(r.data || r.models || [], {
          default_model: r.default_model,
          storage: r.storage || "postgres",
        });
      } else {
        await loadModels();
      }
      try { await refreshModelHealthStatus(); } catch (_) {}
      const n = ((dashCache && dashCache.models) || []).length;
      const up = r && (r.upstream_count != null ? r.upstream_count : r.pg_count);
      const via = r && r.fetched_via ? ` · 经 ${r.fetched_via}` : "";
      toast(
        r.message ||
          (up != null && up !== n
            ? `已同步上游 ${up} 个，目录共 ${n} 个${via}`
            : `已同步 ${n} 个模型${via}`)
      );
    } catch (e) {
      toast((e && e.message) || "同步模型失败", false);
    } finally {
      if (btn) {
        btn.disabled = false;
        btn.textContent = btn.dataset.label || "同步上游";
      }
    }
  });
  on("btn-models-reload", "onclick", async () => {
    try {
      await loadModels();
      try { await refreshModelHealthStatus(); } catch (_) {}
      toast("已刷新模型列表");
    } catch (e) {
      toast((e && e.message) || "刷新失败", false);
    }
  });
  on("btn-probe-all-models", "onclick", () => runProbeAll());
  const q = $("models-q");
  if (q && !q._g2aBound) {
    q._g2aBound = true;
    let t = null;
    q.addEventListener("input", () => {
      modelsFilterQ = (q.value || "").trim();
      clearTimeout(t);
      t = setTimeout(() => renderModels(), 120);
    });
  }
  const fr = $("models-filter-reason");
  if (fr && !fr._g2aBound) {
    fr._g2aBound = true;
    fr.addEventListener("change", () => {
      modelsFilterReason = fr.value || "all";
      renderModels();
    });
  }
}


async function refreshModelHealthStatus() {
  try {
    const st = await api("/model-health");
    if (!st || typeof st !== "object") return st;
    statusCache = statusCache || {};
    dashCache = dashCache || {};
    statusCache.model_health = st;
    dashCache.model_health = st;
    try { renderModelHealthInfo(); } catch (_) {}
    return st;
  } catch (e) {
    console.warn("refreshModelHealthStatus", e);
    return null;
  }
}

async function loadModels() {
  // Prefer lightweight admin catalog over stale/empty dashCache.models.
  // /dashboard is skipped on the models page for performance with large pools.
  const seq = ++modelsLoadSeq;
  let list = [];
  const tbody = $("models-tbody");
  const hasRows = !!(tbody && tbody.querySelector("tr[data-model-id]"));
  if (tbody && !hasRows) {
    tbody.innerHTML = `<tr><td colspan="5" class="g2a-muted">加载模型目录中…</td></tr>`;
  }
  try {
    const r = await api("/models");
    if (seq !== modelsLoadSeq) return (dashCache && dashCache.models) || [];
    list = (r && (r.data || r.models)) || [];
    if (!Array.isArray(list)) list = [];
    dashCache = dashCache || {};
    // Always replace — never keep a stale single-model dashboard snapshot.
    dashCache.models = list.slice();
    if (r && r.default_model) dashCache.default_model = r.default_model;
    if (r && r.storage) dashCache.models_storage = r.storage;
    if (r && r.meta) dashCache.models_meta = r.meta;
  } catch (e) {
    if (seq !== modelsLoadSeq) return (dashCache && dashCache.models) || [];
    console.warn("loadModels failed", e);
    try { toast((e && e.message) || "加载模型列表失败", false); } catch (_) {}
  }
  if (seq !== modelsLoadSeq) return (dashCache && dashCache.models) || list || [];
  try { bindModelsControls(); } catch (_) {}
  renderModels();
  return (dashCache && dashCache.models) || list || [];
}

function filteredModelsList() {
  const models = (dashCache && Array.isArray(dashCache.models) ? dashCache.models : []) || [];
  const q = (modelsFilterQ || ($("models-q") && $("models-q").value) || "").trim().toLowerCase();
  const reason = modelsFilterReason || ($("models-filter-reason") && $("models-filter-reason").value) || "all";
  return models.filter((m) => {
    if (!m) return false;
    if (q) {
      const id = String(m.id || "").toLowerCase();
      const name = String(m.name || "").toLowerCase();
      if (!id.includes(q) && !name.includes(q)) return false;
    }
    if (reason === "yes" && !m.supports_reasoning_effort) return false;
    if (reason === "no" && m.supports_reasoning_effort) return false;
    return true;
  });
}

function renderModels() {
  try { bindModelsControls(); } catch (_) {}
  const all = (dashCache && Array.isArray(dashCache.models) ? dashCache.models : []) || [];
  const models = filteredModelsList();
  const tbody = $("models-tbody");
  if (!tbody) return;
  const defaultModel = (dashCache && dashCache.default_model)
    || (statusCache && statusCache.default_model)
    || "";
  const storage = (dashCache && dashCache.models_storage) || "postgres";
  // Subtitle count
  try {
    const sub = $("models-card-sub") || (tbody.closest(".g2a-card") && tbody.closest(".g2a-card").querySelector(".g2a-card-head p"));
    if (sub) {
      if (!all.length) {
        sub.textContent = "模型目录为空。请点「同步上游模型」拉取并写入数据库。";
      } else {
        sub.textContent = `目录共 ${all.length} 个模型 · 数据源 ${storage}`
          + (defaultModel ? ` · 默认 ${defaultModel}` : "")
          + "。可搜索筛选，或同步/探测。";
      }
    }
  } catch (_) {}
  if ($("models-count")) {
    $("models-count").textContent = all.length
      ? `显示 ${models.length} / ${all.length}`
      : "无模型";
  }
  if (!all.length) {
    tbody.innerHTML = `<tr><td colspan="5" class="g2a-muted">暂无模型。请点「同步上游模型」或刷新；默认可用 grok-4.5 / grok-build</td></tr>`;
    return;
  }
  if (!models.length) {
    tbody.innerHTML = `<tr><td colspan="5" class="g2a-muted">无匹配模型（请调整搜索/筛选）</td></tr>`;
    return;
  }
  tbody.innerHTML = models.map((m) => {
    const id = String(m.id || "");
    const isDefault = defaultModel && id === String(defaultModel);
    const localish = /build|search|coding|local/i.test(id) || m.local || m.synthetic;
    const tags = [];
    if (isDefault) tags.push('<span class="g2a-tag ok">默认</span>');
    if (localish) tags.push('<span class="g2a-tag">本地/扩展</span>');
    const reason = m.supports_reasoning_effort
      ? '<span class="g2a-tag ok">支持</span>'
      : '<span class="g2a-muted">—</span>';
    const ctx = m.context_window != null && m.context_window !== ""
      ? Number(m.context_window).toLocaleString()
      : "—";
    return `<tr data-model-id="${esc(id)}">
      <td class="mono" title="${esc(id)}">${esc(id)}</td>
      <td>${esc(m.name || id || "—")}</td>
      <td class="mono g2a-muted">${esc(String(ctx))}</td>
      <td>${reason}</td>
      <td>${tags.join(" ") || '<span class="g2a-muted">—</span>'}</td>
    </tr>`;
  }).join("");
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
  const effBatch = mh.probe_batch_effective ?? mh.probe_batch ?? mh.batch;
  const workers = mh.probe_workers ?? mh.workers;
  el.textContent =
    `模型探测：每 ${mh.interval_sec ?? "?"}s · 批 ${effBatch ?? "?"}${workers != null ? " ×" + workers + " workers" : ""} · 模型 ${modelsTxt}${rotateTxt} · ${lastTxt}${sweepTxt}${etaTxt}`;
}

/* ── Upstream live monitor (models page) ───────────────── */
let upstreamMonitorTimer = null;
let upstreamMonitorInFlight = false;
let upstreamMonitorLast = null;
let upstreamMonitorBound = false;
let upstreamMonitorInFlightSince = 0;
const UPSTREAM_MONITOR_INTERVAL_MS = 8000;
const UPSTREAM_MONITOR_STUCK_MS = 15000;

function bindUpstreamMonitorControls() {
  const chk = $("chk-upstream-live");
  const btn = $("btn-upstream-probe");
  if (!chk && !btn) return;
  if (chk) {
    try {
      const saved = localStorage.getItem("g2a_upstream_live");
      // Default ON when unset — old "0" stuck in localStorage made the card look dead.
      if (saved === "0") chk.checked = false;
      else chk.checked = true;
    } catch (_) {
      chk.checked = true;
    }
    // Property handler so soft-nav rebind is idempotent (same as on()).
    chk.onchange = () => {
      try { localStorage.setItem("g2a_upstream_live", chk.checked ? "1" : "0"); } catch (_) {}
      if (chk.checked) startUpstreamMonitor({ force: true });
      else stopUpstreamMonitor({ keepLast: true });
    };
  }
  if (btn) {
    btn.onclick = async (ev) => {
      try { if (ev && ev.preventDefault) ev.preventDefault(); } catch (_) {}
      const label = btn.dataset.label || btn.textContent || "立即探测";
      btn.dataset.label = label;
      btn.disabled = true;
      btn.textContent = "探测中…";
      // Clear stuck in-flight so manual probe always works.
      upstreamMonitorInFlight = false;
      upstreamMonitorInFlightSince = 0;
      try {
        await refreshUpstreamStatus({ force: true, toast: true });
      } catch (e) {
        try { toast((e && e.message) || "探测失败", false); } catch (_) {}
      } finally {
        btn.disabled = false;
        btn.textContent = btn.dataset.label || "立即探测";
      }
    };
  }
  upstreamMonitorBound = true;
}

function stopUpstreamMonitor({ keepLast = false } = {}) {
  if (upstreamMonitorTimer) {
    try { clearInterval(upstreamMonitorTimer); } catch (_) {}
    upstreamMonitorTimer = null;
  }
  if (!keepLast) {
    // leave last paint; only clear timer
  }
  const hint = $("upstream-live-hint");
  if (hint) {
    const live = $("chk-upstream-live");
    if (live && !live.checked) hint.textContent = "实时监控已关闭";
    else if (document.body && document.body.dataset.page !== "models") hint.textContent = "已离开模型页 · 监控已停";
  }
}

function startUpstreamMonitor({ force = false } = {}) {
  const page = (document.body && document.body.dataset.page) || pageFromPath(location.pathname) || "";
  if (page !== "models") {
    stopUpstreamMonitor();
    return;
  }
  if (!$("upstream-monitor-card") && !$("upstream-stat-grid")) return;
  try { bindUpstreamMonitorControls(); } catch (_) {}
  // Soft-nav may leave body.g2a-softnav-busy with pointer-events:none on content.
  try { document.body && document.body.classList.remove("g2a-softnav-busy"); } catch (_) {}
  const chk = $("chk-upstream-live");
  const enabled = !chk || !!chk.checked;
  // Entering models page: always re-bind + force one live probe (card used to stay on "等待探测…").
  if (force || !upstreamMonitorLast) {
    refreshUpstreamStatus({ force: true }).catch(() => {});
  } else {
    try { renderUpstreamMonitor(upstreamMonitorLast); } catch (_) {}
    // Still refresh in background so soft-nav re-entry is not a stale snapshot forever.
    refreshUpstreamStatus({ force: !!force }).catch(() => {});
  }
  if (!enabled) {
    stopUpstreamMonitor({ keepLast: true });
    const hint = $("upstream-live-hint");
    if (hint) hint.textContent = "实时监控已关闭（可勾选开启）";
    return;
  }
  if (upstreamMonitorTimer) {
    // Timer already running — still ensure hint is correct.
    const hint = $("upstream-live-hint");
    if (hint) hint.textContent = `轮询间隔 ${Math.round(UPSTREAM_MONITOR_INTERVAL_MS / 1000)}s`;
    return;
  }
  const hint = $("upstream-live-hint");
  if (hint) hint.textContent = `轮询间隔 ${Math.round(UPSTREAM_MONITOR_INTERVAL_MS / 1000)}s`;
  upstreamMonitorTimer = setInterval(() => {
    try {
      if (document.hidden) return;
      const p = (document.body && document.body.dataset.page) || pageFromPath(location.pathname) || "";
      if (p !== "models") { stopUpstreamMonitor(); return; }
      const c = $("chk-upstream-live");
      if (c && !c.checked) { stopUpstreamMonitor({ keepLast: true }); return; }
      // Auto-heal stuck in-flight (tab sleep / aborted fetch without finally).
      if (upstreamMonitorInFlight && upstreamMonitorInFlightSince &&
          (Date.now() - upstreamMonitorInFlightSince) > UPSTREAM_MONITOR_STUCK_MS) {
        upstreamMonitorInFlight = false;
        upstreamMonitorInFlightSince = 0;
      }
      refreshUpstreamStatus({ force: false }).catch(() => {});
    } catch (_) {}
  }, UPSTREAM_MONITOR_INTERVAL_MS);
}

async function refreshUpstreamStatus({ force = false, toast: doToast = false } = {}) {
  // Heal stuck lock from a previous hung request.
  if (upstreamMonitorInFlight && upstreamMonitorInFlightSince &&
      (Date.now() - upstreamMonitorInFlightSince) > UPSTREAM_MONITOR_STUCK_MS) {
    upstreamMonitorInFlight = false;
    upstreamMonitorInFlightSince = 0;
  }
  if (upstreamMonitorInFlight && !force) return upstreamMonitorLast;
  upstreamMonitorInFlight = true;
  upstreamMonitorInFlightSince = Date.now();
  // Immediate UI feedback so the card never sits on "等待探测…" with no motion.
  try {
    const errEl = $("upstream-stat-error");
    const stateEl = $("upstream-stat-state");
    if (errEl && (!upstreamMonitorLast || force)) errEl.textContent = "探测中…";
    if (stateEl && (!upstreamMonitorLast || force)) stateEl.textContent = "探测中";
    const pill = $("upstream-status-pill");
    if (pill && force) {
      pill.className = "g2a-tag warn";
      pill.textContent = "● 探测中";
    }
  } catch (_) {}
  try {
    const path = force ? "/upstream-status?force=1" : "/upstream-status";
    const st = await api(path);
    if (!st || typeof st !== "object") {
      throw new Error("上游监控接口返回空数据");
    }
    upstreamMonitorLast = st;
    dashCache = dashCache || {};
    statusCache = statusCache || {};
    dashCache.upstream_status = st;
    statusCache.upstream_status = st;
    try { renderUpstreamMonitor(st); } catch (e) { console.warn("renderUpstreamMonitor", e); }
    if (doToast) {
      if (st.ok) toast(`上游正常 · ${st.latency_ms != null ? st.latency_ms + " ms" : "—"}`);
      else if (st.reachable) toast(`上游可达但异常：${st.error || "HTTP " + (st.status_code || "?")}`, false);
      else toast(`上游不可达：${st.error || "dial failed"}`, false);
    }
    return st;
  } catch (e) {
    console.warn("refreshUpstreamStatus", e);
    const stub = {
      ok: false,
      reachable: false,
      error: (e && e.message) || "探测请求失败",
      checked_at: Math.floor(Date.now() / 1000),
      base_url: (statusCache && statusCache.upstream) || (dashCache && dashCache.upstream) || "",
      status_code: e && e.status != null ? e.status : undefined,
    };
    // Keep last good latency if any; still paint the error.
    try { renderUpstreamMonitor(Object.assign({}, upstreamMonitorLast || {}, stub, { ok: false })); } catch (_) {}
    if (doToast) toast(stub.error, false);
    return stub;
  } finally {
    upstreamMonitorInFlight = false;
    upstreamMonitorInFlightSince = 0;
  }
}

function renderUpstreamMonitor(st) {
  if (!st || typeof st !== "object") return;
  bindUpstreamMonitorControls();
  const pill = $("upstream-status-pill");
  const ok = !!st.ok;
  const reachable = st.reachable !== false && (ok || st.status_code || st.dial_ms != null);
  const probing = !!st.probing;
  let tone = "bad";
  let label = "● 不可达";
  if (probing && !st.latency_ms) {
    tone = "warn";
    label = "● 探测中";
  } else if (ok) {
    tone = "ok";
    label = "● 正常";
  } else if (reachable || (st.status_code && st.status_code < 500)) {
    tone = "warn";
    label = "● 降级";
  }
  if (pill) {
    pill.className = "g2a-tag " + tone;
    pill.textContent = label;
  }
  const setText = (id, v) => { const el = $(id); if (el) el.textContent = v; };
  setText("upstream-stat-state", ok ? "正常" : (reachable ? "降级" : "不可达"));
  const errEl = $("upstream-stat-error");
  if (errEl) {
    if (ok) {
      errEl.textContent = st.cached ? "缓存命中 · 边缘可达" : "边缘可达 · /models 正常";
      errEl.className = "sub";
    } else {
      errEl.textContent = st.error || (st.status_code ? ("HTTP " + st.status_code) : "—");
      errEl.className = "sub";
    }
  }
  const lat = st.latency_ms != null ? Number(st.latency_ms) : null;
  setText("upstream-stat-latency", lat != null && Number.isFinite(lat) ? (lat + " ms") : "—");
  const dial = st.dial_ms != null ? Number(st.dial_ms) : null;
  setText("upstream-stat-dial", dial != null && Number.isFinite(dial) ? ("拨号 " + dial + " ms") : "拨号 —");
  const code = st.status_code != null ? String(st.status_code) : "—";
  setText("upstream-stat-http", code);
  const mc = st.models_count != null ? st.models_count : null;
  setText("upstream-stat-models", mc != null ? ("模型数 " + mc) : "模型数 —");
  setText("upstream-stat-base", st.base_url || st.origin || "—");
  const auth = st.auth === "account" ? "账号鉴权" : (st.auth === "anonymous" ? "匿名" : "—");
  const via = st.via ? (" · " + st.via) : "";
  setText("upstream-stat-via", "鉴权 " + auth + via);
  const checked = st.checked_at || st.checked_at_ms;
  let checkedTxt = "上次探测：—";
  if (checked) {
    // checked_at is unix sec; checked_at_ms is ms
    const sec = Number(checked) > 1e12 ? Number(checked) / 1000 : Number(checked);
    checkedTxt = "上次探测：" + (typeof fmtTime === "function" ? fmtTime(sec) : new Date(sec * 1000).toLocaleString());
  }
  setText("upstream-stat-checked", checkedTxt);
  const cacheEl = $("upstream-stat-cache");
  if (cacheEl) {
    if (st.cached) {
      const age = st.cache_age_ms != null ? Math.round(Number(st.cache_age_ms) / 1000) + "s 前" : "";
      cacheEl.textContent = "缓存" + (age ? " · " + age : "");
    } else {
      cacheEl.textContent = "实时";
    }
  }
  // Color latency value subtly via class on parent .stat if present
  try {
    const latEl = $("upstream-stat-latency");
    if (latEl && lat != null) {
      const parent = latEl.closest(".stat");
      if (parent) {
        parent.style.borderColor = "";
        if (lat > 2000) parent.style.borderColor = "rgba(220,68,70,.45)";
        else if (lat > 800) parent.style.borderColor = "rgba(216,150,20,.45)";
        else if (ok) parent.style.borderColor = "rgba(73,170,25,.35)";
      }
    }
  } catch (_) {}
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
    const recovered = !!(res.recovered || (res.auto_action && res.auto_action.recovered))
      || !!(pool.pool_status === "normal" && ok && !pool.in_cooldown)
      || !!(r.pool_status === "normal" && ok);
    // Live kick flags must count even when pool view lags (async last_probe).
    const cooling = !!(
      res.kicked_cooldown
      || pool.in_cooldown
      || r.in_cooldown
      || pool.pool_status === "cooldown"
      || (Number(pool.cooldown_remaining_sec || 0) > 0)
    );
    const lines = [
      ok ? "✓ 探测成功" : "✗ 探测失败",
      `账号: ${r.email || res.email || accountId}`,
      `模型: ${res.model || pool.cooldown_model || "—"}`,
      res.latency_ms != null ? `耗时: ${res.latency_ms} ms` : null,
      res.status_code != null ? `HTTP: ${res.status_code}` : null,
      res.error ? `错误: ${res.error}` : null,
      cooling ? "状态：冷却中（已写库）" : null,
      ok && recovered ? "状态：冷却中 → 正常（已写库）" : null,
      (pool.cooldown_code || res.cooldown_code) ? `code: ${pool.cooldown_code || res.cooldown_code}` : null,
      (res && res.auto_disabled) ? "已自动屏蔽模型 / 移出轮询" : null,
    ].filter(Boolean);
    setLogPanel("probe-result", lines.join("\n"), { forceShow: true });
    toast(
      ok
        ? (recovered ? "测活成功，已恢复为正常" : "账号模型探测成功")
        : (cooling ? "测活失败，已进入冷却中" : (res.error || r.error || "探测失败")),
      ok
    );
    const poolPatch = poolPatchFromProbeResponse(r);
    // Hot-update ONLY this account row — never rewrite the whole table (scroll jump).
    const topPatch = {
      email: r.email || res.email,
      _pool: poolPatch,
    };
    if (r.account_type || (poolPatch && poolPatch.account_type)) {
      topPatch.account_type = r.account_type || poolPatch.account_type;
      topPatch.plan = topPatch.account_type;
    }
    if (r.plan_label || (poolPatch && poolPatch.plan_label)) {
      topPatch.plan_label = r.plan_label || poolPatch.plan_label;
    }
    applyAccountLivePatch(accountId, topPatch);
    // Paint 类型 / 额度使用 from probe-attached quota (persisted server-side).
    if (poolPatch && poolPatch.last_quota && typeof applyQuotaResultsToUI === "function") {
      try {
        applyQuotaResultsToUI([{
          ...poolPatch.last_quota,
          account_id: accountId,
          id: accountId,
        }]);
      } catch (_) {}
    } else if (r.quota && typeof applyQuotaResultsToUI === "function") {
      try {
        applyQuotaResultsToUI([{ ...r.quota, account_id: accountId, id: accountId }]);
      } catch (_) {}
    }
    // Soft pool chip counts when status may have changed (cooldown enter/leave).
    if (cooling || recovered || (poolPatch && (poolPatch.in_cooldown || poolPatch.pool_status === "cooldown" || poolPatch.pool_status === "normal"))) {
      Promise.resolve().then(() => softRefreshPoolChips({ stats: true }));
    }
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
      const cooling = !!(
        res.kicked_cooldown
        || pool.in_cooldown
        || item.in_cooldown
        || pool.pool_status === "cooldown"
        || (Number(pool.cooldown_remaining_sec || 0) > 0)
      );
      if (cooling) coolN += 1;
      const poolPatch = poolPatchFromProbeResponse(item);
      const topPatch = {
        email: item.email || res.email,
        _pool: poolPatch,
      };
      if (item.account_type || (poolPatch && poolPatch.account_type)) {
        topPatch.account_type = item.account_type || poolPatch.account_type;
        topPatch.plan = topPatch.account_type;
      }
      if (item.plan_label || (poolPatch && poolPatch.plan_label)) {
        topPatch.plan_label = item.plan_label || poolPatch.plan_label;
      }
      applyAccountLivePatch(id, topPatch);
      if (poolPatch && poolPatch.last_quota && typeof applyQuotaResultsToUI === "function") {
        try {
          applyQuotaResultsToUI([{
            ...poolPatch.last_quota,
            account_id: id,
            id,
          }]);
        } catch (_) {}
      } else if (item.quota && typeof applyQuotaResultsToUI === "function") {
        try {
          applyQuotaResultsToUI([{ ...item.quota, account_id: id, id }]);
        } catch (_) {}
      }
      setRowBusy(id, false);
      lines.push(
        `${ok ? "✓" : "✗"} ${item.email || id}` +
          (cooling ? " · 冷却中" : (pool.pool_status ? ` · ${pool.pool_status}` : "")) +
          (res.error ? ` · ${String(res.error).slice(0, 80)}` : "")
      );
    });
    // any ids missing from response
    list.forEach((id) => setRowBusy(id, false));
    lines.splice(1, 0, `成功 ${okN} · 失败 ${badN} · 冷却 ${coolN}`);
    setLogPanel("probe-result", lines.join("\n"), { forceShow: true });
    toast(`批量测活：成功 ${okN} · 失败 ${badN} · 冷却 ${coolN}`, badN === 0);
    // Only the patched rows were rewritten; soft-refresh pool chips once.
    // Do NOT loadAccountsPage — full tbody rewrite scrolls the page under the user.
    Promise.resolve().then(() => softRefreshPoolChips({ stats: true }));
  } catch (e) {
    list.forEach((id) => setRowBusy(id, false));
    setLogPanel("probe-result", "✗ " + e.message, { forceShow: true });
    toast(e.message, false);
  }
}

async function runProbeAll() {
  const btns = ["btn-probe-all", "btn-probe-all-2", "btn-probe-all-models"].map((id) => $(id)).filter(Boolean);
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
    // Match backend defaultManualJobTimeout (45m) with a little headroom.
    const pollDeadline = Date.now() + 50 * 60 * 1000;
    let lastSig = "";
    while (true) {
      const job = (r && r.running != null) ? r : ((r && r.job) || r);
      const running = !!(job && job.running);
      const wave = job && (job.wave || job.waves) || 0;
      const probed = job && (job.probed || job.count) || 0;
      const avail = job && (job.available_count ?? job.available) || 0;
      const failed = job && (job.unavailable_count ?? job.failed) || 0;
      const rem = job && job.sweep && (job.sweep.remaining != null ? job.sweep.remaining : null);
      const covered = job && job.sweep && job.sweep.covered;
      const live = job && job.sweep && job.sweep.live;
      const elapsed = Math.max(1, Math.round((Date.now() - startedAt) / 1000));
      const progressLines = [
        running
          ? (start && start.already_running ? `全部账号模型探测进行中（已有任务 · ${elapsed}s）` : `全部账号模型探测进行中（${elapsed}s）`)
          : `全部账号模型探测完成（${elapsed}s）`,
        wave ? `波次 ${wave}` : null,
        `已探测 ${probed}` +
          (covered != null || live != null
            ? ` · 扫池 ${covered ?? "—"}/${live ?? "—"}`
            : "") +
          (rem != null ? ` · 剩余 ${rem}` : (job && job.deferred ? ` · 延后 ${job.deferred}` : "")),
        `可用 ${avail}` + (failed ? ` · 不可用 ${failed}` : ""),
        job && job.models ? `模型 ${(job.models || []).join(", ")}` : null,
        job && job.kick_cooldown ? `进入冷却 ${job.kick_cooldown}` : null,
      ].filter(Boolean);
      const sig = progressLines.join("\n");
      // Avoid blanking the panel on identical soft polls (same anti-flicker idea as logs).
      if (sig !== lastSig) {
        lastSig = sig;
        setLogPanel("probe-result", sig, { forceShow: true });
      }
      if (!running) {
        r = job || r;
        break;
      }
      if (Date.now() > pollDeadline) {
        throw new Error("探测超时（>50min），请查看 model-health / 任务日志状态");
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
      r.error ? `错误: ${r.error}` : null,
      r.deferred_busy ? "维护锁曾忙碌（已自动重试或跳过）" : null,
      `探测 ${r.probed ?? r.count ?? 0}` + (r.deferred ? ` · 延后 ${r.deferred}` : ""),
      `可用 ${r.available_count ?? r.available ?? 0}/${r.count ?? r.probed ?? 0}`,
      `不可用 ${r.unavailable_count ?? r.failed ?? 0}`,
      `自动处理 ${r.auto_action_count ?? 0}` + (r.kick_cooldown ? ` · 进入冷却 ${r.kick_cooldown}` : ""),
      r.waves ? `波次 ${r.waves}` : null,
      r.sweep
        ? `扫池 ${r.sweep.covered ?? "—"}/${r.sweep.live ?? "—"}` +
          (r.sweep.remaining != null ? ` · 剩余 ${r.sweep.remaining}` : "")
        : null,
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
    const okN = r.available_count ?? r.available ?? 0;
    const totalN = r.count ?? r.probed ?? 0;
    if (r.error && totalN === 0) {
      toast(String(r.error), false);
    } else if (totalN === 0) {
      toast("探测完成：0 个账号被选中（可能全在冷却/无 token/锁忙）", false);
    } else {
      toast(`探测完成：${okN}/${totalN} 可用`);
    }
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
    try { await loadAccountsPage({ reset: false, silent: true }); } catch (_) {}
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
let _autoRefreshBackoffUntil = 0;
let _autoRefreshFailStreak = 0;
let _lastAccountsHotAt = 0;
let _accountsHotInFlight = false;

// Shared manual refresh for accounts page (header 刷新 + 列表旁 刷新).
// /status chips and list rows are independent: status failure must not block the list.
async function refreshAccountsListUI({ toastOk = "", force = true } = {}) {
  accountsLoadWatchdog(12000);
  try {
    _statusFetchedAt = 0;
    statusCache = await api("/status");
    _statusFetchedAt = Date.now();
    if (window.G2A && G2A.state) G2A.state.status = statusCache;
    try { renderAccountStatusChips(); } catch (_) {}
    try { renderStats(); } catch (_) {}
  } catch (e) {
    if (e && e.status === 401) throw e;
    console.warn("refreshAccountsListUI /status", e && (e.message || e));
  }
  if (typeof hotRefreshAccountsPage === "function") {
    await hotRefreshAccountsPage({ force: !!force });
  } else {
    await loadAccountsPage({ reset: false, silent: !!(accountsList && accountsList.length) });
  }
  if (toastOk) toast(toastOk);
}

// Silent accounts hot-refresh: re-fetch current page from DB and patch rows
// without flashing "加载账号中…" or resetting selection/scroll.
async function hotRefreshAccountsPage({ force = false } = {}) {
  // Unstick a hung full load so force refresh can proceed after tab freezes.
  if (force) accountsLoadWatchdog(12000);
  // Background ticks never stack; manual force never silently no-ops.
  if (!force && (_accountsHotInFlight || accountsLoading)) return;
  if (force && accountsLoading) {
    // Full loader owns accountsLoading — piggy-back on its seq guard (silent keeps rows).
    return loadAccountsPage({ reset: false, silent: true });
  }
  // force + only hot-inflight: wait briefly for the other hot pass, then continue
  // so manual 刷新 never becomes a silent no-op after a stacked auto tick.
  if (force && _accountsHotInFlight) {
    const deadline = Date.now() + 2500;
    while (_accountsHotInFlight && Date.now() < deadline) {
      await new Promise((r) => setTimeout(r, 50));
    }
    if (_accountsHotInFlight) {
      return loadAccountsPage({ reset: false, silent: true });
    }
  }
  const page = document.body.dataset.page || pageFromPath(location.pathname) || "";
  if (page !== "accounts" && page !== "overview") return;
  // overview may not have the table mounted
  if (page === "accounts" && !$("accounts-tbody")) return;
  if (_accountsHotInFlight) {
    if (!force) return;
    return loadAccountsPage({ reset: false, silent: true });
  }
  _accountsHotInFlight = true;
  const hotSeq = accountsLoadSeq;
  try {
    const { page: pageNo, pageSize, path } = resolveAccountsListQuery();
    const data = await api(path);
    // A newer full load started while we were in flight — let it own the paint.
    if (hotSeq !== accountsLoadSeq) return;
    const rawAccounts = Array.isArray(data && data.accounts) ? data.accounts : [];
    // Merge with prior rows (probe/quota live patches + ephemeral busy flags).
    accountsList = mergeAccountsFromServer(rawAccounts);
    applyQuotaCacheFromAccounts(accountsList);
    accountsTotal = Number(data.total != null ? data.total : (data.account_count || accountsList.length)) || 0;
    accountsTotalPages = Number(data.total_pages || Math.max(1, Math.ceil((accountsTotal || 0) / pageSize))) || 1;
    accountsPage = Number(data.page || pageNo) || 1;
    if (data.pool) {
      if (statusCache) statusCache.pool = Object.assign({}, statusCache.pool || {}, data.pool);
      if (dashCache) dashCache.pool = Object.assign({}, dashCache.pool || {}, data.pool);
      try { renderStats(); } catch (_) {}
    }
    // Soft-nav may have swapped DOM while we awaited — re-check before paint.
    if (page === "accounts" && !$("accounts-tbody")) return;
    if (hotSeq !== accountsLoadSeq) return;
    // Preserve scroll: background hot-refresh must not jump the page under the user
    // (especially mid model-test click).
    withAccountsScrollStable(() => {
      try { renderAccountStatusChips(); } catch (_) {}
      try { renderAccountsPage(); } catch (_) {}
    });
    _lastAccountsHotAt = Date.now();
  } catch (e) {
    if (e && e.status === 401) throw e;
    console.warn("accounts hot refresh", e && (e.message || e));
    if (force) throw e;
  } finally {
    _accountsHotInFlight = false;
  }
}

function startAutoUiRefresh() {
  if (uiRefreshTimer) return;
  // Only the overview page may auto-poll, and only when the user checks
  // 「界面自动刷新」. Accounts / usage never background-reload — that was the
  // "别有事没事刷新" problem (table jump, chip flicker, wasted /status).
  uiRefreshTimer = setInterval(async () => {
    try {
      if (_autoRefreshBackoffUntil && Date.now() < _autoRefreshBackoffUntil) return;
      const page = document.body.dataset.page || pageFromPath(location.pathname) || "overview";
      if (page !== "overview") return;
      if (document.hidden) return;
      const chk = $("chk-auto-refresh-ui");
      if (!chk || !chk.checked) return;
      if (_autoRefreshInFlight) return;
      _autoRefreshInFlight = true;
      try {
        const now = Date.now();
        // Status at most every 15s (was 5s — too chatty for a passive dashboard).
        if (!statusCache || (now - (_statusFetchedAt || 0)) > 15000) {
          statusCache = await api("/status");
          _statusFetchedAt = Date.now();
          _autoRefreshFailStreak = 0;
          _autoRefreshBackoffUntil = 0;
          if (window.G2A && G2A.state) G2A.state.status = statusCache;
        }
        try { renderStats(); } catch (_) {}
        try { renderMaintainer(); } catch (_) {}
        try { renderModelHealthInfo(); } catch (_) {}
        try { renderStoreConn("overview-conn"); } catch (_) {}
        const tm = (statusCache && statusCache.token_maintainer) || {};
        const rem = tm.min_remaining_sec;
        // Background token renew only when near expiry, at most once per 10 min.
        if (rem != null && rem < 15 * 60 && Date.now() - lastAutoTokenRefreshAt > 10 * 60 * 1000) {
          lastAutoTokenRefreshAt = Date.now();
          try { await api("/accounts/refresh", { method: "POST", body: JSON.stringify({ force: false }) }); } catch (_) {}
        }
      } finally {
        _autoRefreshInFlight = false;
      }
    } catch (e) {
      _autoRefreshInFlight = false;
      if (e && e.status === 401) return;
      const gateway = !!(e && (e.gateway || e.status === 502 || e.status === 503 || e.status === 504 || e.html));
      const network = !!(e && (e.network || e.status === 0));
      if (gateway || network) {
        _autoRefreshFailStreak = Math.min(8, (_autoRefreshFailStreak || 0) + 1);
        const waitMs = Math.min(180000, 15000 * Math.pow(2, Math.max(0, _autoRefreshFailStreak - 1)));
        _autoRefreshBackoffUntil = Date.now() + waitMs;
        console.warn("auto refresh backoff", waitMs + "ms", e && (e.message || e));
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
  try { clearTimeout(regPollTimer); } catch (_) {}
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
  try { clearTimeout(regPollTimer); } catch (_) {}
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

function _desiredRegPollInterval() {
  // Adaptive: snappy when polls are fast; back off only under real load so we
  // don't pile up concurrent ticks (regPollInFlight lock freezes the log).
  if (regStopping) return 400;
  const last = Number(regPollLastDurationMs) || 0;
  if (last >= 800) return 550;
  if (last >= 500) return 320;
  if (last >= 250) return 200;
  return 140; // healthy batch-only path: ~7 paints/sec
}

function _scheduleRegPollTick(ms) {
  try { clearInterval(regPollTimer); } catch (_) {}
  try { clearTimeout(regPollTimer); } catch (_) {}
  regPollIntervalMs = Math.max(120, Number(ms) || 180);
  // setTimeout chain (not setInterval) so a slow tick never overlaps its own
  // cadence and we can adapt the next wait to the previous duration.
  regPollTimer = setTimeout(() => {
    pollRegSession()
      .catch(() => {})
      .finally(() => {
        if (!regPollTimer && regFinishedNotified) return;
        if (regFinishedNotified) return;
        if (!(regBatchId || regSessionId || (regSessionIds && regSessionIds.length))) return;
        _scheduleRegPollTick(_desiredRegPollInterval());
      });
  }, regPollIntervalMs);
}

function startRegPolling({ immediate = true, intervalMs = 180 } = {}) {
  regFinishedNotified = false;
  regPollIntervalMs = Math.max(regStopping ? 400 : 120, Number(intervalMs) || 180);
  if (immediate) {
    // First paint ASAP; subsequent ticks use adaptive schedule.
    try { clearInterval(regPollTimer); } catch (_) {}
    try { clearTimeout(regPollTimer); } catch (_) {}
    regPollTimer = setTimeout(() => {
      pollRegSession()
        .catch(() => {})
        .finally(() => {
          if (regFinishedNotified) return;
          if (!(regBatchId || regSessionId || (regSessionIds && regSessionIds.length))) return;
          _scheduleRegPollTick(_desiredRegPollInterval());
        });
    }, 0);
  } else {
    _scheduleRegPollTick(regPollIntervalMs);
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
    startRegPolling({ immediate: true, intervalMs: 180 });
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
// Bumps on every successful save; loadRegConfig ignores responses older than this.
let regConfigSaveEpoch = 0;
let regConfigLoadSeq = 0;
let regConfigSaving = false;

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
  syncRegConcurrencyWarning();
}

function syncRegConcurrencyWarning() {
  const input = $("reg-concurrency");
  const warning = $("reg-concurrency-warning");
  if (!input || !warning) return;
  const concurrency = Number.parseInt(input.value || "", 10);
  const provider = $("reg-captcha-provider")
    ? ($("reg-captcha-provider").value || "local").trim().toLowerCase()
    : "local";
  const threshold = provider === "yescaptcha" ? 4 : 2;
  if (!Number.isFinite(concurrency) || concurrency <= threshold) {
    warning.hidden = true;
    warning.textContent = "";
    return;
  }
  warning.textContent = provider === "yescaptcha"
    ? `警告：并发 ${concurrency} 较高，可能触发远程过盾或 xAI 限流；系统不会自动降低。`
    : `警告：本地并发 ${concurrency} 较高，Camoufox 每个浏览器约占 200–400MB，可能导致内存不足；系统不会自动降低。`;
  warning.hidden = false;
}

// Per-provider mail fields: each provider has dedicated DOM inputs + DB slots.
// Switching the dropdown only toggles visibility; values never share one input.
const REG_MAIL_PROVIDERS = ["moemail", "yyds", "gptmail", "cfmail", "tempmail"];
const REG_MAIL_KEY_SLOTS = {
  moemail: "moemail_api_key",
  yyds: "yyds_api_key",
  gptmail: "gptmail_api_key",
  cfmail: "cfmail_api_key",
  tempmail: "tempmail_api_key",
};
const REG_MAIL_DOMAIN_SLOTS = {
  moemail: "moemail_domain",
  yyds: "yyds_domain",
  gptmail: "gptmail_domain",
  cfmail: "cfmail_domain",
  tempmail: "tempmail_domain",
};
const REG_MAIL_BASE_SLOTS = {
  moemail: "moemail_base_url",
  cfmail: "cfmail_base_url",
};
const REG_MAIL_INPUT_IDS = {
  moemail: { key: "reg-moemail-api-key", domain: "reg-moemail-domain", base: "reg-moemail-base-url" },
  yyds: { key: "reg-yyds-api-key", domain: "reg-yyds-domain", base: null },
  gptmail: { key: "reg-gptmail-api-key", domain: "reg-gptmail-domain", base: null },
  cfmail: { key: "reg-cfmail-api-key", domain: "reg-cfmail-domain", base: "reg-cfmail-base-url" },
  tempmail: { key: "reg-tempmail-api-key", domain: "reg-tempmail-domain", base: null },
};
// In-memory cache (mirrors dedicated inputs / DB). Empty string = cleared.
let regMailKeys = { moemail: "", yyds: "", gptmail: "", cfmail: "", tempmail: "" };
let regMailDomains = { moemail: "", yyds: "", gptmail: "", cfmail: "", tempmail: "" };
let regMailBaseUrls = { moemail: "", cfmail: "" };
let regMailProviderPrev = "moemail";

function currentRegMailProvider() {
  const mail = $("reg-mail-provider")
    ? ($("reg-mail-provider").value || "moemail").trim().toLowerCase()
    : "moemail";
  if (mail === "yyds") return "yyds";
  if (mail === "gptmail") return "gptmail";
  if (mail === "cfmail") return "cfmail";
  if (mail === "tempmail" || mail === "tempmail.lol" || mail === "lol") return "tempmail";
  return "moemail";
}

function _cleanSecretDisplay(v) {
  const k = v == null ? "" : String(v);
  if (!k) return "";
  if (k.indexOf("…") >= 0 || k.indexOf("...") >= 0 || /^\*+$/.test(k)) return "";
  return k;
}

/** Pull every provider's dedicated inputs into memory (never cross-assign). */
function stashRegMailFieldsFromInput() {
  const active = currentRegMailProvider();
  for (const mail of REG_MAIL_PROVIDERS) {
    const ids = REG_MAIL_INPUT_IDS[mail] || {};
    const isActive = mail === active;
    // Key: never clobber a known secret with empty (masked display or disabled blank).
    if (ids.key && $(ids.key)) {
      const el = $(ids.key);
      const v = _cleanSecretDisplay(el.value || "");
      if (v) {
        regMailKeys[mail] = v;
      } else if (isActive && !el.disabled) {
        // Visible active field intentionally cleared by user.
        regMailKeys[mail] = "";
      }
      // else keep previous regMailKeys[mail]
    }
    // Domain / base: inactive disabled empty must not wipe memory.
    if (ids.domain && $(ids.domain)) {
      const el = $(ids.domain);
      const v = el.value || "";
      if (v || (isActive && !el.disabled)) {
        regMailDomains[mail] = v;
      }
    }
    if (ids.base && $(ids.base)) {
      const el = $(ids.base);
      const v = el.value || "";
      if (v || (isActive && !el.disabled)) {
        regMailBaseUrls[mail] = v;
      }
    }
  }
  // Legacy shared inputs (if old soft-nav HTML still present) → active provider only.
  if ($("reg-api-key") && !$("reg-moemail-api-key")) {
    const v = _cleanSecretDisplay($("reg-api-key").value || "");
    if (v || $("reg-api-key").value === "") regMailKeys[active] = v;
  }
  if ($("reg-domain") && !$("reg-moemail-domain")) {
    regMailDomains[active] = $("reg-domain").value || "";
  }
  if ($("reg-base-url") && !$("reg-moemail-base-url") && (active === "moemail" || active === "cfmail")) {
    regMailBaseUrls[active] = $("reg-base-url").value || "";
  }
}

function stashRegMailKeyFromInput() {
  stashRegMailFieldsFromInput();
}

/** Write memory → dedicated DOM inputs (each provider keeps its own value). */
function paintRegMailFieldsToInput() {
  for (const mail of REG_MAIL_PROVIDERS) {
    const ids = REG_MAIL_INPUT_IDS[mail] || {};
    if (ids.key && $(ids.key)) {
      $(ids.key).value = _cleanSecretDisplay(regMailKeys[mail] || "");
    }
    if (ids.domain && $(ids.domain)) {
      $(ids.domain).value = regMailDomains[mail] || "";
    }
    if (ids.base && $(ids.base)) {
      $(ids.base).value = regMailBaseUrls[mail] || "";
    }
  }
}

function regMailProviderMeta(mail) {
  const m = mail || "moemail";
  const table = {
    moemail: { title: "MoeMail", help: "", temp24h: false },
    yyds: { title: "YYDS Mail", help: "X-API-Key: AC-… · https://maliapi.215.im/v1 · 文档 vip.215.im/docs", temp24h: true },
    gptmail: { title: "GPTMail", help: "X-API-Key: sk-… · https://mail.chatgpt.org.uk/zh/api/", temp24h: true },
    cfmail: { title: "Cloudflare Temp Email", help: "Worker API + ADMIN_PASSWORDS(x-admin-auth) · github.com/dreamhunter2333/cloudflare_temp_email", temp24h: false },
    tempmail: { title: "TempMail.lol", help: "免费无需 Key · api.tempmail.lol · 文档 tempmail.lol/zh/api", temp24h: true },
  };
  return table[m] || table.moemail;
}

function syncRegMailProviderUI(opts) {
  const options = opts || {};
  const mail = currentRegMailProvider();
  // Always stash all dedicated inputs first so nothing is lost on switch.
  stashRegMailFieldsFromInput();
  regMailProviderPrev = mail;
  const meta = regMailProviderMeta(mail);
  const isTemp24h = !!meta.temp24h;

  if ($("reg-mail-help")) {
    // No long marketing/help copy — keep the box hidden.
    const help = String(meta.help || "").trim();
    if (!help) {
      $("reg-mail-help").classList.add("hidden");
      $("reg-mail-help").innerHTML = "";
      $("reg-mail-help").style.display = "none";
    } else {
      $("reg-mail-help").classList.remove("hidden");
      $("reg-mail-help").style.display = "";
      $("reg-mail-help").innerHTML = `<strong>${esc(meta.title)}</strong> — ${esc(help)}`;
    }
  }

  // Show only the selected provider's dedicated panels.
  // Force via style.display (inline attribute may be "display:none" from HTML).
  // Use "" for show so grid-column / other inline styles stay intact.
  document.querySelectorAll(".reg-mail-panel").forEach((el) => {
    const forMail = (el.getAttribute("data-mail") || "").toLowerCase();
    const show = forMail === mail;
    el.style.display = show ? "" : "none";
    el.hidden = !show;
    el.setAttribute("aria-hidden", show ? "false" : "true");
  });
  // Also toggle dedicated inputs' disabled so hidden fields are not submitted accidentally.
  for (const p of REG_MAIL_PROVIDERS) {
    const ids = REG_MAIL_INPUT_IDS[p] || {};
    const active = p === mail;
    for (const key of ["key", "domain", "base"]) {
      const id = ids[key];
      if (!id || !$(id)) continue;
      try { $(id).disabled = !active; } catch (_) {}
    }
  }

  // Paint memory into all dedicated inputs (hidden ones keep values for next save).
  paintRegMailFieldsToInput();

  // YYDS / GPTMail temp mail is ~24h — hide permanent / 3d options for clarity.
  if ($("reg-expiry-ms")) {
    const optsEl = $("reg-expiry-ms").options || [];
    for (let i = 0; i < optsEl.length; i++) {
      const v = String(optsEl[i].value || "");
      if (v === "0" || v === "259200000") {
        optsEl[i].hidden = isTemp24h;
        optsEl[i].disabled = isTemp24h;
      }
    }
    if (isTemp24h) {
      const curExp = String($("reg-expiry-ms").value || "");
      if (curExp === "0" || curExp === "259200000") {
        $("reg-expiry-ms").value = "86400000";
      }
    }
  }

  if (options.toast) {
    try {
      toast(`已切换到 ${meta.title}（各邮箱配置独立；点「保存配置」写入数据库）`);
    } catch (_) {}
  }
}


// Bind (or re-bind) registration mail/captcha form after soft-nav DOM swaps.
// Soft-nav replaces .g2a-content; change events are handled by document delegation
// below so we never depend on property handlers on a disposed <select>.
function bindRegMailFormControls() {
  if (!$("reg-mail-provider") && !$("reg-captcha-provider")) return;

  // Clear stale property handlers on the *new* node (delegation is the source of truth).
  try {
    if ($("reg-mail-provider")) $("reg-mail-provider").onchange = null;
    if ($("reg-captcha-provider")) $("reg-captcha-provider").onchange = null;
  } catch (_) {}

  // Paint panels for the currently selected provider (or memory after soft-nav).
  try {
    if ($("reg-mail-provider")) {
      const cur = ($("reg-mail-provider").value || "").trim().toLowerCase();
      if (!cur || !REG_MAIL_PROVIDERS.includes(cur)) {
        $("reg-mail-provider").value = regMailProviderPrev || "moemail";
      }
    }
    // Memory → inputs first, then show only active provider panels.
    paintRegMailFieldsToInput();
    syncRegMailProviderUI();
    syncRegCaptchaProviderUI();
  } catch (e) {
    console.warn("bindRegMailFormControls paint", e);
  }
}

// Document-level delegation: single handler, survives soft-nav HTML swaps.
// Capture phase so it runs even if something stops propagation later.
if (!window.__g2aRegMailDelegated) {
  window.__g2aRegMailDelegated = true;
  document.addEventListener(
    "change",
    (e) => {
      const t = e && e.target;
      if (!t || !t.id) return;
      if (t.id === "reg-mail-provider") {
        try {
          syncRegMailProviderUI({ toast: true });
          clearTimeout(window.__g2aRegMailSaveT);
          window.__g2aRegMailSaveT = setTimeout(() => {
            if (regConfigSaving) return;
            if (typeof saveRegConfig === "function") {
              saveRegConfig().catch((err) => console.warn("auto-save reg mail config", err));
            }
          }, 600);
        } catch (err) {
          console.warn("reg-mail-provider change", err);
        }
        return;
      }
      if (t.id === "reg-captcha-provider") {
        try { syncRegCaptchaProviderUI(); } catch (_) {}
        return;
      }
      if (t.id === "reg-concurrency") {
        try { syncRegConcurrencyWarning(); } catch (_) {}
      }
    },
    true
  );
}

function readRegConfig() {
  const provider = $("reg-captcha-provider")
    ? ($("reg-captcha-provider").value || "local").trim().toLowerCase()
    : "local";
  const isLocal = provider !== "yescaptcha";
  const mailProvider = currentRegMailProvider();
  // Capture ALL dedicated provider inputs (never share one box across services).
  stashRegMailFieldsFromInput();
  regMailProviderPrev = mailProvider;
  const activeKey = regMailKeys[mailProvider] || "";
  const activeDomain = regMailDomains[mailProvider] || "";
  const activeBase =
    mailProvider === "moemail" || mailProvider === "cfmail"
      ? (regMailBaseUrls[mailProvider] || "")
      : "";
  // Always persist ALL provider slots so switching never loses another service's config.
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
    tempmail_domain: regMailDomains.tempmail || "",
    expiry_ms: $("reg-expiry-ms") ? $("reg-expiry-ms").value.trim() : "",
    // Active key + all per-provider keys (empty keeps previous secret server-side on save,
    // except TempMail.lol free tier which intentionally uses empty key/domain).
    api_key: activeKey,
    moemail_api_key: regMailKeys.moemail || "",
    yyds_api_key: regMailKeys.yyds || "",
    gptmail_api_key: regMailKeys.gptmail || "",
    cfmail_api_key: regMailKeys.cfmail || "",
    tempmail_api_key: regMailKeys.tempmail || "",
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
    concurrency: $("reg-concurrency") ? $("reg-concurrency").value.trim() : "2",
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
  const mailProv = mail === "yyds" ? "yyds" : mail === "gptmail" ? "gptmail" : mail === "cfmail" ? "cfmail" : mail === "tempmail" ? "tempmail" : "moemail";
  if ($("reg-mail-provider")) {
    $("reg-mail-provider").value = mailProv;
  }
  // Hydrate per-provider key/domain caches.
  // Prefer dedicated fields; only fall back to active field for the *current*
  // provider. Never invent values for other providers.
  const cleanKey = (v) => {
    const s = v == null ? "" : String(v);
    if (!s) return "";
    // Masked server secrets are not usable — keep previous memory instead.
    if (s.indexOf("…") >= 0 || s.indexOf("...") >= 0 || /^\*+$/.test(s) || s === "****") return "";
    return s;
  };
  const activeKey = cleanKey(cfg.api_key);
  const activeDomain = cfg.domain == null ? "" : String(cfg.domain);
  // Keys: dedicated DB slot always wins when the field is present.
  // - non-empty / unmasked → use it
  // - empty string + *_set false (or dedicated field present as "") → cleared, do NOT restore prev
  // - masked / missing → keep previous UI memory
  const pickKey = (slotVal, isActive, prev, setFlag) => {
    if (slotVal != null && String(slotVal) !== "") {
      const slot = cleanKey(slotVal);
      if (slot) return slot;
      // Masked secret: keep previous memory (do not wipe just-saved key).
      return prev || "";
    }
    // Explicit empty from server: honor clear for this provider slot.
    // setFlag === false means "no secret stored"; true+empty is masked-as-empty edge.
    if (slotVal === "" || setFlag === false) {
      return "";
    }
    if (isActive && activeKey) return activeKey;
    return prev || "";
  };
  regMailKeys = {
    moemail: pickKey(cfg.moemail_api_key, mailProv === "moemail", regMailKeys.moemail || "", cfg.moemail_api_key_set),
    yyds: pickKey(cfg.yyds_api_key, mailProv === "yyds", regMailKeys.yyds || "", cfg.yyds_api_key_set),
    gptmail: pickKey(cfg.gptmail_api_key, mailProv === "gptmail", regMailKeys.gptmail || "", cfg.gptmail_api_key_set),
    cfmail: pickKey(cfg.cfmail_api_key, mailProv === "cfmail", regMailKeys.cfmail || "", cfg.cfmail_api_key_set),
    tempmail: pickKey(cfg.tempmail_api_key, mailProv === "tempmail", regMailKeys.tempmail || "", cfg.tempmail_api_key_set),
  };
  // Dedicated DB slots always win when present (independent of active provider).
  // Empty string clears that provider only — never restore from prev/local cache.
  for (const [prov, slot] of Object.entries(REG_MAIL_KEY_SLOTS || {})) {
    if (!Object.prototype.hasOwnProperty.call(cfg, slot)) continue;
    const raw = cfg[slot];
    const setFlag = cfg[slot + "_set"];
    if (raw == null || raw === "") {
      // Explicit empty / not set → clear this provider's key only.
      if (setFlag === false || raw === "") regMailKeys[prov] = "";
      continue;
    }
    const cleaned = cleanKey(raw);
    if (cleaned) regMailKeys[prov] = cleaned;
    // Masked: leave existing regMailKeys[prov] (from pickKey prev).
  }
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
    tempmail: pickDomain("tempmail_domain", mailProv === "tempmail"),
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
  if (mailProv === "tempmail" && Object.prototype.hasOwnProperty.call(cfg, "tempmail_domain")) {
    regMailDomains.tempmail = cfg.tempmail_domain == null ? "" : String(cfg.tempmail_domain);
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
  // Paint dedicated per-provider inputs (and show only the active panel).
  paintRegMailFieldsToInput();
  if ($("reg-expiry-ms")) {
    const exp = normalizeRegExpiryMs(cfg.expiry_ms);
    $("reg-expiry-ms").value = exp;
    // Keep select valid if browser rejected an unexpected value.
    if ($("reg-expiry-ms").value !== exp) $("reg-expiry-ms").value = "3600000";
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
  if ($("reg-concurrency")) $("reg-concurrency").value = cfg.concurrency != null ? String(cfg.concurrency) : "2";
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
  const saveEpoch = ++regConfigSaveEpoch;
  regConfigSaving = true;
  try {
    const r = await api("/accounts/register-email/config", {
      method: "PUT",
      body: JSON.stringify(cfg),
    });
    if (saveEpoch !== regConfigSaveEpoch) {
      // A newer save started; ignore this response for paint.
      return (r && r.config) || cfg;
    }
    const saved = (r && r.config) || cfg;
    // Force active provider domain/key from what we just submitted when server
    // omits empty strings, so UI stays cleared.
    const mail = String(saved.mail_provider || cfg.mail_provider || "moemail").toLowerCase();
    // Always keep every provider's dedicated slots (key/domain/base) so a save
    // for one service never blanks another in the UI.
    for (const k of [
      "moemail_api_key", "yyds_api_key", "gptmail_api_key", "cfmail_api_key", "tempmail_api_key",
      "moemail_domain", "yyds_domain", "gptmail_domain", "cfmail_domain", "tempmail_domain",
      "moemail_base_url", "cfmail_base_url", "domain", "api_key", "base_url",
    ]) {
      if (!Object.prototype.hasOwnProperty.call(saved, k)) {
        saved[k] = cfg[k] != null ? cfg[k] : "";
      }
    }
    // Submitted empty secrets/domains must win over masked/stale server echoes
    // so "delete + save" does not restore the previous value.
    for (const k of [
      "tempmail_api_key", "tempmail_domain",
      "moemail_domain", "yyds_domain", "gptmail_domain", "cfmail_domain",
      "moemail_base_url", "cfmail_base_url",
    ]) {
      if (Object.prototype.hasOwnProperty.call(cfg, k)) {
        const submitted = cfg[k] == null ? "" : String(cfg[k]);
        // Empty submit → force empty on paint (independent per-provider slot).
        if (submitted === "") saved[k] = "";
        else if (k.endsWith("_api_key")) {
          // Non-empty key: prefer plain submitted over masked server value.
          const echo = saved[k] == null ? "" : String(saved[k]);
          if (!echo || echo.indexOf("…") >= 0 || echo.indexOf("...") >= 0 || /^\*+$/.test(echo)) {
            saved[k] = submitted;
          }
        } else {
          saved[k] = submitted;
        }
      }
    }
    // Active mirrors for the selected provider (adapter-facing).
    if (mail === "yyds") {
      saved.domain = Object.prototype.hasOwnProperty.call(cfg, "yyds_domain")
        ? (cfg.yyds_domain || "")
        : (saved.yyds_domain || saved.domain || "");
      saved.api_key = Object.prototype.hasOwnProperty.call(cfg, "yyds_api_key")
        ? (cfg.yyds_api_key || "")
        : (saved.yyds_api_key || saved.api_key || "");
    } else if (mail === "gptmail") {
      saved.domain = Object.prototype.hasOwnProperty.call(cfg, "gptmail_domain")
        ? (cfg.gptmail_domain || "")
        : (saved.gptmail_domain || saved.domain || "");
      saved.api_key = Object.prototype.hasOwnProperty.call(cfg, "gptmail_api_key")
        ? (cfg.gptmail_api_key || "")
        : (saved.gptmail_api_key || saved.api_key || "");
    } else if (mail === "cfmail") {
      saved.domain = Object.prototype.hasOwnProperty.call(cfg, "cfmail_domain")
        ? (cfg.cfmail_domain || "")
        : (saved.cfmail_domain || saved.domain || "");
      saved.api_key = Object.prototype.hasOwnProperty.call(cfg, "cfmail_api_key")
        ? (cfg.cfmail_api_key || "")
        : (saved.cfmail_api_key || saved.api_key || "");
      if (!saved.base_url) saved.base_url = saved.cfmail_base_url || cfg.base_url || "";
    } else if (mail === "tempmail") {
      // Free tier defaults: empty key + empty domain; never rehydrate from other providers.
      saved.tempmail_domain = Object.prototype.hasOwnProperty.call(cfg, "tempmail_domain")
        ? (cfg.tempmail_domain == null ? "" : String(cfg.tempmail_domain))
        : (saved.tempmail_domain || "");
      saved.tempmail_api_key = Object.prototype.hasOwnProperty.call(cfg, "tempmail_api_key")
        ? (cfg.tempmail_api_key == null ? "" : String(cfg.tempmail_api_key))
        : (saved.tempmail_api_key || "");
      saved.domain = saved.tempmail_domain;
      saved.api_key = saved.tempmail_api_key;
      saved.base_url = "";
    } else {
      saved.domain = Object.prototype.hasOwnProperty.call(cfg, "moemail_domain")
        ? (cfg.moemail_domain || "")
        : (saved.moemail_domain || saved.domain || "");
      saved.api_key = Object.prototype.hasOwnProperty.call(cfg, "moemail_api_key")
        ? (cfg.moemail_api_key || "")
        : (saved.moemail_api_key || saved.api_key || "");
      if (!saved.base_url) saved.base_url = saved.moemail_base_url || cfg.base_url || "";
    }
    // Prefer form values for non-secret fields when server echoes stale/default.
    // Secrets: keep clean submitted keys if server returns masked.
    for (const k of ["count", "concurrency", "stagger_ms", "probe_delay_sec", "proxy",
      "proxy_username", "proxy_strategy", "captcha_provider", "expiry_ms",
      "moemail_domain", "yyds_domain", "gptmail_domain", "cfmail_domain", "tempmail_domain",
      "moemail_base_url", "cfmail_base_url", "domain", "base_url", "mail_provider"]) {
      if (Object.prototype.hasOwnProperty.call(cfg, k) && cfg[k] != null && cfg[k] !== "") {
        // Keep user-submitted value authoritative after save.
        if (saved[k] == null || saved[k] === "" || String(saved[k]) !== String(cfg[k])) {
          // Only override when cfg had a concrete value for this save.
          if (cfg[k] !== "" || Object.prototype.hasOwnProperty.call(cfg, k)) {
            saved[k] = cfg[k];
          }
        }
      }
    }
    // Always pin numeric fields from the form we just saved.
    for (const k of ["count", "concurrency", "stagger_ms", "probe_delay_sec"]) {
      if (cfg[k] != null && cfg[k] !== "") saved[k] = cfg[k];
    }
    for (const k of ["moemail_domain", "yyds_domain", "gptmail_domain", "cfmail_domain", "tempmail_domain",
      "moemail_base_url", "cfmail_base_url", "domain", "base_url", "proxy", "proxy_username", "proxy_strategy"]) {
      if (Object.prototype.hasOwnProperty.call(cfg, k)) saved[k] = cfg[k];
    }
    applyRegConfig(saved);
    cacheRegConfigLocal(saved);
    regConfigCache = Object.assign({}, saved);
    regConfigLoadedAt = Date.now();
    const pname = (regMailProviderMeta(mail) || {}).title || mail;
    toast(r.message || `已保存「${pname}」及全部邮箱配置到数据库`);
    return saved;
  } catch (e) {
    // Only cache on failure so a retry still has the typed values.
    cacheRegConfigLocal(cfg);
    toast((e && e.message) || "保存失败（已写本地缓存）", false);
    throw e;
  } finally {
    if (saveEpoch === regConfigSaveEpoch) regConfigSaving = false;
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
  // Skip remote reload while a save is in flight or just finished (3s grace)
  // so soft-nav / auto-refresh cannot "还原" the form to a pre-save snapshot.
  const now = Date.now();
  if (regConfigSaving || (regConfigLoadedAt && now - regConfigLoadedAt < 3000 && regConfigCache)) {
    if (regConfigCache) {
      applyRegConfig(regConfigCache);
      return regConfigCache;
    }
  }
  if (!force && !regConfigCache) loadRegConfigLocal();
  if (!force && regConfigCache && now - regConfigLoadedAt < 2000) {
    applyRegConfig(regConfigCache);
    return regConfigCache;
  }
  const loadSeq = ++regConfigLoadSeq;
  const saveEpochAtStart = regConfigSaveEpoch;
  try {
    const r = await api("/accounts/register-email/config");
    // Ignore if a save happened while we were fetching, or a newer load started.
    if (loadSeq !== regConfigLoadSeq || saveEpochAtStart !== regConfigSaveEpoch) {
      return regConfigCache;
    }
    if (regConfigSaving) {
      return regConfigCache;
    }
    const cfg = (r && r.config) || null;
    if (cfg) {
      applyRegConfig(cfg);
      cacheRegConfigLocal(cfg);
      regConfigLoadedAt = Date.now();
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
          : mailProvider === "tempmail" || mailProvider === "tempmail.lol" || mailProvider === "lol"
            ? "tempmail"
            : "moemail";
  // Keep legacy field for older backends.
  body.provider = body.mail_provider;
  // MoeMail + CF Temp Email need base_url; YYDS/GPTMail/TempMail.lol use fixed hosts.
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
  // YYDS / GPTMail / TempMail.lol are ~24h; still send 1d when selected.
  body.expiry_ms = Number.parseInt(normalizeRegExpiryMs(config.expiry_ms), 10);
  if (
    (body.mail_provider === "yyds" ||
      body.mail_provider === "gptmail" ||
      body.mail_provider === "tempmail") &&
    (body.expiry_ms === 0 || body.expiry_ms === 259200000)
  ) {
    body.expiry_ms = 86400000;
  }
  // Always send active key/domain, including empty string, so "delete + save"
  // clears DB instead of restoring the previous value.
  // TempMail.lol free: empty api_key + empty domain is valid (system random domain).
  body.api_key = config.api_key == null ? "" : String(config.api_key);
  if (body.mail_provider === "moemail") {
    body.moemail_api_key = config.moemail_api_key == null ? body.api_key : String(config.moemail_api_key);
    body.moemail_domain = config.moemail_domain == null ? body.domain : String(config.moemail_domain);
    body.domain = body.moemail_domain;
    // Adapter historical field:
    body.api_key = body.moemail_api_key;
  } else if (body.mail_provider === "yyds") {
    body.yyds_api_key = config.yyds_api_key == null ? body.api_key : String(config.yyds_api_key);
    body.yyds_domain = config.yyds_domain == null ? body.domain : String(config.yyds_domain);
    body.domain = body.yyds_domain;
    // CRITICAL: adapter only reads moemail_api_key — pass active YYDS key there.
    body.moemail_api_key = body.yyds_api_key;
    body.api_key = body.yyds_api_key;
    body.moemail_base_url = "";
    body.base_url = "";
  } else if (body.mail_provider === "gptmail") {
    body.gptmail_api_key = config.gptmail_api_key == null ? body.api_key : String(config.gptmail_api_key);
    body.gptmail_domain = config.gptmail_domain == null ? body.domain : String(config.gptmail_domain);
    body.domain = body.gptmail_domain;
    body.moemail_api_key = body.gptmail_api_key;
    body.api_key = body.gptmail_api_key;
    body.moemail_base_url = "";
    body.base_url = "";
  } else if (body.mail_provider === "cfmail") {
    body.cfmail_api_key = config.cfmail_api_key == null ? body.api_key : String(config.cfmail_api_key);
    body.cfmail_domain = config.cfmail_domain == null ? body.domain : String(config.cfmail_domain);
    body.domain = body.cfmail_domain;
    body.moemail_api_key = body.cfmail_api_key;
    body.api_key = body.cfmail_api_key;
    const cfBase = config.cfmail_base_url != null ? String(config.cfmail_base_url) : (config.base_url != null ? String(config.base_url) : "");
    body.cfmail_base_url = cfBase;
    body.moemail_base_url = cfBase;
    body.base_url = cfBase;
  } else if (body.mail_provider === "tempmail") {
    // Free tier: empty key + empty domain (api.tempmail.lol random inbox).
    // Plus/Ultra: optional Bearer key + optional custom domain.
    body.tempmail_api_key =
      config.tempmail_api_key == null ? (body.api_key || "") : String(config.tempmail_api_key || "");
    body.tempmail_domain =
      config.tempmail_domain == null ? (body.domain || "") : String(config.tempmail_domain || "");
    body.domain = body.tempmail_domain;
    body.moemail_api_key = body.tempmail_api_key;
    body.api_key = body.tempmail_api_key;
    body.moemail_base_url = "";
    body.base_url = "";
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
  const concurrency = Number.parseInt(config.concurrency || "2", 10);
  const stagger = Number.parseInt(config.stagger_ms || "300", 10);
  const probeDelay = Number.parseInt(config.probe_delay_sec || "30", 10);
  if (Number.isFinite(count) && count > 0) body.count = Math.floor(count);
  // threads / concurrency: pass the user's positive integer through unchanged.
  if (Number.isFinite(concurrency) && concurrency > 0) body.concurrency = Math.max(1, Math.floor(concurrency));
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
    startRegPolling({ immediate: true, intervalMs: 180 });
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
      for (const id of ids.slice(0, 24)) {
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
  // Prefer live progress log tail when present (real-time registration steps).
  const logs = Array.isArray(s && s.log_lines) ? s.log_lines : [];
  const tail = logs.length ? String(logs[logs.length - 1] || "") : "";
  const msg = tail || (s && (s.message || s.error)) || "";
  const probe = s && s.probe;
  let probeTxt = "";
  if (probe && typeof probe === "object") {
    probeTxt = ` | 测活 ok=${probe.ok ?? 0} fail=${probe.fail ?? 0}`;
  } else if (st === "probing") {
    probeTxt = " | 测活中…";
  }
  const shortMsg = msg ? ` | ${String(msg).slice(0, 160)}` : "";
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
  // Adapter stores free-text failure reasons under error_detail / spawn_errors
  // (batch.error is the integer fail counter and is not a message).
  const errDetail = batch && (batch.error_detail || (
    typeof batch.error === "string" && batch.error && !/^\d+$/.test(String(batch.error).trim())
      ? batch.error
      : ""
  ));
  if (errDetail) lines.push(`error_detail: ${String(errDetail).slice(0, 500)}`);
  const spawnErrs = batch && Array.isArray(batch.spawn_errors) ? batch.spawn_errors : [];
  if (spawnErrs.length) {
    lines.push("-------- 失败原因 --------");
    spawnErrs.slice(0, 20).forEach((e) => lines.push(`  · ${String(e).slice(0, 240)}`));
  }
  lines.push("-------- 会话明细 --------");
  (sessions || []).forEach((s, i) => lines.push(formatRegSessionLine(s, i)));
  // Real-time step timeline (written by adapter update() → log_lines / Redis).
  const timeline = [];
  // Prefer live sessions' steps; cap total lines so setLogPanel stays cheap.
  const liveFirst = [];
  const rest = [];
  for (const s of sessions || []) {
    const st = regStatusOf(s);
    if (REG_TERMINAL_OK.has(st) || REG_TERMINAL_BAD.has(st)) rest.push(s);
    else liveFirst.push(s);
  }
  for (const s of liveFirst.concat(rest)) {
    const rows = Array.isArray(s && s.log_lines) ? s.log_lines : [];
    const email = (s && s.email) || regSessionKey(s) || "";
    if (rows.length) {
      for (const row of rows.slice(-12)) {
        timeline.push(`${email ? email + " " : ""}${row}`);
      }
    } else if (s && (s.log || s.output_tail)) {
      String(s.log || s.output_tail)
        .split("\n")
        .filter(Boolean)
        .slice(-6)
        .forEach((row) => timeline.push(`${email ? email + " " : ""}${row}`));
    }
  }
  if (timeline.length) {
    lines.push("-------- 实时步骤 --------");
    lines.push(...timeline.slice(-48));
  }
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
  const liveLog = Array.isArray(s && s.log_lines) && s.log_lines.length
    ? s.log_lines.slice(-30).join("\n")
    : (s && (s.output_tail || s.log) ? String(s.output_tail || s.log).slice(0, 2000) : "");
  const log = buildRegLogText(s ? [s] : [], {
    batch: opts.batch || null,
    extraLines: [
      liveLog ? "-------- 步骤日志 --------\n" + liveLog : "",
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
  const pollStartedAt = (typeof performance !== "undefined" && performance.now)
    ? performance.now()
    : Date.now();
  const regApi = (path, extra) => api(path, Object.assign({ timeoutMs: REG_POLL_TIMEOUT_MS }, extra || {}));
  try {
  // Prefer batch endpoint when available for accurate total/success/fail.
  // Batch embeds compact sessions WITH log_lines — one request is the fast path.
  // Avoid extra /sessions list sweeps and deep session GETs (main latency source).
  let batch = null;
  let batchMissing = false;
  if (regBatchId) {
    try {
      batch = await regApi("/accounts/register-email/batches/" + encodeURIComponent(regBatchId));
      if (batch && Array.isArray(batch.session_ids)) {
        // Cap tracked ids — UI only needs embed window + a little headroom.
        const incoming = batch.session_ids.filter(Boolean);
        for (const id of incoming) {
          if (id && !regSessionIds.includes(id)) regSessionIds.push(id);
        }
        if (regSessionIds.length > 120) regSessionIds = regSessionIds.slice(-120);
      }
    } catch (e) {
      batchMissing = isNotFoundError(e);
      // Timeout / network: keep previous card, don't mark missing.
      if (!batchMissing && e && (e.timeout || e.network)) {
        return;
      }
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
      // Batch embed already includes log_lines. Deep-fetch ONLY when embed has
      // zero timeline AND we are not under load (slow last tick = skip extra RTT).
      const lastMs = Number(regPollLastDurationMs) || 0;
      if (lastMs < 450) {
        const needDeep = sessions
          .filter((s) => {
            const st = regStatusOf(s);
            if (REG_TERMINAL_OK.has(st) || REG_TERMINAL_BAD.has(st)) return false;
            const logs = Array.isArray(s.log_lines) ? s.log_lines : [];
            return logs.length < 1;
          })
          .map((s) => regSessionKey(s))
          .filter(Boolean);
        const n = Math.min(REG_POLL_LIVE_REFRESH, needDeep.length);
        if (n > 0) {
          const start = regPollLiveCursor % needDeep.length;
          const liveId = needDeep[start];
          regPollLiveCursor = (start + 1) % Math.max(1, needDeep.length);
          try {
            const data = await regApi("/accounts/register-email/sessions/" + encodeURIComponent(liveId));
            if (data) {
              const id = regSessionKey(data) || liveId;
              const idx = sessions.findIndex((s) => regSessionKey(s) === id);
              if (idx >= 0) sessions[idx] = data;
              else sessions.push(data);
            }
          } catch (e) {
            if (isNotFoundError(e)) sessionMisses += 1;
          }
        }
      }
    } else {
      // No batch embed: parallel-fetch a tiny window (hard cap keeps tick snappy).
      const fetchIds = ids.slice(0, 2);
      const results = await Promise.all(
        fetchIds.map(async (id) => {
          try {
            return {
              ok: true,
              data: await regApi("/accounts/register-email/sessions/" + encodeURIComponent(id)),
            };
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

    // Never call full /sessions list during steady-state polling — it is expensive
    // (Redis SCAN + merge) and was the main multi-second log lag source.
    // Only a one-shot recovery when we have zero hits and no embed at all.
    let listHasTrackedBatch = false;
    const needListSweep =
      sessionHits === 0 &&
      !batch &&
      ids.length === 0 &&
      !!regBatchId;
    if (needListSweep) try {
      const all = await regApi("/accounts/register-email/sessions");
      if (all && Array.isArray(all.sessions)) {
        const trackedIds = new Set(
          (regSessionIds && regSessionIds.length
            ? regSessionIds
            : (regSessionId ? [regSessionId] : [])
          ).map((x) => String(x || "").trim()).filter(Boolean)
        );
        for (const s of all.sessions) {
          const id = regSessionKey(s);
          if (!id) continue;
          const sameBatch = !!(regBatchId && s.batch_id && s.batch_id === regBatchId);
          const tracked = trackedIds.has(id);
          if (!(sameBatch || tracked)) continue;
          if (!regSessionIds.includes(id)) regSessionIds.push(id);
          sessions.push(s);
          sessionHits += 1;
        }
        if (regBatchId && Array.isArray(all.batches)) {
          const listed = all.batches.find((b) => (b.id || b.batch_id) === regBatchId) || null;
          if (listed) {
            listHasTrackedBatch = true;
            batch = listed;
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
      try { clearTimeout(regPollTimer); } catch (_) {}
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
    try { clearTimeout(regPollTimer); } catch (_) {}
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
      try { await afterAccountsIngested({ reset: true }); } catch (_) {
        try { await loadAccountsPage({ reset: true }); } catch (__) {}
      }
    } catch (_) {}
  } catch (_) {}
  } finally {
    try {
      const ended = (typeof performance !== "undefined" && performance.now)
        ? performance.now()
        : Date.now();
      regPollLastDurationMs = Math.max(0, ended - pollStartedAt);
    } catch (_) {
      regPollLastDurationMs = 0;
    }
    regPollInFlight = false;
    // Drain trailing-edge request so a tick that arrived mid-flight still runs.
    if (regPollPending && !regFinishedNotified && (regBatchId || regSessionId || (regSessionIds && regSessionIds.length))) {
      regPollPending = false;
      setTimeout(() => { pollRegSession().catch(() => {}); }, 40);
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
    // Broken mobile chips used to set href="undefined" → /admin/undefined → 404.
    const pathOnly = (href.split("?")[0] || "").replace(/\/$/, "");
    if (/\/admin\/(undefined|null)$/i.test(pathOnly)) {
      e.preventDefault();
      try { toast("导航链接无效，请刷新页面后重试", false); } catch (_) {}
      return;
    }
    const page = pageFromPath(pathOnly);
    if (!(page in PAGE_HREF) && page !== "overview") return;
    e.preventDefault();
    let hash = "";
    try {
      const u2 = new URL(href, location.origin);
      hash = normalizeSectionHash(u2.hash);
    } catch (_) {}
    softNavigate(page, { hash });
  }, true);

  window.addEventListener("popstate", () => {
    const page = pageFromPath(location.pathname);
    if (page === "login") return;
    const hash = normalizeSectionHash(location.hash);
    softNavigate(page, { replace: true, force: true, hash });
  });
  window.addEventListener("hashchange", () => {
    try {
      const root = pageSubnavRoot();
      if (!root) return;
      setPageSection(normalizeSectionHash(location.hash), { replace: true, skipHistory: true });
    } catch (_) {}
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
on("btn-refresh-all", "onclick", async () => {
  try {
    const page = document.body.dataset.page || pageFromPath(location.pathname) || "";
    if (page === "keys" && typeof renderKeys === "function") {
      await renderKeys();
      toast("已刷新 Key 列表");
      return;
    }
    statusCache = await api("/status");
    await loadDashboard();
    toast("已刷新");
  } catch (e) { toast(e.message, false); }
});

on("btn-create-key", "onclick", () => { createApiKeyFromForm(); });


on("keys-tbody", "onclick", async (e) => {
  // Soft-nav primary handler binds via bindKeysControls (_g2aBound).
  if ($("keys-tbody") && $("keys-tbody")._g2aBound) return;
  const btn = e.target && e.target.closest ? e.target.closest("button[data-act]") : null;
  if (!btn) return;
  await handleKeyRowAction(btn);
});


on("accounts-tbody", "onclick", async (e) => {  // checkbox selection
  // Soft-nav rebindPageControls attaches the primary handler (_g2aBound).
  // Skip this legacy bootstrap handler entirely to avoid double API calls
  // and full list reloads after quota / model test.
  if ($("accounts-tbody") && $("accounts-tbody")._g2aBound) return;

  const chk = e.target.closest(".acc-check-one");
  if (chk) {
    const id = accountIdKey(chk.dataset.id);
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
        if (!q || typeof q !== "object") { toast("额度查询返回空结果", false); return; }
        quotaCache[id] = q;
        if (q.auto_disabled || q.exhausted || q.disabled_for_quota) toast("该账号额度已耗尽，已进入冷却池", false);
        else if (q.ok) toast((q.display && q.display.summary) || "额度已更新");
        else toast(q.error || "额度查询失败", false);
        let qPatch = {};
        if (q.pool && typeof q.pool === "object" && typeof poolPatchFromStatusAccount === "function") {
          qPatch = poolPatchFromStatusAccount({ pool: q.pool, _pool: q.pool }) || {};
          qPatch.last_quota = q;
        } else if (typeof poolPatchFromQuotaResult === "function") {
          qPatch = poolPatchFromQuotaResult(q) || {};
        } else {
          const dead = !!(q.auto_disabled || q.exhausted || q.disabled_for_quota);
          qPatch = {
            last_quota: q,
            disabled_for_quota: dead,
            enabled: dead ? false : true,
            pool_status: dead ? "quota_disabled" : "normal",
            disabled_reason: null,
                cooldown_reason: dead ? (q.exhaust_reason || q.error || "额度耗尽") : null,
                pool_status: dead ? "cooldown" : (qPatch.pool_status || "normal"),
                in_cooldown: !!dead,
          };
        }
        if (!qPatch.last_quota) qPatch.last_quota = q;
        // Hot-update ONLY this account row — never rewrite the whole pool list.
        applyAccountLivePatch(id, { _pool: qPatch });
        Promise.resolve().then(() => softRefreshPoolChips({ stats: true }));
      } catch (err) {
        toast((err && err.message) || "额度查询失败", false);
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
            // Hot-update this row only — do not reload the whole account pool list.
            const enPool = en
              ? {
                  enabled: true,
                  disabled_for_quota: false,
                  disabled_reason: null,
                  quota_disabled_at: null,
                  quota_source: null,
                  pool_status: "normal",
                }
              : { enabled: false, pool_status: "disabled" };
            applyAccountLivePatch(id, { _pool: enPool });
            Promise.resolve().then(() => softRefreshPoolChips({ stats: true }));
          } catch (err) {
            toast((err && err.message) || "操作失败", false);
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
        selectedAccountIds.delete(accountIdKey(id));
            accountsList = (accountsList || []).filter((a) => accountIdKey(a.id) !== accountIdKey(id));
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


/** After any入库 path (JSON / SSO / device / reg): force list + chips to match DB. */
async function afterAccountsIngested({ reset = true, silent = false } = {}) {
  try {
    _statusFetchedAt = 0;
  } catch (_) {}
  // Prefer silent list refresh after bulk SSO/JSON import so the page does not
  // thrash (full dashboard + chips + table rewrites) for multi-hundred accounts.
  const useSilent = silent || true;
  try {
    if (typeof loadAccountsPage === "function") {
      await loadAccountsPage({ reset: !!reset, silent: useSilent });
      try {
        if (typeof softRefreshPoolChips === "function") {
          await softRefreshPoolChips({ stats: true });
        } else if (typeof renderAccountStatusChips === "function") {
          renderAccountStatusChips();
        }
      } catch (_) {}
      return;
    }
  } catch (e) {
    console.warn("afterAccountsIngested loadAccountsPage", e);
  }
  try {
    if (typeof refreshAccountsListUI === "function") {
      await refreshAccountsListUI({ force: true });
      return;
    }
  } catch (e) {
    console.warn("afterAccountsIngested refreshAccountsListUI", e);
  }
  try {
    await loadDashboard();
  } catch (__) {}
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
      try { await afterAccountsIngested({ reset: true }); } catch (_) { try { await loadDashboard(); } catch (__) {} }
      return;
    }

        // Sync response (Go /admin/api/accounts/import-files is synchronous; no job_id).
    if (!started.job_id) {
      const importedArr = Array.isArray(started.imported) ? started.imported : [];
      const count = Number(
        started.count != null ? started.count
          : (started.success != null ? started.success : importedArr.length)
      ) || importedArr.length || 0;
      const parseErrors = Number(started.parse_errors || 0) || 0;
      const fileResults = Array.isArray(started.file_results) ? started.file_results
        : (Array.isArray(started.file_meta) ? started.file_meta : []);
      const fileFail = fileResults.filter((x) => x && x.ok === false).length || parseErrors;
      const okFiles = Math.max(0, files.length - fileFail);
      // file_meta for log panel
      const metaLines = fileResults.map((x, idx) => {
        if (!x) return "";
        const name = x.filename || x.name || `file#${x.index || idx + 1}`;
        return `${x.ok === false ? "❌" : "✅"} ${name}${x.error ? " · " + x.error : (x.count != null ? " · " + x.count + " 条" : "")}`;
      }).filter(Boolean);
      setJsonIoProgress({
        percent: 100,
        label: started.message || (count ? `导入完成：${count} 个账号` : "导入完成"),
        detail: parseErrors ? `${parseErrors} 个文件解析失败` : (started.total_accounts != null ? `库内共 ${started.total_accounts}` : ""),
        done: files.length,
        total: files.length,
        success: count,
        fail: fileFail,
        status: (fileFail > 0 && count > 0) ? "partial" : (fileFail > 0 && count === 0 ? "error" : "done"),
      });
      setLogPanel(
        "json-io-result",
        [started.message || `导入完成：${count} 个账号`, metaLines.length ? metaLines.join("\n") : ""].filter(Boolean).join("\n") || "—",
        { forceShow: true }
      );
      toast(
        started.message || `导入完成：${count} 个账号` + (fileFail ? `，${fileFail} 个文件失败` : ""),
        count > 0 || fileFail === 0
      );
      if (input) input.value = "";
      if (nameLabelId && $(nameLabelId)) $(nameLabelId).textContent = "未选择文件";
      try { await afterAccountsIngested({ reset: true }); } catch (_) { try { await loadDashboard(); } catch (__) {} }
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
    try { await afterAccountsIngested({ reset: true }); } catch (_) { try { await loadDashboard(); } catch (__) {} }
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
    wrap.classList.toggle("is-done", status === "done" || status === "partial");
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
        // Higher concurrency for bulk convert; backend hard-caps at SSO_IMPORT_WORKERS.
        max_workers: delay >= 5 ? 4 : (delay >= 1 ? 8 : 12),
      }),
    });

    // Backward-compat: old sync response already has results.
    if (!started.job_id && Array.isArray(started.results)) {
      const rows = formatSsoResultRows(started.results || []);
      const successN = Number(started.success || 0) || 0;
      const failN = Number(started.fail || 0) || 0;
      const st = started.ok === false && successN === 0 ? "error" : (failN > 0 ? "partial" : "done");
      setSsoProgress({
        percent: 100,
        label: started.message || "SSO 导入完成",
        done: started.total || lines.length,
        total: started.total || lines.length,
        success: successN,
        fail: failN,
        status: st,
      });
      setLogPanel("sso-result", `${started.message || ""}\n${rows.join("\n")}`, { forceShow: true });
      toast(started.message || `SSO 导入完成：${successN}/${started.total || lines.length}`, successN > 0);
      if (successN > 0) {
        if (ta) ta.value = "";
        if (fileInput) fileInput.value = "";
        if ($("sso-file-name")) $("sso-file-name").textContent = "未选择文件";
      }
      try { await afterAccountsIngested({ reset: true }); } catch (_) { try { await loadDashboard(); } catch (__) {} }
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
      if (st === "done" || st === "partial" || st === "error") break;
      await new Promise((resolve) => setTimeout(resolve, 900));
    }

    if (!finalJob || !["done", "partial", "error"].includes(String(finalJob.status || ""))) {
      // One last fetch
      try { finalJob = await pollSsoImportJob(jobId, { totalHint: lines.length }); } catch (_) {}
    }

    const st = String((finalJob && finalJob.status) || "");
    if (st !== "done" && st !== "partial" && st !== "error") {
      throw new Error("SSO 导入超时，请稍后刷新账号列表确认是否已部分入库");
    }

    const rows = formatSsoResultRows((finalJob && finalJob.results) || []);
    const successN = Number((finalJob && finalJob.success) || 0) || 0;
    const failN = Number((finalJob && finalJob.fail) || 0) || 0;
    const msg =
      (finalJob && finalJob.message) ||
      `SSO 导入完成：${successN} 成功, ${failN} 失败`;
    setSsoProgress({
      percent: 100,
      label: msg,
      detail: finalJob.job_id ? `job_id: ${finalJob.job_id}` : "",
      done: finalJob.total || lines.length,
      total: finalJob.total || lines.length,
      success: successN,
      fail: failN,
      status: st === "error" && successN === 0 ? "error" : (failN > 0 ? "partial" : "done"),
    });
    setLogPanel("sso-result", `${msg}\n${rows.join("\n")}`, { forceShow: true });
    // Green when any success; red on total failure / error with zero imports.
    toast(msg, successN > 0);
    // Refresh list whenever something landed (done/partial with success).
    if (successN > 0 || st === "done" || st === "partial") {
      if (successN > 0) {
        if (ta) ta.value = "";
        if (fileInput) fileInput.value = "";
        if ($("sso-file-name")) $("sso-file-name").textContent = "未选择文件";
      }
      try {
        if (typeof refreshAccountsListUI === "function") {
          await refreshAccountsListUI({ force: true });
        } else {
          await loadAccountsPage({ reset: true });
        }
      } catch (_) {
        try { await loadDashboard(); } catch (__) {}
      }
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
      try { await afterAccountsIngested({ reset: false }); } catch (_) { try { await loadDashboard(); } catch(__){} }
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

// Module-level fallbacks (first paint). Soft-nav rebinds the same ids in rebindPageControls.
on("import-file", "onchange", () => {
  const files = $("import-file") && $("import-file").files;
  const label = $("import-file-name");
  if (!label) return;
  if (!files || !files.length) label.textContent = "未选择文件";
  else if (files.length === 1) label.textContent = `已选择：${files[0].name}（${(files[0].size / 1024).toFixed(1)} KB）`;
  else {
    const totalKb = Array.from(files).reduce((s, f) => s + f.size, 0) / 1024;
    label.textContent = `已选择 ${files.length} 个文件（共 ${totalKb.toFixed(1)} KB）`;
  }
});
on("btn-import", "onclick", () => importJsonFiles());
on("btn-import-sso", "onclick", () => importSsoCookies());
on("btn-export-sso", "onclick", () => exportRegistrationSso());
on("sso-file", "onchange", () => {
  const f = $("sso-file") && $("sso-file").files && $("sso-file").files[0];
  const label = $("sso-file-name");
  if (!label) return;
  label.textContent = f ? `已选择：${f.name}（${(f.size / 1024).toFixed(1)} KB）` : "未选择文件";
});
if ($("btn-export")) {
  on("btn-export", "onclick", () => exportAllAccounts());
}

// Module-level fallback; soft-nav rebind overwrites via rebindPageControls → refreshAccountsListUI.
on("btn-refresh-acc", "onclick", async () => {
  try {
    if (typeof refreshAccountsListUI === "function") {
      await refreshAccountsListUI({ toastOk: "已热更新", force: true });
    } else {
      await loadAccountsPage({ reset: false, silent: !!(accountsList && accountsList.length) });
      toast("已热更新");
    }
    if (loginSessionId) await pollDeviceSession();
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
    const hasPw = !!(cfg.has_password || (cfg.password && String(cfg.password).trim()));
    $("set-sub2api-password").placeholder = hasPw ? "已保存，留空不改" : "登录密码";
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
    const hasKey = !!(cfg.has_management_key || (cfg.management_key && String(cfg.management_key).trim()));
    $("set-cliproxyapi-key").placeholder = hasKey ? "已保存，留空不改" : "Management Key";
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
  const autoPush = !!(
    $("set-cliproxyapi-auto-push-register") && $("set-cliproxyapi-auto-push-register").checked
  );
  // auto_push_on_register requires enabled; otherwise registration skips with cliproxyapi_disabled.
  let enabled = !!( $("set-cliproxyapi-enabled") && $("set-cliproxyapi-enabled").checked );
  if (autoPush) {
    enabled = true;
    if ($("set-cliproxyapi-enabled") && !$("set-cliproxyapi-enabled").checked) {
      $("set-cliproxyapi-enabled").checked = true;
    }
  }
  const patch = {
    enabled,
    base_url: $("set-cliproxyapi-url") ? ($("set-cliproxyapi-url").value || "").trim() : "",
    auto_push_on_register: autoPush,
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
  else if (window.__g2aSettingsResetClearSecrets) {
    patch.management_key = "";
    patch.clear_management_key = true;
  }
  return patch;
}

async function saveCliproxyapiConfig(opts) {
  const __silent = !!(arguments[0] && arguments[0].silent);

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
    toast(msg, ok);
    return r;
  } catch (e) {
    if (pre) pre.textContent = String(e.message || e);
    toast(e.message || String(e), false);
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
      fail === 0
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
  const autoPush = !!(
    $("set-sub2api-auto-push-register") && $("set-sub2api-auto-push-register").checked
  );
  // auto_push_on_register requires enabled; otherwise registration skips with sub2api_disabled.
  let enabled = !!( $("set-sub2api-enabled") && $("set-sub2api-enabled").checked );
  if (autoPush) {
    enabled = true;
    if ($("set-sub2api-enabled") && !$("set-sub2api-enabled").checked) {
      $("set-sub2api-enabled").checked = true;
    }
  }
  const patch = {
    enabled,
    base_url: $("set-sub2api-url") ? ($("set-sub2api-url").value || "").trim() : "",
    email: $("set-sub2api-email") ? ($("set-sub2api-email").value || "").trim() : "",
    group_name: $("set-sub2api-group-name") ? ($("set-sub2api-group-name").value || "").trim() : "",
    auto_create_group: !!( $("set-sub2api-auto-group") && $("set-sub2api-auto-group").checked ),
    auto_push_on_register: autoPush,
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
  else if (window.__g2aSettingsResetClearSecrets) {
    // Reset-default: explicitly clear stored password.
    patch.password = "";
    patch.clear_password = true;
  }
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
  // Surface auto-push readiness so the option is not a silent no-op.
  try {
    const cfg = (r && r.config) || patch || {};
    if (cfg.auto_push_on_register && !cfg.enabled) {
      console.warn("sub2api auto_push_on_register set but enabled=false");
    }
  } catch (_) {}
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
    toast(r && r.ok ? `连接成功，${r.group_count || 0} 个分组` : (r && r.error) || "失败", !!(r && r.ok));
    return r;
  } catch (e) {
    if (pre) pre.textContent = String(e.message || e);
    toast(e.message || String(e), false);
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
    toast(e.message || String(e), false);
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
  toast(r && r.ok ? `分组已创建 #${(r.group && r.group.id) || "?"}` : "创建失败", !!(r && r.ok));
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
    toast(`sub2api 导入完成：成功 ${ok} / 失败 ${fail} / 共 ${total}`, fail === 0);
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
    toast(e.message || String(e), false);
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
    toast(e.message || String(e), false);
  }
}

function bindSub2apiUi() {
  on("btn-sub2api-test", "onclick", () => { testSub2apiConnection().catch(() => {}); });
  on("btn-sub2api-load-groups", "onclick", () => { loadSub2apiGroups().catch((e) => toast(e.message || String(e), false)); });
  on("btn-sub2api-create-group", "onclick", () => { createSub2apiGroup().catch((e) => toast(e.message || String(e), false)); });
  // Checking "注册后自动入库" also turns on the integration (push requires enabled).
  on("set-sub2api-auto-push-register", "onchange", () => {
    const auto = $("set-sub2api-auto-push-register");
    if (auto && auto.checked && $("set-sub2api-enabled")) {
      $("set-sub2api-enabled").checked = true;
    }
  });
  on("set-cliproxyapi-auto-push-register", "onchange", () => {
    const auto = $("set-cliproxyapi-auto-push-register");
    if (auto && auto.checked && $("set-cliproxyapi-enabled")) {
      $("set-cliproxyapi-enabled").checked = true;
    }
  });
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
  if ($("set-model-health-batch") && s.model_health_probe_batch != null) {
    $("set-model-health-batch").value = s.model_health_probe_batch;
  }
  if ($("set-model-health-workers") && s.model_health_probe_workers != null) {
    $("set-model-health-workers").value = s.model_health_probe_workers;
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
  if ($("set-debug-shell-args")) $("set-debug-shell-args").checked = !!s.debug_shell_args;
  // Pool / kick policy — fill with effective values (server defaults when DB
  // has none). Cooldown itself is a sticky status (no duration knobs).
  const polDefaults = {
    soft_model_block_ttl_sec: 180,
    durable_model_block_ttl_sec: 3600,
    probe_fail_kick_streak: 2,
    probe_fail_disable_streak: 4,
    max_failover_attempts: 4,
  };
  const pol = Object.assign({}, polDefaults, s.pool_policy || {}, {
    soft_model_block_ttl_sec: s.soft_model_block_ttl_sec,
    durable_model_block_ttl_sec: s.durable_model_block_ttl_sec,
    probe_fail_kick_streak: s.probe_fail_kick_streak,
    probe_fail_disable_streak: s.probe_fail_disable_streak,
    max_failover_attempts: s.max_failover_attempts,
  });
  // Drop undefined/null so defaults remain.
  Object.keys(pol).forEach((k) => { if (pol[k] == null) delete pol[k]; });
  const polEff = Object.assign({}, polDefaults, pol);
  if ($("set-soft-ttl")) $("set-soft-ttl").value = polEff.soft_model_block_ttl_sec;
  if ($("set-durable-ttl")) $("set-durable-ttl").value = polEff.durable_model_block_ttl_sec;
  if ($("set-probe-kick-streak")) $("set-probe-kick-streak").value = polEff.probe_fail_kick_streak;
  if ($("set-probe-disable-streak")) $("set-probe-disable-streak").value = polEff.probe_fail_disable_streak;
  if ($("set-max-failover")) $("set-max-failover").value = polEff.max_failover_attempts;
  // Outbound proxy pool (account chat / probe / refresh)
  const ob = s.outbound_proxy_config || s.outbound_proxy || {};
  if ($("set-outbound-proxy-enabled")) {
    $("set-outbound-proxy-enabled").checked = ob.enabled !== false;
  }
  if ($("set-outbound-proxy")) $("set-outbound-proxy").value = ob.proxy || "";
  if ($("set-outbound-proxy-username")) $("set-outbound-proxy-username").value = ob.proxy_username || "";
  // Never echo proxy secrets into the form (has_password only).
  if ($("set-outbound-proxy-password")) {
    $("set-outbound-proxy-password").value = "";
    const hasPw = !!(ob.has_password || (ob.proxy_password && String(ob.proxy_password).trim()));
    $("set-outbound-proxy-password").placeholder = hasPw ? "已保存，留空不改" : "可选，共享认证";
  }
  if ($("set-outbound-proxy-strategy")) {
    const st = String(ob.proxy_strategy || "round_robin").toLowerCase();
    $("set-outbound-proxy-strategy").value =
      st === "random" ? "random" : st === "sticky" ? "sticky" : "round_robin";
  }
  try { updateOutboundProxyHint(s); } catch (_) {}
  // sub2api push config
  try { fillSub2apiForm(s && s.sub2api_config); } catch (_) {}
  try { fillCliproxyapiForm(s && s.cliproxyapi_config); } catch (_) {}
  // Admin password fields must stay empty — never prefill from API / password managers.
  clearAdminPasswordFields();
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
  try { bindVersionSettingsUI(); } catch (_) {}
  try { checkVersionUpdate({ soft: true }); } catch (_) {}
  clearAdminPasswordFields();
  // Browsers may autofill password fields after paint; clear again shortly after.
  setTimeout(clearAdminPasswordFields, 0);
  setTimeout(clearAdminPasswordFields, 250);
  setTimeout(clearAdminPasswordFields, 1000);
  return s;
}

function collectSystemSettingsPatch(groups) {
  // groups: undefined/null = all (legacy). Or array of: pool|proxy|relay|cooldown|sub2api|cliproxyapi
  const want = (name) => {
    if (!groups || !groups.length) return true;
    return groups.indexOf(name) >= 0;
  };
  const patch = {};
  if (want("pool")) {
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
    if ($("set-model-health-batch") && $("set-model-health-batch").value !== "") {
      patch.model_health_probe_batch = Number($("set-model-health-batch").value);
    }
    if ($("set-model-health-workers") && $("set-model-health-workers").value !== "") {
      patch.model_health_probe_workers = Number($("set-model-health-workers").value);
    }
    if ($("set-affinity-ttl") && $("set-affinity-ttl").value !== "") {
      patch.conversation_affinity_ttl_sec = Number($("set-affinity-ttl").value);
    }
    if ($("set-probe-models")) {
      patch.probe_models = ($("set-probe-models").value || "").trim();
    }
  }
  if (want("relay")) {
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
    if ($("set-debug-shell-args")) patch.debug_shell_args = !!$("set-debug-shell-args").checked;
  }
  if (want("cooldown")) {
    if ($("set-soft-ttl") && $("set-soft-ttl").value !== "") patch.soft_model_block_ttl_sec = Number($("set-soft-ttl").value);
    if ($("set-durable-ttl") && $("set-durable-ttl").value !== "") patch.durable_model_block_ttl_sec = Number($("set-durable-ttl").value);
    if ($("set-probe-kick-streak") && $("set-probe-kick-streak").value !== "") patch.probe_fail_kick_streak = Number($("set-probe-kick-streak").value);
    if ($("set-probe-disable-streak") && $("set-probe-disable-streak").value !== "") patch.probe_fail_disable_streak = Number($("set-probe-disable-streak").value);
    if ($("set-max-failover") && $("set-max-failover").value !== "") patch.max_failover_attempts = Number($("set-max-failover").value);
  }
  if (want("proxy")) {
    if ($("set-outbound-proxy-enabled")) patch.outbound_proxy_enabled = !!$("set-outbound-proxy-enabled").checked;
    if ($("set-outbound-proxy")) patch.outbound_proxy = $("set-outbound-proxy").value || "";
    if ($("set-outbound-proxy-username")) patch.outbound_proxy_username = $("set-outbound-proxy-username").value || "";
    if ($("set-outbound-proxy-password")) {
      const pw = $("set-outbound-proxy-password").value || "";
      if (pw) patch.outbound_proxy_password = pw;
      else if (window.__g2aSettingsResetClearSecrets) {
        patch.outbound_proxy_password = "";
        patch.clear_outbound_proxy_password = true;
      }
    }
    if ($("set-outbound-proxy-strategy")) patch.outbound_proxy_strategy = $("set-outbound-proxy-strategy").value || "round_robin";
  }
  if (want("sub2api")) {
    try {
      const s2 = collectSub2apiPatch();
      if (s2) patch.sub2api_config = s2;
    } catch (_) {}
  }
  if (want("cliproxyapi")) {
    try {
      const cpa = collectCliproxyapiPatch();
      if (cpa) patch.cliproxyapi_config = cpa;
    } catch (_) {}
  }
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

async function saveSystemSettings(opts) {
  const options = opts || {};
  const groups = options.groups || null; // null = all
  const btn = options.btnId ? $(options.btnId) : ($("btn-save-settings") || null);
  const label = options.label || "设置";
  if (btn) {
    if (!btn.dataset.label) btn.dataset.label = btn.textContent;
    btn.disabled = true;
    btn.textContent = "保存中…";
  }
  try {
    const patch = collectSystemSettingsPatch(groups);
    if (patch.outbound_max_tools != null && (Number.isNaN(patch.outbound_max_tools) || patch.outbound_max_tools < 0)) {
      throw new Error("每轮工具数无效");
    }
    if (patch.outbound_tool_gap_sec != null && (Number.isNaN(patch.outbound_tool_gap_sec) || patch.outbound_tool_gap_sec < 0)) {
      throw new Error("工具间隔无效");
    }
    const saveAll = !groups || !groups.length;
    const wantS2 = saveAll || (groups && groups.indexOf("sub2api") >= 0);
    const wantCpa = saveAll || (groups && groups.indexOf("cliproxyapi") >= 0);
    let s2err = null;
    let cpaErr = null;
    if (wantS2) {
      try {
        if ($("set-sub2api-url") || $("set-sub2api-email")) {
          await saveSub2apiConfig({ silent: true });
        }
      } catch (e) {
        s2err = e;
        console.warn("sub2api save failed", e);
        if (groups && groups.length === 1 && groups[0] === "sub2api") throw e;
      }
    }
    if (wantCpa) {
      try {
        if ($("set-cliproxyapi-url") || $("set-cliproxyapi-key")) {
          await saveCliproxyapiConfig({ silent: true });
        }
      } catch (e) {
        cpaErr = e;
        console.warn("cliproxyapi save failed", e);
        if (groups && groups.length === 1 && groups[0] === "cliproxyapi") throw e;
      }
    }
    if (patch.sub2api_config) delete patch.sub2api_config;
    if (patch.cliproxyapi_config) delete patch.cliproxyapi_config;

    // Only PUT /settings when there are general keys (not pure sub2api/cpa card).
    const generalKeys = Object.keys(patch);
    let s = null;
    if (generalKeys.length) {
      const r = await api("/settings", { method: "PUT", body: JSON.stringify(patch) });
      s = (r && r.settings) || patch;
      if (dashCache) dashCache.settings = Object.assign({}, dashCache.settings || {}, s);
      if (statusCache) statusCache.settings = Object.assign({}, statusCache.settings || {}, s);
      fillSystemSettingsForm(s);
    }
    if (wantS2) {
      try {
        const s2 = await api("/settings/sub2api");
        if (s2 && s2.config) fillSub2apiForm(s2.config);
      } catch (_) {}
    }
    if (wantCpa) {
      try {
        const cpa = await api("/settings/cliproxyapi");
        if (cpa && cpa.config) fillCliproxyapiForm(cpa.config);
      } catch (_) {}
    }
    try { await refreshOverviewStatus({ force: true, render: true }); } catch (_) {}
    if (s2err || cpaErr) {
      const parts = [];
      if (s2err) parts.push("sub2api: " + (s2err.message || s2err));
      if (cpaErr) parts.push("CLIProxyAPI: " + (cpaErr.message || cpaErr));
      toast(label + " 已保存，但 " + parts.join("；"), true);
    } else {
      toast((options.toastSuffix) ? (label + " " + options.toastSuffix) : (label + " 已保存"));
    }
    // Light status refresh for updated_at
    try {
      const up = $("settings-updated-at");
      if (up) up.textContent = "上次更新：" + new Date().toLocaleString();
    } catch (_) {}
    return s || patch;
  } finally {
    if (btn) {
      btn.disabled = false;
      btn.textContent = btn.dataset.label || "保存";
    }
  }
}


/** Built-in defaults per settings card (matches server PublicSettings defaults). */
function settingsGroupDefaults(group) {
  switch (group) {
    case "pool":
      return {
        account_mode: "round_robin",
        default_model: "grok-4.5",
        token_maintain_enabled: true,
        model_health_enabled: true,
        model_health_auto_disable: true,
        conversation_affinity_enabled: true,
        token_maintain_interval_sec: 90,
        token_refresh_skew_sec: 120,
        model_health_interval_sec: 180,
        model_health_probe_batch: 120,
        model_health_probe_workers: 12,
        conversation_affinity_ttl_sec: 7200,
        probe_models: "grok-4.5",
      };
    case "proxy":
      return {
        outbound_proxy_enabled: true,
        outbound_proxy: "",
        outbound_proxy_username: "",
        outbound_proxy_password: "",
        outbound_proxy_strategy: "round_robin",
      };
    case "relay":
      return {
        reasoning_compat: "off",
        outbound_max_tools: 1,
        outbound_max_tools_openai: 0,
        outbound_tool_gap_sec: 0.08,
        sse_keepalive: 8,
        history_compact_enabled: false,
        history_compact_auto_chars: 0,
        history_keep_tool_rounds: 32,
        history_max_tool_result_chars: 48000,
        debug_shell_args: false,
      };
    case "cooldown":
      return {
        soft_model_block_ttl_sec: 180,
        durable_model_block_ttl_sec: 3600,
        probe_fail_kick_streak: 2,
        probe_fail_disable_streak: 4,
        max_failover_attempts: 4,
      };
    case "sub2api":
      return {
        enabled: false,
        url: "",
        email: "",
        password: "",
        default_group_id: "",
        default_group_name: "grokcli-2api",
        auto_create_group: true,
        auto_push_on_register: false,
        push_concurrency: 4,
        account_concurrency: 3,
        account_priority: 50,
        account_rate_multiplier: 1,
        notes_prefix: "grokcli-2api",
      };
    case "cliproxyapi":
      return {
        enabled: false,
        url: "",
        management_key: "",
        auto_push_on_register: false,
      };
    default:
      return {};
  }
}

/** Apply defaults into the form for one card only (no network). */
function applySettingsGroupDefaults(group) {
  const d = settingsGroupDefaults(group);
  if (group === "pool") {
    if ($("set-account-mode")) $("set-account-mode").value = d.account_mode;
    if ($("set-default-model")) $("set-default-model").value = d.default_model;
    if ($("set-token-maintain")) $("set-token-maintain").checked = d.token_maintain_enabled;
    if ($("set-model-health")) $("set-model-health").checked = d.model_health_enabled;
    if ($("set-model-health-auto-disable")) $("set-model-health-auto-disable").checked = d.model_health_auto_disable;
    if ($("set-affinity")) $("set-affinity").checked = d.conversation_affinity_enabled;
    if ($("set-token-maintain-interval")) $("set-token-maintain-interval").value = d.token_maintain_interval_sec;
    if ($("set-token-refresh-skew")) $("set-token-refresh-skew").value = d.token_refresh_skew_sec;
    if ($("set-model-health-interval")) $("set-model-health-interval").value = d.model_health_interval_sec;
    if ($("set-model-health-batch")) $("set-model-health-batch").value = d.model_health_probe_batch;
    if ($("set-model-health-workers")) $("set-model-health-workers").value = d.model_health_probe_workers;
    if ($("set-affinity-ttl")) $("set-affinity-ttl").value = d.conversation_affinity_ttl_sec;
    if ($("set-probe-models")) $("set-probe-models").value = d.probe_models;
  } else if (group === "proxy") {
    if ($("set-outbound-proxy-enabled")) $("set-outbound-proxy-enabled").checked = d.outbound_proxy_enabled;
    if ($("set-outbound-proxy")) $("set-outbound-proxy").value = d.outbound_proxy;
    if ($("set-outbound-proxy-username")) $("set-outbound-proxy-username").value = d.outbound_proxy_username;
    if ($("set-outbound-proxy-password")) {
      $("set-outbound-proxy-password").value = "";
      $("set-outbound-proxy-password").placeholder = "可选，共享认证";
    }
    if ($("set-outbound-proxy-strategy")) $("set-outbound-proxy-strategy").value = d.outbound_proxy_strategy;
    try { updateOutboundProxyHint(); } catch (_) {}
  } else if (group === "relay") {
    if ($("set-reasoning")) $("set-reasoning").value = d.reasoning_compat;
    if ($("set-max-tools")) $("set-max-tools").value = d.outbound_max_tools;
    if ($("set-max-tools-openai")) $("set-max-tools-openai").value = d.outbound_max_tools_openai;
    if ($("set-tool-gap")) $("set-tool-gap").value = d.outbound_tool_gap_sec;
    if ($("set-sse-keepalive")) $("set-sse-keepalive").value = d.sse_keepalive;
    if ($("set-history-compact")) $("set-history-compact").checked = d.history_compact_enabled;
    if ($("set-history-auto-chars")) $("set-history-auto-chars").value = d.history_compact_auto_chars;
    if ($("set-history-keep-rounds")) $("set-history-keep-rounds").value = d.history_keep_tool_rounds;
    if ($("set-history-tool-max")) $("set-history-tool-max").value = d.history_max_tool_result_chars;
    if ($("set-debug-shell-args")) $("set-debug-shell-args").checked = !!d.debug_shell_args;
  } else if (group === "cooldown") {
    if ($("set-soft-ttl")) $("set-soft-ttl").value = d.soft_model_block_ttl_sec;
    if ($("set-durable-ttl")) $("set-durable-ttl").value = d.durable_model_block_ttl_sec;
    if ($("set-probe-kick-streak")) $("set-probe-kick-streak").value = d.probe_fail_kick_streak;
    if ($("set-probe-disable-streak")) $("set-probe-disable-streak").value = d.probe_fail_disable_streak;
    if ($("set-max-failover")) $("set-max-failover").value = d.max_failover_attempts;
  } else if (group === "sub2api") {
    if ($("set-sub2api-enabled")) $("set-sub2api-enabled").checked = d.enabled;
    if ($("set-sub2api-url")) $("set-sub2api-url").value = d.url;
    if ($("set-sub2api-email")) $("set-sub2api-email").value = d.email;
    if ($("set-sub2api-password")) {
      $("set-sub2api-password").value = "";
      $("set-sub2api-password").placeholder = "已保存则留空不改";
    }
    if ($("set-sub2api-group-id")) $("set-sub2api-group-id").value = d.default_group_id;
    if ($("set-sub2api-group-name")) $("set-sub2api-group-name").value = d.default_group_name;
    if ($("set-sub2api-auto-group")) $("set-sub2api-auto-group").checked = d.auto_create_group;
    if ($("set-sub2api-auto-push-register")) $("set-sub2api-auto-push-register").checked = d.auto_push_on_register;
    if ($("set-sub2api-concurrency")) $("set-sub2api-concurrency").value = d.push_concurrency;
    if ($("set-sub2api-account-concurrency")) $("set-sub2api-account-concurrency").value = d.account_concurrency;
    if ($("set-sub2api-account-priority")) $("set-sub2api-account-priority").value = d.account_priority;
    if ($("set-sub2api-account-rate")) $("set-sub2api-account-rate").value = d.account_rate_multiplier;
    if ($("set-sub2api-notes")) $("set-sub2api-notes").value = d.notes_prefix;
    if ($("sub2api-pill")) {
      $("sub2api-pill").textContent = "默认";
      $("sub2api-pill").className = "g2a-tag";
    }
  } else if (group === "cliproxyapi") {
    if ($("set-cliproxyapi-enabled")) $("set-cliproxyapi-enabled").checked = d.enabled;
    if ($("set-cliproxyapi-url")) $("set-cliproxyapi-url").value = d.url;
    if ($("set-cliproxyapi-key")) {
      $("set-cliproxyapi-key").value = "";
      $("set-cliproxyapi-key").placeholder = "Management API Key";
    }
    // optional auto-push checkbox if present
    if ($("set-cliproxyapi-auto-push-register")) {
      $("set-cliproxyapi-auto-push-register").checked = d.auto_push_on_register;
    }
    if ($("cliproxyapi-pill")) {
      $("cliproxyapi-pill").textContent = "默认";
      $("cliproxyapi-pill").className = "g2a-tag";
    }
  }
}

async function resetSettingsCard(group, btnId, label) {
  const name = label || group;
  if (!confirm(`将「${name}」恢复为系统默认值并保存？\n仅影响本卡片，其它设置不变。`)) {
    return;
  }
  applySettingsGroupDefaults(group);
  // Tell collectors to emit empty secrets so server can clear them.
  window.__g2aSettingsResetClearSecrets = true;
  try {
    await saveSystemSettings({
      groups: [group],
      btnId: btnId || null,
      label: name,
      toastSuffix: "已重置为默认并保存",
    });
  } finally {
    window.__g2aSettingsResetClearSecrets = false;
  }
}


async function saveSettingsCard(group, btnId, label) {
  return saveSystemSettings({ groups: [group], btnId, label: label || "本卡片" });
}


function clearAdminPasswordFields() {
  // Always blank — admin password is never returned by the API and must not
  // remain from browser autofill of the earlier login form.
  ["set-cur-password", "set-new-password", "set-confirm-password"].forEach((id) => {
    const el = $(id);
    if (!el) return;
    try {
      el.value = "";
      el.defaultValue = "";
      // re-arm readonly trap so autofill cannot inject until user focuses
      if (id === "set-cur-password") {
        el.setAttribute("readonly", "readonly");
        el.setAttribute("autocomplete", "off");
      }
    } catch (_) {}
  });
}

async function changeAdminPassword() {
  const cur = ($("set-cur-password") && $("set-cur-password").value) || "";
  const nw = ($("set-new-password") && $("set-new-password").value) || "";
  const cf = ($("set-confirm-password") && $("set-confirm-password").value) || "";
  if (!cur) throw new Error("请输入当前密码（不会自动填入，需手动输入）");
  if (!nw || nw.length < 4) throw new Error("新密码至少 4 位");
  if (nw !== cf) throw new Error("两次输入的新密码不一致");
  if (cur === nw) throw new Error("新密码不能与当前密码相同");
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
    clearAdminPasswordFields();
    // Do not re-fill password-adjacent secrets from response.
    if (r && r.settings) fillSystemSettingsForm(r.settings);
    toast(r.message || "密码已更新");
  } finally {
    if (btn) btn.disabled = false;
    clearAdminPasswordFields();
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

try { bindModelsControls(); } catch (_) {}


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
        startRegPolling({ immediate: true, intervalMs: 180 });
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
// First paint + soft-nav: single entry that rebinds select + paints panels.
try { bindRegMailFormControls(); } catch (_) {
  if ($("reg-captcha-provider")) {
    on("reg-captcha-provider", "onchange", () => { syncRegCaptchaProviderUI(); });
    try { syncRegCaptchaProviderUI(); } catch (_) {}
  }
  if ($("reg-mail-provider")) {
    on("reg-mail-provider", "onchange", () => {
      syncRegMailProviderUI({ toast: true });
    });
    try { syncRegMailProviderUI(); } catch (_) {}
  }
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
    try { if (regPollTimer) clearTimeout(regPollTimer); } catch(_){}
    try { if (uiRefreshTimer) clearInterval(uiRefreshTimer); } catch(_){}
    try { if (typeof stopUpstreamMonitor === "function") stopUpstreamMonitor(); } catch(_){}
    try { if (typeof stopQuotaLiveRefresh === "function") stopQuotaLiveRefresh(); } catch(_){}
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
/** @type {Map<string, any>} detail payload by event id (avoid huge data-* attrs that break table layout) */
let usageEventsDetailById = new Map();

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
  ["usage-events-protocol", "usage-events-ok", "usage-events-stream", "usage-events-page-size"].forEach((id) => {
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
      const tr = e.target.closest("tr[data-usage-id]");
      if (!tr) return;
      try {
        const id = tr.getAttribute("data-usage-id") || "";
        const detail = (usageEventsDetailById && usageEventsDetailById.get(String(id)))
          || (() => {
            // fallback for stale rows
            try { return JSON.parse(tr.getAttribute("data-usage-detail") || "null"); } catch (_) { return null; }
          })();
        if (!detail) return;
        const panel = $("usage-events-detail");
        if (!panel) return;
        panel.hidden = false;
        panel.classList.remove("hidden", "is-empty");
        panel.textContent = JSON.stringify(detail, null, 2);
      } catch (_) {}
    });
  }
}


// Normalize usage_events.stream for display. Never leave 模式 empty — an empty
// cell under a fixed table makes the whole row look misaligned.
function usageEventStreamFlag(it) {
  if (!it || typeof it !== "object") return null;
  const v = it.stream;
  if (v === true || v === 1 || v === "1" || v === "true" || v === "t" || v === "T" || v === "yes") return true;
  if (v === false || v === 0 || v === "0" || v === "false" || v === "f" || v === "F" || v === "no") return false;
  // Infer from protocol/path when DB NULL / legacy rows.
  const proto = String(it.protocol || "").toLowerCase();
  const path = String(it.path || "").toLowerCase();
  if (proto === "openai_responses" || path.includes("/responses")) return true;
  if (proto === "anthropic" || path.includes("/messages")) {
    // Anthropic can be either; prefer detail.stream if present.
    const d = it.detail || {};
    if (d.stream === true || d.stream === false) return !!d.stream;
    if (d.stream === "true" || d.stream === "false") return d.stream === "true";
    // SSE route is the common case for Claude Code.
    return true;
  }
  if (path.includes("/chat/completions")) {
    const d = it.detail || {};
    if (d.stream === true || d.stream === false) return !!d.stream;
    return null;
  }
  return null;
}

function usageEventStreamPill(it) {
  const flag = usageEventStreamFlag(it);
  if (flag === true) {
    return '<span class="ue-mode-pill is-stream" title="流式 · stream=true">流</span>';
  }
  if (flag === false) {
    return '<span class="ue-mode-pill is-sync" title="非流式 · stream=false">非流</span>';
  }
  return '<span class="ue-mode-pill is-unknown" title="stream 未记录">—</span>';
}

async function loadUsageEvents({ reset = false, silent = false } = {}) {
  if (!$("usage-events-tbody")) return;
  bindUsageEventsControls();
  if (reset) usageEventsPage = 1;
  // Drop stacked requests: only the latest seq paints (prevents flash from double soft-nav).
  if (usageEventsLoading && silent) {
    // Still bump seq so an older in-flight paint is ignored when it returns.
  }
  usageEventsLoading = true;
  const seq = ++usageEventsLoadSeq;
  const q = ($("usage-events-q") && $("usage-events-q").value || "").trim();
  const protocol = ($("usage-events-protocol") && $("usage-events-protocol").value) || "all";
  const ok = ($("usage-events-ok") && $("usage-events-ok").value) || "all";
  const streamMode = ($("usage-events-stream") && $("usage-events-stream").value) || "all";
  usageEventsPageSize = parseInt(($("usage-events-page-size") && $("usage-events-page-size").value) || "50", 10) || 50;
  const tbody = $("usage-events-tbody");
  const hasRows = !!(tbody && tbody.querySelector("tr[data-usage-id]"));
  // Never flash "加载明细中…" over existing rows — that is the table flicker.
  if (!silent && !hasRows) {
    tbody.innerHTML = `<tr><td colspan="15" class="g2a-muted">加载明细中…</td></tr>`;
    if ($("usage-events-info")) $("usage-events-info").textContent = "查询中…";
  } else if (!silent && $("usage-events-info")) {
    $("usage-events-info").textContent = ( $("usage-events-info").textContent || "—" ).replace(/ · 更新中…$/, "") + " · 更新中…";
  }
  try {
    // Backend stores chat as openai_chat; keep UI label "openai".
    const protocolFilter = protocol === "openai" ? "openai_chat" : protocol;
    const params = new URLSearchParams({
      page: String(usageEventsPage),
      page_size: String(usageEventsPageSize),
      q,
      protocol: protocolFilter,
      ok,
      stream: streamMode,
    });
    const data = await api("/usage/events?" + params.toString());
    if (seq !== usageEventsLoadSeq) return;
    const items = (data && data.items) || [];
    usageEventsPage = Number(data.page || usageEventsPage) || 1;
    usageEventsTotalPages = Number(data.total_pages || 1) || 1;
    if ($("usage-events-info")) {
      const streamLabel = streamMode === "1" ? "流" : (streamMode === "0" ? "非流" : "");
      $("usage-events-info").textContent =
        `共 ${fmtNum(data.total || 0)} 条 · 源 ${(data.store_source || "none")}` +
        (q ? ` · 关键词 “${q}”` : "") +
        (streamLabel ? ` · 模式「${streamLabel}」` : "");
    }
    if ($("usage-events-page-info")) {
      $("usage-events-page-info").textContent =
        `第 ${usageEventsPage} / ${usageEventsTotalPages} 页`;
    }
    if (!items.length) {
      $("usage-events-tbody").innerHTML =
        `<tr><td colspan="15" class="g2a-muted">暂无请求明细（新请求完成后会出现在这里）</td></tr>`;
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
      const streamPill = usageEventStreamPill(it);
      const streamFlag = usageEventStreamFlag(it);
      const proto = String(it.protocol || "—");
      const pathStr = String(it.path || "");
      const cacheRead = Number(it.cache_read_tokens || 0);
      const cacheCreate = Number(it.cache_creation_tokens || 0);
      const promptTok = Number(it.prompt_tokens || 0);
      const totalTok = Number(it.total_tokens || 0);
      const billedTok = (it.billed_tokens != null && it.billed_tokens !== "")
        ? Number(it.billed_tokens) || 0
        : Math.max(0, totalTok - cacheRead);
      const cacheTokens = cacheRead + cacheCreate;
      const hitPct = promptTok > 0 && cacheRead > 0
        ? Math.min(100, Math.round((cacheRead / promptTok) * 1000) / 10)
        : null;
      const cacheParts = [];
      if (cacheRead > 0) cacheParts.push(`读 ${fmtNum(cacheRead)}`);
      if (cacheCreate > 0) cacheParts.push(`写 ${fmtNum(cacheCreate)}`);
      if (hitPct != null) cacheParts.push(`${hitPct}%`);
      const cacheSub = cacheParts.join(" · ");
      const cacheTitle = cacheSub || (cacheTokens > 0 ? String(cacheTokens) : "");
      const reasoningTokens = Number(it.reasoning_tokens || 0);
      const effortRaw = (
        it.reasoning_effort
        || (it.detail && (it.detail.reasoning_effort || it.detail.thinking_intensity || it.detail.thinking_effort))
        || ""
      );
      // Claude Code / Anthropic: low · medium · high · xhigh · max · ultracode
      const effort = String(effortRaw || "").trim().toLowerCase();
      let effortPill;
      if (!effort) {
        effortPill = '<span class="g2a-muted">—</span>';
      } else if (effort === "low") {
        effortPill = `<span class="g2a-tag" title="reasoning_effort: ${esc(effort)}">${esc(effort)}</span>`;
      } else if (effort === "medium") {
        effortPill = `<span class="g2a-tag warn" title="reasoning_effort: ${esc(effort)}">${esc(effort)}</span>`;
      } else if (effort === "high") {
        effortPill = `<span class="g2a-tag bad" title="reasoning_effort: ${esc(effort)}">${esc(effort)}</span>`;
      } else if (effort === "xhigh" || effort === "max" || effort === "ultracode") {
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
        stream: streamFlag == null ? it.stream : streamFlag,
        ok: it.ok,
        prompt_tokens: it.prompt_tokens,
        completion_tokens: it.completion_tokens,
        total_tokens: it.total_tokens,
        billed_tokens: billedTok,
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
      const uid = String(it.id != null ? it.id : `${it.created_at || ""}-${Math.random().toString(36).slice(2, 8)}`);
      usageEventsDetailById.set(uid, detail);
      const keyTitle = [keyLabel, it.api_key_id].filter(Boolean).join(" · ");
      const modelTitle = [it.model, it.account_email || it.account_id].filter(Boolean).join(" · ");
      const billTitle = cacheRead > 0
        ? `计费 ${fmtNum(billedTok)}（原 total ${fmtNum(totalTok)} − cache_read ${fmtNum(cacheRead)}）`
        : `计费 ${fmtNum(billedTok)}`;
      return `<tr data-usage-id="${esc(uid)}" style="cursor:pointer" title="点击查看完整明细">
        <td class="mono" title="${esc(fmtTime(it.created_at))}"><span class="ue-main">${esc(fmtTime(it.created_at))}</span></td>
        <td class="ue-proto" title="${esc(proto + (pathStr ? " " + pathStr : ""))}">
          <span class="ue-main">${esc(proto)}</span>
          ${pathStr ? `<span class="ue-sub mono">${esc(pathStr)}</span>` : ""}
        </td>
        <td class="ue-center ue-mode">${streamPill}</td>
        <td class="mono" title="${esc(keyTitle)}">
          <span class="ue-main">${esc(keyLabel)}</span>
          ${it.api_key_id ? `<span class="ue-sub">${esc(it.api_key_id)}</span>` : ""}
        </td>
        <td class="mono" title="${esc(it.client_ip || "")}"><span class="ue-main">${esc(it.client_ip || "—")}</span></td>
        <td class="mono" title="${esc(modelTitle)}">
          <span class="ue-main">${esc(it.model || "—")}</span>
          ${(it.account_email || it.account_id) ? `<span class="ue-sub">${esc(it.account_email || it.account_id)}</span>` : ""}
        </td>
        <td class="ue-num" title="${esc(String(it.prompt_tokens ?? 0))}">${fmtNum(it.prompt_tokens)}</td>
        <td class="ue-num" title="${esc(String(it.completion_tokens ?? 0))}">${fmtNum(it.completion_tokens)}</td>
        <td class="ue-num" title="${esc(billTitle)}">${fmtNum(billedTok)}${cacheRead > 0 ? `<span class="ue-sub">原 ${fmtNum(totalTok)}</span>` : ""}</td>
        <td class="ue-num" title="${esc(cacheTitle)}">${cacheTokens > 0 ? fmtNum(cacheTokens) : "—"}${cacheSub ? `<span class="ue-sub">${esc(cacheSub)}</span>` : ""}</td>
        <td class="ue-num">${reasoningTokens > 0 ? fmtNum(reasoningTokens) : "—"}</td>
        <td class="ue-center">${effortPill}</td>
        <td class="ue-num mono" title="首字延迟 TTFT">${esc(fmtLatency(ttftMs))}</td>
        <td class="ue-num mono" title="请求完成总耗时">${esc(fmtLatency(doneMs))}</td>
        <td class="ue-center">${okPill}</td>
      </tr>`;
    }).join("");
  } catch (e) {
    if (seq !== usageEventsLoadSeq) return;
    console.warn("loadUsageEvents", e);
    $("usage-events-tbody").innerHTML =
      `<tr><td colspan="15" class="g2a-muted">加载失败：${esc((e && e.message) || e)}</td></tr>`;
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
    return `<div class="g2a-usage-bar" title="${esc(r.day)} · ${fmtTokens(tok)}（已扣缓存） · ${fmtNum(req)} 请求">
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
    tb.innerHTML = `<tr><td colspan="4" class="g2a-muted">暂无数据</td></tr>`;
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
      <td class="mono" title="已扣缓存">${fmtTokens(usageBilled(it))}</td>
      <td>${esc(rate)}</td>
    </tr>`;
  }).join("");
}

async function loadUsageSoft() {
  // Lightweight poll for the usage page: refresh summary + events without
  // resetting pagination/filters or showing "加载中" flashes.
  if (usageLoading || usageEventsLoading) return;
  if (!$("usage-stats-grid") && !$("usage-events-tbody")) return;
  try {
    const daysEl = $("usage-days");
    if (daysEl) usageDays = Number(daysEl.value || usageDays) || 7;
    const [sum, byKey, byModel] = await Promise.all([
      api("/usage/summary?days=" + encodeURIComponent(usageDays)),
      api("/usage/by-key?days=" + encodeURIComponent(usageDays) + "&limit=30"),
      api("/usage/by-model?days=" + encodeURIComponent(usageDays) + "&limit=30"),
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
        <div class="stat"><div class="label">今日计费 token</div><div class="value mono">${fmtTokens(usageBilled(today))}</div>
          <div class="sub">输入 ${fmtNum(today.prompt_tokens_billed != null ? today.prompt_tokens_billed : Math.max(0, (Number(today.prompt_tokens)||0) - (Number(today.cache_read_tokens)||0)))} · 输出 ${fmtNum(today.completion_tokens)} · 已扣缓存 ${fmtNum(today.cache_read_tokens || 0)}</div></div>
        <div class="stat"><div class="label">今日缓存命中</div><div class="value mono">${fmtRatio(cacheToday.token_hit_ratio)}</div>
          <div class="sub">读 ${fmtNum(cacheToday.cache_read_tokens || 0)} / 输入 ${fmtNum(cacheToday.prompt_tokens || 0)} · 请求命中 ${fmtRatio(cacheToday.request_hit_ratio)}</div></div>
        <div class="stat"><div class="label">近 ${usageDays} 天计费 token</div><div class="value mono">${fmtTokens(usageBilled(window))}</div>
          <div class="sub">请求 ${fmtNum(window.requests)}${window.success_rate != null ? ` · 成功率 ${window.success_rate}%` : ""} · 已扣缓存 ${fmtNum(window.cache_read_tokens || 0)}</div></div>
        <div class="stat"><div class="label">近 ${usageDays} 天缓存命中</div><div class="value mono">${fmtRatio(cacheWin.token_hit_ratio)}</div>
          <div class="sub">读 ${fmtNum(cacheWin.cache_read_tokens || 0)} / 输入 ${fmtNum(cacheWin.prompt_tokens || 0)} · 请求命中 ${fmtRatio(cacheWin.request_hit_ratio)}</div></div>
        <div class="stat"><div class="label">累计计费 token</div><div class="value mono">${fmtTokens(usageBilled(life))}</div>
          <div class="sub">请求 ${fmtNum(life.requests)} · 累计缓存读 ${fmtNum(cacheLife.cache_read_tokens || 0)} · 单位 k/M/B · 源 ${esc((sum && sum.source) || "—")}</div></div>
      `;
    }
    if ($("usage-source")) {
      $("usage-source").textContent = "数据源: " + ((sum && sum.source) || "none") +
        " · 计费 token = total − cache_read · 命中率 = cache_read / prompt" +
        " · 上海时区（UTC+8）日切 · 失败请求不计 token" +
        (cache.source ? ` · cache源 ${cache.source}` : "");
    }
    renderUsageBars((sum && sum.series) || []);
    renderUsageTable("usage-by-key-tbody", (byKey && byKey.items) || [], "key");
    renderUsageTable("usage-by-model-tbody", (byModel && byModel.items) || [], "model");
    // Silent events refresh only — never blank the detail table.
    try { await loadUsageEvents({ reset: false, silent: true }); } catch (_) {}
  } catch (e) {
    console.warn("loadUsageSoft", e);
  }
}

async function loadUsage() {
  if (usageLoading) return;
  window.__g2aUsageLoadAt = Date.now();
  usageLoading = true;
  try {
    const daysEl = $("usage-days");
    if (daysEl) usageDays = Number(daysEl.value || usageDays) || 7;
    const [sum, byKey, byModel] = await Promise.all([
      api("/usage/summary?days=" + encodeURIComponent(usageDays)),
      api("/usage/by-key?days=" + encodeURIComponent(usageDays) + "&limit=30"),
      api("/usage/by-model?days=" + encodeURIComponent(usageDays) + "&limit=30"),
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
        <div class="stat"><div class="label">今日计费 token</div><div class="value mono">${fmtTokens(usageBilled(today))}</div>
          <div class="sub">输入 ${fmtNum(today.prompt_tokens_billed != null ? today.prompt_tokens_billed : Math.max(0, (Number(today.prompt_tokens)||0) - (Number(today.cache_read_tokens)||0)))} · 输出 ${fmtNum(today.completion_tokens)} · 已扣缓存 ${fmtNum(today.cache_read_tokens || 0)}</div></div>
        <div class="stat"><div class="label">今日缓存命中</div><div class="value mono">${fmtRatio(cacheToday.token_hit_ratio)}</div>
          <div class="sub">读 ${fmtNum(cacheToday.cache_read_tokens || 0)} / 输入 ${fmtNum(cacheToday.prompt_tokens || 0)} · 请求命中 ${fmtRatio(cacheToday.request_hit_ratio)}</div></div>
        <div class="stat"><div class="label">近 ${usageDays} 天计费 token</div><div class="value mono">${fmtTokens(usageBilled(window))}</div>
          <div class="sub">请求 ${fmtNum(window.requests)}${window.success_rate != null ? ` · 成功率 ${window.success_rate}%` : ""} · 已扣缓存 ${fmtNum(window.cache_read_tokens || 0)}</div></div>
        <div class="stat"><div class="label">近 ${usageDays} 天缓存命中</div><div class="value mono">${fmtRatio(cacheWin.token_hit_ratio)}</div>
          <div class="sub">读 ${fmtNum(cacheWin.cache_read_tokens || 0)} / 输入 ${fmtNum(cacheWin.prompt_tokens || 0)} · 请求命中 ${fmtRatio(cacheWin.request_hit_ratio)}</div></div>
        <div class="stat"><div class="label">累计计费 token</div><div class="value mono">${fmtTokens(usageBilled(life))}</div>
          <div class="sub">请求 ${fmtNum(life.requests)} · 累计缓存读 ${fmtNum(cacheLife.cache_read_tokens || 0)} · 单位 k/M/B · 源 ${esc((sum && sum.source) || "—")}</div></div>
      `;
    }
    if ($("usage-source")) {
      $("usage-source").textContent = "数据源: " + ((sum && sum.source) || "none") +
        " · 计费 token = total − cache_read · 命中率 = cache_read / prompt" +
        " · 上海时区（UTC+8）日切 · 失败请求不计 token" +
        (cache.source ? ` · cache源 ${cache.source}` : "");
    }
    renderUsageBars((sum && sum.series) || []);
    renderUsageTable("usage-by-key-tbody", (byKey && byKey.items) || [], "key");
    renderUsageTable("usage-by-model-tbody", (byModel && byModel.items) || [], "model");
    // Events: keep page/filters; silent if rows already painted (no "加载中" flash).
    try {
      const hasEv = !!($("usage-events-tbody") && $("usage-events-tbody").querySelector("tr[data-usage-id]"));
      await loadUsageEvents({ reset: !hasEv, silent: hasEv });
    } catch (_) {}
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
// Auto-poll while the logs page is open so progress upserts surface without manual refresh.
let logsAutoTimer = null;
const LOGS_AUTO_MS = 5000;
// Soft poll signature: when unchanged, skip tbody rewrite (stops 4–5s flicker).
let logsLastSig = "";
let logsLastDetailText = "";

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
  // Probe/renew: done=success, total=attempted (matches backend task_logs semantics).
  if (total > 0) return `${done}/${total}`;
  if (done > 0) return String(done);
  // Fall back to detail fields for older rows written before progress fix.
  const d = it.detail || {};
  const avail = Number(d.available_count != null ? d.available_count : d.available || 0) || 0;
  const probed = Number(d.probed != null ? d.probed : d.count || 0) || 0;
  if (probed > 0) return `${avail}/${probed}`;
  const ref = Number((d.refresh && d.refresh.refreshed) || 0) || 0;
  const att = Number((d.refresh && d.refresh.attempted) || 0) || 0;
  if (att > 0) return `${ref}/${att}`;
  return "—";
}

function stopLogsAutoRefresh() {
  if (logsAutoTimer != null) {
    try { clearInterval(logsAutoTimer); } catch (_) {}
    logsAutoTimer = null;
  }
}

function startLogsAutoRefresh() {
  // Idempotent: do not clear+reset the interval on every soft load (timer restart flicker).
  if (logsAutoTimer != null) return;
  if (!$("logs-tbody")) return;
  logsAutoTimer = setInterval(() => {
    try {
      if (document.hidden) return;
      if ((document.body && document.body.dataset.page) !== "logs") {
        stopLogsAutoRefresh();
        return;
      }
      if (logsLoading) return;
      // Soft: never blank the table; skip DOM rewrite when signature unchanged.
      loadAdminLogs({ reset: false, soft: true }).catch(() => {});
    } catch (_) {}
  }, LOGS_AUTO_MS);
}

function bindLogsControls() {
  on("btn-logs-search", "onclick", () => loadAdminLogs({ reset: true, soft: false }));
  on("btn-logs-reload", "onclick", () => loadAdminLogs({ reset: false, soft: false }));
  on("logs-page-prev", "onclick", () => {
    if (logsPage > 1 && !logsLoading) { logsPage -= 1; loadAdminLogs({ soft: false }); }
  });
  on("logs-page-next", "onclick", () => {
    if (!logsLoading && logsPage < logsTotalPages) { logsPage += 1; loadAdminLogs({ soft: false }); }
  });
  const q = $("logs-q");
  if (q && !q._logsBound) {
    q._logsBound = true;
    q.addEventListener("keydown", (e) => {
      if (e.key === "Enter") loadAdminLogs({ reset: true, soft: false });
    });
  }
  const act = $("logs-action");
  if (act && !act._logsBound) {
    act._logsBound = true;
    act.addEventListener("change", () => loadAdminLogs({ reset: true, soft: false }));
  }
  const st = $("logs-status");
  if (st && !st._logsBound) {
    st._logsBound = true;
    st.addEventListener("change", () => loadAdminLogs({ reset: true, soft: false }));
  }
  const ps = $("logs-page-size");
  if (ps && !ps._logsBound) {
    ps._logsBound = true;
    ps.addEventListener("change", () => loadAdminLogs({ reset: true, soft: false }));
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
        // Force detail paint on user click even if payload text matches last soft paint.
        logsLastDetailText = "";
        logsSelectedId = id || null;
        if (logsSelectedId) {
          try { sessionStorage.setItem("g2a_logs_selected_id", String(logsSelectedId)); } catch (_) {}
        }
        setLogPanel("logs-detail", JSON.stringify(detail || {}, null, 2), { forceShow: true });
        try { logsLastDetailText = JSON.stringify(detail || {}, null, 2); } catch (_) {}
      } catch (_) {}
    });
  }
  startLogsAutoRefresh();
}

async function ensureLogActions() {
  const sel = $("logs-action");
  if (!sel) return;
  try {
    const r = await api("/logs/actions");
    const actions = (r && (r.kinds || r.actions)) || [];
    const have = new Set(Array.from(sel.options).map((o) => o.value));
    actions.forEach((a) => {
      if (!a || have.has(a)) return;
      const opt = document.createElement("option");
      opt.value = a;
      opt.textContent = a;
      sel.appendChild(opt);
      have.add(a);
    });
  } catch (_) {}
}

function logRowActivityTs(it) {
  // Prefer updated_at / finished_at so progress upserts show "just now" instead of
  // the original created_at (that was the main "日志不及时" perception).
  const cand = [it && it.updated_at, it && it.finished_at, it && it.created_at];
  let best = 0;
  for (const v of cand) {
    const n = Number(v);
    if (Number.isFinite(n) && n > best) best = n;
  }
  return best > 0 ? best : (it && it.created_at);
}

// Compact fingerprint of the visible page — skip DOM rewrite when soft poll is a no-op.
function logsPageSignature(items, page, totalPages, total) {
  const parts = [String(page || 1), String(totalPages || 1), String(total ?? ""), String(logsSelectedId || "")];
  for (const it of items || []) {
    parts.push([
      it.id != null ? it.id : (it.task_id || ""),
      it.status || "",
      it.summary || "",
      it.progress_done ?? "",
      it.progress_total ?? "",
      it.ok === false ? 0 : (it.ok === true ? 1 : 2),
      // minute-granularity time only (fmtTime is YYYY-MM-DD HH:mm) — second-level
      // updated_at churn must not thrash the table every soft poll.
      Math.floor(Number(logRowActivityTs(it) || 0) / 60),
      it.kind || it.action || "",
    ].join(":"));
  }
  return parts.join("|");
}

function paintLogsTable(items, { soft = false } = {}) {
  const tbody = $("logs-tbody");
  if (!tbody) return;
  const nextCache = Object.create(null);
  if (!items.length) {
    logsDetailCache = nextCache;
    // Soft empty: only wipe if we currently show data (filter became empty).
    if (!soft || tbody.querySelector("tr[data-log-id]")) {
      tbody.innerHTML = `<tr><td colspan="6" class="g2a-muted">暂无任务日志</td></tr>`;
    }
    return;
  }

  const rowIds = items.map((it, idx) => String(it.id != null ? it.id : (it.task_id || `row-${idx}`)));
  const existing = Array.from(tbody.querySelectorAll("tr[data-log-id]"));
  const existingIds = existing.map((tr) => tr.getAttribute("data-log-id") || "");
  // In-place cell updates when soft poll keeps the same row set/order — avoids full
  // tbody replace flash (scroll jump + white blink every 5s).
  const canPatch = soft
    && existing.length === rowIds.length
    && existing.length > 0
    && existingIds.every((id, i) => id === rowIds[i]);

  const rowHTML = (it, idx, rowId) => {
    const kind = it.kind || it.action || "—";
    const st = it.status || "—";
    const selected = logsSelectedId && String(logsSelectedId) === rowId;
    const when = logRowActivityTs(it);
    const tip = (it.updated_at && it.created_at && Number(it.updated_at) !== Number(it.created_at))
      ? (`创建 ${fmtTime(it.created_at)} · 更新 ${fmtTime(it.updated_at)}`)
      : "";
    return {
      title: tip,
      when: fmtTime(when),
      kind,
      st,
      summary: it.summary || "—",
      progress: taskProgressText(it),
      statusHtml: taskStatusTag(st, it.ok),
      selected,
    };
  };

  items.forEach((it, idx) => {
    const rowId = rowIds[idx];
    nextCache[rowId] = {
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
  });
  logsDetailCache = nextCache;

  if (canPatch) {
    items.forEach((it, idx) => {
      const tr = existing[idx];
      const rowId = rowIds[idx];
      const cells = rowHTML(it, idx, rowId);
      const tds = tr.children;
      if (tds.length < 6) return;
      // Only touch text when value changed — keeps selection outline / layout stable.
      if (tds[0].getAttribute("title") !== cells.title) tds[0].setAttribute("title", cells.title);
      if (tds[0].textContent !== cells.when) tds[0].textContent = cells.when;
      if (tds[1].textContent !== cells.kind) tds[1].textContent = cells.kind;
      if (tds[2].textContent !== cells.st) tds[2].textContent = cells.st;
      if (tds[3].textContent !== cells.summary) tds[3].textContent = cells.summary;
      if (tds[4].textContent !== cells.progress) tds[4].textContent = cells.progress;
      if (tds[5].innerHTML !== cells.statusHtml) tds[5].innerHTML = cells.statusHtml;
      const wantOutline = cells.selected ? "1px solid var(--g2a-primary, #4f8cff)" : "";
      const curOutline = tr.style.outline || "";
      if (cells.selected) {
        if (!curOutline.includes("solid")) tr.style.outline = wantOutline;
        tr.style.cursor = "pointer";
      } else if (curOutline) {
        tr.style.outline = "";
      }
    });
    return;
  }

  tbody.innerHTML = items.map((it, idx) => {
    const rowId = rowIds[idx];
    const cells = rowHTML(it, idx, rowId);
    return `<tr data-log-id="${esc(rowId)}" style="cursor:pointer${cells.selected ? ";outline:1px solid var(--g2a-primary, #4f8cff)" : ""}">
      <td class="g2a-muted" title="${esc(cells.title)}">${esc(cells.when)}</td>
      <td class="mono">${esc(cells.kind)}</td>
      <td class="mono g2a-muted">${esc(cells.st)}</td>
      <td>${esc(cells.summary)}</td>
      <td class="mono g2a-muted">${esc(cells.progress)}</td>
      <td>${cells.statusHtml}</td>
    </tr>`;
  }).join("");
}

function refreshSelectedLogDetail() {
  if (!logsSelectedId || !logsDetailCache[logsSelectedId]) return;
  let next = "";
  try {
    next = JSON.stringify(logsDetailCache[logsSelectedId], null, 2);
  } catch (_) {
    return;
  }
  // Skip identical detail — setLogPanel rewrite still costs layout when forceShow.
  if (next === logsLastDetailText) {
    const el = $("logs-detail");
    if (el && !el.classList.contains("hidden")) return;
  }
  logsLastDetailText = next;
  setLogPanel("logs-detail", next, { forceShow: true });
}

async function loadAdminLogs({ reset = false, soft = false } = {}) {
  if (!$("logs-tbody")) return;
  // Avoid re-binding / restarting the auto timer on every soft tick.
  if (!soft) bindLogsControls();
  else if (!$("logs-tbody")._logsBound) bindLogsControls();
  if (!soft) await ensureLogActions();
  if (reset) {
    logsPage = 1;
    logsLastSig = "";
  }
  // Restore last selected row after hard refresh / soft-nav.
  if (!logsSelectedId) {
    try { logsSelectedId = sessionStorage.getItem("g2a_logs_selected_id") || null; } catch (_) {}
  }
  if (logsLoading && soft) return;
  logsLoading = true;
  const seq = ++logsLoadSeq;
  const q = ($("logs-q") && $("logs-q").value || "").trim();
  const action = ($("logs-action") && $("logs-action").value) || "all";
  const status = ($("logs-status") && $("logs-status").value) || "all";
  logsPageSize = parseInt(($("logs-page-size") && $("logs-page-size").value) || "50", 10) || 50;
  // Soft/auto refresh must NOT blank the table — full "加载中…" wipe was the main flash.
  // Hard load only blanks when the table has no data rows yet (first open).
  if (!soft) {
    const tbody = $("logs-tbody");
    const hasData = !!(tbody && tbody.querySelector("tr[data-log-id]"));
    if (!hasData) {
      tbody.innerHTML = `<tr><td colspan="6" class="g2a-muted">加载任务日志中…</td></tr>`;
      if ($("logs-info")) $("logs-info").textContent = "查询中…";
    }
    // Keep last signature when re-hard-loading with existing rows (header refresh).
    // Only force full repaint after reset/filter change (logsLastSig cleared above).
  }
  try {
    const kindQs = (action && action !== "all") ? `&kind=${encodeURIComponent(action)}&action=${encodeURIComponent(action)}` : "";
    const statusQs = (status && status !== "all") ? `&status=${encodeURIComponent(status)}` : "";
    // Cache-buster so intermediate proxies never serve a stale task list.
    const bust = soft ? `&_=${Date.now()}` : "";
    const data = await api(
      `/logs?page=${encodeURIComponent(logsPage)}&page_size=${encodeURIComponent(logsPageSize)}&q=${encodeURIComponent(q)}${kindQs}${statusQs}${bust}`
    );
    if (seq !== logsLoadSeq) return;
    const items = (data && data.items) || [];
    logsTotalPages = Number(data.total_pages || 1) || 1;
    logsPage = Number(data.page || logsPage) || 1;
    const total = data.total ?? items.length;
    const infoTxt = `共 ${total} 条任务 · 数据源 ${data.store_source || "postgres"} · 自动刷新 ${Math.round(LOGS_AUTO_MS / 1000)}s · 点击行查看详情`;
    if ($("logs-info") && $("logs-info").textContent !== infoTxt) {
      $("logs-info").textContent = infoTxt;
    }
    const pageTxt = `${logsPage} / ${logsTotalPages}`;
    if ($("logs-page-info") && $("logs-page-info").textContent !== pageTxt) {
      $("logs-page-info").textContent = pageTxt;
    }
    if ($("logs-page-prev")) $("logs-page-prev").disabled = logsPage <= 1;
    if ($("logs-page-next")) $("logs-page-next").disabled = logsPage >= logsTotalPages;

    const sig = logsPageSignature(items, logsPage, logsTotalPages, total);
    if (soft && sig === logsLastSig) {
      // No visible change — leave DOM alone (kills soft-poll flicker).
      // Still refresh detail cache payload quietly for click handlers.
      logsDetailCache = Object.create(null);
      items.forEach((it, idx) => {
        const rowId = String(it.id != null ? it.id : (it.task_id || `row-${idx}`));
        logsDetailCache[rowId] = {
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
      });
      refreshSelectedLogDetail();
    } else {
      logsLastSig = sig;
      paintLogsTable(items, { soft });
      refreshSelectedLogDetail();
    }
    startLogsAutoRefresh();
  } catch (e) {
    if (seq !== logsLoadSeq) return;
    if (!soft) {
      $("logs-tbody").innerHTML = `<tr><td colspan="6" class="g2a-muted">加载失败：${esc(e.message || e)}</td></tr>`;
      toast(e.message || "加载任务日志失败", false);
      logsLastSig = "";
    }
  } finally {
    if (seq === logsLoadSeq) logsLoading = false;
  }
}


window.G2AAdmin = { bootstrap, loadDashboard, api, $, toast, PAGE_META, renderAccounts, renderKeys, checkVersionUpdate, bindVersionSettingsUI };
  if (document.body && document.body.dataset.page) {
    const _boot = () => {
      try { if (document.body.dataset.page === "accounts") renderAccountStatusChips(); } catch (_) {}
      bootstrap();
    };
    if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", _boot);
    else _boot();
  }

/* ── Version check + hot update ─────────────────────────────────── */
let _versionBannerCheckedAt = 0;
let _versionInfoCache = null;

function scheduleVersionBannerCheck() {
  const now = Date.now();
  if (now - _versionBannerCheckedAt < 10 * 60 * 1000 && _versionInfoCache) {
    try { renderVersionBanner(_versionInfoCache); } catch (_) {}
    return;
  }
  _versionBannerCheckedAt = now;
  // Soft check — never block page paint.
  setTimeout(() => {
    checkVersionUpdate({ soft: true }).catch(() => {});
  }, 1200);
}

async function checkVersionUpdate({ soft = false, force = false } = {}) {
  const path = force ? "/version/check" : "/version";
  let info;
  try {
    info = await api(path + (force ? "" : ""));
  } catch (e) {
    if (!soft) toast((e && e.message) || "检查版本失败", false);
    return null;
  }
  _versionInfoCache = info;
  try { renderVersionBanner(info); } catch (_) {}
  try { renderVersionSettingsCard(info); } catch (_) {}
  if (!soft && info) {
    if (info.update_available) {
      toast(`发现新版本 v${info.latest}（当前 v${info.current}）`, true);
    } else if (info.latest) {
      toast(`已是最新版本 v${info.current}`, true);
    } else if (info.error) {
      toast("检查更新失败: " + info.error, false);
    }
  }
  return info;
}

function renderVersionBanner(info) {
  if (!info || !info.update_available) {
    const old = document.getElementById("g2a-version-banner");
    if (old) old.remove();
    return;
  }
  let el = document.getElementById("g2a-version-banner");
  if (!el) {
    el = document.createElement("div");
    el.id = "g2a-version-banner";
    el.setAttribute("role", "status");
    el.style.cssText = [
      "position:sticky", "top:0", "z-index:40",
      "padding:10px 16px", "display:flex", "gap:12px", "align-items:center",
      "justify-content:space-between", "flex-wrap:wrap",
      "background:linear-gradient(90deg,rgba(59,130,246,.18),rgba(16,185,129,.12))",
      "border-bottom:1px solid rgba(59,130,246,.35)",
      "font-size:13px",
    ].join(";");
    const host = document.querySelector(".g2a-main") || document.body;
    host.insertBefore(el, host.firstChild);
  }
  const cur = info.current || "?";
  const lat = info.latest || "?";
  el.innerHTML = `
    <div>
      <strong>发现新版本</strong>
      <span class="g2a-muted">当前 <code>v${esc(cur)}</code> → 最新 <code>v${esc(lat)}</code></span>
      ${info.release_name ? `<span class="g2a-muted"> · ${esc(info.release_name)}</span>` : ""}
    </div>
    <div style="display:flex;gap:8px;align-items:center">
      ${info.release_url ? `<a class="g2a-btn g2a-btn-default g2a-btn-sm" href="${esc(info.release_url)}" target="_blank" rel="noopener">Release</a>` : ""}
      <a class="g2a-btn g2a-btn-default g2a-btn-sm" href="/admin/settings#sec-version">去更新</a>
      <button type="button" class="g2a-btn g2a-btn-primary g2a-btn-sm" id="g2a-banner-update">热更新并重启</button>
      <button type="button" class="g2a-btn g2a-btn-default g2a-btn-sm" id="g2a-banner-dismiss" aria-label="关闭">×</button>
    </div>`;
  const btnU = el.querySelector("#g2a-banner-update");
  if (btnU) btnU.onclick = () => requestVersionUpdate({ tag: lat }).catch(() => {});
  const btnD = el.querySelector("#g2a-banner-dismiss");
  if (btnD) btnD.onclick = () => { try { el.remove(); } catch (_) {} };
}

function renderVersionSettingsCard(info) {
  if (!$("sec-version")) return;
  if (!info) return;
  if ($("ver-current")) $("ver-current").textContent = info.current ? ("v" + info.current) : "—";
  if ($("ver-latest")) $("ver-latest").textContent = info.latest ? ("v" + info.latest) : (info.error ? "检查失败" : "—");
  if ($("ver-release-name")) $("ver-release-name").textContent = info.release_name || "";
  if ($("ver-notes")) $("ver-notes").textContent = info.release_notes || info.error || "—";
  if ($("ver-update-hint")) $("ver-update-hint").textContent = info.update_hint || "";
  const pill = $("version-pill");
  if (pill) {
    if (info.update_available) {
      pill.className = "g2a-tag warn";
      pill.textContent = "有新版本 v" + info.latest;
    } else if (info.latest) {
      pill.className = "g2a-tag ok";
      pill.textContent = "已是最新";
    } else {
      pill.className = "g2a-tag";
      pill.textContent = info.error ? "检查失败" : "—";
    }
  }
  const btn = $("btn-version-update");
  if (btn) {
    const canUpdate = !!(info.update_supported && info.latest);
    btn.disabled = !canUpdate;
    btn.title = info.update_supported
      ? (info.update_hint || "")
      : (info.update_hint || "热更新未启用");
    btn.textContent = info.update_available
      ? ("热更新并重启到 v" + info.latest)
      : (info.update_supported ? "热更新并重启（当前已是最新也可 recreate）" : "热更新未启用");
  }
  const link = $("ver-release-link");
  if (link) {
    if (info.release_url) {
      link.href = info.release_url;
      link.style.display = "";
      link.textContent = "查看 Release";
    } else {
      link.style.display = "none";
    }
  }
  const st = info.update_status;
  if ($("ver-update-status")) {
    if (st && st.status) {
      $("ver-update-status").textContent =
        `状态: ${st.status}` +
        (st.tag ? ` · 目标 v${st.tag}` : "") +
        (st.message ? ` · ${st.message}` : "");
    } else {
      $("ver-update-status").textContent =
        info.update_supported
          ? ("模式: " + (info.update_mode || "docker"))
          : "热更新未启用";
    }
  }
}

async function requestVersionUpdate({ tag, restartOnly = false } = {}) {
  const tip = restartOnly
    ? "确认在容器内重启服务？（force-recreate，不强制换版本）服务会短暂中断。"
    : "确认在容器内热更新并自动重启？\n\n容器将：docker pull 镜像 + compose force-recreate\n无需宿主机 watcher。";
  if (!confirm(tip)) return;
  try {
    const body = {};
    if (tag) body.tag = String(tag).replace(/^v/, "");
    if (restartOnly) body.tag = body.tag || (window.__g2aCurrentVersion || "latest");
    const r = await api("/version/update", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    toast((r && r.message) || "已提交容器内热更新/重启（将自动 recreate）", true);
    // Poll status longer — pull can take minutes.
    for (let i = 0; i < 40; i++) {
      await new Promise((res) => setTimeout(res, 3000));
      let info = null;
      try { info = await checkVersionUpdate({ soft: true, force: false }); } catch (_) {}
      const st = info && info.update_status;
      if (st && (st.status === "done" || st.status === "error")) {
        if (st.status === "done") {
          toast("更新/重启完成，即将刷新页面…", true);
          setTimeout(() => location.reload(), 2000);
        } else {
          toast("更新失败: " + (st.message || "unknown"), false);
        }
        break;
      }
      if (i === 5 || i === 15) {
        toast("仍在更新中（pull/recreate）…", true);
      }
    }
  } catch (e) {
    toast((e && e.message) || "热更新请求失败", false);
  }
}

function bindVersionSettingsUI() {
  const chk = $("btn-version-check");
  if (chk && !chk._bound) {
    chk._bound = true;
    chk.onclick = () => checkVersionUpdate({ force: true }).catch(() => {});
  }
  const upd = $("btn-version-update");
  if (upd && !upd._bound) {
    upd._bound = true;
    upd.onclick = () => {
      const tag = (_versionInfoCache && _versionInfoCache.latest) || "";
      requestVersionUpdate({ tag }).catch(() => {});
    };
  }
  const rst = $("btn-version-restart");
  if (rst && !rst._bound) {
    rst._bound = true;
    rst.onclick = () => requestVersionUpdate({ restartOnly: true }).catch(() => {});
  }
  // Load once when settings page is present
  if ($("sec-version")) {
    checkVersionUpdate({ soft: true }).catch(() => {});
  }
}

// Auto-bind when settings page content is live
try {
  document.addEventListener("DOMContentLoaded", () => {
    try { bindVersionSettingsUI(); } catch (_) {}
  });
} catch (_) {}

})();
/* g2a-cache-bust-20260715-reg-restore-fix */
