/* admin API client */
window.G2A = window.G2A || {};
(function (G2A) {
  "use strict";
  const TOKEN_KEY = "g2a_admin_token";
  const TOKEN_TS_KEY = "g2a_admin_token_ts";
  function adminBasePath() {
    const path = String(location.pathname || "");
    const i = path.indexOf("/admin");
    return (i >= 0 ? path.slice(0, i) : "") + "/admin";
  }
  const API_BASE = adminBasePath() + "/api";
  // Soft trust window: after successful auth, do not force-redirect on transient 401
  // for a short period (cookie / redis touch races, multi-worker lag).
  const AUTH_GRACE_MS = 5 * 60 * 1000;
  let token = localStorage.getItem(TOKEN_KEY) || "";
  const listeners = { unauthorized: [] };
  let lastAuthOkAt = Number(localStorage.getItem(TOKEN_TS_KEY) || 0) || 0;

  function getToken() { return token; }
  function setToken(t) {
    token = t || "";
    if (token) {
      localStorage.setItem(TOKEN_KEY, token);
      lastAuthOkAt = Date.now();
      localStorage.setItem(TOKEN_TS_KEY, String(lastAuthOkAt));
    } else {
      localStorage.removeItem(TOKEN_KEY);
      localStorage.removeItem(TOKEN_TS_KEY);
      lastAuthOkAt = 0;
    }
  }
  function clearToken() { setToken(""); }
  function markAuthOk() {
    lastAuthOkAt = Date.now();
    try { localStorage.setItem(TOKEN_TS_KEY, String(lastAuthOkAt)); } catch (_) {}
  }
  function inAuthGrace() {
    return !!(token && lastAuthOkAt && (Date.now() - lastAuthOkAt) < AUTH_GRACE_MS);
  }
  function headers(json = true) {
    const h = {};
    if (json) h["Content-Type"] = "application/json";
    if (token) h["X-Admin-Token"] = token;
    return h;
  }
  function onUnauthorized(fn) { listeners.unauthorized.push(fn); }

  function _errMessage(data, fallback) {
    if (!data) return fallback || "请求失败";
    const msg = data.detail || data.error || data.message || data.msg;
    if (typeof msg === "string" && msg.trim()) return msg;
    if (msg != null) {
      try { return JSON.stringify(msg); } catch (_) { return String(msg); }
    }
    return fallback || "请求失败";
  }

  function _apiHtmlError(path, status, text) {
    const sample = String(text || "").replace(/\s+/g, " ").trim().slice(0, 180);
    let hint =
      "Admin API 返回了 HTML 页面，通常是反向代理或部署子路径没有把 " +
      API_BASE + path + " 转发到后端。请检查 /admin/api 路由。";
    // Cloudflare / nginx gateway HTML on 502/504 is the common auto-refresh failure mode
    // when /admin/api/status was too slow (heavy usage scan) or the upstream was restarting.
    if (status === 502 || status === 504 || status === 503) {
      hint =
        "网关 " + status + "：反代连不上后端或上游超时（常见于 /admin/api/status 过重/重启）。" +
        "请确认 grokcli-2api 健康，并把 /admin/api/* 反代到应用端口（勿落到静态站）。";
    }
    const err = new Error(hint + (sample ? " 响应片段：" + sample : ""));
    err.status = status;
    err.path = path;
    err.html = true;
    err.gateway = status === 502 || status === 503 || status === 504;
    err.detail = sample;
    return err;
  }

  function _networkError(path, cause) {
    const raw = (cause && (cause.message || String(cause))) || "Failed to fetch";
    // Browser TypeError "Failed to fetch" is opaque — expand for operators.
    let hint = raw;
    if (/failed to fetch|networkerror|load failed|network request failed/i.test(raw)) {
      hint = "网络请求失败（服务不可达、反向代理中断、或请求超时）。请检查 grokcli-2api 是否在线、端口/反代是否正确。";
    }
    const err = new Error(hint);
    err.status = 0;
    err.path = path;
    err.network = true;
    err.cause = cause;
    return err;
  }

  async function api(path, opts = {}) {
    let res;
    // Optional hard timeout (ms). Registration poll uses this so a hung sidecar
    // never freezes the log UI for tens of seconds.
    const timeoutMs = Number(opts.timeoutMs || opts.timeout || 0) || 0;
    const { timeoutMs: _dropTm, timeout: _dropT, signal: outerSignal, ...fetchOpts } = opts;
    let abortCtrl = null;
    let abortTimer = null;
    let signal = outerSignal;
    if (timeoutMs > 0) {
      abortCtrl = new AbortController();
      if (outerSignal) {
        if (outerSignal.aborted) abortCtrl.abort();
        else {
          try {
            outerSignal.addEventListener("abort", () => abortCtrl.abort(), { once: true });
          } catch (_) {}
        }
      }
      signal = abortCtrl.signal;
      abortTimer = setTimeout(() => {
        try { abortCtrl.abort(); } catch (_) {}
      }, timeoutMs);
    }
    try {
      res = await fetch(API_BASE + path, {
        ...fetchOpts,
        signal,
        credentials: "same-origin",
        headers: {
          ...headers(!(fetchOpts.body instanceof FormData) && fetchOpts.method !== "GET"),
          ...(fetchOpts.headers || {}),
        },
      });
    } catch (cause) {
      const aborted = !!(cause && (cause.name === "AbortError" || (signal && signal.aborted)));
      if (aborted && timeoutMs > 0) {
        const err = _networkError(path, cause);
        err.timeout = true;
        err.message = "请求超时 (" + timeoutMs + "ms)： " + path;
        throw err;
      }
      throw _networkError(path, cause);
    } finally {
      if (abortTimer) {
        try { clearTimeout(abortTimer); } catch (_) {}
      }
    }
    let data = null;
    const ct = (res.headers.get("content-type") || "").toLowerCase();
    try {
      if (ct.includes("application/json")) data = await res.json();
      else {
        const text = await res.text();
        if (/^\s*<!doctype\s+html|^\s*<html[\s>]/i.test(text || "")) {
          throw _apiHtmlError(path, res.status, text);
        }
        data = text ? { detail: text.slice(0, 300) } : null;
      }
    } catch (e) {
      if (e && e.html) throw e;
      data = null;
    }
    if (!res.ok) {
      const fallback = res.status === 500
        ? "服务器内部错误 (500)"
        : (res.statusText || ("HTTP " + res.status));
      const msg = _errMessage(data, fallback);
      const err = new Error(typeof msg === "string" ? msg : JSON.stringify(msg));
      err.status = res.status;
      err.data = data;
      err.path = path;
      const pathStr = String(path || "");
      const ignore401 =
        pathStr.startsWith("/status") ||
        pathStr.includes("/register-email/sessions/");
      // During grace window keep token; only notify unauthorized outside grace.
      if (res.status === 401 && !ignore401) {
        if (!inAuthGrace()) {
          listeners.unauthorized.forEach((fn) => {
            try { fn(err); } catch (_) {}
          });
        } else {
          err.soft = true;
        }
      }
      throw err;
    }
    // Successful authenticated call refreshes grace window.
    if (token && !String(path || "").startsWith("/status")) {
      markAuthOk();
    }
    return data;
  }

  async function download(path, opts = {}) {
    const res = await fetch(API_BASE + path, {
      ...opts,
      credentials: "same-origin",
      headers: { ...headers(false), ...(opts.headers || {}) },
    });
    if (!res.ok) {
      let msg = res.statusText;
      try {
        const d = await res.json();
        msg = d.detail || d.error || msg;
      } catch (_) {}
      const err = new Error(msg);
      err.status = res.status;
      throw err;
    }
    const blob = await res.blob();
    let filename = "download.bin";
    const cd = res.headers.get("Content-Disposition") || "";
    const m = /filename\*?=(?:UTF-8''|")?([^";]+)/i.exec(cd);
    if (m) filename = decodeURIComponent(m[1].replace(/"/g, ""));
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = filename;
    a.click();
    URL.revokeObjectURL(url);
    return filename;
  }

  G2A.TOKEN_KEY = TOKEN_KEY;
  G2A.API_BASE = API_BASE;
  G2A.getToken = getToken;
  G2A.setToken = setToken;
  G2A.clearToken = clearToken;
  G2A.markAuthOk = markAuthOk;
  G2A.inAuthGrace = inAuthGrace;
  G2A.headers = headers;
  G2A.api = api;
  G2A.download = download;
  G2A.onUnauthorized = onUnauthorized;
})(window.G2A);
/* g2a-cache-bust-20260712-local-solver */
