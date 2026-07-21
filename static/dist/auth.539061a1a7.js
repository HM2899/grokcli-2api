/* auth / login / session gate */
window.G2A = window.G2A || {};
(function (G2A) {
  "use strict";
  function ensureApiLoaded() {
    return !!(G2A && typeof G2A.api === "function" && typeof G2A.onUnauthorized === "function");
  }
  function $id(id) {
    if (typeof G2A.$ === "function") return G2A.$(id);
    try { return document.getElementById(String(id)); } catch (_) { return null; }
  }
  const $ = $id;

  function adminBasePath() {
    const path = String(location.pathname || "");
    const i = path.indexOf("/admin");
    return (i >= 0 ? path.slice(0, i) : "") + "/admin";
  }
  function safeNext(raw) {
    const base = adminBasePath();
    try {
      if (!raw) return base;
      const u = new URL(raw, location.origin);
      if (u.origin !== location.origin) return base;
      if (!u.pathname.startsWith(base)) return base;
      return u.pathname + u.search + u.hash;
    } catch { return base; }
  }

  function redirectLogin(next) {
    const base = adminBasePath();
    const n = encodeURIComponent(next || (location.pathname + location.search));
    location.replace(base + "/login?next=" + n);
  }

  let _authRedirecting = false;
  if (typeof G2A.onUnauthorized !== "function") {
    console.error("G2A.onUnauthorized missing — api.js failed to load (often ERR_CONTENT_LENGTH_MISMATCH). Hard-refresh the page.");
  } else G2A.onUnauthorized((err) => {
    // Ignore background registration session probes / optional polls if token already empty
    const p = (err && err.path) || "";
    if (p.includes("/register-email/sessions/")) return;
    if (p === "/session" && location.pathname.startsWith("/admin/login")) {
      // login page validating token — just clear, no redirect
      try { G2A.clearToken(); } catch (_) {}
      return;
    }
    if (err && err.soft) return; // short-lived race after login
    // On login page never redirect-to-self
    if (location.pathname.startsWith("/admin/login")) {
      try { G2A.clearToken(); } catch (_) {}
      return;
    }
    // Grace only suppresses redirect for brief post-login races, still clear after grace.
    if (typeof G2A.inAuthGrace === "function" && G2A.inAuthGrace()) return;
    if (_authRedirecting) return;
    _authRedirecting = true;
    G2A.clearToken();
    redirectLogin();
  });

  async function requireSession() {
    if (location.protocol === "file:") {
      throw new Error("file://");
    }
    // Soft gate: local token OR valid cookie session. No per-page password prompt.
    let st = null;
    try {
      st = await G2A.refreshStatus();
      if (G2A.state) G2A.state.status = st;
    } catch (e) {
      st = (G2A.state && G2A.state.status) || null;
    }
    if (st && st.setup_needed) {
      location.replace(adminBasePath() + "/login");
      return null;
    }
    if (!G2A.getToken()) {
      // Cookie may still be valid (HttpOnly). Probe once.
      try {
        await G2A.api("/session");
        // cookie auth works; synthesize a local marker so UI stays authed
        if (typeof G2A.markAuthOk === "function") G2A.markAuthOk();
        // keep empty header token — cookie alone is enough for subsequent calls
      } catch (_) {
        redirectLogin();
        return null;
      }
    }
    return st;
  }

  
  function renderConnList(hostId, st, health) {
    const host = $(hostId);
    if (!host) return;
    const store = (st && st.store) || (health && health.store) || {};
    const redis = store.redis || {};
    const pg = store.postgres || {};
    const acc = (st && st.accounts) || {};
    const pool = (st && st.pool) || {};
    const rows = [
      {
        name: "管理鉴权",
        ok: st ? !st.setup_needed : false,
        warn: !!(st && st.setup_needed),
        desc: st ? (st.setup_needed ? "需要初始化管理员密码" : "密码已配置，登录后颁发 Token") : "检测中",
      },
      {
        name: "PostgreSQL",
        ok: !!(pg.ok || pg.enabled),
        desc: pg.ok ? ("已连接 · backend=" + (store.backend || "hybrid")) : (pg.error || (pg.configured ? "已配置但不可用" : "未配置")),
      },
      {
        name: "Redis",
        ok: !!(redis.ok || redis.enabled),
        desc: redis.ok ? ("已连接 · workers=" + (store.workers || "?")) : (redis.error || (redis.configured ? "已配置但不可用" : "未配置")),
      },
      {
        name: "账号数据",
        ok: Number(acc.account_count || pool.total || health?.accounts_total || 0) > 0,
        desc: "共 " + (acc.account_count || pool.total || health?.accounts_total || 0) +
              " 个 · 可用 " + (acc.active_count || pool.live || health?.accounts_live || 0),
      },
    ];
    host.innerHTML = rows.map((r) => {
      const cls = r.ok ? "ok" : (r.warn ? "warn" : "bad");
      const tag = r.ok ? "正常" : (r.warn ? "待设置" : "异常");
      return `<div class="g2a-conn-item">
        <div class="left"><span class="g2a-dot ${cls}"></span><div>
          <div class="name">${G2A.esc(r.name)}</div>
          <div class="desc">${G2A.esc(r.desc)}</div>
        </div></div>
        <span class="g2a-tag ${cls}">${tag}</span>
      </div>`;
    }).join("");
  }

  async function fetchHealth() {
    try {
      const res = await fetch("/health");
      return await res.json();
    } catch (_) {
      return null;
    }
  }

  async function initLoginPage() {
    const $ = $id;
    try { if (G2A.bindThemeToggle) G2A.bindThemeToggle(document); } catch (_) {}
    if (!ensureApiLoaded() || typeof G2A.api !== "function") {
      console.error("G2A.api missing — static/js failed to load");
      const hint = document.getElementById("boot-hint") || document.getElementById("boot-desc");
      if (hint) hint.textContent = "静态脚本加载失败，请强制刷新 (Ctrl+Shift+R)";
      return;
    }
    if (location.protocol === "file:") {
      $("boot-view")?.classList.remove("hidden");
      $("auth-view")?.classList.add("hidden");
      if ($("boot-desc")) $("boot-desc").textContent = "请通过服务打开管理台";
      if ($("boot-hint")) $("boot-hint").innerHTML = '不要直接双击 HTML。请运行 <span class="mono">./start.sh</span>，然后打开 <span class="mono">/admin</span>。';
      G2A.toast("检测到 file:// 打开，无法连接 API", false);
      return;
    }
    $("boot-view")?.classList.remove("hidden");
    $("auth-view")?.classList.add("hidden");
    let st = null;
    let health = null;
    try {
      // public status + health for store connectivity
      const pair = await Promise.all([
        G2A.api("/status"),
        fetchHealth(),
      ]);
      st = pair[0];
      health = pair[1];
      if (G2A.state) G2A.state.status = st;
    } catch (e) {
      if ($("boot-desc")) $("boot-desc").textContent = "无法连接 grokcli-2api 服务";
      if ($("boot-hint")) $("boot-hint").innerHTML = `请先启动服务，再打开管理台。<br><br>错误：${G2A.esc(e.message)}`;
      renderConnList("boot-conn", null, null);
      G2A.toast("无法连接服务: " + e.message, false);
      return;
    }
    try { renderConnList("boot-conn", st, health); } catch (_) {}
    try { renderConnList("auth-conn", st, health); } catch (_) {}

    // Always reveal login form once backend is reachable.
    $("boot-view")?.classList.add("hidden");
    $("auth-view")?.classList.remove("hidden");
    try { $("auth-view")?.classList.remove("hidden"); $("auth-view").hidden = false; } catch (_) {}
    const setup = !!st.setup_needed;
    if ($("auth-title")) $("auth-title").textContent = setup ? "初始化管理密码" : "登录管理台";
    if ($("auth-desc")) $("auth-desc").textContent = setup
      ? "首次使用，请设置管理员密码（至少 4 位）。登录后将连接 PostgreSQL / Redis 账号数据。"
      : "使用管理员密码进入。登录成功后通过 Token 鉴权读取数据库中的账号池。";
    if ($("auth-submit")) $("auth-submit").textContent = setup ? "创建并进入" : "登录并连接数据";
    const next = safeNext(new URLSearchParams(location.search).get("next"));

    // Already has a local token: validate session before leaving login page.
    // Blind redirect caused "stuck on login?next=/" loops when the token was
    // stale (admin page bounced back here, login bounced forward again).
    if (!setup && G2A.getToken()) {
      try {
        await G2A.api("/session");
        if (typeof G2A.markAuthOk === "function") G2A.markAuthOk();
        location.replace(next || "/admin");
        return;
      } catch (e) {
        // Token/cookie invalid — stay on login and clear local marker.
        try { G2A.clearToken(); } catch (_) {}
        if ($("auth-desc")) {
          $("auth-desc").textContent = "会话已失效，请重新输入管理员密码登录。";
        }
      }
    }

    const submit = $("auth-submit");
    const pass = $("auth-password") || $("password");
    const errBox = $("auth-error");
    pass?.focus();

    async function doSubmit(ev) {
      ev?.preventDefault?.();
      if (errBox) { errBox.classList.add("hidden"); errBox.textContent = ""; }
      const password = (pass && pass.value) || "";
      if (!password || password.length < 4) {
        G2A.toast("密码至少 4 位", false);
        if (errBox) { errBox.textContent = "密码至少 4 位"; errBox.classList.remove("hidden"); }
        return;
      }
      G2A.setBusy(submit, true, "连接中…");
      try {
        if (setup) {
          const r = await G2A.api("/setup", { method: "POST", body: JSON.stringify({ password }) });
          if (r.token) G2A.setToken(r.token);
        } else {
          const r = await G2A.api("/login", { method: "POST", body: JSON.stringify({ password }) });
          if (r.token) G2A.setToken(r.token);
        }
        if (typeof G2A.markAuthOk === "function") G2A.markAuthOk();
        // Optional warm-up; failure should not force re-login within grace.
        try {
          await G2A.api("/session");
        } catch (_) {}
        G2A.toast(setup ? "已创建管理密码，进入管理台" : "登录成功");
        location.replace(next);
      } catch (e) {
        G2A.toast(e.message || "失败", false);
        if (errBox) { errBox.textContent = e.message || "登录失败"; errBox.classList.remove("hidden"); }
      } finally {
        G2A.setBusy(submit, false);
      }
    }

    submit?.addEventListener("click", doSubmit);
    $("auth-form")?.addEventListener("submit", doSubmit);
    pass?.addEventListener("keydown", (e) => { if (e.key === "Enter") doSubmit(e); });
    $("auth-refresh")?.addEventListener("click", () => location.reload());
  }

async function logout() {
    try { await G2A.api("/logout", { method: "POST", body: "{}" }); } catch (_) {}
    G2A.clearToken();
    location.replace(adminBasePath() + "/login");
  }

  G2A.auth = { requireSession, initLoginPage, logout, redirectLogin, safeNext };
})(window.G2A);
/* g2a-cache-bust-20260712-local-solver */
