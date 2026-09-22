/* 主题必须在首次绘制前落到 <html>：等 app.js / mobile.js 执行会先闪一帧深色。
   解析规则与 app.js / mobile.js 的 applyTheme 共用同一份语义（模式存在 sb.theme）。 */
(function () {
  try {
    var mode = localStorage.getItem("sb.theme") || "system";
    var light = window.matchMedia("(prefers-color-scheme: light)").matches;
    document.documentElement.dataset.theme = mode === "system" ? (light ? "light" : "dark") : mode;
  } catch (e) { /* 保留 HTML 上的 dark 兜底 */ }
})();
/* escapeHtml 必须在 chat.js 之前定义：chat.js 加载时即捕获 global.escapeHtml。 */
window.escapeHtml = function (s) {
  return String(s ?? "").replace(/[&<>"']/g, function (c) {
    return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
  });
};