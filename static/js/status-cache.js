/* admin API status cache & preload optimization */
window.G2A = window.G2A || {};
(function (G2A) {
  "use strict";

  const STATUS_CACHE_KEY = "g2a_status_cache";
  const STATUS_CACHE_TTL = 30000; // 30 seconds

  let cachedStatus = null;
  let cacheTimestamp = 0;

  // 预加载优化：使用缓存的状态快速渲染，然后异步更新
  function getCachedStatus() {
    try {
      const cached = sessionStorage.getItem(STATUS_CACHE_KEY);
      if (cached) {
        const data = JSON.parse(cached);
        if (data.timestamp && (Date.now() - data.timestamp) < STATUS_CACHE_TTL) {
          return data.status;
        }
      }
    } catch (e) {
      console.warn("Failed to parse status cache:", e);
    }
    return null;
  }

  function setCachedStatus(status) {
    try {
      sessionStorage.setItem(STATUS_CACHE_KEY, JSON.stringify({
        status: status,
        timestamp: Date.now()
      }));
      cachedStatus = status;
      cacheTimestamp = Date.now();
    } catch (e) {
      console.warn("Failed to cache status:", e);
    }
  }

  function clearStatusCache() {
    try {
      sessionStorage.removeItem(STATUS_CACHE_KEY);
      cachedStatus = null;
      cacheTimestamp = 0;
    } catch (e) {}
  }

  // 预加载状态数据，在页面加载时立即调用
  async function preloadStatus() {
    const cached = getCachedStatus();
    if (cached) {
      return cached;
    }

    try {
      const status = await G2A.api("/status");
      setCachedStatus(status);
      return status;
    } catch (e) {
      console.warn("Failed to preload status:", e);
      return null;
    }
  }

  // 获取状态，优先使用缓存
  async function getStatus(forceRefresh = false) {
    if (!forceRefresh && cachedStatus && (Date.now() - cacheTimestamp) < STATUS_CACHE_TTL) {
      return cachedStatus;
    }

    if (!forceRefresh) {
      const cached = getCachedStatus();
      if (cached) {
        return cached;
      }
    }

    const status = await G2A.api("/status");
    setCachedStatus(status);
    return status;
  }

  // 导出函数
  G2A.status = {
    preload: preloadStatus,
    get: getStatus,
    clearCache: clearStatusCache,
  };

  // 页面卸载时不清除缓存，保留给下次加载使用
  // 仅在登出时清除
  G2A.onLogout = function() {
    clearStatusCache();
  };

})(window.G2A);

// 页面加载时立即预加载状态
if (document.readyState === "loading") {
  document.addEventListener("DOMContentLoaded", function() {
    G2A.status && G2A.status.preload();
  });
} else {
  G2A.status && G2A.status.preload();
}
