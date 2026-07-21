/* thin shared state */
window.G2A = window.G2A || {};
(function (G2A) {
  "use strict";
  const state = { status: null, dashboard: null };
  async function refreshStatus() {
    state.status = await G2A.api("/status");
    return state.status;
  }
  async function refreshDashboard() {
    state.dashboard = await G2A.api("/dashboard");
    return state.dashboard;
  }
  G2A.state = state;
  G2A.refreshStatus = refreshStatus;
  G2A.refreshDashboard = refreshDashboard;
})(window.G2A);
/* g2a-cache-bust-20260712-local-solver */
