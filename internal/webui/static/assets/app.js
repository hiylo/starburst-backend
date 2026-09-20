/* ============================================================================
 * StarBurst Console — app logic
 * 布局：左栏导航 + 顶栏 + 内容区；所有 API 端点与后端保持稳定契约。
 * ========================================================================== */
"use strict";

const SESSION_KEY = "ocb_web_session";
const APP_TOKEN_KEY = "ocb_app_token";
let session = localStorage.getItem(SESSION_KEY) || "";

const TASK_STATUS = {
  queued: "排队中", running: "执行中", succeeded: "已完成",
  failed: "失败", retrying: "重试中", canceled: "已取消",
  pending: "等待前置", blocked: "被阻塞",
};
const KIND_LABEL = { cron: "cron 定时", git: "git 监听", http: "webhook" };

/* ---------- 外观主题 ---------- */
// 模式取值与安卓 Theme.kt 对齐：system / light / dark / amoled / dim，
// style.css 里每个非 system 取值都有一组 html[data-theme] 令牌覆盖。
const THEME_KEY = "sb.theme";
const THEME_MODES = [
  { mode: "system", label: "跟随系统" },
  { mode: "light", label: "浅色" },
  { mode: "dark", label: "深色" },
  { mode: "amoled", label: "纯黑 AMOLED" },
  { mode: "dim", label: "柔和 Dim" },
];
const prefersLight = window.matchMedia("(prefers-color-scheme: light)");

// resolveTheme 把「模式」翻译成样式里真实存在的 data-theme 值。
function resolveTheme(mode) {
  return mode === "system" ? (prefersLight.matches ? "light" : "dark") : mode;
}
function currentThemeMode() {
  const m = localStorage.getItem(THEME_KEY) || "system";
  return THEME_MODES.some(t => t.mode === m) ? m : "system";
}
function applyTheme() {
  document.documentElement.dataset.theme = resolveTheme(currentThemeMode());
  const sel = document.getElementById("themeMode");
  if (sel) sel.value = currentThemeMode();
}
function setThemeMode(mode) {
  localStorage.setItem(THEME_KEY, mode);
  applyTheme();
}
function initThemePicker() {
  const sel = document.getElementById("themeMode");
  if (sel) {
    sel.innerHTML = THEME_MODES.map(t => `<option value="${t.mode}">${t.label}</option>`).join("");
    sel.addEventListener("change", () => setThemeMode(sel.value));
  }
  // 跟随系统时，操作系统的深浅色切换要即时生效（桌面按时段自动换色就走这条）。
  prefersLight.addEventListener("change", () => { if (currentThemeMode() === "system") applyTheme(); });
  applyTheme();
}

function hdr(extra) {
  const h = { "Content-Type": "application/json" };
  if (session) h["X-Web-Session"] = session;
  return Object.assign(h, extra || {});
}
function appHeaders() {
  const t = localStorage.getItem(APP_TOKEN_KEY) || "";
  // 统一带 JSON Content-Type：Proxy 会把请求头原样转发给上游 OpenCode，
  // 快捷回复/问题答复这类 body 为 JSON 的 POST 若不声明 Content-Type，
  // 上游会以 415 Unsupported Media Type 拒绝。
  const h = { "Content-Type": "application/json" };
  if (t) h["Authorization"] = "Bearer " + t;
  // 同时带 web session：这些端点后端接受 'web session 或 APP token'，未配置
  // APP token 的浏览器用户也能查看任务/项目/归档（否则无 token 请求 401）。
  if (session) h["X-Web-Session"] = session;
  return h;
}
function show(el, msg, ok) {
  el.textContent = msg || "";
  el.className = "msg " + (ok ? "ok" : "err");
}
// toast 弹出右上角通知，用于展示服务端推送的关键任务事件。
// kind 为 info / warn / crit；ttl 毫秒后自动消失（默认 6 秒）。
function toast(title, text, kind, ttl) {
  const wrap = document.getElementById("toastWrap");
  if (!wrap) return;
  const el = document.createElement("div");
  el.className = "toast " + (kind || "info");
  el.innerHTML = "<b>" + escapeHtml(title) + "</b>" + escapeHtml(text || "");
  wrap.appendChild(el);
  setTimeout(() => {
    el.style.transition = "opacity .3s";
    el.style.opacity = "0";
    setTimeout(() => el.remove(), 300);
  }, ttl || 6000);
}
function escapeHtml(s) {
  return String(s ?? "").replace(/[&<>"']/g, c =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}
/* ---------------- 代码语法高亮（零依赖，正则分词） ---------------- */
const HL_KEYWORDS = {
  c: ("if else for while do switch case default break continue return func function var let const class interface " +
    "type struct enum import from export extends implements new delete typeof instanceof this super static async " +
    "await yield try catch finally throw get set public private protected package require select defer chan map " +
    "range go int int8 int16 int32 int64 uint float float32 float64 double string bool boolean byte rune nil null " +
    "void true false").split(/\s+/),
  py: ("and as assert async await break class continue def del elif else except finally for from global if import " +
    "in is lambda nonlocal not or pass raise return try while with yield True False None self").split(/\s+/),
  sh: ("if then else elif fi for while until do done case esac function in return local export unset readonly " +
    "declare echo printf cd ls mkdir rm cp mv cat grep sed awk test true false source sudo curl wget git docker go").split(/\s+/),
  sql: ("select from where insert into values update set delete create table index view drop alter add column " +
    "primary key foreign references join left right inner outer on group by order having limit offset and or not " +
    "null default unique constraint cascade begin commit rollback as union all distinct exists case when then end " +
    "cast coalesce count sum avg min max").split(/\s+/),
};
const HL_RE_CACHE = {};
// 构建语言相关的分词正则：组1注释 / 组2字符串 / 组3数字 / 组4关键字 / 组5函数调用。
function buildHlRe(lang) {
  if (HL_RE_CACHE[lang]) return HL_RE_CACHE[lang];
  const kw = HL_KEYWORDS[lang] || HL_KEYWORDS.c;
  const sorted = kw.slice().sort((a, b) => b.length - a.length).join("|");
  let cm;
  if (lang === "py" || lang === "sh") cm = "#[^\\n]*";
  else if (lang === "sql") cm = "\\/\\*[\\s\\S]*?\\*\\/|--[^\\n]*";
  else cm = "\\/\\*[\\s\\S]*?\\*\\/|\\/\\/[^\\n]*";
  const str = "\"(?:[^\"\\\\\\n]|\\\\.)*\"|'(?:[^'\\\\\\n]|\\\\.)*'|`(?:[^`\\\\]|\\\\.)*`";
  const re = new RegExp(
    "(" + cm + ")|(" + str + ")|(\\b\\d+(?:\\.\\d+)?(?:[eE][+-]?\\d+)?\\b)|(\\b(?:" + sorted + ")\\b)|([A-Za-z_]\\w*(?=\\s*\\())",
    "g"
  );
  HL_RE_CACHE[lang] = re;
  return re;
}
function hlCode(src, lang) {
  if (!src || typeof src !== "string") return escapeHtml(src || "");
  let l = String(lang || "").toLowerCase().split(/[^a-z0-9]+/)[0] || "";
  if (["js", "ts", "jsx", "tsx", "java", "go", "kt", "kts", "rs", "c", "cpp", "cxx", "h", "hpp", "swift", "php", "scala", "cs", "dart", "zig"].includes(l)) l = "c";
  else if (["py", "python"].includes(l)) l = "py";
  else if (["sh", "bash", "zsh", "shell", "fish", "pwsh"].includes(l)) l = "sh";
  else if (l === "sql") l = "sql";
  else return escapeHtml(src);
  const re = buildHlRe(l);
  let out = "", last = 0, m;
  re.lastIndex = 0;
  while ((m = re.exec(src))) {
    if (m.index > last) out += escapeHtml(src.slice(last, m.index));
    const cls = m[1] ? "cm" : m[2] ? "st" : m[3] ? "nu" : m[4] ? "kw" : "fn";
    out += `<span class="tok ${cls}">${escapeHtml(m[0])}</span>`;
    last = re.lastIndex;
  }
  if (last < src.length) out += escapeHtml(src.slice(last));
  return out;
}
// 代码块外壳：语言标签 + 行数 + 复制按钮；超过 24 行折叠成 <details>。
const MD_CODE_FOLD = 24;
function renderCodeBlock(code, lang) {
  const langLabel = String(lang || "").toLowerCase() || "text";
  const n = (code.match(/\n/g) || []).length + 1;
  const pre = `<pre class="md-code"><code>${hlCode(code, lang)}</code></pre>`;
  if (n > MD_CODE_FOLD) {
    return `<div class="md-code-wrap"><details class="md-code-details"><summary>
      <span class="md-code-lang">${escapeHtml(langLabel)}</span><span class="md-code-lines">${n} 行 · 点击展开</span>
      <button type="button" class="md-code-copy" data-md-copy="${escapeHtml(code)}">复制</button>
      </summary>${pre}</details></div>`;
  }
  return `<div class="md-code-wrap"><div class="md-code-head">
    <span class="md-code-lang">${escapeHtml(langLabel)}</span><span class="md-code-lines">${n} 行</span>
    <button type="button" class="md-code-copy" data-md-copy="${escapeHtml(code)}">复制</button>
  </div>${pre}</div>`;
}
// 列表（含嵌套与任务清单）。items: {depth, ordered, task, content}。
function buildNestedList(items, inlineFn) {
  let html = "";
  const stack = [];   // {tag, depth}：已打开列表
  for (const it of items) {
    const tag = it.ordered ? "ol" : "ul";
    // 先关掉比当前层级更深的列表
    while (stack.length && stack[stack.length - 1].depth > it.depth) html += `</${stack.pop().tag}>`;
    const top = stack[stack.length - 1];
    if (top && top.depth === it.depth && top.tag === tag) {
      // 同层同类型：并入当前列表
    } else {
      // 同层换类型（关旧开新）或更深层级（开新嵌套列表）或空栈（开新列表）
      if (top && top.depth === it.depth) html += `</${stack.pop().tag}>`;
      html += `<${tag}>`;
      stack.push({ tag, depth: it.depth });
    }
    if (it.task === null) html += `<li>${inlineFn(it.content)}</li>`;
    else html += `<li class="task"><input type="checkbox" disabled${it.task ? " checked" : ""}><span>${inlineFn(it.content)}</span></li>`;
  }
  while (stack.length) html += `</${stack.pop().tag}>`;
  return html;
}
function isListLine(l) {
  return /^\s*[-*+]\s+/.test(l) || /^\s*\d+[.)]\s+/.test(l);
}
function isTaskContent(content) {
  const m = /^\[([ xX])\]\s+/.exec(content);
  return m ? { done: m[1].toLowerCase() === "x", rest: content.slice(m[0].length) } : null;
}
// 轻量 Markdown 渲染：先整体转义防 XSS，再按块处理标题/代码/列表/表格/引用/分隔线，
// 行内处理加粗/斜体/删除线/行内代码/链接/自动链接。结果均为安全 HTML。
function mdRender(src) {
  if (!src) return "";
  const esc = escapeHtml;
  const lines = String(src).replace(/\r\n/g, "\n").split("\n");
  let html = "";
  let i = 0;
  const inline = (t) => {
    let s = esc(t);
    s = s.replace(/`([^`]+)`/g, (m, c) => `<code>${c}</code>`);
    s = s.replace(/!\[([^\]]*)\]\(([^)\s]+)\)/g, (m, alt, url) => `<a href="${url}" target="_blank" rel="noopener noreferrer">${alt || url}</a>`);
    s = s.replace(/\[([^\]]+)\]\(([^)\s]+)\)/g, (m, txt, url) => `<a href="${url}" target="_blank" rel="noopener noreferrer">${txt}</a>`);
    s = s.replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>");
    s = s.replace(/\*([^*\n]+)\*/g, "<em>$1</em>");
    s = s.replace(/__([^_]+)__/g, "<strong>$1</strong>");
    s = s.replace(/~~([^~]+)~~/g, "<del>$1</del>");
    s = s.replace(/(^|[\s(])((?:https?|ftp):\/\/[^\s<]+)/g, '$1<a href="$2" target="_blank" rel="noopener noreferrer">$2</a>');
    return s;
  };
  const isHr = (l) => /^\s*([-*_])\s*\1\s*\1\s*$/.test(l);
  while (i < lines.length) {
    const line = lines[i];
    // 围栏代码块
    const fm = line.match(/^\s*```([\w+\-.]*)\s*$/);
    if (fm) {
      const lang = fm[1];
      const buf = [];
      i++;
      while (i < lines.length && !/^\s*```\s*$/.test(lines[i])) buf.push(lines[i++]);
      i++;
      html += renderCodeBlock(buf.join("\n"), lang);
      continue;
    }
    // 标题
    const hm = line.match(/^(#{1,6})\s+(.*)$/);
    if (hm) {
      const lvl = hm[1].length;
      html += `<h${lvl}>${inline(hm[2])}</h${lvl}>\n`;
      i++;
      continue;
    }
    if (isHr(line)) { html += "<hr>\n"; i++; continue; }
    // 引用块
    if (/^\s*>/.test(line)) {
      const buf = [];
      while (i < lines.length && /^\s*>/.test(lines[i])) buf.push(lines[i++].replace(/^\s*>\s?/, ""));
      html += `<blockquote>${mdRender(buf.join("\n"))}</blockquote>\n`;
      continue;
    }
    // 表格
    if (line.includes("|") && i + 1 < lines.length && /^\s*\|?[\s:|-]+\|[\s:|-]*$/.test(lines[i + 1])) {
      const parseRow = (r) => r.trim().replace(/^\|/, "").replace(/\|$/, "").split("|").map(c => c.trim());
      const header = parseRow(line);
      i += 2;
      const rows = [];
      while (i < lines.length && lines[i].includes("|")) rows.push(parseRow(lines[i++]));
      html += `<div class="md-table-wrap"><table><thead><tr>${header.map(h => `<th>${inline(h)}</th>`).join("")}</tr></thead><tbody>${rows.map(r => `<tr>${r.map(c => `<td>${inline(c)}</td>`).join("")}</tr>`).join("")}</tbody></table></div>\n`;
      continue;
    }
    // 有序 / 无序列表（含嵌套缩进与任务清单 [ ] / [x]）
    if (isListLine(line)) {
      const collected = [];
      while (i < lines.length && isListLine(lines[i])) collected.push(lines[i++]);
      const base = /^\s*/.exec(collected[0])[0].length;
      const items = collected.map(l => {
        const ind = /^\s*/.exec(l)[0].length;
        const depth = Math.max(0, Math.min(8, Math.round((ind - base) / 2)));
        const t = l.trim();
        const marker = /^([-*+]|\d+[.)])\s+/.exec(t);
        if (!marker) return { depth, ordered: false, task: null, content: "" };
        let content = t.slice(marker[0].length);
        let task = null;
        const tk = isTaskContent(content);
        if (tk) { task = tk.done; content = tk.rest; }
        return { depth, ordered: /^\d+[.)]$/.test(marker[1]), task, content };
      });
      html += buildNestedList(items, inline);
      continue;
    }
    if (!line.trim()) { i++; continue; }
    // 普通段落（多行合并，换行转 <br>）
    const buf = [];
    while (i < lines.length && lines[i].trim() && !isListLine(lines[i]) && !/^\s*```/.test(lines[i]) && !/^\s*>/.test(lines[i]) && !/^#{1,6}\s/.test(lines[i]) && !isHr(lines[i])) {
      buf.push(lines[i++]);
    }
    html += `<p>${inline(buf.join("\n")).replace(/\n/g, "<br>")}</p>\n`;
  }
  return html;
}
// 复制按钮全局委托：mdRender 产物出现在工作台对话与知识库问答两处，统一走 document。
document.addEventListener("click", (e) => {
  const btn = e.target.closest("[data-md-copy]");
  if (!btn) return;
  e.preventDefault();
  e.stopPropagation();
  const text = btn.dataset.mdCopy || "";
  (navigator.clipboard ? navigator.clipboard.writeText(text).catch(() => {}) : Promise.resolve())
    .finally(() => {
      const old = btn.textContent;
      btn.textContent = "已复制";
      setTimeout(() => { btn.textContent = old; }, 1200);
    });
});
function fmtDate(v) { return v ? new Date(v).toLocaleString() : "-"; }
// 事件摘要里 token 数的紧凑格式（1.2k / 1.34M）。
function fmtTok(n) {
  n = Number(n) || 0;
  if (n < 1000) return String(n);
  if (n < 1000000) return (n / 1000).toFixed(n < 10000 ? 1 : 0) + "k";
  return (n / 1000000).toFixed(2) + "M";
}

async function api(path, opts) {
  opts = opts || {};
  // 请求兜底超时：上游挂起 / 网络抖动时不能永久 pending——否则聊天区会一直
  // 卡在「加载对话…」。默认 30s，个别大上传/长操作可传 opts.timeout 覆盖。
  const ctrl = new AbortController();
  const timeoutMs = opts.timeout || 30000;
  const timer = setTimeout(() => ctrl.abort(), timeoutMs);
  opts.signal = ctrl.signal;
  try {
    const res = await fetch(path, opts);
    // 仅在请求携带 web session 头（即该请求本就依赖管理员会话）且收到 401
    // 时才判定会话过期。用 APP token 的请求失败（如 token 被撤销）不应连坐
    // 清掉仍有效的 web session，否则切菜单时任何一个 token 请求失败都会把
    // 用户踢回登录页。
    const usesWebSession = !!(opts.headers && (opts.headers["X-Web-Session"] || opts.headers["x-web-session"]));
    if (res.status === 401 && session && usesWebSession) {
      session = "";
      localStorage.removeItem(SESSION_KEY);
      show(document.getElementById("loginMsg"), "会话已过期，请重新登录");
      renderAuth();
      throw new Error("unauthorized");
    }
    return res;
  } finally {
    clearTimeout(timer);
  }
}

/* ---------- 路由 ---------- */
const TITLES = {
  workbench: "AI 工作台", tasks: "任务", workflow: "编排", stream: "实时流", projects: "项目 / 会话",
  rules: "自动化规则", archives: "会话归档", audit: "审计日志",
  tokens: "Token 管理", settings: "设置", intel: "智能测试",
};
function switchPage(name) {
  // 不记忆所在页：每次打开一律从 AI 工作台开始，避免误入/混叠其他页面。
  document.querySelectorAll(".page").forEach(p => p.classList.add("hidden"));
  const target = document.getElementById("page-" + name);
  if (target) target.classList.remove("hidden");
  document.querySelectorAll("#nav button").forEach(b => b.classList.toggle("active", b.dataset.page === name));
  document.getElementById("pageTitle").textContent = TITLES[name] || name;
  // 工作台大屏布局：内容区整宽并纵向撑满，消除右侧/底部空余。
  const content = document.querySelector(".content");
  if (content) {
    content.classList.toggle("wb-tight", name === "workbench");
    content.classList.toggle("full", name === "intel");
  }
  // 每个页面切到时自动加载数据，避免打开就是空的、还得手动点"刷新"。
  // 对话区的 SSE 通道只在停在工作台时保持（见 wbSyncStream）。
  wbSyncStream();
  if (name === "workbench") {
    wbLoadLocal();
    loadWorkbench(); loadWbEvents(); renderWbFilters(); wbRenderTagFilters(); ensureWbProviders();
    // 回到工作台补一次服务端快照：离开期间 token 级增量不再回放，靠整轮拉取收敛。
    if (wbChat) wbChat.reload();
  }
  else if (name === "tasks") { loadTasks(); loadTaskStats(); }
  else if (name === "workflow") { loadWorkflows(); if (!document.getElementById("wfSteps").children.length) wfAddStep(); }
  else if (name === "projects") loadProjects();
  else if (name === "rules") loadRules();
  else if (name === "archives") { loadArchives(); loadArchiveSessions(); }
  else if (name === "audit") loadAudit();
  else if (name === "tokens") { loadTokens(); loadTokenUsage(); }
  else if (name === "settings") { loadLLMConfig(); loadEmbedConfig(); loadIntelAiRules(); }
  else if (name === "stream") ensureStream();
  else if (name === "intel") loadIntelProjects();
}
document.querySelectorAll("#nav button").forEach(b => {
  b.addEventListener("click", () => switchPage(b.dataset.page));
});

/* ---------- 登录 ---------- */
async function doLogin() {
  const pw = document.getElementById("pw").value;
  const res = await fetch("/api/web/session", { method: "POST", headers: hdr(), body: JSON.stringify({ password: pw }) });
  const data = await res.json();
  if (!res.ok) { show(document.getElementById("loginMsg"), data.error || "登录失败"); return; }
  session = data.session;
  localStorage.setItem(SESSION_KEY, session);
  renderAuth();
}
function doLogout() {
  session = "";
  localStorage.removeItem(SESSION_KEY);
  if (taskWS) { taskWS.close(); taskWS = null; }
  if (streamES) { streamES.close(); streamES = null; }
  // 对话区的 SSE 通道用的是同一条登录会话，退出后必须退订，否则重连会一直 401。
  if (wbChatUnsub) { wbChatUnsub(); wbChatUnsub = null; }
  if (window.SBChat) SBChat.Bus.close();
  renderAuth();
}

// 智能测试入口可见性由后端能力决定：SQLite 轻量化部署无 pgvector，不显示
// 智能测试；PG(pgvector)+embedding 就绪时（vectorCapable=true）才显示。
let vectorCapable = false;
async function loadCapabilities() {
  try {
    const res = await fetch("/api/system", { headers: appHeaders() });
    const data = await res.json();
    vectorCapable = !!data.vectorCapable;
  } catch (_) {
    vectorCapable = false;
  }
  const intelBtn = document.querySelector('#nav [data-page="intel"]');
  if (intelBtn) intelBtn.style.display = vectorCapable ? "" : "none";
  // 若智能测试当前不可用但正停留在其页面，退回工作台。
  if (!vectorCapable) {
    const cur = document.querySelector(".page:not(.hidden)");
    if (cur && cur.id === "page-intel") switchPage("workbench");
  }
}

function renderAuth() {
  const logged = !!session;
  document.getElementById("loginPage").classList.toggle("hidden", logged);
  document.getElementById("nav").style.display = logged ? "" : "none";
  document.querySelector(".sidebar-foot").style.display = logged ? "" : "none";
  // 未登录时整个侧栏隐藏，登录页居中；登录后再恢复。
  const sidebar = document.querySelector(".sidebar");
  if (sidebar) sidebar.style.display = logged ? "" : "none";
  // 未登录时顶栏（页面标题/版本 plate）也隐藏，只留居中的登录卡片。
  const topbar = document.querySelector(".topbar");
  if (topbar) topbar.style.display = logged ? "" : "none";
  if (!logged) {
    document.querySelectorAll(".page").forEach(p => {
      if (p.id !== "loginPage") p.classList.add("hidden");
    });
  } else {
    // 登录/刷新后一律进入 AI 工作台：不记忆上次所在页，保证每次打开都是工作台。
    loadCapabilities().then(() => {
      switchPage("workbench");
    });
  }
}

/* ---------- App token ---------- */
function saveAppToken() {
  const v = document.getElementById("appToken").value.trim();
  localStorage.setItem(APP_TOKEN_KEY, v);
  document.getElementById("appToken").value = v;
  if (taskWS) taskWS.close();
  connectTaskWS();
  // 换 Token 后 SSE 连接仍带旧凭据，退订重连一次（未订阅时是空操作）。
  if (window.SBChat && wbChatUnsub) SBChat.Bus.restart();
  if (document.getElementById("page-stream").classList.contains("active")
    || !document.getElementById("page-stream").classList.contains("hidden")) ensureStream(true);
}

/* ---------- Token 调用次数（Token 管理页） ---------- */
async function loadTokenUsage() {
  try {
    const st = await (await api("/api/stats", { headers: hdr() })).json();
    const tb = document.querySelector("#usageTable tbody");
    if (!tb) return;
    tb.innerHTML = "";
    const usage = st.tokenUsage || [];
    if (!usage.length) {
      tb.insertAdjacentHTML("beforeend", `<tr><td colspan="2" class="muted">暂无调用记录</td></tr>`);
    } else {
      for (const u of usage) {
        tb.insertAdjacentHTML("beforeend", `<tr><td>${escapeHtml(u.tokenName || u.tokenId)}</td><td>${u.calls}</td></tr>`);
      }
    }
  } catch (_) {}
}

/* ---------- 编排（多步工作流） ---------- */
let wfStepSeq = 0;
function wfStepRow() {
  const i = ++wfStepSeq;
  const row = document.createElement("div");
  row.className = "wf-step";
  row.style.cssText = "display:flex;gap:6px;flex-wrap:wrap;align-items:center;margin-top:8px";
  row.innerHTML = `
    <span class="muted" style="font-size:12px;flex-shrink:0">步骤 ${i}</span>
    <input type="text" placeholder="名称" style="flex:0 0 130px" class="wf-s-name">
    <input type="text" placeholder="Prompt（必填）" style="flex:1;min-width:200px" class="wf-s-prompt">
    <input type="text" placeholder="目录(可选)" style="flex:0 0 160px" class="wf-s-dir">
    <input type="number" placeholder="超时s" title="超时秒数" style="flex:0 0 80px" class="wf-s-timeout">
    <input type="number" placeholder="优先级" title="优先级0-100" value="50" style="flex:0 0 70px" class="wf-s-priority">
    <button class="danger sm" title="删除步骤" onclick="this.parentElement.remove()">×</button>`;
  return row;
}
function wfAddStep() {
  document.getElementById("wfSteps").appendChild(wfStepRow());
}
async function wfCreate() {
  const name = document.getElementById("wfName").value.trim();
  const directory = document.getElementById("wfDirectory").value.trim();
  const steps = [...document.querySelectorAll("#wfSteps .wf-step")].map(el => {
    const g = q => (el.querySelector(q) || {}).value;
    const tsec = parseInt(g(".wf-s-timeout"), 10);
    const pri = parseInt(g(".wf-s-priority"), 10);
    return {
      name: g(".wf-s-name").trim(),
      prompt: g(".wf-s-prompt").trim(),
      directory: g(".wf-s-dir").trim(),
      timeoutSeconds: isNaN(tsec) || tsec <= 0 ? 0 : tsec,
      priority: isNaN(pri) ? 50 : pri,
    };
  }).filter(s => s.prompt);
  if (!steps.length) { show(document.getElementById("wfMsg"), "请至少填写一个步骤的 Prompt"); return; }
  const res = await api("/api/workflow", { method: "POST", headers: appHeaders(), body: JSON.stringify({ name, directory, steps }) });
  const data = await res.json();
  if (!res.ok) { show(document.getElementById("wfMsg"), data.error || "创建失败"); return; }
  show(document.getElementById("wfMsg"), `已创建编排 ${data.workflowId}，共 ${data.tasks.length} 步`, true);
  document.getElementById("wfName").value = "";
  document.getElementById("wfSteps").innerHTML = "";
  wfAddStep();
  loadWorkflows();
}
async function loadWorkflows() {
  const box = document.getElementById("wfList");
  const msg = document.getElementById("wfListMsg");
  const res = await api("/api/workflows", { headers: appHeaders() });
  const data = await res.json();
  if (!res.ok) { if (msg) show(msg, data.error || "加载失败"); return; }
  const list = data.workflows || [];
  if (msg) show(msg, "");
  if (!list.length) { box.innerHTML = `<div class="muted" style="font-size:13px">暂无编排</div>`; box.onclick = null; return; }
  box.innerHTML = list.map(w =>
    `<div class="card" style="margin-bottom:8px">
      <div class="row" style="justify-content:space-between">
        <b>${escapeHtml(w.name || w.workflowId)}</b>
        <span class="muted mono" style="font-size:11px">${escapeHtml(w.workflowId)}</span>
        <span class="muted" style="font-size:12px">${fmtDate(w.createdAt)}</span>
      </div>
      <div class="row" style="gap:12px;margin-top:6px">
        <span class="badge ok">成功 ${w.succeeded}</span>
        <span class="badge danger">失败 ${w.failed}</span>
        <span class="badge primary">执行中/待执行 ${w.running}</span>
        <span class="badge">共 ${w.steps} 步</span>
      </div>
      <div class="row" style="margin-top:8px">
        <button class="ghost sm" data-wf="${escapeHtml(w.workflowId)}">${w.succeeded}/${w.steps} 步成功 · 详情</button>
        <button class="ghost sm" data-wf-action="rerun" data-wf="${escapeHtml(w.workflowId)}">重跑（从失败处）</button>
        <button class="danger sm" data-wf-action="cancel" data-wf="${escapeHtml(w.workflowId)}">取消</button>
      </div>
      <div class="hidden wf-steps" data-wf-steps="${escapeHtml(w.workflowId)}"></div>
    </div>`).join("");
  box.onclick = (e) => {
    const st = e.target.closest("button[data-wf-step]");
    if (st) {
      const row = box.querySelector(`[data-wf-step-detail="${st.dataset.wfStep}"]`);
      if (row) row.classList.toggle("hidden");
      return;
    }
    const actionBtn = e.target.closest("button[data-wf-action]");
    if (actionBtn) {
      const id = actionBtn.dataset.wf;
      const act = actionBtn.dataset.wfAction;
      if (act === "cancel" && !confirm("确认取消该编排的所有未完成任务？")) return;
      api(`/api/workflow/${encodeURIComponent(id)}/${act}`, { method: "POST", headers: appHeaders() }).then(async r => {
        const d = await r.json();
        if (!r.ok) { toast("操作失败", d.error || ("HTTP " + r.status), "crit"); return; }
        toast(act === "cancel" ? "已取消编排" : "已重跑编排", act === "cancel" ? `取消 ${d.canceled ?? 0} 步` : `重置 ${d.reset ?? 0} 步`, "info");
        loadWorkflows();
      });
      return;
    }
    const btn = e.target.closest("button[data-wf]");
    if (!btn) return;
    const id = btn.dataset.wf;
    const target = box.querySelector(`[data-wf-steps="${id}"]`);
    if (!target) return;
    if (!target.classList.contains("hidden")) { target.classList.add("hidden"); return; }
    target.classList.remove("hidden");
    target.innerHTML = `<div class="muted" style="font-size:12px;padding:6px">加载中…</div>`;
    api(`/api/workflow/${encodeURIComponent(id)}`, { headers: appHeaders() }).then(async r => {
      if (!r.ok) { target.innerHTML = `<div class="muted">加载失败</div>`; return; }
      const d = await r.json();
      target.innerHTML = `<div class="table-wrap" style="margin-top:8px"><table>
        <thead><tr><th>步骤</th><th>目录</th><th>Prompt</th><th>状态</th><th>优先级</th><th>创建时间</th><th></th></tr></thead>
        <tbody>${(d.steps || []).map((s, idx) => {
          const detail = [
            `名称: ${s.name || "-"}`, `状态: ${TASK_STATUS[s.status] || s.status}`,
            `工作流: ${s.workflowId || "-"}`, `上游: ${s.dependsOn || "-"}`,
            `尝试: ${s.attempts ?? 0}`, `超时: ${s.timeoutSeconds ? s.timeoutSeconds + "s" : "-"}`,
            `错误: ${s.error || "-"}`, `进度: ${s.progress || "-"}`, `结果: ${s.result || "-"}`,
            `AI 摘要: ${s.aiSummary || "-"}`,
          ].join("\n");
          return `<tr>
            <td>#${idx + 1} ${escapeHtml(s.name || "")}</td>
            <td class="mono clip" title="${escapeHtml(s.directory)}">${escapeHtml(s.directory) || "-"}</td>
            <td class="clip" title="${escapeHtml(s.prompt)}">${escapeHtml(s.prompt)}</td>
            <td><span class="badge ${escapeHtml(s.status)}">${TASK_STATUS[s.status] || s.status}</span></td>
            <td class="mono">${s.priority ?? 50}</td>
            <td>${fmtDate(s.createdAt)}</td>
            <td><button class="ghost sm" data-wf-step="${escapeHtml(s.id)}">详情</button></td>
          </tr>
          <tr class="hidden" data-wf-step-detail="${escapeHtml(s.id)}"><td colspan="7"><div class="pre" style="margin:0">${escapeHtml(detail)}</div></td></tr>`;
        }).join("")}
        </tbody></table></div>`;
    });
  };
}

/* ---------- 任务 ---------- */
async function submitTask() {
  const prompt = document.getElementById("taskPrompt").value.trim();
  const directory = document.getElementById("taskDirectory").value.trim();
  const dependsOn = document.getElementById("taskDependsOn").value.trim();
  const name = document.getElementById("taskName").value.trim();
  const body = { prompt, directory };
  if (dependsOn) body.dependsOn = dependsOn;
  if (name) body.name = name;
  const p = parseInt(document.getElementById("taskPriority").value, 10);
  if (!isNaN(p) && p >= 0 && p <= 100) body.priority = p;
  const tsec = parseInt(document.getElementById("taskTimeout").value, 10);
  if (!isNaN(tsec) && tsec > 0) body.timeoutSeconds = tsec;
  const schedAt = document.getElementById("taskScheduledAt").value.trim();
  if (schedAt) body.scheduledAt = schedAt;
  const cron = document.getElementById("taskCron").value.trim();
  if (cron) body.cron = cron;
  if (!prompt) { show(document.getElementById("taskSubmitMsg"), "请填写 Prompt"); return; }
  const res = await api("/api/tasks", { method: "POST", headers: appHeaders(), body: JSON.stringify(body) });
  const data = await res.json();
  if (!res.ok) { show(document.getElementById("taskSubmitMsg"), data.error || "提交失败"); return; }
  const note = data.status === "pending" ? "（等待前置任务）" : "";
  show(document.getElementById("taskSubmitMsg"), `已提交任务 ${data.id}${note}`, true);
  document.getElementById("taskPrompt").value = "";
  document.getElementById("taskDependsOn").value = "";
  document.getElementById("taskName").value = "";
  loadTasks();
}
let taskSelected = new Set();
let taskExpanded = new Set();
let taskLastList = [];

async function loadTasks() {
  const status = document.getElementById("taskFilter").value;
  const url = "/api/tasks" + (status ? "?status=" + status : "");
  const res = await api(url, { headers: appHeaders() });
  const data = await res.json();
  if (!res.ok) { show(document.getElementById("taskMsg"), data.error || "加载失败"); return; }
  const list = data.tasks || [];
  taskLastList = list;
  // 目录下拉（增量维护，不打断已选值）。
  const dirSel = document.getElementById("taskDirFilter");
  const dirs = [...new Set(list.map(t => t.directory).filter(Boolean))];
  const curDir = dirSel.value;
  dirSel.innerHTML = `<option value="">全部目录</option>` + dirs.map(d => `<option value="${escapeHtml(d)}">${escapeHtml(d)}</option>`).join("");
  dirSel.value = curDir;

  const kw = document.getElementById("taskSearch").value.trim().toLowerCase();
  const dirFilter = dirSel.value;
  const shown = list.filter(t => {
    if (dirFilter && t.directory !== dirFilter) return false;
    if (kw) {
      const hay = (t.name + " " + t.prompt + " " + (t.directory || "") + " " + (t.workflowId || "") + " " + t.id).toLowerCase();
      if (!hay.includes(kw)) return false;
    }
    return true;
  });

  const tb = document.querySelector("#taskTable tbody");
  tb.innerHTML = "";
  if (!shown.length) {
    tb.insertAdjacentHTML("beforeend", `<tr><td colspan="9" class="muted">暂无任务</td></tr>`);
    return;
  }
  for (const t of shown) {
    const cancellable = t.status === "queued" || t.status === "running" || t.status === "pending" || t.status === "retrying";
    const retriable = t.status === "failed" || t.status === "canceled";
    const tid = escapeHtml(t.id);
    const checked = taskSelected.has(t.id) ? "checked" : "";
    const detailBits = [];
    if (t.attempts) detailBits.push(`尝试 ${t.attempts} 次`);
    if (t.timeoutSeconds) detailBits.push(`超时 ${t.timeoutSeconds}s`);
    if (t.priority && t.priority !== 50) detailBits.push(`优先级 ${t.priority}`);
    if (t.scheduledAt) detailBits.push(`排期 ${new Date(t.scheduledAt).toLocaleString()}`);
    if (t.cron) detailBits.push(`周期 ${t.cron}`);
    if (t.startedAt && t.finishedAt) {
      const dur = (new Date(t.finishedAt) - new Date(t.startedAt)) / 1000;
      if (dur > 0) detailBits.push(`耗时 ${dur.toFixed(0)}s`);
    }
    const action = `<div class="actions">
      <button class="ghost sm" data-detail="${tid}">详情</button>
      ${t.status === "blocked" ? `<button class="ghost sm" data-unblock="${tid}">解阻</button>` : ""}
      ${retriable ? `<button class="ghost sm" data-retry="${tid}">重试</button>` : ""}
      ${cancellable ? `<button class="danger sm" data-cancel="${tid}">取消</button>` : ""}
    </div>`;
    tb.insertAdjacentHTML("beforeend", `<tr data-task="${tid}">
      <td><input type="checkbox" class="task-sel" data-sel="${tid}" ${checked}></td>
      <td class="mono clip" title="${tid}">${tid}</td>
      <td class="clip" title="${escapeHtml(t.directory)}">${escapeHtml(t.directory) || "-"}</td>
      <td class="clip" title="${escapeHtml(t.prompt)}">${escapeHtml(t.name ? "[" + t.name + "] " : "")}${escapeHtml(t.prompt)}</td>
      <td class="mono">${t.priority ?? 50}</td>
      <td><span class="badge ${escapeHtml(t.status)}">${TASK_STATUS[t.status] || t.status}</span></td>
      <td class="mono clip muted" title="${escapeHtml(t.dependsOn || "")}">${escapeHtml(t.dependsOn) || "-"}</td>
      <td>${fmtDate(t.createdAt)}</td>
      <td>${action}</td>
    </tr>`);
    const detailRow = document.createElement("tr");
    detailRow.className = "task-detail" + (taskExpanded.has(t.id) ? "" : " hidden");
    detailRow.innerHTML = `<td colspan="9"><div class="pre" style="margin:0">
      ${escapeHtml([
        ["状态", TASK_STATUS[t.status] || t.status],
        ["名称", t.name || "-"],
        ["工作流", t.workflowId || "-"],
        ["上游", t.dependsOn || "-"],
        ["错误", t.error || "-"],
        ["进度", t.progress || "-"],
        ["结果", t.result || "-"],
        ["AI 摘要", t.aiSummary || "-"],
        ["详情", detailBits.join(" · ") || "-"],
      ].map(([k, v]) => `${k}: ${v}`).join("\n"))}
      ${t.dependsOn ? `<div class="task-down" data-down="${escapeHtml(t.id)}">下游：加载中…</div>` : ""}
    </div></td>`;
    tb.insertAdjacentElement("beforeend", detailRow);
    if (taskExpanded.has(t.id)) loadTaskDownstream(t.id, detailRow);
  }
  tb.onclick = (e) => {
    const dt = e.target.closest("button[data-detail]");
    if (dt) {
      const id = dt.dataset.detail;
      if (taskExpanded.has(id)) { taskExpanded.delete(id); } else { taskExpanded.add(id); loadTaskDownstream(id); }
      loadTasks();
      return;
    }
    const ub = e.target.closest("button[data-unblock]");
    if (ub) { unblockTask(ub.dataset.unblock); return; }
    const rt = e.target.closest("button[data-retry]");
    if (rt) { retryTask(rt.dataset.retry); return; }
    const cc = e.target.closest("button[data-cancel]");
    if (cc) cancelTask(cc.dataset.cancel);
  };
  tb.onchange = (e) => {
    const cb = e.target.closest(".task-sel");
    if (cb) {
      if (cb.checked) taskSelected.add(cb.dataset.sel); else taskSelected.delete(cb.dataset.sel);
      document.getElementById("taskSelAll").checked = taskSelected.size === shown.length && shown.length > 0;
    }
  };
}
// 加载某任务的下游（依赖它的任务），渲染进其详情。
async function loadTaskDownstream(id, row) {
  const el = row ? row.querySelector(`.task-down[data-down="${id}"]`) : null;
  if (!el) return;
  const res = await api(`/api/tasks/${encodeURIComponent(id)}/dependents`, { headers: appHeaders() });
  if (!res.ok) { el.textContent = "下游：加载失败"; return; }
  const d = await res.json();
  const deps = d.dependents || [];
  el.textContent = deps.length ? ("下游：\n" + deps.map(x => `  ${x.id} [${TASK_STATUS[x.status] || x.status}] ${x.prompt}`).join("\n")) : "下游：无";
}
// 批量操作（取消 / 重试选中的任务）。
async function taskBatch(action) {
  const ids = [...taskSelected];
  if (!ids.length) { toast("批量操作", "请先勾选任务", "warn"); return; }
  const label = action === "cancel" ? "取消" : "重试";
  if (action === "cancel" && !confirm(`确认取消选中的 ${ids.length} 个任务？`)) return;
  const res = await api("/api/tasks/action", { method: "POST", headers: appHeaders(), body: JSON.stringify({ ids, action }) });
  const data = await res.json();
  if (!res.ok) { toast("批量" + label + "失败", data.error || "HTTP " + res.status, "crit"); return; }
  toast("批量" + label, `已${label} ${data.affected ?? ids.length} 个`, "info");
  taskSelected.clear();
  document.getElementById("taskSelAll").checked = false;
  loadTasks();
}
document.getElementById("taskSearch").addEventListener("input", loadTasks);
document.getElementById("taskDirFilter").addEventListener("change", loadTasks);
document.getElementById("taskFilter").addEventListener("change", loadTasks);
document.getElementById("taskSelAll").addEventListener("change", (e) => {
  taskSelected.clear();
  if (e.target.checked) taskLastList.forEach(t => taskSelected.add(t.id));
  loadTasks();
});
// 手动重试已失败/已取消的任务。
async function retryTask(id) {
  const res = await api(`/api/tasks/${encodeURIComponent(id)}/retry`, { method: "POST", headers: appHeaders() });
  if (!res.ok) { toast("重试失败", "任务重试未生效 (" + res.status + ")", "crit"); return; }
  toast("已重试", "任务已重新排队", "info");
  loadTasks();
}
// 任务统计（近 N 天）：状态分布 + 成功率 + 平均耗时 + 每日趋势 + 并发占用。
async function loadTaskStats() {
  try {
    const res = await api("/api/tasks/stats?days=7", { headers: appHeaders() });
    if (!res.ok) return;
    const st = await res.json();
    document.getElementById("tsTotal").textContent = st.total ?? 0;
    document.getElementById("tsSucceeded").textContent = st.succeeded ?? 0;
    document.getElementById("tsFailed").textContent = st.failed ?? 0;
    document.getElementById("tsRate").textContent = ((st.successRate ?? 0) * 100).toFixed(0) + "%";
    const avg = st.avgDurationSec ?? 0;
    document.getElementById("tsAvg").textContent = avg > 60 ? (avg / 60).toFixed(1) + "m" : avg.toFixed(0) + "s";
    const trend = (st.trend || []);
    const maxV = Math.max(1, ...trend.map(d => Math.max(d.created, d.succeeded, d.failed)));
    document.getElementById("taskTrend").innerHTML = trend.map(d =>
      `<div style="display:inline-block;margin-right:14px;vertical-align:bottom;text-align:center">
        <div style="display:flex;gap:2px;align-items:flex-end;height:40px">
          <span style="width:7px;background:var(--primary);height:${(d.succeeded / maxV * 40).toFixed(0)}px;border-radius:2px" title="成功 ${d.succeeded}"></span>
          <span style="width:7px;background:var(--danger);height:${(d.failed / maxV * 40).toFixed(0)}px;border-radius:2px" title="失败 ${d.failed}"></span>
        </div>
        <div style="font-size:10px;color:var(--ink-tertiary)">${escapeHtml(d.day)}</div>
      </div>`).join("") || "近 7 天无任务";
    // 并发占用（运行中 / 上限）。
    try {
      const s2 = await (await api("/api/stats", { headers: hdr() })).json();
      const running = (s2.tasks && (s2.tasks.running ?? 0)) || 0;
      const cap = s2.maxConcurrency || 0;
      const el = document.getElementById("tsConc");
      if (el) el.textContent = cap > 0 ? `${running}/${cap}` : `${running}`;
    } catch (_) {}
  } catch (_) {}
}
async function cancelTask(id) {
  await api("/api/tasks/" + encodeURIComponent(id), { method: "DELETE", headers: appHeaders() });
  loadTasks();
}
// unblockTask 手动解阻：人工处理完前置问题后，把 blocked 任务重新排队。
async function unblockTask(id) {
  if (!confirm("确认手动解阻该任务？\n前置任务不会重新执行，该任务会直接重新排队。")) return;
  const res = await api("/api/tasks/" + encodeURIComponent(id), { method: "POST", headers: appHeaders() });
  if (!res.ok) {
    const data = await res.json();
    show(document.getElementById("taskMsg"), data.error || "解阻失败");
    return;
  }
  loadTasks();
}

/* ---------- 项目 ---------- */
async function loadProjects() {
  const res = await api("/api/projects", { headers: appHeaders() });
  const data = await res.json();
  if (!res.ok) { show(document.getElementById("projectMsg"), data.error || "加载失败"); return; }
  const list = data.projects || [];
  const tb = document.querySelector("#projectTable tbody");
  tb.innerHTML = "";
  for (const p of list) {
    tb.insertAdjacentHTML("beforeend", `<tr>
      <td class="clip" title="${escapeHtml(p.directory)}">${escapeHtml(p.directory) || "-"}</td>
      <td class="mono">${p.sessionCount ?? 0}</td>
      <td><button class="ghost sm" data-dir="${escapeHtml(p.id)}">查看会话</button></td>
    </tr>`);
  }
  tb.onclick = (e) => {
    const btn = e.target.closest("button[data-dir]");
    if (btn) loadProjectSessions(btn.dataset.dir);
  };
  if (!list.length) {
    tb.insertAdjacentHTML("beforeend", `<tr><td colspan="3" class="muted">暂无会话</td></tr>`);
  }
}
async function loadProjectSessions(dir) {
  const card = document.getElementById("projectSessionCard");
  card.classList.remove("hidden");
  document.getElementById("projectSessionDir").textContent = dir;
  const tb = document.querySelector("#projectSessionTable tbody");
  tb.innerHTML = `<tr><td colspan="8" class="muted">加载中…</td></tr>`;
  const res = await api("/api/projects/" + encodeURIComponent(dir), { headers: appHeaders() });
  const data = await res.json();
  if (!res.ok) { tb.innerHTML = `<tr><td colspan="8" class="muted">${escapeHtml(data.error || "加载失败")}</td></tr>`; return; }
  const list = data.sessions || [];
  tb.innerHTML = "";
  for (const s of list) {
    const cls = s.busy ? "busy" : "idle";
    const label = s.busy ? "忙碌" : "空闲";
    const tokens = s.inputTokens != null ? `${fmtNum(s.inputTokens)} / ${fmtNum(s.outputTokens)}` : "-";
    const model = (s.model && typeof s.model === "object" && s.model.id) ? s.model.id : (s.model || "-");
    const sdir = (s.directory || s.path || "").toString();
    tb.insertAdjacentHTML("beforeend", `<tr>
      <td class="clip" title="${escapeHtml(s.title || s.id)}">${escapeHtml(s.title || s.slug || s.id)}</td>
      <td class="mono clip muted" title="${escapeHtml(s.id)}">${escapeHtml(s.id.slice(0, 22))}</td>
      <td class="clip">${escapeHtml(model)}</td>
      <td>${escapeHtml(s.agent) || "-"}</td>
      <td class="mono clip muted" title="${escapeHtml(s.directory || s.path || "")}">${escapeHtml(sdir)}</td>
      <td class="mono">${tokens}</td>
      <td><span class="badge ${cls}">${label}</span></td>
      <td>${s.updatedMs ? new Date(s.updatedMs).toLocaleString() : "-"}</td>
    </tr>`);
  }
  if (!list.length) {
    tb.insertAdjacentHTML("beforeend", `<tr><td colspan="8" class="muted">暂无会话</td></tr>`);
  }
}
function closeProjectSessions() {
  document.getElementById("projectSessionCard").classList.add("hidden");
  document.getElementById("projectSessionDir").textContent = "";
}
function fmtNum(n) {
  if (n >= 1e9) return (n / 1e9).toFixed(1) + "B";
  if (n >= 1e6) return (n / 1e6).toFixed(1) + "M";
  if (n >= 1e3) return (n / 1e3).toFixed(1) + "K";
  return String(n);
}

/* ---------- 规则 ---------- */
function ruleScheduleLabel() {
  const kind = document.getElementById("ruleKind").value;
  document.getElementById("ruleScheduleLabel").textContent =
    kind === "cron" ? "调度（cron 表达式「秒 分 时 日 月 周」，或如 5m/1h 间隔）"
    : kind === "git" ? "仓库路径（Schedule，监听 HEAD 变化）"
    : "触发路径（target 过滤，如 /workspaces/opencode）";
}
document.getElementById("ruleKind").addEventListener("change", ruleScheduleLabel);
ruleScheduleLabel();

/* ---------- 智能生成规则 ---------- */
async function generateRule() {
  const desc = document.getElementById("nlDesc").value.trim();
  const msg = document.getElementById("nlMsg");
  const hint = document.getElementById("nlHint");
  if (!desc) { show(msg, "请填写自动化需求描述"); return; }
  hint.textContent = "生成中…（可能需要数秒）";
  try {
    const res = await api("/api/rules/generate", { method: "POST", headers: hdr(), body: JSON.stringify({ description: desc }) });
    const data = await res.json();
    hint.textContent = "";
    if (!res.ok) {
      show(msg, data.error || "生成失败");
      return;
    }
    const d = data.draft || {};
    document.getElementById("nlName").value = d.name || "";
    document.getElementById("nlKind").value = d.kind || "cron";
    document.getElementById("nlSchedule").value = d.schedule || "";
    document.getElementById("nlDirectory").value = d.directory || "";
    document.getElementById("nlPrompt").value = d.prompt || "";
    document.getElementById("nlDraft").classList.remove("hidden");
    show(msg, "已生成草稿，请确认后创建", true);
  } catch (e) {
    hint.textContent = "";
    if (e.message !== "unauthorized") show(msg, "生成失败，请稍后重试");
  }
}
async function confirmNlRule() {
  const body = {
    name: document.getElementById("nlName").value.trim(),
    kind: document.getElementById("nlKind").value,
    schedule: document.getElementById("nlSchedule").value.trim(),
    directory: document.getElementById("nlDirectory").value.trim(),
    prompt: document.getElementById("nlPrompt").value.trim(),
    enabled: true,
  };
  const msg = document.getElementById("nlMsg");
  if (!body.name || !body.prompt) { show(msg, "名称和 Prompt 不能为空"); return; }
  const res = await api("/api/rules", { method: "POST", headers: hdr(), body: JSON.stringify(body) });
  const data = await res.json();
  if (!res.ok) { show(msg, data.error || "创建失败"); return; }
  show(msg, "规则已创建", true);
  resetNl();
  loadRules();
}
function resetNl() {
  document.getElementById("nlDesc").value = "";
  document.getElementById("nlDraft").classList.add("hidden");
  document.getElementById("nlMsg").textContent = "";
  document.getElementById("nlHint").textContent = "";
}

async function createRule() {
  const body = {
    name: document.getElementById("ruleName").value.trim(),
    kind: document.getElementById("ruleKind").value,
    schedule: document.getElementById("ruleSchedule").value.trim(),
    directory: document.getElementById("ruleDirectory").value.trim(),
    sessionId: document.getElementById("ruleSessionId").value.trim(),
    prompt: document.getElementById("rulePrompt").value.trim(),
    enabled: true,
  };
  if (!body.name || !body.prompt) { show(document.getElementById("ruleMsg"), "请填写名称和 Prompt"); return; }
  const res = await api("/api/rules", { method: "POST", headers: hdr(), body: JSON.stringify(body) });
  const data = await res.json();
  if (!res.ok) { show(document.getElementById("ruleMsg"), data.error || "创建失败"); return; }
  show(document.getElementById("ruleMsg"), "规则已创建", true);
  document.getElementById("ruleName").value = ""; document.getElementById("rulePrompt").value = "";
  document.getElementById("ruleSessionId").value = "";
  loadRules();
}
async function loadRules() {
  const res = await api("/api/rules", { headers: hdr() });
  const data = await res.json();
  if (!res.ok) return;
  const list = data.rules || [];
  const tb = document.querySelector("#ruleTable tbody");
  tb.innerHTML = "";
  for (const r of list) {
    tb.insertAdjacentHTML("beforeend", `<tr data-rule="${escapeHtml(r.id)}">
      <td>${escapeHtml(r.name)}</td>
      <td>${KIND_LABEL[r.kind] || r.kind}</td>
      <td class="clip mono" title="${escapeHtml(r.schedule)}">${escapeHtml(r.schedule) || "-"}</td>
      <td class="clip" title="${escapeHtml(r.prompt)}">${escapeHtml(r.prompt)}</td>
      <td class="mono clip" title="${escapeHtml(r.sessionId || "")}">${escapeHtml(r.sessionId) || "自动新建"}</td>
      <td><span class="badge ${r.enabled ? "enabled" : "disabled"}">${r.enabled ? "启用" : "停用"}</span></td>
      <td>${fmtDate(r.lastFiredAt)}</td>
      <td id="exec-count-${escapeHtml(r.id)}">…</td>
      <td><div class="actions"><button class="danger sm" data-del-rule="${escapeHtml(r.id)}">删除</button></div></td>
    </tr>`);
    loadRuleExecCount(r.id);
  }
  tb.onclick = (e) => {
    const btn = e.target.closest("button[data-del-rule]");
    if (btn) deleteRule(btn.dataset.delRule);
  };
}
async function loadRuleExecCount(id) {
  try {
    const res = await api(`/api/rules/${encodeURIComponent(id)}/executions`, { headers: hdr() });
    const data = await res.json();
    const el = document.getElementById("exec-count-" + id);
    if (el) el.textContent = data.total ?? 0;
  } catch (_) {}
}
async function deleteRule(id) {
  await api("/api/rules/" + encodeURIComponent(id), { method: "DELETE", headers: hdr() });
  loadRules();
}

/* ---------- 归档 ---------- */
// 加载本机 OpenCode 会话列表到归档下拉框（按标题+ID 展示，便于选择）。
async function loadArchiveSessions() {
  const sel = document.getElementById("archiveSessionId");
  if (!sel || sel.dataset.loaded) return;
  sel.dataset.loaded = "1";
  sel.innerHTML = '<option value="">加载中…</option>';
  try {
    const res = await api("/api/opencode/session", { headers: appHeaders() });
    const data = await res.json();
    const items = (Array.isArray(data) ? data : (data.sessions || data.data || [])).slice(0, 200);
    sel.innerHTML = "";
    if (!items.length) {
      sel.innerHTML = '<option value="">无可用会话</option>';
      return;
    }
    for (const it of items) {
      const id = it.id || "";
      const label = (it.title || it.slug || id).toString().slice(0, 60);
      sel.add(new Option(label + "  ·  " + id.slice(0, 20), id));
    }
  } catch (_) {
    sel.innerHTML = '<option value="">会话列表加载失败</option>';
  }
}
async function createArchive() {
  const sessionId = document.getElementById("archiveSessionId").value.trim();
  const format = document.getElementById("archiveFormat").value;
  if (!sessionId) { show(document.getElementById("archiveMsg"), "请填写会话 ID"); return; }
  const res = await api("/api/archives", { method: "POST", headers: appHeaders(), body: JSON.stringify({ sessionId, format }) });
  const data = await res.json();
  if (!res.ok) { show(document.getElementById("archiveMsg"), data.error || "归档失败"); return; }
  show(document.getElementById("archiveMsg"), `已归档（${data.size} 字节）`, true);
  loadArchives();
}
async function loadArchives() {
  const res = await api("/api/archives", { headers: appHeaders() });
  const data = await res.json();
  if (!res.ok) return;
  const list = data.archives || [];
  const tb = document.querySelector("#archiveTable tbody");
  tb.innerHTML = "";
  for (const a of list) {
    tb.insertAdjacentHTML("beforeend", `<tr>
      <td class="mono clip" title="${escapeHtml(a.id)}">${escapeHtml(a.id)}</td>
      <td class="mono clip">${escapeHtml(a.sessionId)}</td>
      <td><span class="badge">${escapeHtml(a.format)}</span></td>
      <td>${a.size}</td>
      <td>${fmtDate(a.createdAt)}</td>
      <td>
        <div class="actions">
          <button class="ghost sm" data-view-archive="${escapeHtml(a.id)}">查看</button>
          <button class="ghost sm" data-download-archive="${escapeHtml(a.id)}" data-fmt="${escapeHtml(a.format)}">下载</button>
          <button class="danger sm" data-del-archive="${escapeHtml(a.id)}">删除</button>
        </div>
      </td>
    </tr>`);
  }
  tb.onclick = (e) => {
    const vw = e.target.closest("button[data-view-archive]");
    if (vw) { viewArchive(vw.dataset.viewArchive); return; }
    const dl = e.target.closest("button[data-download-archive]");
    if (dl) { downloadArchive(dl.dataset.downloadArchive, dl.dataset.fmt); return; }
    const btn = e.target.closest("button[data-del-archive]");
    if (btn) deleteArchive(btn.dataset.delArchive);
  };
}
async function deleteArchive(id) {
  await api("/api/archives/" + encodeURIComponent(id), { method: "DELETE", headers: appHeaders() });
  loadArchives();
}
// viewArchive 在新标签页展示归档全文。
async function viewArchive(id) {
  const res = await api("/api/archives/" + encodeURIComponent(id), { headers: appHeaders() });
  const data = await res.json();
  if (!res.ok) { show(document.getElementById("archiveMsg"), data.error || "加载失败"); return; }
  const win = window.open("", "_blank");
  if (!win) { show(document.getElementById("archiveMsg"), "浏览器拦截了新窗口，请允许弹窗"); return; }
  const isJson = data.format === "json";
  win.document.write("<!DOCTYPE html><html><head><meta charset=utf-8><title>" + escapeHtml(data.title || data.id) + "</title></head><body><pre style='white-space:pre-wrap'>" + (isJson ? escapeHtml(JSON.stringify(JSON.parse(data.content || "[]"), null, 2)) : escapeHtml(data.content || "")) + "</pre></body></html>");
  win.document.close();
}
// downloadArchive 把归档全文下载为本地文件。
async function downloadArchive(id, fmt) {
  const res = await api("/api/archives/" + encodeURIComponent(id), { headers: appHeaders() });
  const data = await res.json();
  if (!res.ok) { show(document.getElementById("archiveMsg"), data.error || "下载失败"); return; }
  const ext = fmt === "json" ? "json" : "md";
  const blob = new Blob([data.content || ""], { type: fmt === "json" ? "application/json" : "text/markdown" });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = (data.id || "archive") + "." + ext;
  a.click();
  URL.revokeObjectURL(url);
}

/* ---------- 审计 ---------- */
async function loadAudit() {
  const tid = document.getElementById("auditTokenFilter").value.trim();
  const url = "/api/audit?limit=100" + (tid ? "&tokenId=" + encodeURIComponent(tid) : "");
  const res = await api(url, { headers: hdr() });
  const data = await res.json();
  if (!res.ok) return;
  const list = data.audit || [];
  const tb = document.querySelector("#auditTable tbody");
  tb.innerHTML = "";
  if (!list.length) {
    tb.insertAdjacentHTML("beforeend", `<tr><td colspan="5" class="muted">暂无审计记录</td></tr>`);
    return;
  }
  for (const a of list) {
    const cls = a.status >= 400 ? "danger" : (a.status >= 300 ? "warn" : "ok");
    tb.insertAdjacentHTML("beforeend", `<tr>
      <td>${fmtDate(a.createdAt)}</td>
      <td>${escapeHtml(a.tokenName || a.tokenId)}</td>
      <td class="mono">${escapeHtml(a.method)}</td>
      <td class="mono">${escapeHtml(a.path)}</td>
      <td><span class="badge ${cls}">${a.status}</span></td>
    </tr>`);
  }
}

/* ---------- Token ---------- */
async function loadTokens() {
  const res = await api("/api/tokens", { headers: hdr() });
  if (!res.ok) return;
  const tokens = await res.json();
  const tb = document.querySelector("#tokenTable tbody");
  tb.innerHTML = "";
  if (!tokens.length) {
    tb.insertAdjacentHTML("beforeend", `<tr><td colspan="6" class="muted">暂无 Token</td></tr>`);
    return;
  }
  for (const t of tokens) {
    const active = !t.revokedAt;
    tb.insertAdjacentHTML("beforeend", `<tr>
      <td>${escapeHtml(t.name || "(未命名)")}</td>
      <td class="mono clip" title="${escapeHtml(t.id)}">${escapeHtml(t.id)}</td>
      <td>${fmtDate(t.createdAt)}</td>
      <td>${fmtDate(t.lastUsed)}</td>
      <td><span class="badge ${active ? "active" : "revoked"}">${active ? "启用" : "已撤销"}</span></td>
      <td><div class="actions">${active ? `<button class="danger sm" data-revoke="${escapeHtml(t.id)}">撤销</button>` : ""}</div></td>
    </tr>`);
  }
  tb.onclick = (e) => {
    const btn = e.target.closest("button[data-revoke]");
    if (btn) doRevoke(btn.dataset.revoke);
  };
}
async function doCreateToken() {
  const name = document.getElementById("tokenName").value.trim();
  if (!name) { show(document.getElementById("tokenMsg"), "请填写设备名称"); return; }
  const res = await api("/api/tokens", { method: "POST", headers: hdr(), body: JSON.stringify({ name }) });
  const data = await res.json();
  if (!res.ok) { show(document.getElementById("tokenMsg"), data.error || "生成失败"); return; }
  document.getElementById("newTokenBox").classList.remove("hidden");
  document.getElementById("newToken").value = data.token;
  show(document.getElementById("tokenMsg"), "Token 已生成，仅显示一次", true);
  document.getElementById("tokenName").value = "";
  loadTokens();
}
async function doRevoke(id) {
  await api("/api/tokens/" + encodeURIComponent(id), { method: "DELETE", headers: hdr() });
  loadTokens();
}

/* ---------- 设置 ---------- */
async function loadLLMConfig() {
  try {
    const res = await api("/api/llm", { headers: hdr() });
    const data = await res.json();
    document.getElementById("llmUrl").value = data.url || "";
    document.getElementById("llmModel").value = data.model || "";
    document.getElementById("llmKey").placeholder = data.keySet ? "已设置（留空不修改）" : "API Key";
    document.getElementById("llmKey").value = "";
    const st = document.getElementById("llmStatus");
    st.textContent = data.enabled ? "已启用" : "未启用";
    st.style.color = data.enabled ? "var(--success)" : "var(--ink-subtle)";
  } catch (_) {}
}
async function saveLLMConfig() {
  const body = {
    url: document.getElementById("llmUrl").value.trim(),
    key: document.getElementById("llmKey").value,
    model: document.getElementById("llmModel").value.trim(),
  };
  const res = await api("/api/llm", { method: "POST", headers: hdr(), body: JSON.stringify(body) });
  const data = await res.json();
  if (!res.ok) { show(document.getElementById("llmMsg"), data.error || "保存失败"); return; }
  show(document.getElementById("llmMsg"), "已保存" + (data.enabled ? "（已启用）" : "（已禁用）"), true);
  loadLLMConfig();
}
async function loadEmbedConfig() {
  try {
    const res = await api("/api/embed", { headers: hdr() });
    const data = await res.json();
    document.getElementById("embedUrl").value = data.url || "";
    document.getElementById("embedModel").value = data.model || "";
    document.getElementById("embedKey").placeholder = data.keySet ? "已设置（留空不修改）" : "API Key";
    document.getElementById("embedKey").value = "";
    const st = document.getElementById("embedStatus");
    st.textContent = data.enabled ? "已启用" : "未启用";
    st.style.color = data.enabled ? "var(--success)" : "var(--ink-subtle)";
  } catch (_) {}
}
async function saveEmbedConfig() {
  const body = {
    url: document.getElementById("embedUrl").value.trim(),
    key: document.getElementById("embedKey").value,
    model: document.getElementById("embedModel").value.trim(),
  };
  const res = await api("/api/embed", { method: "POST", headers: hdr(), body: JSON.stringify(body) });
  const data = await res.json();
  if (!res.ok) { show(document.getElementById("embedMsg"), data.error || "保存失败"); return; }
  show(document.getElementById("embedMsg"), "已保存" + (data.enabled ? "（已启用）" : "（已禁用）"), true);
  loadEmbedConfig();
}
async function doChangePassword() {
  const cp = document.getElementById("curpw").value;
  const np = document.getElementById("newpw").value;
  const res = await api("/api/web/password", { method: "POST", headers: hdr(), body: JSON.stringify({ currentPassword: cp, newPassword: np }) });
  const data = await res.json();
  show(document.getElementById("pwMsg"), res.ok ? "密码已更新，其他设备的会话已失效" : (data.error || "修改失败"), res.ok);
  if (res.ok) { document.getElementById("curpw").value = ""; document.getElementById("newpw").value = ""; }
}

/* ---------- 自动刷新任务 ---------- */
setInterval(() => {
  if (!document.getElementById("taskAutoRefresh").checked) return;
  const page = document.querySelector(".page:not(.hidden)");
  if (page && page.id === "page-tasks" && session) { loadTasks(); loadTaskStats(); }
}, 4000);

/* ---------- AI 工作台轮询（事件 5s，状态 12s，对话跟随；WS 推送实时） ---------- */
setInterval(() => {
  const page = document.querySelector(".page:not(.hidden)");
  if (!page || page.id !== "page-workbench" || !session) return;
  loadWbEvents();
  if (Date.now() - wbLastListLoad > 12000) loadWorkbench();
  // 仅在选中会话活跃（处理中/提问/重试）时拉取面板，空闲时新消息不会有，
  // 可省掉每个标签页每 5s 一次的消息请求。
  if (wbSelected) {
    const it = wbItems.find(x => x.session.id === wbSelected);
    const live = !it || it.status === "busy" || it.status === "retry" || it.status === "question";
    if (live) refreshWbPanel(wbSelected);
  }
}, 5000);

/* ---------- 实时推送（任务 AI 摘要 / 状态） ---------- */
let taskWS = null;
function connectTaskWS() {
  const t = localStorage.getItem(APP_TOKEN_KEY) || "";
  // 优先用 web 登录会话（已登录一定有效）；未登录才回退 APP token。
  const cred = session ? "session=" + encodeURIComponent(session) : (t ? "token=" + encodeURIComponent(t) : "");
  if (!cred) return;
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  try {
    taskWS = new WebSocket(`${proto}//${location.host}/api/ws?${cred}`);
  } catch (_) { return; }
  taskWS.onmessage = (ev) => {
    let msg;
    try { msg = JSON.parse(ev.data); } catch (_) { return; }
    // AI 工作台会话事件（/api/events 实时推送）先于 task.* 处理。
    if (msg.type === "session.event" && msg.payload) {
      handleWbPush(msg.payload);
      return;
    }
    if (msg.type === "intel.run.event" && msg.payload && msg.payload.run) {
      refreshIntelRunsIfVisible();
      return;
    }
    if (!msg.payload) return;
    if (msg.type === "task.summary") {
      updateTaskAI(msg.payload.id, msg.payload.summary, false);
    } else if (msg.type === "task.failure") {
      updateTaskAI(msg.payload.id, msg.payload.analysis, true);
    } else if (msg.type === "task.event") {
      const p = msg.payload || {};
      const label = TASK_STATUS[p.status] || p.status || "";
      if (p.status === "blocked") {
        // 前置任务失败/取消：需要人工介入，弹通知并跳到任务页。
        toast("任务被阻塞：" + label, (p.reason ? "原因：" + p.reason : "") + (p.upstream ? "（前置 " + p.upstream + "）" : ""), "warn", 12000);
      } else if (p.status === "failed") {
        toast("任务失败：" + label, "", "crit", 12000);
      } else if (p.status === "queued" && (p.reason === "manual unblock" || p.upstream)) {
        const why = p.reason === "manual unblock" ? "已手动解阻" : "前置 " + p.upstream + " 已完成";
        toast("任务已重新排队", why, "info");
      }
      const page = document.querySelector(".page:not(.hidden)");
      if (page && page.id === "page-tasks" && session) loadTasks();
    }
  };
  taskWS.onclose = () => { setTimeout(connectTaskWS, 5000); };
}
function updateTaskAI(id, text, isFailure) {
  const tr = document.querySelector('#taskTable tr[data-task="' + id + '"]');
  if (!tr || tr.children.length < 7) return;
  const aiCell = tr.children[6];
  aiCell.textContent = text || "";
  aiCell.title = text || "";
  aiCell.className = isFailure ? "clip" : "clip muted";
  if (isFailure) aiCell.style.color = "var(--danger)";
}

/* ============================================================================
 * AI 工作台（大屏 3 栏）：左 会话列表 + 中 决策面板 + 右 实时动态
 * 增强：状态筛选 Tab、会话模型/Agent 信息、提问徽标、气泡式最近对话、Enter 快捷回复
 * ========================================================================== */
// 动态栏跳过的纯噪音事件：心跳 / 同步快照 / 逐 token 增量 / 会话时间戳刷新。
// message.updated 与 message.part.updated（工具/文件 part，后端已过滤入库）是有
// 动作语义的，必须保留；message.part.delta / removed 仍是逐 token 噪音。
const WB_HIGH_FREQ = new Set(["heartbeat", "server.heartbeat", "sync", "server.connected", "message.part.delta", "message.part.removed", "session.updated"]);
const WB_MAX_EVENTS = 50;
let wbSessions = [];
let wbStatuses = {};
let wbPending = {};
let wbItems = [];
let wbEvents = [];
let wbEventsCursor = null;
let wbSelected = null;
let wbPanelData = null;
let wbSending = false;
let wbLoading = false;
let wbLastListLoad = 0;
let wbFilterOption = "all";
// 会话书签（收藏）与标签分组（localStorage 本地持久化，对齐 App 书签/标签能力）。
let wbStars = new Set();
let wbSessTags = {};
// 标签筛选：null = 全部；"star" = 书签会话；"tag:<name>" = 按标签。
let wbTagFilter = null;
function wbLoadLocal() {
  try { wbStars = new Set(JSON.parse(localStorage.getItem("ocb_session_stars") || "[]")); } catch (_) { wbStars = new Set(); }
  try { const t = JSON.parse(localStorage.getItem("ocb_session_tags") || "{}"); wbSessTags = (t && typeof t === "object") ? t : {}; } catch (_) { wbSessTags = {}; }
}
function wbSaveStars() { try { localStorage.setItem("ocb_session_stars", JSON.stringify([...wbStars])); } catch (_) {} }
function wbSaveTags() { try { localStorage.setItem("ocb_session_tags", JSON.stringify(wbSessTags)); } catch (_) {} }
function wbAllTags() {
  const m = new Map();
  for (const sid of Object.keys(wbSessTags)) for (const t of (wbSessTags[sid] || [])) m.set(t, (m.get(t) || 0) + 1);
  return [...m.entries()].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]));
}
// 决策面板是否处于「改名」编辑态。
let wbRenaming = false;
// 会话是否正在压缩（压缩为同步长耗时操作，期间禁用操作按钮）。
let wbCompacting = false;
// 有未读新消息的会话集合（后端 session_unread 为准，Web/App 共享）。
let wbNewSet = new Set();
// 待授权操作：sessionId → [PermissionRequest{id, permission, patterns, always, tool}]。
let wbPermissions = {};
// 待决问题的作答暂存：wbQAnswers[requestId][questionIndex] = [selectedLabels...]。
// 单选且仅一问时点选即提交；其余情况（多选 / 一问以上）逐题暂存后整体提交，
// answers 数组按序对应 questions，未作答的题为空数组。
let wbQAnswers = {};
// 待授权 / 待决问题的回复在途集合，防止连点二次提交（第二次必 404 报错）。
const wbPermBusy = new Set();
const wbQBusy = new Set();
// 对话区控制器（SBChat.ChatView）与 SSE 订阅句柄；对话区独立于面板外壳，
// 外壳重绘不得触碰它的 DOM（逐 token 增量与 innerHTML 会互相覆盖）。
let wbChat = null;
let wbChatUnsub = null;
// 可选 Agent 列表（GET /agent 缓存）与每个会话用户选定的 agent。
let wbAgents = [];
let wbAgentChoice = {};
// 正在拉取并渲染的决策面板会话 id（同会话去重，避免 5s 轮询与 WS 推送重复拉取）。
let wbPanelFetching = null;
// 可选模型列表（GET /config/providers 缓存）；用于面板内切换会话模型。
let wbProviders = null;
let wbProvidersState = "idle"; // idle | loading | loaded | failed
// 防抖/去重：避免每次轮询或推送都把页面整块重绘，造成滚动跳动或输入被清空。
let wbListSig = "";
let wbEventsSig = "";
let wbPanelSig = "";
let wbListRenderTimer = null;
let wbPanelRefreshTimer = null;
let wbWorkbenchTimer = null;

function wbRank(st) { return st === "question" ? 0 : (st === "busy" || st === "retry") ? 1 : 2; }
function wbStatusLabel(st) { return { question: "提问中", busy: "处理中", retry: "重试中", idle: "空闲" }[st] || "空闲"; }

// 决策面板外壳输入快照：重绘前记住待决问题自定义输入的内容与焦点。
// 对话区的输入/滚动由 ChatView 自己托管，不在这条路径上（外壳重绘不会碰到它）。
function wbSnapshotPanel() {
  const snap = { q: {}, focused: null };
  document.querySelectorAll('#wbPanel input[id^="wbQInput_"]').forEach(inp => {
    snap.q[inp.id] = inp.value;
    if (document.activeElement === inp) snap.focused = inp.id;
  });
  return snap;
}
function wbRestorePanel(snap) {
  if (!snap) return;
  Object.keys(snap.q || {}).forEach(id => {
    const inp = document.getElementById(id);
    if (inp) inp.value = snap.q[id];
  });
  if (snap.focused) {
    const inp = document.getElementById(snap.focused);
    if (inp) { inp.focus(); try { inp.setSelectionRange(inp.value.length, inp.value.length); } catch (_) {} }
  }
}
// 重绘决策面板但不丢待决问题的作答输入（snapshot → render → restore）。
function wbRerenderPanel() {
  const snap = wbSnapshotPanel();
  renderWbPanel();
  wbRestorePanel(snap);
}
// 决策面板内容指纹：内容没变化就不重绘（也不重复拉接口）。
// 注意：状态必须取「当前」wbItems 的实时状态（而不是 wbPanelData 里的旧值），
// 否则 WS 推送会话状态变化时指纹不变、面板徽标停留在旧的空闲/处理中。
function wbPanelSigFor(d) {
  if (!d) return "";
  const it = wbItems.find(x => x.session.id === d.id);
  const st = it ? it.status : d.status;
  const s = d.session || {};
  const model = (s.model && s.model.id) || "";
  const share = (s.share && s.share.url) ? 1 : 0;
  return [d.id, st, model, share, s.title || s.slug || "", s.agent || wbAgentChoice[d.id] || "",
    (d.pending || []).map(q => q.id).join(","),
    (d.permissions || []).map(p => p.id).join(","),
  ].join("\u0001");
}

/* ---- 对话区（SBChat.ChatView）装配 ---- */
// 对话区一次性挂在 #wbChat 上：外壳（标题栏 / 待决卡片）重绘不得销毁该节点，
// 否则逐 token 增量与输入焦点会被打断。
function wbEnsureChat() {
  const host = document.getElementById("wbChat");
  if (!host || !window.SBChat) return null;
  if (wbChat && wbChat.host === host) return wbChat;
  wbChat = new SBChat.ChatView();
  wbChat.mount(host, wbChatCtx());
  wbSyncStream();
  return wbChat;
}
// SSE 通道只在「停在工作台 + 已打开会话」时保持：/api/stream 后端有并发上限，
// 切页 / 关面板立即退订，避免长期占用一条上游连接。
function wbStreamCred() {
  if (session) return "session=" + encodeURIComponent(session);
  const t = localStorage.getItem(APP_TOKEN_KEY) || "";
  return t ? "token=" + encodeURIComponent(t) : "";
}
function wbSyncStream() {
  const page = document.getElementById("page-workbench");
  const want = !!window.SBChat && !!wbSelected && !!wbChat && page && !page.classList.contains("hidden");
  if (want && !wbChatUnsub) {
    SBChat.Bus.cred = wbStreamCred;
    wbChatUnsub = SBChat.Bus.subscribe(wbOnStream);
  } else if (!want && wbChatUnsub) {
    wbChatUnsub();
    wbChatUnsub = null;
  }
}
// SSE 事件入口：先交给对话区做增量渲染，再补列表/未读所需的状态副作用。
function wbOnStream(obj) {
  if (!wbChat) return;
  try { wbChat.onEvent(obj); } catch (_) { /* 单条事件异常不应断流 */ }
}
function wbChatCtx() {
  const sid = () => wbSelected;
  const curSession = () => {
    const it = wbItems.find(x => x.session.id === wbSelected);
    return (it && it.session) || (wbPanelData && wbPanelData.session) || {};
  };
  return {
    api,
    toast,
    dirHeaders: () => wbDirHeaders(wbCurrentDir()),
    session: curSession,
    isBusy: () => {
      const it = wbItems.find(x => x.session.id === wbSelected);
      const st = it ? it.status : (wbPanelData && wbPanelData.status) || "idle";
      return st === "busy" || st === "retry";
    },
    isSending: () => wbSending,
    confirm: (title, body) => window.confirm(title + "\n" + (body || "")),
    copyText: (text) => wbCopyText(text, "已复制到剪贴板"),
    openSession: (id) => openWbPanel(id),
    agents: () => wbAgents.map(a => a.name),
    modelOptions: () => wbModelOptions(),
    contextBudget: () => wbContextBudget(),
    send: (body) => wbSendPrompt(body),
    sent: () => { wbStatuses[sid()] = { type: "busy" }; wbScheduleRenderList(); if (wbChat) wbChat.syncSendIcon(); },
    abort: () => wbAbortWbSession(sid()),
    command: (name, args) => wbRunWbCommand(sid(), name, args),
    revert: (turn) => wbRevertTo(turn),
    regenerate: (turn) => wbRegenerate(turn),
    switchModel: (value) => wbModelChangeValue(value),
    switchAgent: (name) => wbAgentChange(name),
    findFiles: (q) => wbFindFiles(q),
    listCommands: () => wbListCommands(),
    recognize: (onText) => wbRecognize(onText),
    statusChanged: (st) => {
      const id = sid();
      if (!id) return;
      wbStatuses[id] = { type: st === "retry" ? "retry" : (st === "busy" ? "busy" : "idle") };
      wbScheduleRenderList();
      // 忙碌/空闲切换后同步发送/停止按钮图标与禁用态。
      if (wbChat && wbChat.sessionId === id) wbChat.syncSendIcon();
    },
  };
}
// 上下文预算：取当前会话模型在 /config/providers 中声明的 context 上限。
function wbContextBudget() {
  const s = (wbPanelData && wbPanelData.session) || {};
  const mid = modelId(s.model);
  const pid = (s.model && s.model.providerID) || "";
  for (const p of (wbProviders || [])) {
    if (pid && p.id !== pid) continue;
    const m = (p.models || []).find(x => x.id === mid);
    if (m && m.context) return m.context;
  }
  return 0;
}
async function wbFindFiles(q) {
  const dir = wbCurrentDir();
  const url = `/api/opencode/find/file?query=${encodeURIComponent(q)}`
    + (dir ? "&directory=" + encodeURIComponent(dir) : "");
  const res = await api(url, { headers: appHeaders() });
  if (!res.ok) return [];
  const data = await res.json().catch(() => []);
  return (Array.isArray(data) ? data : []).filter(x => typeof x === "string");
}
let wbCommandsCache = null;
async function wbListCommands() {
  if (wbCommandsCache) return wbCommandsCache;
  const res = await api("/api/opencode/command", { headers: appHeaders() }).catch(() => null);
  if (!res || !res.ok) return [];
  const data = await res.json().catch(() => []);
  wbCommandsCache = (Array.isArray(data) ? data : []).filter(c => c && c.name);
  return wbCommandsCache;
}
// 语音输入：对齐 App ServerAsrRecorder —— 16kHz 单声道 PCM16LE 裸字节，
// 200ms（3200 样本）一片推到 /api/stt/sessions/{id}/chunks，响应体即累积文本，
// 松手后 /finish 取终稿、再交 /refine 用大模型校对（后端永不失败，兜底本地规则）。
// 注意：整棵 /api/stt 只认 Authorization: Bearer（requireToken），web session 无效。
const STT_RATE = 16000;
const STT_CHUNK_SAMPLES = 3200;
const STT_MAX_FAILURES = 3;

async function wbRecognize(onText) {
  if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) throw new Error("浏览器不支持录音");
  const token = localStorage.getItem(APP_TOKEN_KEY) || "";
  if (!token) throw new Error("未配置 APP Token，无法使用服务端识别");
  const bearer = { "Content-Type": "application/json", "Authorization": "Bearer " + token };
  const octet = { "Content-Type": "application/octet-stream", "Authorization": "Bearer " + token };

  const csRes = await fetch("/api/stt/sessions", { method: "POST", headers: bearer });
  if (csRes.status === 503) throw new Error("后端未配置语音识别引擎");
  if (!csRes.ok) throw new Error("识别会话创建失败 (" + csRes.status + ")");
  const sid = ((await csRes.json().catch(() => ({}))) || {}).session_id || "";
  if (!sid) throw new Error("识别会话创建失败");

  const stream = await navigator.mediaDevices.getUserMedia({
    audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true },
  });
  const AC = window.AudioContext || window.webkitAudioContext;
  if (!AC) { stream.getTracks().forEach(t => t.stop()); throw new Error("浏览器不支持音频采集"); }
  let ctx = null;
  try { ctx = new AC({ sampleRate: STT_RATE }); } catch (_) { ctx = new AC(); }
  const ratio = ctx.sampleRate / STT_RATE;

  let tail = [];            // 重采样后未满一片的样本，留待下一次
  let failures = 0;
  let stopped = false;
  let lastText = "";
  let chain = Promise.resolve();   // 分片严格串行：乱序会让引擎拼出颠倒的文本

  const sendChunk = (samples) => {
    const pcm = new Int16Array(samples);
    for (let i = 0; i < samples.length; i++) {
      const s = Math.max(-1, Math.min(1, samples[i]));
      pcm[i] = s < 0 ? s * 0x8000 : s * 0x7fff;
    }
    chain = chain.then(async () => {
      if (stopped) return;
      let res;
      try {
        res = await fetch("/api/stt/sessions/" + encodeURIComponent(sid) + "/chunks",
          { method: "POST", headers: octet, body: pcm.buffer });
      } catch (_) { res = null; }
      if (!res || !res.ok) {
        if (++failures >= STT_MAX_FAILURES) { stopped = true; toast("识别中断", "语音识别请求连续失败", "crit"); }
        return;
      }
      failures = 0;
      const d = await res.json().catch(() => ({}));
      const t = String(d.text || "").trim();
      if (t && t !== lastText) { lastText = t; onText(t, false); }
    });
  };

  const src = ctx.createMediaStreamSource(stream);
  // ScriptProcessor 虽已标记废弃，但仍是唯一无需额外 worklet 文件的可行方案。
  const proc = ctx.createScriptProcessor(4096, 1, 1);
  let pos = 0;              // 上次读取的整数样本位，保证重采样相位连续
  proc.onaudioprocess = (e) => {
    if (stopped) return;
    const input = e.inputBuffer.getChannelData(0);
    const out = [];
    let p = pos;
    for (; p < input.length; p += ratio) {
      const i = Math.floor(p), f = p - i;
      const a = input[i];
      const b = i + 1 < input.length ? input[i + 1] : a;
      out.push(a + (b - a) * f);
    }
    pos = p - input.length;   // 落在下一段缓冲里的相位
    tail = tail.concat(out);
    while (tail.length >= STT_CHUNK_SAMPLES) {
      sendChunk(tail.slice(0, STT_CHUNK_SAMPLES));
      tail = tail.slice(STT_CHUNK_SAMPLES);
    }
  };
  src.connect(proc); proc.connect(ctx.destination);

  const teardown = async () => {
    stopped = true;
    try { proc.disconnect(); src.disconnect(); } catch (_) {}
    stream.getTracks().forEach(t => t.stop());
    try { await ctx.close(); } catch (_) {}
  };

  return {
    stop: async () => {
      if (tail.length) { sendChunk(tail); tail = []; }
      await chain;                       // 等最后一片落账，否则 finish 会丢尾部
      const wasBroken = failures >= STT_MAX_FAILURES;
      await teardown();
      if (wasBroken) return;
      try {
        const fr = await fetch("/api/stt/sessions/" + encodeURIComponent(sid) + "/finish",
          { method: "POST", headers: bearer });
        if (!fr.ok) throw new Error("finish " + fr.status);
        let text = String(((await fr.json().catch(() => ({}))) || {}).text || "").trim();
        if (!text) { toast("未识别到内容", "请靠近麦克风再试一次", "warn"); return; }
        onText(text, true);
        // 流式识别常有重字与同音错字，交给大模型就地校对后替换（与 App 一致）。
        const rr = await fetch("/api/stt/refine", { method: "POST", headers: bearer, body: JSON.stringify({ text }) });
        if (rr.ok) {
          const rd = await rr.json().catch(() => ({}));
          const polished = String(rd.text || "").trim();
          if (polished && polished !== text) onText(polished, true);
        }
      } catch (e) {
        toast("识别失败", String(e && e.message || e), "crit");
      }
    },
    cancel: async () => {
      await teardown();
      fetch("/api/stt/sessions/" + encodeURIComponent(sid), { method: "DELETE", headers: bearer }).catch(() => {});
    },
  };
}

// 拉取可切换的 provider + 模型列表（懒加载并缓存；失败只尝试一次，避免每次刷新重复打接口）。
async function ensureWbProviders(force) {
  if (wbProviders && !force) return wbProviders;
  // 已加载 / 正在加载时不重复请求；失败态允许重试（每次 refreshWbPanel 轮询都会重试）。
  if (!force && (wbProvidersState === "loading" || wbProvidersState === "loaded")) return wbProviders;
  wbProvidersState = "loading";
  try {
    const res = await api("/api/opencode/config/providers", { headers: appHeaders() });
    if (!res.ok) { wbProvidersState = "failed"; if (wbChat) wbChat.renderSelectors(); return wbProviders; }
    const d = await res.json();
    const list = (d.providers || []).map(p => ({
      id: p.id,
      name: p.name || p.id,
      models: Object.values(p.models || {}).map(m => ({
        id: m.id,
        name: m.name || m.id,
        variant: m.variant || "default",
        // limit.context 决定上下文占用百分比；capabilities.attachment 决定能否带附件。
        context: (m.limit && m.limit.context) || 0,
        attachment: !!(m.capabilities && m.capabilities.attachment),
      })),
    })).filter(p => p.models.length);
    wbProviders = list;
    wbProvidersState = "loaded";
    return list;
  } catch (_) {
    wbProvidersState = "failed";
    return wbProviders;
  } finally {
    // 加载完成（无论成败）都重绘一次模型下拉：会话可能在加载完成前就打开了，
    // 否则下拉会一直停在「加载模型列表…」。
    if (wbChat) wbChat.renderSelectors();
  }
}
// 当前会话生效的模型选择值（providerID␁modelID␁variant），供下拉回填比对。
function wbSessionModelValue() {
  const m = (wbPanelData && wbPanelData.session && wbPanelData.session.model) || {};
  if (!m.id) return "";
  return (m.providerID || "") + "\u0001" + m.id + "\u0001" + (m.variant || "default");
}
// 渲染「模型」下拉选项：按 provider 分组并选中当前会话模型；
// 当前模型不在列表时额外补一项（否则下拉会假装选中了第一个模型）。
function wbModelOptions() {
  const list = wbProviders;
  // disabled 且无 selected 时浏览器会把下拉渲染成空白，用户看不到「正在加载」提示。
  if (!list) return `<option value="" disabled selected>${
    wbProvidersState === "failed" ? "模型列表加载失败，自动重试中…" : "加载模型列表…"}</option>`;
  const want = wbSessionModelValue();
  let html = "";
  let found = false;
  for (const p of list) {
    const opts = p.models.map(m => {
      const v = p.id + "\u0001" + m.id + "\u0001" + m.variant;
      if (v === want) found = true;
      return `<option value="${escapeHtml(v)}"${v === want ? " selected" : ""}>${escapeHtml(m.name + " (" + p.id + ")")}</option>`;
    }).join("");
    html += `<optgroup label="${escapeHtml(p.name)}">${opts}</optgroup>`;
  }
  if (!found && want) {
    const seg = want.split("\u0001");
    html = `<option value="${escapeHtml(want)}" selected>${escapeHtml(seg[1] + "（当前，未在列表）")}</option>` + html;
  }
  return html;
}
// 模型下拉 change：POST /api/session/{id}/model 切换并刷新面板。
// body 为扁平 V2ModelRef（providerID/modelID/variant），与 App switchSessionModelV2 一致；
// directory 走 query 参数。
async function wbModelChangeValue(v) {
  const id = wbSelected;
  if (!id || !v) return;
  const [providerID, modelID, variant] = v.split("\u0001");
  if (!providerID || !modelID) return;
  const dir = wbCurrentDir();
  const url = `/api/opencode/api/session/${encodeURIComponent(id)}/model` + (dir ? "?directory=" + encodeURIComponent(dir) : "");
  const body = JSON.stringify({ model: { id: modelID, providerID, variant: variant || "default" } });
  const res = await api(url, { method: "POST", headers: appHeaders(), body }).catch(() => null);
  if (!res || !res.ok) {
    toast("切换失败", `模型切换未生效 (${res ? res.status : "网络错误"})`, "crit");
    if (wbChat) wbChat.refreshSelectors();
    loadWorkbench();
    return;
  }
  toast("已切换", `已切换到 ${modelID}`, "info");
  const s = (wbItems.find(x => x.session.id === id) || {}).session;
  if (s) s.model = { id: modelID, providerID, variant: variant || "default" };
  if (wbPanelData && wbPanelData.session) {
    wbPanelData.session.model = { id: modelID, providerID, variant: variant || "default" };
  }
  if (wbChat) wbChat.refreshSelectors();
  wbRerenderPanel();
}
// Agent 切换：POST /api/session/{id}/agent（App switchSessionAgentV2 同契约）。
// 失败时仅记到本地选择（下一次 prompt 仍会带上 agent），不把下拉重置回错误值。
async function wbAgentChange(name) {
  const id = wbSelected;
  if (!id || !name) return;
  wbAgentChoice[id] = name;
  const dir = wbCurrentDir();
  const url = `/api/opencode/api/session/${encodeURIComponent(id)}/agent` + (dir ? "?directory=" + encodeURIComponent(dir) : "");
  const res = await api(url, { method: "POST", headers: appHeaders(), body: JSON.stringify({ agent: name }) }).catch(() => null);
  if (!res || !res.ok) {
    toast("切换未生效", `Agent ${name} 未能切换（发送时仍会带上）`, "warn");
    return;
  }
  const s = (wbItems.find(x => x.session.id === id) || {}).session;
  if (s) s.agent = name;
  if (wbPanelData && wbPanelData.session) wbPanelData.session.agent = name;
  wbRerenderPanel();
}
// 可选 Agent 列表（GET /agent），只保留 primary/非隐藏（同 App 模式选择器口径）。
async function ensureWbAgents() {
  if (wbAgents.length) return wbAgents;
  const res = await api("/api/opencode/agent", { headers: appHeaders() }).catch(() => null);
  if (!res || !res.ok) return [];
  const data = await res.json().catch(() => []);
  wbAgents = (Array.isArray(data) ? data : []).filter(a => a && a.name && !a.hidden && a.mode !== "subagent");
  return wbAgents;
}

// 归并子会话的忙/待决问题/待授权操作到父会话（口径与 App 工作台一致）。
function buildWbItems() {
  const childBusy = {};
  const parentQ = new Set();
  const parentPending = {};
  const parentPerm = {};
  for (const s of wbSessions) {
    const pid = s.parentID;
    if (!pid) continue;
    const pq = wbPending[s.id];
    if (pq && pq.length) {
      parentQ.add(pid);
      (parentPending[pid] = parentPending[pid] || []).push(...pq);
    }
    const pps = wbPermissions[s.id];
    if (pps && pps.length) (parentPerm[pid] = parentPerm[pid] || []).push(...pps);
    const st = wbStatuses[s.id];
    if (st && (st.type === "busy" || st.type === "retry")) childBusy[pid] = st.type;
  }
  const roots = wbSessions.filter(s => !s.parentID && !(s.time && s.time.archived));
  return roots.map(s => {
    const self = wbStatuses[s.id];
    const hasQ = (wbPending[s.id] && wbPending[s.id].length) || parentQ.has(s.id);
    const perms = [...(wbPermissions[s.id] || []), ...(parentPerm[s.id] || [])];
    const pending = [...(wbPending[s.id] || []), ...(parentPending[s.id] || [])];
    let st;
    if (hasQ || (perms && perms.length)) st = "question";
    else if (self && (self.type === "busy" || self.type === "retry")) st = self.type;
    else st = childBusy[s.id] || (self && self.type) || "idle";
    return { session: s, status: st, pending, permissions: perms };
  }).sort((a, b) => {
    const r = wbRank(a.status) - wbRank(b.status);
    if (r) return r;
    const ta = (a.session.time && a.session.time.updated) || 0;
    const tb = (b.session.time && b.session.time.updated) || 0;
    if (ta !== tb) return tb - ta;
    return a.session.id < b.session.id ? 1 : -1;
  });
}

// 状态筛选 Tab：全部 / 提问中 / 处理中 / 空闲。
function renderWbFilters() {
  const box = document.getElementById("wbFilters");
  if (!box) return;
  const opts = [
    ["all", "全部"],
    ["question", "提问中"],
    ["busy", "处理中"],
    ["idle", "空闲"],
  ];
  box.innerHTML = opts.map(([k, l]) =>
    `<button class="wb-filter ${wbFilterOption === k ? "active" : ""}" data-f="${k}">${l}</button>`).join("");
  box.onclick = (e) => {
    const btn = e.target.closest("button[data-f]");
    if (!btn) return;
    wbFilterOption = btn.dataset.f;
    renderWbFilters();
    renderWbList();
  };
}

// 书签 / 标签筛选 Tab：全部 / ⭐书签 / 各标签（点标签直接跳转到该分组）。
function wbRenderTagFilters() {
  const box = document.getElementById("wbTagFilters");
  if (!box) return;
  const chips = [["", "全部"]];
  if (wbStars.size) chips.push(["star", `⭐ 书签 ${wbStars.size}`]);
  const allTags = wbAllTags();
  for (const [t, n] of allTags) chips.push(["tag:" + t, `#${t} ${n}`]);
  box.innerHTML = chips.map(([k, l]) =>
    `<button class="wb-tagfilter ${wbTagFilter === k ? "active" : ""}" data-tf="${escapeHtml(k)}">${escapeHtml(l)}</button>`).join("");
  box.onclick = (e) => {
    const btn = e.target.closest("button[data-tf]");
    if (!btn) return;
    wbTagFilter = btn.dataset.tf || null;
    wbRenderTagFilters();
    renderWbList();
  };
}
// 会话标签编辑弹窗（对齐 App 书签 ManageTagsDialog）：输入新标签回车/按钮添加，点标签移除。
function wbEditSessionTags(sid) {
  const s = wbSessions.find(x => x.id === sid) || {};
  const title = (s.title || s.slug || sid).toString();
  const tags = wbSessTags[sid] || [];
  const tagRow = (t) => `<span class="wb-edit-tag" data-rm-sess-tag="${escapeHtml(sid)}|${escapeHtml(t)}">#${escapeHtml(t)} ×</span>`;
  wbModalOpen(`会话标签 · ${title}`,
    `<div class="wb-tag-edit">
      <div class="muted" style="font-size:12px;margin-bottom:8px">给会话打标签，可用于列表分组筛选；点击标签可移除。</div>
      <div class="wb-edit-tags" id="wbEditTags">${tags.length ? tags.map(tagRow).join("") : `<span class="muted" style="font-size:12px">暂无标签</span>`}</div>
      <div class="row" style="gap:6px;margin-top:10px">
        <input type="text" id="wbTagInput" placeholder="输入标签后回车，如：发布" style="flex:1" onkeydown="if(event.key==='Enter'){wbAddSessionTag('${escapeHtml(sid)}');}">
        <button class="ghost" onclick="wbAddSessionTag('${escapeHtml(sid)}')">添加</button>
      </div>
    </div>`);
  const body = document.getElementById("wbModalBody");
  body.onclick = (e) => {
    const rm = e.target.closest("[data-rm-sess-tag]");
    if (!rm) return;
    const [rid, rtag] = rm.dataset.rmSessTag.split("|");
    wbSessTags[rid] = (wbSessTags[rid] || []).filter(t => t !== rtag);
    if (!wbSessTags[rid].length) delete wbSessTags[rid];
    wbSaveTags();
    wbRenderTagFilters();
    wbScheduleRenderList();
    wbEditSessionTags(rid);
  };
  const inp = document.getElementById("wbTagInput");
  if (inp) inp.focus();
}
function wbAddSessionTag(sid) {
  const inp = document.getElementById("wbTagInput");
  // 只保留中文/字母/数字/下划线/连字符/点/空格，去除会破坏 data 属性与 URL 的字符。
  let tag = (inp && inp.value || "").trim().replace(/^#/, "").replace(/[^\w\u4e00-\u9fa5\-. ]+/g, "-").replace(/-+/g, "-").trim();
  if (!tag) return;
  const cur = wbSessTags[sid] || [];
  if (!cur.includes(tag)) cur.push(tag);
  wbSessTags[sid] = cur;
  if (inp) inp.value = "";
  wbSaveTags();
  wbRenderTagFilters();
  wbScheduleRenderList();
  wbEditSessionTags(sid);
}

function refreshWbStats() {
  const items = wbItems.length ? wbItems : buildWbItems();
  let q = 0, b = 0, i = 0;
  for (const it of items) {
    if (it.status === "question") q++;
    else if (it.status === "busy" || it.status === "retry") b++;
    else i++;
  }
  document.getElementById("wbCountQ").textContent = q;
  document.getElementById("wbCountB").textContent = b;
  document.getElementById("wbCountI").textContent = i;
  document.getElementById("wbCountT").textContent = items.length;
  document.getElementById("wbCountE").textContent = wbEvents.length;
}

// 新建会话：展开内联表单，目录支持从已有会话目录选择（datalist）。
function wbToggleNew() {
  const f = document.getElementById("wbNewForm");
  if (!f) return;
  f.classList.toggle("hidden");
  if (!f.classList.contains("hidden")) {
    const dirs = [...new Set(wbSessions.map(s => s.directory).filter(Boolean))];
    document.getElementById("wbDirList").innerHTML = dirs.map(d => `<option value="${escapeHtml(d)}">`).join("");
    show(document.getElementById("wbNewMsg"), "");
  }
}
async function wbCreateSession() {
  const dir = document.getElementById("wbNewDir").value.trim();
  const title = document.getElementById("wbNewTitle").value.trim();
  const msg = document.getElementById("wbNewMsg");
  if (!dir) { show(msg, "请填写工作目录"); return; }
  const body = {};
  if (title) body.title = title;
  // 工作目录走 query 参数（opencode 只认 directory query / x-opencode-directory 头），
  // 否则新会话回退到 process.cwd()，落错路径。
  const url = `/api/opencode/session?directory=${encodeURIComponent(dir)}&x-opencode-directory=${encodeURIComponent(dir)}`;
  const headers = appHeaders();
  headers["x-starburst-directory"] = dir;
  headers["x-opencode-directory"] = dir;
  const res = await api(url, { method: "POST", headers, body: JSON.stringify(body) });
  if (!res.ok) { show(msg, "创建失败 (" + res.status + ")"); return; }
  const data = await res.json();
  toast("已创建会话", data.id, "info");
  show(msg, "已创建 " + data.id, true);
  document.getElementById("wbNewTitle").value = "";
  document.getElementById("wbNewDir").value = "";
  wbToggleNew();
  await loadWorkbench();
  if (data && data.id) openWbPanel(data.id);
}

function wbItemHtml(it) {
  const s = it.session;
  const id = s.id;
  const title = (s.title || s.slug || id).toString();
  const dir = (s.directory || "").toString();
  const model = modelId(s.model);
  const metaParts = [];
  if (model) metaParts.push(`<span class="model" title="会话模型">${escapeHtml(model)}</span>`);
  if (s.time && s.time.updated) metaParts.push(escapeHtml(new Date(s.time.updated).toLocaleTimeString()));
  const q = (it.pending || []).length;
  const pc = (it.permissions || []).length;
  const active = wbSelected === id ? "active" : "";
  const wrap = (q ? `<span class="qbadge">提问 ${q}</span>` : "") +
    (pc ? `<span class="qbadge" title="待授权操作">授权 ${pc}</span>` : "");
  const newDot = wbNewSet.has(id) ? `<span class="newdot" title="有新消息"></span>` : "";
  const starred = wbStars.has(id);
  const tags = (wbSessTags[id] || []);
  const tagsHtml = tags.length
    ? `<div class="wb-tags">${tags.map(t => `<span class="wb-tag" data-tag-jump="${escapeHtml(t)}">#${escapeHtml(t)}</span>`).join("")}
        <button class="wb-tag-add" type="button" data-tag-edit="${escapeHtml(id)}" title="管理标签">＋</button></div>`
    : `<div class="wb-tags"><button class="wb-tag-add" type="button" data-tag-edit="${escapeHtml(id)}" title="添加标签">＋标签</button></div>`;
  return `<div class="wb-item ${active}" data-sid="${escapeHtml(id)}">
    <div class="t"><button class="wb-star${starred ? " on" : ""}" type="button" data-star="${escapeHtml(id)}" title="${starred ? "取消收藏" : "收藏会话"}" aria-label="收藏">${starred ? "★" : "☆"}</button><span class="dot ${escapeHtml(it.status)}"></span><span class="ttl">${escapeHtml(title)}</span>${newDot}${wrap}</div>
    <div class="dir">${escapeHtml(dir) || "-"}</div>
    ${tagsHtml}
    ${metaParts.length ? `<div class="meta">${metaParts.join(" · ")}</div>` : ""}
  </div>`;
}

function renderWbList() {
  wbItems = buildWbItems();
  refreshWbStats();
  const box = document.getElementById("wbList");
  const kw = (document.getElementById("wbFilter").value || "").trim().toLowerCase();
  const filter = wbFilterOption;
  // 内容指纹：状态/排序/关键字/标签筛选/选中项都没变就不重建 DOM，避免滚动条跳动；
  // 选中项必须参与比对，否则点选其他会话时高亮不会更新。
  const sig = filter + "\u0001" + kw + "\u0001" + (wbTagFilter || "") + "\u0001" + (wbSelected || "") + "\u0001" + wbItems.map(it =>
    it.session.id + ":" + it.status + ":" + (it.pending || []).length + ":" + (it.permissions || []).length + ":" + (it.session.time && it.session.time.updated || 0) + ":M" + modelId(it.session.model) + ":U" + (wbNewSet.has(it.session.id) ? 1 : 0) + ":S" + (wbStars.has(it.session.id) ? 1 : 0) + ":T" + (wbSessTags[it.session.id] || []).join(",")
  ).join(",");
  if (sig === wbListSig) return;
  wbListSig = sig;
  const scroller = box.closest(".wb-body") || box;
  const prevScroll = scroller.scrollTop || 0;
  let matched = wbItems.filter(it => {
    if (filter === "question" && wbRank(it.status) !== 0) return false;
    if (filter === "busy" && wbRank(it.status) !== 1) return false;
    if (filter === "idle" && wbRank(it.status) !== 2) return false;
    if (wbTagFilter === "star" && !wbStars.has(it.session.id)) return false;
    if (wbTagFilter && wbTagFilter.indexOf("tag:") === 0 && !(wbSessTags[it.session.id] || []).includes(wbTagFilter.slice(4))) return false;
    return true;
  });
  if (kw) {
    matched = matched.filter(it => {
      const title = (it.session.title || it.session.slug || it.session.id).toString().toLowerCase();
      const dir = (it.session.directory || "").toLowerCase();
      return title.includes(kw) || dir.includes(kw);
    });
  }
  const groups = { question: [], busy: [], retry: [], idle: [] };
  for (const it of matched) {
    (groups[it.status] || groups.idle).push(it);
  }
  const labels = { question: "提问中", busy: "处理中", retry: "重试中", idle: "空闲" };
  let html = "";
  let shown = 0;
  for (const g of ["question", "busy", "retry", "idle"]) {
    const list = groups[g];
    if (!list.length) continue;
    html += `<div class="wb-group-title">${labels[g]}（${list.length}）</div>`;
    for (const it of list) { shown++; html += wbItemHtml(it); }
  }
  box.innerHTML = html || `<div class="wb-placeholder">无匹配会话<br><span class="muted" style="font-size:12px">调整关键词或筛选条件</span></div>`;
  scroller.scrollTop = prevScroll;
  document.getElementById("wbListHint").textContent = `共 ${shown} 个`;
  box.onclick = (e) => {
    const star = e.target.closest("[data-star]");
    if (star) {
      const sid = star.dataset.star;
      if (wbStars.has(sid)) wbStars.delete(sid); else wbStars.add(sid);
      wbSaveStars();
      wbRenderTagFilters();
      wbScheduleRenderList();
      return;
    }
    const te = e.target.closest("[data-tag-edit]");
    if (te) { wbEditSessionTags(te.dataset.tagEdit); return; }
    const tj = e.target.closest("[data-tag-jump]");
    if (tj) { wbTagFilter = "tag:" + tj.dataset.tagJump; wbRenderTagFilters(); renderWbList(); return; }
    const el = e.target.closest(".wb-item");
    if (el) openWbPanel(el.dataset.sid);
  };
}

async function loadWorkbench() {
  if (wbLoading) return;
  wbLoading = true;
  wbLastListLoad = Date.now();
  try {
    const [sr, stR] = await Promise.all([
      // 拉全量会话（含子会话）：待授权操作/待决问题可能挂在子会话上，需归并到根会话展示，
      // 且子会话目录也必须参与按目录查询，否则漏拉。不能用 /session（只返回当前目录、
      // 且抹平目录），用 /experimental/session 不带 roots 过滤（同 ListAllSessionsDetailed）。
      api("/api/opencode/experimental/session", { headers: appHeaders() }),
      api("/api/opencode/session/status", { headers: appHeaders() }),
    ]);
    if (!sr.ok) { toast("会话列表加载失败", "HTTP " + sr.status, "crit"); return; }
    const data = await sr.json();
    wbSessions = Array.isArray(data) ? data : (data.sessions || data.data || []);
    let st = {};
    if (stR.ok) { try { st = await stR.json(); } catch (_) {} }
    wbStatuses = st || {};
    // 待决问题接口必须按会话目录查询（全局 /question 恒为空），否则选项弹不出来。
    wbPending = {};
    const dirs = [...new Set(wbSessions.map(s => s.directory).filter(Boolean))];
    const qResps = await Promise.all(dirs.map(d =>
      api("/api/opencode/question?directory=" + encodeURIComponent(d), { headers: appHeaders() })
        .then(r => (r.ok ? r.json() : []))
        .catch(() => [])
    ));
    for (const qs of qResps) {
      for (const q of (Array.isArray(qs) ? qs : [])) {
        if (q && q.sessionID) (wbPending[q.sessionID] = wbPending[q.sessionID] || []).push(q);
      }
    }
    // 待授权操作：与 App listPendingPermissions 同一契约，全局可拉全量；
    // 再按目录补充（兼容部分上游版本目录参数才返回的旧行为），按 id 去重。
    wbPermissions = {};
    const permGlobal = await api("/api/opencode/permission", { headers: appHeaders() })
      .then(r => (r.ok ? r.json() : []))
      .catch(() => []);
    const permResps = await Promise.all(dirs.map(d =>
      api("/api/opencode/permission?directory=" + encodeURIComponent(d), { headers: appHeaders() })
        .then(r => (r.ok ? r.json() : []))
        .catch(() => [])
    ));
    const allPerms = [...(Array.isArray(permGlobal) ? permGlobal : [])];
    for (const ps of permResps) {
      if (Array.isArray(ps)) allPerms.push(...ps);
    }
    const permSeen = new Set();
    for (const p of allPerms) {
      if (!p || !p.sessionID || permSeen.has(p.id)) continue;
      permSeen.add(p.id);
      (wbPermissions[p.sessionID] = wbPermissions[p.sessionID] || []).push(p);
    }
    renderWbList();
    if (wbSelected) refreshWbPanel(wbSelected);
    // 同步后端未读集合（Web/App 共享的绿点权威状态）。
    syncWbUnread();
  } catch (e) {
    if (e.message !== "unauthorized") toast("加载失败", e.message || "未知错误", "crit");
  } finally {
    wbLoading = false;
  }
}

/* ---- 实时动态（事件流聚合） ---- */
function wbFilePath(obj, inner) {
  let f = obj.file || inner.file;
  if (typeof f === "string") return f;
  if (f && typeof f === "object") return f.filePath || "";
  return "";
}
function wbFallbackEvent(type) {
  if (!type) return "事件更新";
  if (type.includes("error") || type.includes("failed")) return "出错";
  if (type === "message.complete" || type.includes("idle")) return "处理完成";
  if (type.startsWith("question.")) return "有提问";
  if (type.startsWith("permission.")) return "需要授权";
  if (type.includes("busy")) return "处理中";
  if (type.startsWith("file.")) return "文件变动";
  if (type.startsWith("tool")) return "工具调用";
  if (type.startsWith("message.") || type.includes("assistant")) return "AI 回复";
  if (type.startsWith("user.") || type.includes("prompt")) return "用户指令";
  if (type.startsWith("project.")) return "项目事件";
  if (type.startsWith("session.created")) return "新建会话";
  if (type.includes("status")) return "状态更新";
  return "事件更新";
}
// 从 part 提取「具体动作」：工具/文件/推理/文本（message.part.updated / created 入库时用）。
function describePart(part) {
  if (!part || typeof part !== "object") return "";
  const t = part.type || "";
  const text = String(part.text || "").trim();
  if (t === "tool") {
    const st = part.state || {};
    const stt = String(st.status || st.type || "").toLowerCase();
    const budge = stt === "error" || stt === "failed" ? "工具失败" : stt === "completed" || stt === "success" ? "工具完成" : "运行工具";
    const target = (st.title && String(st.title).trim()) || (st.input && (st.input.command || st.input.filePath || st.input.path || st.input.url || st.input.query)) || part.tool || "工具";
    const targetText = String(target).replace(/\s+/g, " ").slice(0, 80);
    return `${budge}：${targetText}`;
  }
  if (t === "file") return "文件：" + String(part.filename || part.url || "").slice(0, 80);
  if (t === "reasoning") return text ? "推理：" + text.slice(0, 80) : "开始推理";
  if (text) return text.slice(0, 80);
  return "";
}
// 从 message.updated / message.complete 的 info 提取「模型/agent/token/结束原因」动作描述。
function finishLabel(f) {
  const m = { "tool-calls": "工具调用", "error": "出错", "reasoning": "推理完成", "length": "达到长度上限", "stop": "正常结束", "content-filter": "内容过滤", "aborted": "已中止" };
  return m[String(f || "").toLowerCase()] || String(f || "");
}
function describeMessage(info, type, fmtTok) {
  const role = info && info.role;
  const model = info.modelID || (info.model && (info.model.modelID || info.model.id)) || "";
  const agent = info.agent ? " · " + info.agent : "";
  if (role === "assistant") {
    const base = "AI 回复" + (model ? "（" + model + "）" : "") + agent;
    const tk = info.tokens;
    let tok = "";
    if (tk) {
      const total = (tk.input || 0) + (tk.output || 0) + (tk.reasoning || 0);
      tok = " · " + fmtTok(total) + " tok";
      if (tk.reasoning) tok += "（推理 " + fmtTok(tk.reasoning) + "）";
    }
    const finish = info.finish ? " · " + finishLabel(info.finish) : "";
    return type === "message.complete" ? base + tok + finish + " 完成" : base + tok + finish;
  }
  return "用户提问" + agent;
}
// 从 session.diff 提取文件变更摘要：文件名 + 增删行数。
function describeDiff(diff) {
  if (!Array.isArray(diff) || !diff.length) return "";
  let add = 0, del = 0;
  const names = [];
  for (const d of diff) {
    add += d.additions || 0;
    del += d.deletions || 0;
    const n = d.file || d.path || "";
    if (n) names.push(String(n).split("/").pop());
  }
  const list = names.slice(0, 3).join("、") + (names.length > 3 ? " 等" : "");
  return `文件变更：${list} +${add} −${del}`;
}
// 从原始事件 payload 提取展示信息，兼容 v1.18 wrapper（{"payload":{...}}）与非 wrapper 形态。
function wbPayloadMeta(payload, eventType) {
  let obj = payload;
  if (!obj || typeof obj !== "object") obj = {};
  let inner = obj.properties || obj.data || (obj.payload && (obj.payload.properties || obj.payload.data)) || {};
  const core = (obj.payload && obj.payload.properties) ? obj.payload.properties : inner;
  let type = obj.type || (obj.payload && obj.payload.type) || eventType || "";
  const sessionObj = core.session || obj.session;
  const info = core.info || inner.info || obj.info;
  const title = (sessionObj && sessionObj.title) || "";
  const file = wbFilePath(obj, core) || wbFilePath(obj, inner);
  const directory = obj.directory || (sessionObj && sessionObj.directory) || (file ? file.substring(0, file.lastIndexOf("/")) : "");
  let summary = "";
  // 具体动作优先：part（工具/文件/推理/文本）
  const part = core.part || inner.part || obj.part;
  if (part && part.type) summary = describePart(part);
  if (!summary && (type === "message.updated" || type === "message.complete") && info && info.role) summary = describeMessage(info, type, fmtTok);
  if (!summary && type === "session.diff") summary = describeDiff(core.diff);
  if (!summary && (type === "session.status" || type === "session.idle")) {
    const st = core.status;
    const stt = st && typeof st === "object" ? st.type : st;
    summary = stt === "idle" || type === "session.idle" ? "处理完成" : stt === "busy" ? "开始处理" : stt === "retry" ? "重试中" : stt === "error" ? "出错" : (stt || "");
  }
  if (!summary && type === "session.created") summary = "新建会话";
  if (!summary && type === "message.complete") summary = "本轮回复完成";
  if (!summary) {
    const qa = obj.questions || inner.questions || [];
    if (qa.length && qa[0] && qa[0].question) summary = "等待回答：" + String(qa[0].question).trim().slice(0, 60);
  }
  if (!summary) {
    const perm = inner.permission || obj.permission;
    if (perm && perm.type) summary = "需要授权：" + perm.type;
  }
  if (!summary) {
    const err = inner.error || obj.error;
    if (err && err.message) summary = "错误：" + String(err.message).trim().slice(0, 60);
  }
  if (!summary) {
    const tool = inner.tool || obj.tool;
    if (tool && (tool.type || tool.name)) summary = "正在执行工具：" + String(tool.type || tool.name).trim();
  }
  if (!summary && file) summary = "修改文件：" + file.split("/").pop();
  if (!summary) {
    const msg = inner.message || obj.message;
    if (msg && msg.content) {
      summary = (Array.isArray(msg.content) ? msg.content.map(c => (c && c.text) || "").join(" ") : String(msg.content)).trim().replace(/\s+/g, " ").slice(0, 50);
    }
  }
  if (!summary) {
    if (part && part.text) summary = String(part.text).trim().replace(/\s+/g, " ").slice(0, 50);
  }
  if (!summary) summary = wbFallbackEvent(type);
  // 工具调用的稳定标识（callID），用于把「运行中→完成/失败」的多条状态收敛成同一条动态。
  const toolKey = part && part.type === "tool"
    ? String(part.callID || (part.tool + "|" + ((part.state && (part.state.title || (part.state.input && part.state.input.command))) || "")))
    : "";
  // 同一消息的 message.updated 会推多条（创建 + 各里程碑），按 message id 收敛成一条（最新覆盖）。
  const msgKey = (type === "message.updated" || type === "message.complete") && info && info.id ? String(info.id) : "";
  return { title, directory, file, summary, type, toolKey, msgKey };
}
function wbEventItem(ev) {
  const meta = wbPayloadMeta(ev.payload, ev.eventType);
  const s = wbSessions.find(x => x.id === ev.sessionId);
  const title = meta.file || meta.title || (meta.directory || "").split("/").filter(Boolean).pop() || (ev.sessionId || "").slice(0, 8);
  // WS 推送的事件没有 createdAt，用当前时间兜底（轮询拉取的带 createdAt）。
  const ts = Date.parse(ev.createdAt) || Date.now();
  return {
    sessionId: ev.sessionId,
    title,
    directory: (s && s.directory) || meta.directory || "",
    summary: meta.summary,
    ts,
    toolKey: meta.toolKey || "",
    msgKey: meta.msgKey || "",
  };
}
// 时间格式化：当天显示 HH:mm:ss，跨天显示 MM-DD HH:mm。
function wbTimeFormat(ts) {
  if (!ts) return "";
  const d = new Date(ts);
  const now = new Date();
  const sameDay = d.toDateString() === now.toDateString();
  return sameDay
    ? d.toLocaleTimeString("zh-CN", { hour12: false, hour: "2-digit", minute: "2-digit", second: "2-digit" })
    : d.toLocaleString("zh-CN", { hour12: false, month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" });
}
// 事件点颜色：错误红 / 完成绿 / 提问黄 / 文件变更与工具青 / 默认紫罗兰。
function wbEventDot(ev) {
  const t = ev.eventType || "";
  if (t.includes("error") || t.includes("failed")) return "busy";
  if (t === "message.complete" || t.includes("idle") || t.includes("finish")) return "idle";
  if (t.startsWith("question.") || t.startsWith("permission.")) return "question";
  if (t === "session.diff" || t.startsWith("file.") || t.startsWith("tool") || t === "message.part.created" || t === "message.part.updated") return "busy";
  if (t.startsWith("message.")) return "idle";
  return "idle";
}
// 噪声事件：同步快照 / 心跳 / 会话时间戳刷新 / 空 diff 摘要——不产生「具体动作」，动态栏直接跳过。
const WB_NOISE_EVENTS = new Set(["sync", "server.heartbeat", "session.updated", "session.diff", "heartbeat"]);
// 把一条有意义的事件追加进动态历史（不按会话折叠，最多保留最近 WB_MAX_EVENTS 条）。
// 轮询带事件 id 用 id 去重；页面首次加载的历史事件按时间排，让动态栏更丰富。
function wbAppendEvent(ev) {
  if (!ev || !ev.sessionId) return;
  if (WB_NOISE_EVENTS.has(ev.eventType)) return;
  const item = wbEventItem(ev);
  const dot = wbEventDot(ev);
  const key = (ev.id !== undefined && ev.id !== null) ? "id:" + ev.id : "raw:" + ev.eventType + ":" + ev.sessionId + ":" + item.ts;
  if (wbEvents.some(x => x.key === key)) return;
  // 同一个工具调用（callID）会连发多条 part.updated（运行中→完成/失败），
  // 或同一消息连发多条 message.updated——都收敛成同一条动态，用最新状态覆盖。
  const collapseKey = item.toolKey || item.msgKey;
  if (collapseKey) {
    const ix = wbEvents.findIndex(x => x.sessionId === item.sessionId && (x.toolKey === collapseKey || x.msgKey === collapseKey));
    if (ix >= 0) {
      wbEvents[ix] = { ...wbEvents[ix], ...item, dot, key, ts: item.ts };
      wbEvents.sort((a, b) => (b.ts || 0) - (a.ts || 0));
      return;
    }
  }
  wbEvents.push({ ...item, dot, key });
  wbEvents.sort((a, b) => (b.ts || 0) - (a.ts || 0));
  if (wbEvents.length > WB_MAX_EVENTS) wbEvents = wbEvents.slice(0, WB_MAX_EVENTS);
}
function renderWbEvents() {
  const box = document.getElementById("wbEvents");
  document.getElementById("wbEventCount").textContent = "· " + wbEvents.length;
  const sig = wbEvents.map(e => (e.key || e.sessionId) + ":" + e.sessionId + ":" + e.ts + ":" + e.dot + ":" + e.title + ":" + e.summary + ":" + e.directory).join("|");
  if (sig === wbEventsSig) return;
  wbEventsSig = sig;
  const scroller = box.closest(".wb-body") || box;
  const prevScroll = scroller.scrollTop || 0;
  if (!wbEvents.length) { box.innerHTML = `<div class="wb-placeholder">暂无动态，等待事件推送…</div>`; box.onclick = null; return; }
  box.innerHTML = wbEvents.map(e => {
    const d = wbTimeFormat(e.ts);
    const dir = e.directory ? ` <span class="muted">·</span> ${escapeHtml(e.directory)}` : "";
    return `<div class="wb-event" data-sid="${escapeHtml(e.sessionId)}"><span class="dot ${escapeHtml(e.dot || "idle")}" style="margin-top:4px"></span><span class="t">${d}</span><div class="sum"><b>${escapeHtml(e.title)}</b> ${escapeHtml(e.summary)}${dir}</div></div>`;
  }).join("");
  scroller.scrollTop = prevScroll;
  box.onclick = (e) => {
    const el = e.target.closest(".wb-event");
    if (el && el.dataset.sid) openWbPanel(el.dataset.sid);
  };
}
async function loadWbEvents() {
  // 首次加载(无游标)多拉一些历史事件回填动态栏，之后按游标增量。
  const hadCursor = !!wbEventsCursor;
  const q = "/api/events?limit=" + (wbEventsCursor ? "100" : "200") + (wbEventsCursor ? "&since=" + encodeURIComponent(wbEventsCursor) : "");
  const res = await api(q, { headers: appHeaders() });
  if (!res.ok) return;
  let data;
  try { data = await res.json(); } catch (_) { return; }
  const list = data.events || [];
  if (list.length) wbEventsCursor = list[list.length - 1].createdAt;
  let added = false;
  let listChanged = false;
  for (const ev of list) {
    if (WB_HIGH_FREQ.has(ev.eventType)) continue;
    wbAppendEvent(ev);
    added = true;
    // 增量轮询中的新活动 → 本地点亮绿点（权威状态由 syncWbUnread 收敛）。
    if (hadCursor && wbUnreadTrigger(ev.eventType)) wbHandleActivity(ev.sessionId);
    // 会话增删事件即使 WS 断线期间错过，轮询也能兜底触发列表全量刷新。
    if (ev.eventType === "session.created" || ev.eventType === "session.deleted" || ev.eventType === "session.compacted") listChanged = true;
  }
  if (added) renderWbEvents();
  if (listChanged) wbScheduleWorkbench();
  refreshWbStats();
}
function wbClearEvents() {
  wbEvents = [];
  renderWbEvents();
}
// WS 推送：session.event → 更新状态/事件流。渲染走防抖 + 内容指纹，
// 多个连续事件合并为一次渲染，避免列表/面板疯狂重绘。
function wbScheduleRenderList() {
  clearTimeout(wbListRenderTimer);
  wbListRenderTimer = setTimeout(() => { renderWbList(); }, 350);
}
function wbSchedulePanelRefresh(id) {
  clearTimeout(wbPanelRefreshTimer);
  wbPanelRefreshTimer = setTimeout(() => { if (wbSelected === id) refreshWbPanel(id); }, 600);
}
function wbScheduleWorkbench() {
  clearTimeout(wbWorkbenchTimer);
  wbWorkbenchTimer = setTimeout(() => { loadWorkbench(); }, 500);
}
function wbStatusFromPayload(payload) {
  const obj = payload && typeof payload === "object" ? payload : {};
  const inner = obj.properties || obj.data || (obj.payload && (obj.payload.properties || obj.payload.data)) || {};
  const st = inner.status;
  if (!st) return "";
  return typeof st === "object" ? (st.type || "") : String(st);
}
// 判定事件是否会触发「未读」（与后端 isUnreadTriggerEvent 保持一致）。
function wbUnreadTrigger(t) {
  return ["message.complete", "message.created", "message.updated",
    "question.asked", "question.updated", "permission.asked",
    "session.idle", "session.status", "session.error", "session.failed"].includes(t);
}
// 从后端同步未读集合（会话列表绿点的权威来源，Web/App 共享已读状态）。
async function syncWbUnread() {
  try {
    const res = await api("/api/unread", { headers: appHeaders() });
    if (!res.ok) return;
    const d = await res.json();
    const m = d.unread || {};
    const next = new Set(Object.keys(m).filter(k => m[k]));
    if (next.size !== wbNewSet.size || [...next].some(x => !wbNewSet.has(x))) {
      wbNewSet = next;
      renderWbList();
    }
  } catch (_) {}
}
// 标记已读：清本地绿点 + 通知后端（任一端读过后全端不再显示未读）。
async function wbMarkRead(sid) {
  if (!sid) return;
  if (wbNewSet.has(sid)) {
    wbNewSet.delete(sid);
    renderWbList();
  }
  try {
    await api(`/api/unread/${encodeURIComponent(sid)}`, { method: "POST", headers: appHeaders() });
  } catch (_) {}
}
// 会话有新活动：正在看的会话视为已读（同步后端），否则本地亮绿点（随后由同步收敛）。
function wbHandleActivity(sid) {
  if (!sid) return;
  if (wbSelected === sid) { wbMarkRead(sid); return; }
  if (!wbNewSet.has(sid)) {
    wbNewSet.add(sid);
    wbScheduleRenderList();
  }
}
function handleWbPush(ev) {
  const sid = ev.sessionId;
  const et = ev.eventType;
  if (!sid) return;
  // 会话新增/删除/压缩：列表需要全量重新拉取，否则 APP 新建的会话
  // 要等 12s 兜底轮询甚至手动刷新才出现。
  if (et === "session.created" || et === "session.deleted" || et === "session.compacted") {
    wbScheduleWorkbench();
    return;
  }
  if (et === "session.status" || et === "session.idle" || et === "session.updated") {
    // 事件指向一个列表里还没有的会话（APP 新建/另目录会话）→ 全量刷新列表。
    if (!wbSessions.some(x => x.id === sid)) {
      wbScheduleWorkbench();
      return;
    }
    let st = "idle";
    if (et !== "session.idle") {
      const stt = wbStatusFromPayload(ev.payload);
      st = ["busy", "idle", "retry"].includes(stt) ? stt : (wbStatuses[sid] && wbStatuses[sid].type) || "idle";
    }
    wbStatuses[sid] = { type: st };
    wbHandleActivity(sid);
    wbScheduleRenderList();
    if (wbSelected === sid && (et === "session.status" || et === "session.updated")) wbSchedulePanelRefresh(sid);
  }
  if (et === "question.asked" || et === "question.updated" || et === "permission.asked" || et === "message.complete" || et === "session.error" || et === "session.failed") {
    wbHandleActivity(sid);
    wbScheduleWorkbench();
    return;
  }
  // 动态栏由 5s 轮询统一喂（带事件 id 去重），WS 不再追加，避免同一事件重复展示。
  refreshWbStats();
}

/* ---- 决策面板（外壳；对话区由 SBChat.ChatView 托管） ---- */
function wbMsgText(m) {
  return (m.parts || []).filter(p => p && p.type === "text" && !p.synthetic && !p.ignored && p.text).map(p => p.text).join("\n").trim();
}
// #wbPanel 的固定骨架只建一次：外壳写 #wbShell，对话区 #wbChat 作为兄弟节点，
// 外壳重绘不会销毁对话区 DOM。
function wbPanelSkeleton() {
  const panel = document.getElementById("wbPanel");
  if (panel.querySelector("#wbShell")) return panel;
  panel.innerHTML = `<div class="wb-shell" id="wbShell"></div><div class="wb-chat" id="wbChat"></div>`;
  return panel;
}
function openWbPanel(id) {
  const switched = wbSelected !== id;
  if (!id) { closeWbPanel(); return; }
  wbSelected = id;
  wbMarkRead(id);
  wbPanelSkeleton();
  document.getElementById("wbPanelPlaceholder").classList.add("hidden");
  document.getElementById("wbPanel").classList.remove("hidden");
  if (switched) {
    // 切会话时先清空为加载态，避免显示上一个会话的内容。
    wbPanelData = { id, loading: true, pending: [], permissions: [], status: "", session: null };
    // 切换会话时清掉上一会话的待决问题作答暂存。
    wbQAnswers = {};
    wbRenaming = false;
    wbCompacting = false;
    renderWbPanel();
    const chat = wbEnsureChat();
    if (chat) { ensureWbAgents().then(() => chat.refreshSelectors()); chat.open(id); }
  } else if (wbChat) {
    wbChat.renderSelectors();
  }
  renderWbList();
  refreshWbPanel(id, true);
  wbSyncStream();
}
function closeWbPanel() {
  wbSelected = null;
  wbPanelData = null;
  wbChat = null;
  wbSyncStream();
  wbPanelFetching = null;
  const panel = document.getElementById("wbPanel");
  panel.classList.add("hidden");
  panel.innerHTML = "";
  document.getElementById("wbPanelPlaceholder").classList.remove("hidden");
  renderWbList();
}
function modelId(m) {
  if (!m) return "";
  if (typeof m === "object") return m.id || "";
  return String(m);
}
async function refreshWbPanel(id, force) {
  if (id !== wbSelected) return;
  // 同一会话已有一轮拉取在进行中，跳过本轮（进行中那轮会渲染）。
  if (!force && wbPanelFetching === id) return;
  wbPanelFetching = id;
  try {
    if (id !== wbSelected) return;
    if (!wbItems.length) wbItems = buildWbItems();
    const item = wbItems.find(x => x.session.id === id);
    const newData = {
      id,
      pending: (item && item.pending) || [],
      permissions: (item && item.permissions) || [],
      status: item ? item.status : "idle",
      session: item ? item.session : (wbPanelData && wbPanelData.session) || null,
    };
    // 拉取后比较：内容没变化才跳过重绘（对话消息由 ChatView / SSE 负责，不在此拉取）。
    if (!force && wbPanelData && wbPanelData.id === id && wbPanelSigFor(wbPanelData) === wbPanelSigFor(newData)) return;
    wbPanelData = newData;
    const snap = wbSnapshotPanel();
    renderWbPanel();
    wbRestorePanel(snap);
    if (wbChat) wbChat.renderSelectors();
    // 模型下拉首次打开时后台加载 provider 列表，加载完仅重绘一次（保留输入）。
    // idle / failed 都触发：失败会自动重试，成功则回填下拉选项。
    if (wbProvidersState === "idle" || wbProvidersState === "failed") ensureWbProviders().then(() => { if (wbChat) wbChat.renderSelectors(); });
  } finally {
    if (wbPanelFetching === id) wbPanelFetching = null;
  }
}
function renderWbPanel() {
  wbMoreOpen = false;
  const panel = wbPanelSkeleton();
  const shell = document.getElementById("wbShell");
  const d = wbPanelData;
  if (!d) { shell.innerHTML = ""; return; }
  if (d.loading || !d.session) {
    shell.innerHTML = `<div class="wb-placeholder">加载中…</div>`;
    wbPanelSig = wbPanelSigFor(d);
    return;
  }
  const s = d.session || {};
  // 状态取当前实时 wbItems（列表口径），保证面板徽标与列表一致。
  let status = d.status;
  const curItem = wbItems.find(x => x.session.id === d.id);
  if (curItem) status = curItem.status;
  const title = (s.title || s.slug || d.id).toString();
  const dir = (s.directory || "").toString();
  const model = modelId(s.model);
  const agent = s.agent ? String(s.agent) : "";
  const metaBits = [];
  if (model) metaBits.push(`<span class="chip" title="会话模型"><i>模型</i>${escapeHtml(model)}</span>`);
  if (agent) metaBits.push(`<span class="chip" title="会话 Agent"><i>Agent</i>${escapeHtml(agent)}</span>`);
  if (dir) metaBits.push(`<span class="chip" title="工作目录"><i>目录</i>${escapeHtml(dir)}</span>`);
  if (s.id) metaBits.push(`<span class="chip mono" title="会话 ID"><i>ID</i>${escapeHtml(String(s.id).slice(0, 18))}</span>`);
  const statusBadgeCls = status === "question" ? "question" : (status === "busy" || status === "retry") ? "busy" : "idle";
  const isBusyForAct = status === "busy" || status === "retry";
  const shareUrl = (s.share && s.share.url) || "";
  const qhtml = renderWbQuestions();
  const permHtml = renderWbPermissions(d.permissions || []);
  shell.innerHTML = `
    <div class="wb-panel-head">
      <span class="dot ${escapeHtml(status)}" style="margin-top:7px"></span>
      <div class="t">
        <span class="ttl">${escapeHtml(title)}</span>
        <span class="badge ${statusBadgeCls}">${wbStatusLabel(status)}</span>
        ${(s.time && s.time.created) ? `<div class="meta">创建 ${escapeHtml(new Date(s.time.created).toLocaleString())}</div>` : ""}
      </div>
      <div class="wb-head-actions">
        <div style="display:flex;gap:6px;align-items:center">
          <button class="ghost sm" data-wb-action="star" title="收藏 / 取消收藏会话">${wbStars.has(d.id) ? "★ 已收藏" : "☆ 收藏"}</button>
          <button class="ghost sm" data-wb-action="diff" title="查看会话内文件更改">更改</button>
          <button class="ghost sm" data-wb-action="timeline" title="会话时间线">时间线</button>
          <button class="ghost sm" data-wb-rename="1" title="修改会话标题">改名</button>
          <button class="ghost sm" data-wb-compact="1" title="压缩会话：把历史对话汇总为摘要以释放上下文（耗时较长）" ${wbCompacting ? "disabled" : ""}>${wbCompacting ? "压缩中…" : "压缩"}</button>
          <button class="danger sm" data-wb-del="${escapeHtml(d.id)}">删除</button>
          <div class="wb-more-wrap">
            <button class="ghost sm" data-wb-morebtn="1" title="更多操作（同 App 聊天详情菜单）">更多 ▾</button>
            <div class="wb-more-menu hidden" data-wb-more-menu>
              <button data-wb-action="reload">重新加载</button>
              <button data-wb-action="star">${wbStars.has(d.id) ? "取消收藏" : "收藏会话"}</button>
              <button data-wb-action="sess-tag">设置标签</button>
              <button data-wb-action="fork">Fork 会话</button>
              <button data-wb-action="undo" ${isBusyForAct ? "disabled" : ""}>撤销上一条</button>
              <button data-wb-action="redo" ${isBusyForAct ? "disabled" : ""}>重做</button>
              <button data-wb-action="review">运行代码审查</button>
              <button data-wb-action="diff">查看更改（diff）</button>
              <button data-wb-action="timeline">会话时间线</button>
              <button data-wb-action="abort" ${isBusyForAct ? "" : "disabled"}>停止处理</button>
              ${shareUrl ? `<button data-wb-action="copylink">复制分享链接</button><button data-wb-action="unshare">取消分享</button>` : `<button data-wb-action="share">分享会话</button>`}
              <button data-wb-action="export-md">导出 Markdown</button>
              <button data-wb-action="export-json">导出 JSON</button>
            </div>
          </div>
        </div>
      </div>
      ${metaBits.length ? `<div class="wb-panel-meta">${metaBits.join("")}</div>` : ""}
    </div>
    ${wbRenaming ? `<div class="wb-rename-row">
      <input id="wbRenameInput" maxlength="120" value="${escapeHtml(title)}">
      <button class="sm" data-wb-rensave="1">保存</button>
      <button class="ghost sm" data-wb-rencancel="1">取消</button>
      <div id="wbRenameMsg" class="msg"></div>
    </div>` : ""}
    <div class="wb-blocks${(permHtml || qhtml) ? "" : " empty"}">
      ${permHtml ? `<div class="wb-block"><h4>待授权操作<span class="info">${(d.permissions || []).length} 项</span></h4>${permHtml}</div>` : ""}
      ${qhtml ? `<div class="wb-block"><h4>待决问题<span class="info">${(d.pending || []).reduce((n, q) => n + (q.questions || []).length, 0)} 个</span></h4>${qhtml}</div>` : ""}
    </div>`;
  panel.onclick = (e) => {
    const morebtn = e.target.closest("[data-wb-morebtn]");
    if (morebtn) { wbMoreMenuToggle(morebtn); return; }
    const mact = e.target.closest("[data-wb-action]");
    if (mact) { wbMoreMenuAction(mact.dataset.wbAction); return; }
    const cp = e.target.closest("[data-wb-compact]");
    if (cp) { wbCompactSession(d.id); return; }
    const pbp = e.target.closest("[data-wb-perm]");
    if (pbp) { wbPermReply(pbp.dataset.wbPerm, pbp.dataset.reply); return; }
    const ren = e.target.closest("[data-wb-rename]");
    if (ren) {
      wbRenaming = true;
      wbRerenderPanel();
      const ri = document.getElementById("wbRenameInput");
      if (ri) { ri.focus(); ri.select(); }
      return;
    }
    const renc = e.target.closest("[data-wb-rencancel]");
    if (renc) { wbRenaming = false; wbRerenderPanel(); return; }
    const rens = e.target.closest("[data-wb-rensave]");
    if (rens) { wbRenameSession(d.id); return; }
    const del = e.target.closest("[data-wb-del]");
    if (del) { deleteWbSession(del.dataset.wbDel); return; }
    const opt = e.target.closest("[data-qopt]");
    if (opt) { wbQToggle(opt.dataset.qopt, Number(opt.dataset.qidx), opt.dataset.label); return; }
    const rej = e.target.closest("[data-qreject]");
    if (rej) { wbRejectQ(rej.dataset.qreject); return; }
    const sub = e.target.closest("[data-qsubmit]");
    if (sub) { wbQSubmit(sub.dataset.qsubmit); return; }
    const cus = e.target.closest("[data-qcustom]");
    if (cus) { wbQCustom(cus.dataset.qcustom, Number(cus.dataset.qidx)); return; }
  };
  // 自定义回答：Enter 等价于点「填入 / 提交」，避免多一步鼠标操作。
  panel.onkeydown = (e) => {
    if (e.key !== "Enter" || e.isComposing) return;
    const inp = e.target.closest("[id^='wbQInput_']");
    if (!inp) return;
    e.preventDefault();
    const m = inp.id.match(/^wbQInput_(.+)_(\d+)$/);
    if (!m) return;
    wbQCustom(m[1], Number(m[2]));
  };
  wbPanelSig = wbPanelSigFor(wbPanelData);
}
// 渲染待授权操作。每条权限显示 permission 类型 + patterns，提供 拒绝 /
// 仅一次 / 始终允许 三个动作（顺序与 App 一致，「始终允许」需二次确认）。
function renderWbPermissions(perms) {
  return (perms || []).map(p => {
    const patterns = (p.patterns || []).join("、") || "-";
    const tool = (p.tool && (p.tool.type || p.tool.id || p.tool.name)) ? ` · ${escapeHtml(p.tool.type || p.tool.id || p.tool.name)}` : "";
    const always = (p.always || []).join("、");
    const busy = wbPermBusy.has(p.id);
    return `<div class="wb-qcard" data-wb-permcard>
      <div class="qhead">待授权 · ${escapeHtml(p.permission || "操作")}${tool}<span class="info" style="float:right;text-transform:none">${escapeHtml(p.id || "")}</span></div>
      <div class="q">${escapeHtml(patterns)}</div>
      ${always ? `<div class="muted" style="font-size:11px;margin-top:4px">已始终允许：${escapeHtml(always)}</div>` : ""}
      <div class="wb-qopts">
        <button class="reject" data-wb-perm="${escapeHtml(p.id)}" data-reply="reject" ${busy ? "disabled" : ""}>拒绝</button>
        <button data-wb-perm="${escapeHtml(p.id)}" data-reply="once" ${busy ? "disabled" : ""}>仅一次</button>
        <button class="primary" data-wb-perm="${escapeHtml(p.id)}" data-reply="always" ${busy ? "disabled" : ""}>始终允许</button>
      </div>
    </div>`;
  }).join("");
}
// 定位 request（权限/待决问题）挂载的会话目录：request 可能挂在子会话上，
// 其目录可能与父会话不同，回复时 directory 不匹配上游会 404。
function wbOwningDir(reqId) {
  for (const map of [wbPending, wbPermissions]) {
    for (const sid of Object.keys(map)) {
      if ((map[sid] || []).some(x => x.id === reqId)) {
        const s = wbSessions.find(x => x.id === sid);
        if (s && s.directory) return s.directory;
      }
    }
  }
  return "";
}
// 处理待授权操作：allowed once / always / reject。
async function wbPermReply(reqId, reply) {
  if (!reqId || wbPermBusy.has(reqId)) return;
  // 与 App 一致：「始终允许」影响后续会话，必须先二次确认，并展示将生效的规则。
  if (reply === "always") {
    const item = (wbPermissions[wbSelected] || []).find(x => x.id === reqId)
      || Object.values(wbPermissions).flat().find(x => x.id === reqId) || {};
    const always = (item.always || []).join("、");
    const body = "将来匹配操作可能会自动批准。" + (always ? `\n适用于：${always}` : "");
    if (!window.confirm("始终允许此权限？\n" + body)) return;
  }
  wbPermBusy.add(reqId);
  wbRerenderPanel();
  try {
    const item = wbItems.find(x => x.session.id === wbSelected);
    const dir = wbOwningDir(reqId) || (item && item.session && item.session.directory) || "";
    const url = `/api/opencode/permission/${encodeURIComponent(reqId)}/reply` + (dir ? "?directory=" + encodeURIComponent(dir) : "");
    const res = await api(url, { method: "POST", headers: appHeaders(), body: JSON.stringify({ reply }) });
    if (!res.ok) { toast("授权失败", "权限回复未送达 (" + res.status + ")", "crit"); return; }
    toast("已处理", reply === "reject" ? "已拒绝该操作" : (reply === "always" ? "已始终允许" : "已允许一次"), "info");
    if (wbPanelData && wbPanelData.permissions) {
      wbPanelData.permissions = wbPanelData.permissions.filter(p => p.id !== reqId);
    }
    for (const sid of Object.keys(wbPermissions)) {
      wbPermissions[sid] = (wbPermissions[sid] || []).filter(p => p.id !== reqId);
    }
    wbRerenderPanel();
    loadWorkbench();
  } finally {
    wbPermBusy.delete(reqId);
  }
}

// 渲染待决问题。一个 request 可含多个 question，answers 数组须按序一一对应。
// 仅一问且非多选时点选项即提交；否则逐题暂存（高亮不提交），由底部按钮整体送出。
function renderWbQuestions() {
  const pending = wbPanelData.pending || [];
  // 清理已不在待决列表里的 request 的暂存作答，避免旧选择残留。
  const valid = new Set(pending.map(x => x.id));
  Object.keys(wbQAnswers).forEach(k => { if (!valid.has(k)) delete wbQAnswers[k]; });
  return pending.map(q => {
    const qs = q.questions || [];
    if (!qs.length) return "";
    // 与 App 的 isSingle 一致：仅一问且非多选时，点选（或送出自定义答案）即提交。
    const isSingle = qs.length === 1 && qs[0].multiple !== true;
    const busy = wbQBusy.has(q.id);
    const sel = (wbQAnswers[q.id] = wbQAnswers[q.id] || qs.map(() => []));
    const cards = qs.map((qu, qi) => {
      const opts = (qu.options || []).map(o => {
        const active = (sel[qi] || []).includes(o.label);
        return `<button class="wb-qopt${active ? " active" : ""}" data-qopt="${escapeHtml(q.id)}" data-qidx="${qi}" data-label="${escapeHtml(o.label)}" ${busy ? "disabled" : ""}>${escapeHtml(o.label)}${o.description ? `<br><span class="muted" style="font-size:11px;font-weight:400">${escapeHtml(o.description)}</span>` : ""}</button>`;
      }).join("");
      const custom = qu.custom !== false
        ? `<div class="wb-qcustom"><input id="wbQInput_${escapeHtml(q.id)}_${qi}" placeholder="${isSingle ? "输入回答后按 Enter 提交" : "自定义回答（可选）"}" ${busy ? "disabled" : ""}><button class="ghost sm" data-qcustom="${escapeHtml(q.id)}" data-qidx="${qi}" ${busy ? "disabled" : ""}>${isSingle ? "提交" : "填入"}</button></div>`
        : "";
      return `<div class="wb-qcard"><div class="qhead">${escapeHtml(qu.header || "AI 提问")}${qs.length > 1 ? ` <span class="muted" style="font-weight:400">${qi + 1}/${qs.length}</span>` : ""}</div><div class="q">${escapeHtml(qu.question)}</div>
        <div class="wb-qopts">${opts}</div>${custom}</div>`;
    }).join("");
    if (isSingle) {
      return `<div class="wb-qgroup">${cards}<div class="wb-qsubmit"><span class="muted" style="font-size:11px">点选选项即提交</span><button class="reject" data-qreject="${escapeHtml(q.id)}" ${busy ? "disabled" : ""}>拒绝</button></div></div>`;
    }
    const answered = sel.filter(a => a && a.length).length;
    const footer = `<div class="wb-qsubmit"><button class="sm" data-qsubmit="${escapeHtml(q.id)}" ${answered && !busy ? "" : "disabled"} title="已作答 ${answered}/${qs.length}">提交回答${qs.length > 1 ? `（${answered}/${qs.length}）` : ""}</button><button class="reject" data-qreject="${escapeHtml(q.id)}" ${busy ? "disabled" : ""}>拒绝</button></div>`;
    return `<div class="wb-qgroup">${cards}${footer}</div>`;
  }).join("");
}
// 点击问题选项：单选且多问时仅本地暂存；单选且仅一问时点选即提交（与 App 一致）。
function wbQToggle(reqId, qidx, label) {
  const q = (wbPanelData.pending || []).find(x => x.id === reqId);
  if (!q || wbQBusy.has(reqId)) return;
  const qs = q.questions || [];
  const qu = qs[qidx];
  if (!qu) return;
  const sel = (wbQAnswers[reqId] = wbQAnswers[reqId] || qs.map(() => []));
  const cur = sel[qidx] || [];
  if (qu.multiple === true) {
    sel[qidx] = cur.includes(label) ? cur.filter(x => x !== label) : [...cur, label];
  } else {
    sel[qidx] = [label];
    if (qs.length === 1) { postWbAnswer(reqId, [[label]]); return; }
  }
  wbRerenderPanel();
}
// 自定义回答：仅一问单选时直接提交，否则暂存（随「提交回答」一起发出）。
function wbQCustom(reqId, qidx) {
  const input = document.getElementById("wbQInput_" + reqId + "_" + qidx);
  const v = input ? input.value.trim() : "";
  if (!v) { toast("答复失败", "请先输入回答内容"); return; }
  const q = (wbPanelData.pending || []).find(x => x.id === reqId);
  if (!q || wbQBusy.has(reqId)) return;
  const qs = q.questions || [];
  const sel = (wbQAnswers[reqId] = wbQAnswers[reqId] || qs.map(() => []));
  sel[qidx] = [v];
  if (qs.length === 1 && (qs[0].multiple !== true)) { postWbAnswer(reqId, [[v]]); return; }
  wbRerenderPanel();
}
function wbQSubmit(reqId) {
  const q = (wbPanelData.pending || []).find(x => x.id === reqId);
  if (!q || wbQBusy.has(reqId)) return;
  const qs = q.questions || [];
  const sel = (wbQAnswers[reqId] = wbQAnswers[reqId] || qs.map(() => []));
  // answers 数组长度必须与 questions 一致（未作答的给空数组）。
  const answers = qs.map((_, i) => (sel[i] || []).slice());
  if (!answers.some(a => a.length)) { toast("提交失败", "请至少回答一个问题"); return; }
  postWbAnswer(reqId, answers);
}
async function postWbAnswer(reqId, answers) {
  if (wbQBusy.has(reqId)) return;
  wbQBusy.add(reqId);
  wbRerenderPanel();
  try {
    const dir = wbOwningDir(reqId) || (wbPanelData && wbPanelData.session && wbPanelData.session.directory) || "";
    // 与 App 一致：question reply 的目录以 query 参数传递（x-starburst-directory 头只对 prompt_async 生效）。
    const url = `/api/opencode/question/${encodeURIComponent(reqId)}/reply` + (dir ? "?directory=" + encodeURIComponent(dir) : "");
    const res = await api(url, { method: "POST", headers: appHeaders(), body: JSON.stringify({ answers }) });
    if (!res.ok) { toast("答复失败", "问题回复未送达 (" + res.status + ")", "crit"); return; }
    toast("已答复", "问题已回复", "info");
    wbPanelData.pending = wbPanelData.pending.filter(x => x.id !== reqId);
    for (const sid of Object.keys(wbPending)) {
      wbPending[sid] = (wbPending[sid] || []).filter(x => x.id !== reqId);
    }
    delete wbQAnswers[reqId];
    wbRerenderPanel();
    loadWorkbench();
  } finally {
    wbQBusy.delete(reqId);
  }
}
async function wbRejectQ(reqId) {
  if (wbQBusy.has(reqId)) return;
  wbQBusy.add(reqId);
  wbRerenderPanel();
  try {
    const dir = wbOwningDir(reqId) || (wbPanelData && wbPanelData.session && wbPanelData.session.directory) || "";
    const url = `/api/opencode/question/${encodeURIComponent(reqId)}/reject` + (dir ? "?directory=" + encodeURIComponent(dir) : "");
    const res = await api(url, { method: "POST", headers: appHeaders() });
    if (!res.ok) { toast("拒绝失败", "拒绝未送达 (" + res.status + ")", "crit"); return; }
    toast("已拒绝", "问题已拒绝", "info");
    wbPanelData.pending = wbPanelData.pending.filter(q => q.id !== reqId);
    for (const sid of Object.keys(wbPending)) {
      wbPending[sid] = (wbPending[sid] || []).filter(x => x.id !== reqId);
    }
    delete wbQAnswers[reqId];
    wbRerenderPanel();
    loadWorkbench();
  } finally {
    wbQBusy.delete(reqId);
  }
}
// 发送对话消息（prompt_async）。返回 true/false 供 ChatView 决定是否撤回乐观气泡。
// 发送指令的超时（ms）：prompt_async 正常是快速 204；若上游长时间不返回，不能一直
// 占着 wbSending 把输入框和发送按钮卡死——超时按「已提交」处理（fire-and-forget，
// 消息大概率已入队，靠 SSE 回执收敛），而不是还原输入框造成「按了 Enter 内容还在」。
const SEND_TIMEOUT_MS = 15000;
async function wbSendPrompt(body) {
  const id = wbSelected;
  if (!id) return false;
  // 上一条仍在途时不重复发：保留输入框内容（send() 不会清空），避免双发。
  if (wbSending) return false;
  wbSending = true;
  const dir = wbCurrentDir();
  const headers = appHeaders();
  if (dir) headers["x-starburst-directory"] = dir;
  try {
    const ctl = new AbortController();
    const timer = setTimeout(() => ctl.abort(), SEND_TIMEOUT_MS);
    let res = null, timedOut = false;
    try {
      res = await api(`/api/opencode/session/${encodeURIComponent(id)}/prompt_async`,
        { method: "POST", headers, body: JSON.stringify(body), signal: ctl.signal });
    } catch (e) {
      if (e && e.name === "AbortError") timedOut = true;
      else throw e;
    } finally {
      clearTimeout(timer);
    }
    if (timedOut) {
      // 超时：不再还原输入框（消息大概率已入队），保留乐观气泡，靠 SSE 收敛。
      toast("已提交", "指令已发送，若长时间无响应请检查会话状态", "info");
      wbDismissPendingQuestions(id);
      wbStatuses[id] = { type: "busy" };
      renderWbList();
      return true;
    }
    if (!res || !res.ok) {
      toast("发送失败", `指令未送达 (${res ? res.status : "网络错误"})，内容已保留在输入框可重试`, "crit");
      return false;
    }
    // 新指令覆盖上一轮未答的提问：服务端驳回 + 本地移除（与 App 行为一致），
    // 否则切进会话还会看到已经作废的问题卡片。
    wbDismissPendingQuestions(id);
    // 乐观更新状态：直接写 wbStatuses（列表/面板都从它派生），
    // 避免被 renderWbList 的重建丢弃，让「处理中」立即生效直到事件到达。
    wbStatuses[id] = { type: "busy" };
    renderWbList();
    return true;
  } catch (e) {
    if (e.message !== "unauthorized") toast("发送失败", (e.message || "未知错误") + "，内容已保留在输入框可重试", "crit");
    return false;
  } finally {
    wbSending = false;
  }
}
// 拒绝该会话（含子会话）的全部待决问题；不阻塞发送主流程。
function wbDismissPendingQuestions(id) {
  const childIds = wbSessions.filter(s => s.parentID === id).map(s => s.id);
  for (const sid of [id, ...childIds]) {
    const dir = (wbSessions.find(s => s.id === sid) || {}).directory || "";
    for (const q of (wbPending[sid] || [])) {
      api(`/api/opencode/question/${encodeURIComponent(q.id)}/reject` + (dir ? "?directory=" + encodeURIComponent(dir) : ""),
        { method: "POST", headers: appHeaders() }).catch(() => {});
      delete wbQAnswers[q.id];
    }
    wbPending[sid] = [];
  }
  if (wbPanelData && wbPanelData.id === id) { wbPanelData.pending = []; wbRerenderPanel(); }
}
// 回退到某一轮：POST /session/{id}/revert { messageID }（App revertSession 同契约）。
// 上游对处理中的会话（agent 还在跑工具 / 生成）直接回 409 SessionBusyError，
// 这里先试一次；409 则轮询等待空闲自动重试（最多 ~30s），期间 toast 提示。
async function wbRevertMessage(id, mid, dir) {
  const doRev = () => api(`/api/opencode/session/${encodeURIComponent(id)}/revert`,
    { method: "POST", headers: wbDirHeaders(dir), body: JSON.stringify({ messageID: mid }) }).catch(() => null);
  let res = await doRev();
  if (res && res.status === 409) {
    toast("会话处理中", "等待空闲后自动重试…", "info", 2500);
    for (let i = 0; i < 15 && wbSelected === id; i++) {
      await new Promise(r => setTimeout(r, 2000));
      res = await doRev();
      if (!res || res.status !== 409) break;
    }
  }
  return res;
}
async function wbRevertTo(turn) {
  const id = wbSelected;
  const mid = turn && (turn.id || (turn.msgIds || [])[0]);
  if (!id || !mid) return;
  const dir = wbCurrentDir();
  const res = await wbRevertMessage(id, mid, dir);
  if (!res || !res.ok) { toast("回退失败", `revert (${res ? res.status : "网络错误"})，请先停止处理或稍后再试`, "crit"); return; }
  toast("已回退", "该消息之后的内容已撤销，可用「更多 ▾ → 重做」恢复", "info");
  if (wbChat) { wbChat.dropAfter(mid); wbChat.revertTo = mid; }
  loadWorkbench();
}
// 重新生成：回退到本轮对应的用户指令，再用同样的 parts 重发一次。
async function wbRegenerate(turn) {
  const id = wbSelected;
  if (!id || !wbChat) return;
  const turns = wbChat.turns;
  const ix = turns.findIndex(t => t.id === turn.id);
  let userTurn = null;
  for (let i = ix - 1; i >= 0; i--) {
    if (turns[i].role === "user" && turns[i].parts.some(p => p.type === "text" && !p.synthetic)) { userTurn = turns[i]; break; }
  }
  if (!userTurn) { toast("无法重新生成", "未找到该轮对应的指令", "warn"); return; }
  const dir = wbCurrentDir();
  const res = await wbRevertMessage(id, userTurn.id, dir);
  if (!res || !res.ok) { toast("重新生成失败", `revert (${res ? res.status : "网络错误"})，请先停止处理或稍后再试`, "crit"); return; }
  wbChat.revertTo = userTurn.id;
  wbChat.dropAfter(userTurn.id);
  wbChat.resendTurn(userTurn.id);
}
// 修改会话标题（PATCH /session/{id}，与 App updateSession 契约一致）。
// 压缩会话：POST /session/{id}/summarize（App summarizeSession 同契约）。
// 该操作需 LLM 生成摘要，属同步长耗时（可数分钟），期间按钮置灰、不阻塞其它操作。
async function wbCompactSession(id) {
  if (wbCompacting) return;
  if (!confirm("确认压缩该会话？\n会把历史对话汇总为一条摘要以释放上下文，耗时可能较长。")) return;
  const item = wbItems.find(x => x.session.id === id);
  const session = (item && item.session) || (wbPanelData && wbPanelData.session) || {};
  const model = session.model || {};
  const providerID = model.providerID || "litellm";
  const modelID = model.id || "";
  if (!modelID) { toast("压缩失败", "未知会话模型，无法压缩", "crit"); return; }
  const dir = session.directory || "";
  const headers = appHeaders();
  if (dir) headers["x-opencode-directory"] = dir;
  wbCompacting = true;
  wbRerenderPanel();
  toast("压缩中…", "正在生成会话摘要，可能需要几分钟放下不管", "info");
  try {
    const res = await api(`/api/opencode/session/${encodeURIComponent(id)}/summarize`, {
      method: "POST",
      headers,
      body: JSON.stringify({ providerID, modelID }),
    });
    if (!res.ok) { toast("压缩失败", "压缩未生效 (" + res.status + ")", "crit"); return; }
    toast("已压缩", "会话已压缩为摘要", "info");
    renderWbList();
    if (wbChat) wbChat.reload();
    wbScheduleWorkbench();
  } catch (e) {
    if (e.message !== "unauthorized") toast("压缩失败", e.message || "未知错误", "crit");
  } finally {
    wbCompacting = false;
    if (wbSelected === id) wbRerenderPanel();
  }
}

async function wbRenameSession(id) {
  const input = document.getElementById("wbRenameInput");
  const title = input ? input.value.trim() : "";
  const msg = document.getElementById("wbRenameMsg");
  if (!title) { if (msg) show(msg, "标题不能为空"); return; }
  const item = wbItems.find(x => x.session.id === id);
  const dir = (item && item.session && item.session.directory) || "";
  const headers = appHeaders();
  if (dir) headers["x-opencode-directory"] = dir;
  const res = await api(`/api/opencode/session/${encodeURIComponent(id)}`, { method: "PATCH", headers, body: JSON.stringify({ title }) });
  if (!res.ok) { if (msg) show(msg, "改名失败 (" + res.status + ")"); return; }
  wbRenaming = false;
  // 本地同步标题，避免等待下一次轮询才看到新名字。
  const s = wbSessions.find(x => x.id === id);
  if (s) s.title = title;
  if (wbPanelData && wbPanelData.session && wbPanelData.session.id === id) wbPanelData.session.title = title;
  toast("已改名", title, "info");
  wbRerenderPanel();
  renderWbList();
}

async function deleteWbSession(id) {
  if (!confirm("确认删除该会话？此操作不可恢复。")) return;
  const res = await api(`/api/opencode/session/${encodeURIComponent(id)}`, { method: "DELETE", headers: appHeaders() });
  if (!res.ok) { toast("删除失败", "删除会话失败 (" + res.status + ")", "crit"); return; }
  wbSessions = wbSessions.filter(s => s.id !== id);
  delete wbStatuses[id];
  delete wbPending[id];
  if (wbSelected === id) closeWbPanel();
  renderWbList();
  toast("已删除", "会话已删除", "info");
}

/* ---- 决策面板「更多」下拉菜单 ---- */
// 更多操作与 App 聊天详情页菜单对齐：重新加载 / Fork / 撤销 / 重做 /
// 代码审查 / 查看更改 / 会话时间线 / 停止 / 分享 / 导出。
let wbMoreOpen = false;
function wbMoreMenuToggle(btn) {
  const wrap = btn.closest(".wb-more-wrap");
  const menu = wrap && wrap.querySelector("[data-wb-more-menu]");
  if (!menu) return;
  const opening = menu.classList.contains("hidden");
  wbMoreMenuClose();
  if (opening) {
    wbMoreOpen = true;
    menu.classList.remove("hidden");
  }
}
function wbMoreMenuClose() {
  if (!wbMoreOpen) return;
  wbMoreOpen = false;
  document.querySelectorAll("[data-wb-more-menu]").forEach(m => m.classList.add("hidden"));
}
document.addEventListener("click", (e) => {
  // 点击菜单外任意位置关闭下拉。
  if (e.target.closest("[data-wb-more-menu]") || e.target.closest("[data-wb-morebtn]")) return;
  wbMoreMenuClose();
});
function wbCurrentDir() {
  const item = wbItems.find(x => x.session.id === wbSelected);
  return ((item && item.session && item.session.directory) || (wbPanelData && wbPanelData.session && wbPanelData.session.directory) || "").toString();
}
function wbDirHeaders(dir) {
  const headers = appHeaders();
  if (dir) headers["x-opencode-directory"] = dir;
  return headers;
}
async function wbMoreMenuAction(action) {
  const id = wbSelected;
  if (!id) return;
  wbMoreMenuClose();
  if (action === "star") {
    if (wbStars.has(id)) wbStars.delete(id); else wbStars.add(id);
    wbSaveStars();
    wbRenderTagFilters();
    wbScheduleRenderList();
    renderWbPanel();
    toast(wbStars.has(id) ? "已收藏" : "已取消收藏", (wbSessions.find(x => x.id === id) || {}).title || id, "info");
    return;
  }
  if (action === "sess-tag") { wbEditSessionTags(id); return; }
  if (action === "reload") { if (wbChat) wbChat.reload(); refreshWbPanel(id, true); toast("已重新加载", "会话信息已刷新", "info"); return; }
  if (action === "fork") { await wbForkSession(id); return; }
  if (action === "undo") { await wbUndoWbSession(id); return; }
  if (action === "redo") { await wbRedoWbSession(id); return; }
  if (action === "review") { await wbRunWbCommand(id, "review"); return; }
  if (action === "diff") { await wbShowWbDiff(id); return; }
  if (action === "timeline") { await wbShowWbTimeline(id); return; }
  if (action === "abort") { await wbAbortWbSession(id); return; }
  if (action === "share") { await wbShareWbSession(id); return; }
  if (action === "unshare") { await wbUnshareWbSession(id); return; }
  if (action === "copylink") { await wbCopyWbShareLink(id); return; }
  if (action === "export-md") { await wbExportWbSession(id, "markdown"); return; }
  if (action === "export-json") { await wbExportWbSession(id, "json"); return; }
}
// 重新加载 / Fork / 撤销 / 重做 / 停止 / 分享等操作，契约与 App 的 OpenCodeApi 一致。
async function wbForkSession(id) {
  const dir = wbCurrentDir();
  const res = await api(`/api/opencode/session/${encodeURIComponent(id)}/fork`, { method: "POST", headers: wbDirHeaders(dir), body: JSON.stringify({}) });
  let data = {};
  try { data = await res.json(); } catch (_) {}
  if (!res.ok) { toast("Fork 失败", "未创建新会话 (" + res.status + ")", "crit"); return; }
  const nid = data.id || data.sessionID || "";
  toast("已 Fork", "新会话 " + nid, "info");
  await loadWorkbench();
  if (nid) openWbPanel(nid);
}
// 撤销上一条：寻找最近一条用户消息，revert 到它之前（与 App undoMessage 契约一致）。
async function wbUndoWbSession(id) {
  const dir = wbCurrentDir();
  let msgs = [];
  try {
    const mRes = await api(`/api/opencode/session/${encodeURIComponent(id)}/message?limit=50`, { headers: wbDirHeaders(dir) });
    if (mRes.ok) { const d = await mRes.json(); msgs = Array.isArray(d) ? d : (d.messages || []); }
  } catch (_) {}
  const lastUser = [...msgs].reverse().find(m => m.info && m.info.role === "user");
  if (!lastUser) { toast("撤销失败", "未找到可撤销的用户消息", "warn"); return; }
  const mid = lastUser.info.id || lastUser.id || "";
  if (!mid) { toast("撤销失败", "消息缺少 ID", "warn"); return; }
  const res = await api(`/api/opencode/session/${encodeURIComponent(id)}/revert`, { method: "POST", headers: wbDirHeaders(dir), body: JSON.stringify({ messageID: mid }) });
  if (!res.ok) { toast("撤销失败", "revert (" + res.status + ")", "crit"); return; }
  toast("已撤销", "已回退到上一条消息", "info");
  if (wbChat) { wbChat.revertTo = mid; wbChat.reload(); }
  loadWorkbench();
}
async function wbRedoWbSession(id) {
  const dir = wbCurrentDir();
  const res = await api(`/api/opencode/session/${encodeURIComponent(id)}/unrevert`, { method: "POST", headers: wbDirHeaders(dir) });
  if (!res.ok) { toast("重做失败", "unrevert (" + res.status + ")", "crit"); return; }
  toast("已重做", "已恢复上一条撤销", "info");
  if (wbChat) { wbChat.revertTo = null; wbChat.reload(); }
  loadWorkbench();
}
// 运行服务器端命令（如 /review），与 App executeCommand 契约一致。
// 返回布尔值：ChatView 据此决定是否撤回乐观气泡 / 把草稿还给输入框。
async function wbRunWbCommand(id, command, args) {
  const dir = wbCurrentDir();
  const res = await api(`/api/opencode/session/${encodeURIComponent(id)}/command`,
    { method: "POST", headers: wbDirHeaders(dir), body: JSON.stringify({ command, arguments: args || "" }) }).catch(() => null);
  if (!res || !res.ok) { toast("命令失败", `/${command} 未生效 (${res ? res.status : "网络错误"})`, "crit"); return false; }
  toast(`已运行 /${command}`, "命令已下发到会话", "info");
  wbStatuses[id] = { type: "busy" };
  renderWbList();
  if (wbChat) { wbChat.setWorking("命令执行中…"); wbChat.syncSendIcon(); }
  return true;
}
async function wbAbortWbSession(id) {
  if (!id) return;
  const dir = wbCurrentDir();
  const res = await api(`/api/opencode/session/${encodeURIComponent(id)}/abort`, { method: "POST", headers: wbDirHeaders(dir) });
  if (!res.ok) { toast("停止失败", "abort (" + res.status + ")", "crit"); return; }
  toast("已停止", "已发送停止信号", "info");
  wbStatuses[id] = { type: "idle" };
  renderWbList();
  if (wbChat && wbChat.sessionId === id) { wbChat.setWorking(""); wbChat.syncSendIcon(); }
  refreshWbPanel(id, true);
}
async function wbShareWbSession(id) {
  const dir = wbCurrentDir();
  const res = await api(`/api/opencode/session/${encodeURIComponent(id)}/share`, { method: "POST", headers: wbDirHeaders(dir) });
  let data = {};
  try { data = await res.json(); } catch (_) {}
  if (!res.ok) { toast("分享失败", "share (" + res.status + ")", "crit"); return; }
  const url = (data.share && data.share.url) || "";
  // 同步本地会话对象，避免重绘后菜单回到「分享会话」。
  const it = wbItems.find(x => x.session.id === id);
  if (it) it.session.share = data.share || { url };
  if (wbPanelData && wbPanelData.session) wbPanelData.session.share = data.share || { url };
  toast("已分享", url ? "分享链接已生成" : "分享成功", "info");
  if (url) wbCopyText(url, "分享链接已复制");
  wbRerenderPanel();
  loadWorkbench();
}
async function wbUnshareWbSession(id) {
  const dir = wbCurrentDir();
  const res = await api(`/api/opencode/session/${encodeURIComponent(id)}/share`, { method: "DELETE", headers: wbDirHeaders(dir) });
  if (!res.ok) { toast("取消分享失败", "unshare (" + res.status + ")", "crit"); return; }
  const it = wbItems.find(x => x.session.id === id);
  if (it) it.session.share = null;
  if (wbPanelData && wbPanelData.session) wbPanelData.session.share = null;
  toast("已取消分享", "分享链接已移除", "info");
  wbRerenderPanel();
  loadWorkbench();
}
async function wbCopyWbShareLink(id) {
  const it = wbItems.find(x => x.session.id === id);
  const s = (it && it.session && it.session.share && it.session.share.url) || (wbPanelData && wbPanelData.session && wbPanelData.session.share && wbPanelData.session.share.url) || "";
  if (!s) { toast("复制失败", "该会话未分享", "warn"); return; }
  wbCopyText(s, "分享链接已复制");
}
function wbCopyText(text, okTitle) {
  const done = () => toast(okTitle || "已复制", text.slice(0, 80), "info");
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(text).then(done).catch(() => wbCopyTextFallback(text, done));
  } else {
    wbCopyTextFallback(text, done);
  }
}
function wbCopyTextFallback(text, done) {
  const ta = document.createElement("textarea");
  ta.value = text;
  ta.style.position = "fixed"; ta.style.opacity = "0";
  document.body.appendChild(ta);
  ta.select();
  try { document.execCommand("copy"); done(); } catch (_) { toast("复制失败", "无法访问剪贴板", "warn"); }
  document.body.removeChild(ta);
}
// 导出会话为 Markdown / JSON（App 的 SessionExport 同款结构，浏览器端直接下载）。
async function wbExportWbSession(id, fmt) {
  const dir = wbCurrentDir();
  const headers = appHeaders();
  try {
    const [sRes, mRes] = await Promise.all([
      api(`/api/opencode/session/${encodeURIComponent(id)}`, { headers: wbDirHeaders(dir) }),
      api(`/api/opencode/session/${encodeURIComponent(id)}/message`, { headers: wbDirHeaders(dir) }),
    ]);
    const session = sRes.ok ? await sRes.json() : { id };
    let msgs = [];
    if (mRes.ok) { const d = await mRes.json(); msgs = Array.isArray(d) ? d : (d.messages || []); }
    const title = (session.title || session.slug || id).toString();
    const safe = title.replace(/[^\w\u4e00-\u9fa5-]+/g, "_").slice(0, 60) || "session";
    if (fmt === "json") {
      const content = JSON.stringify({ info: session, messages: msgs }, null, 2);
      wbDownFile(safe + ".json", content, "application/json");
    } else {
      let md = `# ${title}\n\n`;
      for (const m of msgs) {
        const role = (m.info && m.info.role) || "user";
        const label = role === "assistant" ? "Assistant" : role === "tool" ? "Tool" : "User";
        const text = (m.parts || []).filter(p => p && p.type === "text" && p.text && !p.synthetic && !p.ignored).map(p => p.text).join("\n").trim();
        if (!text) continue;
        md += `## ${label}\n\n${text}\n\n`;
      }
      wbDownFile(safe + ".md", md.trim() + "\n", "text/markdown");
    }
    toast("已导出", safe + "." + (fmt === "json" ? "json" : "md"), "info");
  } catch (e) {
    if (e.message !== "unauthorized") toast("导出失败", e.message || "未知错误", "crit");
  }
}
function wbDownFile(name, content, type) {
  const blob = new Blob([content], { type });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url; a.download = name;
  a.click();
  URL.revokeObjectURL(url);
}
// 查看更改：GET /session/{id}/diff，弹窗展示每个文件 +增删行数 + before/after 代码块。
async function wbShowWbDiff(id) {
  const dir = wbCurrentDir();
  const res = await api(`/api/opencode/session/${encodeURIComponent(id)}/diff`, { headers: wbDirHeaders(dir) });
  let list = [];
  if (res.ok) { try { const d = await res.json(); list = Array.isArray(d) ? d : (d.diffs || d.files || []); } catch (_) {} }
  if (!res.ok && !list.length) { toast("查看更改失败", "diff (" + res.status + ")", "crit"); return; }
  let html;
  if (!list.length) {
    html = `<div class="wb-diff-empty">该会话暂无文件更改</div>`;
  } else {
    html = list.map((df, i) => {
      const file = df.file || df.path || "";
      const adds = df.additions || 0;
      const dels = df.deletions || 0;
      const before = df.before || "";
      const after = df.after || "";
      const status = (df.status || "modified").toLowerCase();
      const statusCls = status === "added" ? "df-added" : status === "deleted" ? "df-deleted" : "df-modified";
      const statusLabel = status === "added" ? "新增" : status === "deleted" ? "删除" : "修改";
      return `<details class="wb-diff-item" ${i === 0 ? "open" : ""}>
        <summary><b>${escapeHtml(file)}</b><span class="meta"><span class="add">+${adds}</span> <span class="del">-${dels}</span> <span class="badge ${statusCls}">${escapeHtml(statusLabel)}</span></span></summary>
        <div class="wb-diff-body">
          ${before ? `<div class="col before"><h5>改动前 <span class="del">-${dels}</span></h5><pre>${escapeHtml(before)}</pre></div>` : ""}
          ${after ? `<div class="col after"><h5>改动后 <span class="add">+${adds}</span></h5><pre>${escapeHtml(after)}</pre></div>` : ""}
        </div>
      </details>`;
    }).join("");
  }
  wbModalOpen("查看更改（" + list.length + "）", html);
}
// 会话时间线：由当前消息 parts 重建（工具调用 / 授权 / 提问 / 子代理 / agent / 待决项）。
function wbTimelineFromMessages(msgs) {
  const entries = [];
  let toolSeq = 0;
  let miscSeq = 0;
  for (const m of msgs) {
    const created = (m.info && m.info.time && m.info.time.created) || 0;
    const parts = m.parts || [];
    for (const p of parts) {
      if (!p || typeof p !== "object") continue;
      const type = p.type || "";
      if (type === "tool") {
        toolSeq++;
        const state = p.state || {};
        // 上游 ToolState 以 state.status 判别 pending|running|completed|error，
        // 读 state.type 会恒为 undefined → 所有工具都显示「已完成」。
        const st = state.status || state.type || "completed";
        const status = st === "running" ? "busy" : (st === "error" || st === "failed") ? "busy" : "idle";
        let summary = p.tool || "";
        const input = state.input || {};
        try {
          const ks = Object.keys(input).slice(0, 3);
          if (ks.length) summary += " · " + ks.map(k => {
            let v = input[k];
            if (typeof v === "object") v = JSON.stringify(v);
            v = String(v).replace(/\s+/g, " ").trim();
            return `${k}: ${v.length > 48 ? v.slice(0, 45) + "…" : v}`;
          }).join(" · ");
        } catch (_) {}
        entries.push({ icon: status, st: status === "busy" ? (st === "running" ? "运行中" : "失败") : "已完成", title: "工具调用 · " + p.tool, summary, ts: (state.time && state.time.start) || created, key: "tool" + toolSeq });
      } else if (type === "permission") {
        entries.push({ icon: "question", st: "待授权", title: "授权请求", summary: p.message || "", ts: created, key: "perm" + created + "_" + (miscSeq++) });
      } else if (type === "question") {
        entries.push({ icon: "question", st: "提问", title: "待决问题", summary: p.question || "", ts: created, key: "q" + created + "_" + (miscSeq++) });
      } else if (type === "subtask" || type === "agent") {
        entries.push({ icon: "info", st: "子任务", title: type === "subtask" ? (p.description || p.prompt || "") : (p.name || "agent"), summary: "", ts: created, key: "sub" + created + "_" + (miscSeq++) });
      } else if (type === "todo" || type === "plan") {
        const todos = Array.isArray(p.todos) ? p.todos : (Array.isArray(p.items) ? p.items : []);
        const done = todos.filter(t => t.status === "completed" || t.status === "done" || t.completed).length;
        entries.push({
          icon: "info", st: "计划", title: "执行计划 · " + done + "/" + todos.length,
          summary: todos.map(t => (t.content || t.title || t.text || "")).filter(Boolean).join("；").slice(0, 180),
          ts: created, key: "todo" + created + "_" + (miscSeq++),
        });
      }
    }
  }
  // 待授权操作 / 待决问题也并入时间线（当前尚未处理的）。
  for (const perm of (wbPermissions[wbSelected] || [])) {
    entries.push({ icon: "question", st: "待授权", title: "授权请求 · " + (perm.permission || "操作"), summary: (perm.patterns || []).join("、"), ts: 0, key: "permpend" + perm.id });
  }
  for (const q of (wbPending[wbSelected] || [])) {
    const first = (q.questions || [])[0] || {};
    entries.push({ icon: "question", st: "待回答", title: first.header || "待决问题", summary: first.question || "", ts: 0, key: "qpend" + q.id });
  }
  entries.sort((a, b) => (a.ts || 0) - (b.ts || 0));
  return entries;
}
async function wbShowWbTimeline(id) {
  const dir = wbCurrentDir();
  let msgs = [];
  try {
    const mRes = await api(`/api/opencode/session/${encodeURIComponent(id)}/message?limit=500`, { headers: wbDirHeaders(dir) });
    if (mRes.ok) { const d = await mRes.json(); msgs = Array.isArray(d) ? d : (d.messages || []); }
  } catch (_) {}
  const entries = wbTimelineFromMessages(msgs);
  let html;
  if (!entries.length) {
    html = `<div class="wb-diff-empty">时间线为空（等待工具调用 / 授权 / 提问）</div>`;
  } else {
    html = entries.map(e => `<div class="wb-tl">
      <span class="ico ${e.icon}">${e.icon === "busy" ? "⚙" : e.icon === "question" ? "?" : "▤"}</span>
      <div class="t">
        <b>${escapeHtml(e.title)}</b> <span class="badge ${e.icon === "busy" ? "busy" : e.icon === "question" ? "question" : "idle"} st">${escapeHtml(e.st)}</span>
        ${e.summary ? `<div class="sum">${escapeHtml(e.summary)}</div>` : ""}
        ${e.ts ? `<div class="time">${wbTimeFormat(e.ts)}</div>` : ""}
      </div>
    </div>`).join("");
  }
  wbModalOpen("会话时间线（" + entries.length + "）", html);
}
function wbModalOpen(title, html) {
  document.getElementById("wbModalTitle").textContent = title;
  const body = document.getElementById("wbModalBody");
  body.onclick = null;   // 清掉上一个弹窗内容挂的委托（如标签编辑），避免串台
  body.innerHTML = html;
  const card = document.querySelector("#wbModal .modal-card");
  if (window.innerWidth <= 768) card && card.classList.add("wb-modal-scroll");
  else card && card.classList.remove("wb-modal-scroll");
  document.getElementById("wbModal").classList.remove("hidden");
}
function wbModalClose() {
  document.getElementById("wbModal").classList.add("hidden");
  document.getElementById("wbModalBody").innerHTML = "";
}
document.getElementById("wbFilter").addEventListener("input", renderWbList);

/* ---------- 实时事件流（SSE 中继） ---------- */
const STREAM_MAX_LINES = 500;
let streamES = null;

function streamState(text, color) {
  const el = document.getElementById("streamState");
  el.textContent = text;
  el.style.color = color || "var(--ink-subtle)";
}
function appendStreamLine(text) {
  const box = document.getElementById("streamBox");
  box.insertAdjacentHTML("beforeend",
    `<div class="line"><span class="t">${new Date().toLocaleTimeString()}</span> ${escapeHtml(text)}</div>`);
  while (box.children.length > STREAM_MAX_LINES) box.removeChild(box.firstChild);
  box.scrollTop = box.scrollHeight;
}
function ensureStream(force) {
  const t = localStorage.getItem(APP_TOKEN_KEY) || "";
  if (!session && !t) {
    streamState("未连接");
    show(document.getElementById("streamMsg"), "先登录，或在左下角填入 APP Token 并保存，再打开事件流");
    return;
  }
  if (streamES && !force) return;
  if (streamES) { streamES.close(); streamES = null; }
  document.getElementById("streamToggle").textContent = "停止";
  // EventSource 不能设自定义头。浏览器页面优先用登录会话（登录后一定有效），
  // 只有未登录时才回退 APP token——本地残留的无效 token 不会再把页面打回
  // 401 断开（后端已接受 ?session= 校验）。
  const cred = session ? "session=" + encodeURIComponent(session) : "token=" + encodeURIComponent(t);
  const es = new EventSource("/api/stream?" + cred);
  streamES = es;
  streamState("连接中…");
  show(document.getElementById("streamMsg"), "");
  es.addEventListener("connected", () => streamState("已连接", "var(--success)"));
  es.onmessage = (ev) => {
    let pretty = ev.data;
    try { pretty = JSON.stringify(JSON.parse(pretty)); } catch (_) {}
    appendStreamLine(pretty);
  };
  es.onerror = () => {
    if (streamES === es) streamState("已断开，等待后端重连…", "var(--warning)");
  };
}
function toggleStream() {
  if (streamES) {
    streamES.close();
    streamES = null;
    streamState("已停止");
    document.getElementById("streamToggle").textContent = "打开";
  } else {
    ensureStream(true);
  }
}
function clearStream() {
  document.getElementById("streamBox").innerHTML = "";
}

/* ---------- 智能测试（intel） ---------- */
let intelCurrentProject = 0;
let intelMgrProject = null;
let intelEndpointsCache = [];
let intelSummaryCommands = [];
let intelSummaryDescription = "";
const INTEL_TYPE_LABELS = { java: "Java", android: "Android", ios: "iOS", go: "Go", web: "Web", node: "Node" };

function toggleIntelSource() {
  const src = document.getElementById("intelSource").value;
  const ph = document.getElementById("intelPath");
  const refInput = document.getElementById("intelGitRef");
  ph.placeholder = src === "git" ? "https://gitlab.example.com/group/repo.git" : "/path/to/repo";
  refInput.classList.toggle("hidden", src !== "git");
}

// intel tab switcher（切到某 Tab 时按需加载对应分析结果）
document.addEventListener("DOMContentLoaded", () => {
  const tabLoaders = {
    summary: loadIntelSummary, features: loadIntelFeatures, cases: loadIntelCases, findings: loadIntelFindings,
    issues: loadIntelIssues, fixes: loadIntelFixes, runs: loadIntelRuns, impact: loadIntelImpact,
    overview: loadIntelOverview, bindings: loadIntelBindings, env: loadIntelEnv,
    pending: loadIntelPending,
  };
  document.querySelectorAll(".intel-tab").forEach(t => {
    t.addEventListener("click", () => {
      document.querySelectorAll(".intel-tab").forEach(b => b.classList.remove("active"));
      t.classList.add("active");
      document.querySelectorAll(".intel-panel").forEach(p => p.classList.add("hidden"));
      const panel = document.getElementById("intelPanel-" + t.dataset.tab);
      if (panel) panel.classList.remove("hidden");
      const loader = tabLoaders[t.dataset.tab];
      if (loader && intelCurrentProject) loader(intelCurrentProject);
    });
  });
});

// intelStatusBadge renders the project analysis lifecycle status. Backend sets
// analysisStatus = "running" during a background analyze, "ok" after success,
// "failed" on error; ""/absent means never analyzed.
function intelStatusBadge(p) {
  const st = p.analysisStatus || "";
  if (st === "running") return `<span class="intel-status running">分析中…</span>`;
  if (st === "failed") return `<span class="intel-status failed">分析失败</span>`;
  if (st === "ok" || p.analyzedAt) return `<span class="intel-status analyzed">已分析</span>`;
  return `<span class="intel-status pending">待分析</span>`;
}

async function loadIntelProjects() {
  const res = await api("/api/intel/projects", { headers: appHeaders() });
  const data = await res.json();
  if (!res.ok) { show(document.getElementById("intelMsg"), data.error || "加载失败"); return; }
  const list = data.projects || [];
  const tb = document.querySelector("#intelProjectTable tbody");
  tb.innerHTML = "";
  let analyzed = 0;
  for (const p of list) {
    const loc = p.source === "git" ? p.gitUrl : p.localPath;
    if (p.analyzedAt || p.analysisStatus === "ok") analyzed++;
    tb.insertAdjacentHTML("beforeend", `<tr>
      <td class="clip" title="${escapeHtml(p.name)}"><strong>${escapeHtml(p.name)}</strong></td>
      <td><span class="badge">${escapeHtml(p.source)}</span></td>
      <td class="mono clip muted" title="${escapeHtml(loc)}">${escapeHtml(loc || "-")}</td>
      <td>${intelStatusBadge(p)}</td>
      <td>
        <button class="ghost sm" data-id="${p.id}">详情</button>
        <button class="tertiary sm" data-del="${p.id}">删除</button>
      </td>
    </tr>`);
  }
  tb.onclick = (e) => {
    const btn = e.target.closest("button[data-id]");
    if (btn) { openIntelDetail(Number(btn.dataset.id)); return; }
    const del = e.target.closest("button[data-del]");
    if (del) { deleteIntelProject(Number(del.dataset.del)); }
  };
  if (!list.length) tb.insertAdjacentHTML("beforeend", `<tr><td colspan="5" class="muted" style="text-align:center;padding:24px">暂无测试项目，请在上方添加</td></tr>`);
  // stats
  document.getElementById("intelStatTotal").textContent = list.length;
  document.getElementById("intelStatAnalyzed").textContent = analyzed;
  document.getElementById("intelStatPending").textContent = list.length - analyzed;
}

async function createIntelProject() {
  const src = document.getElementById("intelSource").value;
  const body = {
    name: document.getElementById("intelName").value.trim(),
    source: src,
    localPath: src === "local" ? document.getElementById("intelPath").value.trim() : "",
    gitUrl: src === "git" ? document.getElementById("intelPath").value.trim() : "",
    gitRef: src === "git" ? document.getElementById("intelGitRef").value.trim() : "",
    description: document.getElementById("intelDescription") ? document.getElementById("intelDescription").value.trim() : "",
  };
  if (src === "local" && !body.localPath) { show(document.getElementById("intelMsg"), "请填写本地路径"); return; }
  if (src === "git" && !body.gitUrl) { show(document.getElementById("intelMsg"), "请填写 Git URL"); return; }
  const res = await api("/api/intel/projects", { method: "POST", headers: appHeaders(), body: JSON.stringify(body) });
  const data = await res.json();
  if (!res.ok) { show(document.getElementById("intelMsg"), data.error || "添加失败"); return; }
  show(document.getElementById("intelMsg"), "已添加：" + escapeHtml(data.name) + "（自动分析中，稍后刷新可见结果）");
  document.getElementById("intelMsg").classList.add("ok");
  document.getElementById("intelName").value = "";
  document.getElementById("intelPath").value = "";
  document.getElementById("intelGitRef").value = "";
  if (document.getElementById("intelDescription")) document.getElementById("intelDescription").value = "";
  loadIntelProjects();
}

async function deleteIntelProject(id) {
  if (!confirm("删除该项目及其全部测试情报？此操作不可撤销。")) return;
  await api("/api/intel/projects/" + id, { method: "DELETE", headers: appHeaders() });
  loadIntelProjects();
}

// 列表页「详情」→ 跳转到独立的项目详情页
function openIntelDetail(id) {
  intelCurrentProject = id;
  localStorage.setItem("intelDetailProject", String(id));
  showPageIntelDetail();
  loadIntelDetail(id);
}

// 直接切换到详情页（不经过左侧导航高亮）
function showPageIntelDetail() {
  document.querySelectorAll(".page").forEach(p => p.classList.add("hidden"));
  document.getElementById("page-intel-detail").classList.remove("hidden");
  document.getElementById("pageTitle").textContent = "智能测试 · 项目详情";
  document.querySelectorAll("#nav button").forEach(b => b.classList.remove("active"));
  const content = document.querySelector(".content");
  if (content) { content.classList.remove("wb-tight"); content.classList.add("full"); }
}

// 返回列表页
function backToIntelList() {
  localStorage.removeItem("intelDetailProject");
  intelCurrentProject = 0;
  switchPage("intel");
}

async function loadIntelDetail(id) {
  const res = await api("/api/intel/projects/" + id, { headers: appHeaders() });
  const data = await res.json();
  if (!res.ok) { document.getElementById("intelDetailName").textContent = "加载失败"; return; }
  const p = data.project || {};
  intelMgrProject = p;
  document.getElementById("intelDetailName").textContent = p.name || "";
  const loc = p.source === "git" ? p.gitUrl : p.localPath;
  const ref = p.source === "git" ? (p.gitRef || "默认分支") : "-";
  document.getElementById("intelDetailMeta").textContent =
    `来源：${p.source === "git" ? "Git 仓库" : "本地目录"}  ·  关联源码${p.source === "git" ? "仓库" : "目录"}：${loc || "-"}  ·  分支：${ref}  ·  最近分析：${p.analyzedAt ? new Date(p.analyzedAt).toLocaleString() : "未分析"}`;

  const mtb = document.querySelector("#intelModuleTable tbody");
  mtb.innerHTML = "";
  for (const m of data.modules || []) {
    mtb.insertAdjacentHTML("beforeend", `<tr>
      <td class="mono clip" title="${escapeHtml(m.relPath)}">${escapeHtml(m.relPath || ".")}</td>
      <td><span class="badge type-badge">${escapeHtml(INTEL_TYPE_LABELS[m.kindType] || m.kindType || "-")}</span></td>
      <td>${escapeHtml(m.kindRole || "-")}</td>
      <td>${escapeHtml(m.buildTool || "-")}</td>
      <td class="row" style="gap:4px">
        <button class="ghost sm" onclick="showIntelModuleDetail(${m.id})">详情</button>
      </td>
    </tr>`);
  }
  if (!(data.modules || []).length) mtb.insertAdjacentHTML("beforeend", `<tr><td colspan="5" class="muted" style="text-align:center;padding:16px">暂无子模块（分析后自动识别）</td></tr>`);
  document.getElementById("intelDetailStatModules").textContent = (data.modules || []).length;
  await loadIntelContracts(id);
  await loadIntelSummary(id);
  initIntelChats();
}

// 项目管理：编辑项目信息并设置关联源码目录 / 仓库。
// 打开前异步加载已知工作台项目目录，供输入框下拉快捷选择。
function openIntelProjectManage() {
  const p = intelMgrProject || {};
  document.getElementById("intelMgrName").value = p.name || "";
  document.getElementById("intelMgrSource").value = p.source === "git" ? "git" : "local";
  document.getElementById("intelMgrPath").value = p.source === "git" ? (p.gitUrl || "") : (p.localPath || "");
  document.getElementById("intelMgrGitRef").value = p.gitRef || "";
  document.getElementById("intelMgrDescription").value = p.description || "";
  toggleIntelMgrSource();
  document.getElementById("intelMgrMsg").textContent = "";
  document.getElementById("intelProjectManage").classList.remove("hidden");
  // 已知目录建议（来自工作台项目 / 会话目录），仅作输入提示。
  api("/api/projects", { headers: appHeaders() })
    .then(r => r.json())
    .then(d => {
      const dl = document.getElementById("intelMgrDirs");
      if (!dl) return;
      const dirs = (d.projects || []).map(x => x.directory).filter(Boolean);
      dl.innerHTML = [...new Set(dirs)].map(x => `<option value="${escapeHtml(x)}"></option>`).join("");
    })
    .catch(() => {});
  // 加载已关联源码（多端多仓库）列表。
  api(`/api/intel/projects/${intelCurrentProject}/sources`, { headers: appHeaders() })
    .then(r => r.json())
    .then(d => {
      const box = document.getElementById("intelSrcRows");
      if (!box) return;
      box.innerHTML = "";
      for (const src of (d.sources || [])) addIntelSourceRow(src);
      if (!(d.sources || []).length) addIntelSourceRow();
    })
    .catch(() => {});
}

// 新增一行关联源码（端 + 来源 + 目录/URL + 分支），src 为已存在项时回填。
function addIntelSourceRow(src) {
  const box = document.getElementById("intelSrcRows");
  if (!box) return;
  const row = document.createElement("div");
  row.className = "intel-src-row";
  row.style.cssText = "display:flex;gap:8px;align-items:center;flex-wrap:wrap";
  const s = src || {};
  const source = s.source === "git" ? "git" : "local";
  row.innerHTML = `
    <input type="text" class="s-end" placeholder="端（如 android / ios / web）" style="flex:0.9" value="${escapeHtml(s.endName || "")}">
    <select class="s-source" onchange="toggleIntelSrcRow(this)">
      <option value="local"${source === "local" ? " selected" : ""}>本地</option>
      <option value="git"${source === "git" ? " selected" : ""}>Git</option>
    </select>
    <input type="text" class="s-path" placeholder="/path/to/repo 或 git 仓库 URL" style="flex:2" value="${escapeHtml(source === "git" ? (s.gitUrl || "") : (s.localPath || ""))}" list="intelMgrDirs">
    <input type="text" class="s-ref" placeholder="分支（可选）" style="flex:0.9" value="${escapeHtml(s.gitRef || "")}">
    <button class="tertiary sm" onclick="this.closest('.intel-src-row').remove()">删除</button>`;
  toggleIntelSrcRow(row.querySelector(".s-source"));
  box.appendChild(row);
}

function toggleIntelSrcRow(sel) {
  const row = sel.closest(".intel-src-row");
  if (!row) return;
  const ref = row.querySelector(".s-ref");
  if (ref) ref.classList.toggle("hidden", sel.value !== "git");
}

function closeIntelProjectManage() {
  document.getElementById("intelProjectManage").classList.add("hidden");
  document.getElementById("intelMgrMsg").textContent = "";
}

function toggleIntelMgrSource() {
  const src = document.getElementById("intelMgrSource").value;
  const refInput = document.getElementById("intelMgrGitRef");
  refInput.classList.toggle("hidden", src !== "git");
}

async function saveIntelProjectManage() {
  const src = document.getElementById("intelMgrSource").value;
  const body = {
    name: document.getElementById("intelMgrName").value.trim(),
    source: src,
    localPath: src === "local" ? document.getElementById("intelMgrPath").value.trim() : "",
    gitUrl: src === "git" ? document.getElementById("intelMgrPath").value.trim() : "",
    gitRef: src === "git" ? document.getElementById("intelMgrGitRef").value.trim() : "",
    description: document.getElementById("intelMgrDescription") ? document.getElementById("intelMgrDescription").value.trim() : "",
  };
  if (src === "local" && !body.localPath) { show(document.getElementById("intelMgrMsg"), "请填写源码目录路径"); return; }
  if (src === "git" && !body.gitUrl) { show(document.getElementById("intelMgrMsg"), "请填写 Git 仓库 URL"); return; }
  const prev = intelMgrProject || {};
  const changed = prev.source !== src || prev.localPath !== body.localPath || prev.gitUrl !== body.gitUrl;
  const res = await api("/api/intel/projects/" + intelCurrentProject, { method: "PUT", headers: appHeaders(), body: JSON.stringify(body) });
  const data = await res.json();
  if (!res.ok) { show(document.getElementById("intelMgrMsg"), data.error || "保存失败"); return; }
  // 保存关联源码（多端多仓库）：收集每一行，整表替换。
  const rows = [...document.querySelectorAll("#intelSrcRows .intel-src-row")].map(row => {
    const rsrc = row.querySelector(".s-source").value;
    return {
      endName: row.querySelector(".s-end").value.trim(),
      source: rsrc,
      localPath: rsrc === "local" ? row.querySelector(".s-path").value.trim() : "",
      gitUrl: rsrc === "git" ? row.querySelector(".s-path").value.trim() : "",
      gitRef: rsrc === "git" ? row.querySelector(".s-ref").value.trim() : "",
    };
  });
  const srcRes = await api(`/api/intel/projects/${intelCurrentProject}/sources`, { method: "PUT", headers: appHeaders(), body: JSON.stringify({ sources: rows }) });
  const srcData = await srcRes.json();
  if (!srcRes.ok) { show(document.getElementById("intelMgrMsg"), srcData.error || "关联源码保存失败"); return; }
  closeIntelProjectManage();
  loadIntelDetail(intelCurrentProject);
  loadIntelProjects();
  if (changed) toast("已保存", "来源/目录已变更，将自动进行增量分析", "info");
}

// 编辑项目级命令白名单：每行一条命令，运行测试时按工具匹配执行（argv 不经 shell）。
function editIntelProjectCommands() {
  const pid = intelCurrentProject;
  if (!pid) return;
  const current = (intelSummaryCommands || []).slice();
  const input = prompt("每行一条命令（命令白名单，运行测试时与模块构建工具匹配后执行）", current.join("\n"));
  if (input === null) return;
  const list = input.split("\n").map(s => s.trim()).filter(s => s);
  api("/api/intel/projects/" + pid, { method: "PUT", headers: appHeaders(), body: JSON.stringify({ commandsJson: JSON.stringify(list) }) })
    .then(r => r.json())
    .then(d => { if (d.error) alert(d.error); else if (intelCurrentProject) loadIntelDetail(intelCurrentProject); });
}

// 编辑项目描述：展示在项目概览页，便于快速了解项目用途与范围。
function editIntelProjectDescription() {
  const pid = intelCurrentProject;
  if (!pid) return;
  const input = prompt("项目描述（展示在项目概览，用于快速了解项目用途与范围）", intelSummaryDescription || "");
  if (input === null) return;
  api("/api/intel/projects/" + pid, { method: "PUT", headers: appHeaders(), body: JSON.stringify({ description: input.trim() }) })
    .then(r => r.json())
    .then(d => { if (d.error) alert(d.error); else if (intelCurrentProject) loadIntelDetail(intelCurrentProject); });
}

async function showIntelModuleDetail(moduleId) {
  const box = document.getElementById("intelModuleDetail");
  if (!box) return;
  box.innerHTML = `<div class="muted" style="padding:8px">加载中…</div>`;
  const res = await api("/api/intel/modules/" + moduleId, { headers: appHeaders() });
  const data = await res.json();
  if (!res.ok) { box.innerHTML = `<div class="muted" style="padding:8px">${escapeHtml(data.error || "加载失败")}</div>`; return; }
  const m = data.module || {};
  const s = data.stats || {};
  const typeLabel = INTEL_TYPE_LABELS[m.kindType] || m.kindType || "-";
  box.innerHTML = `<div class="card" style="margin:0">
    <div class="row" style="justify-content:space-between;align-items:center">
      <strong style="font-size:14px">${escapeHtml(m.relPath || ".")}</strong>
      <div class="row" style="gap:6px">
        <span class="intel-status analyzed">${escapeHtml(typeLabel)}</span>
        <button class="ghost sm" onclick="reSummarizeIntelModule(${m.id})">重新生成摘要</button>
        <button class="ghost sm" onclick="editIntelModuleOverrides('${m.id}', '${escapeHtml(m.kindRole || "")}', '${escapeHtml(m.summary || "")}')">编辑</button>
      </div>
    </div>
    ${m.summary ? `<div class="muted" style="font-size:12px;margin-top:8px;padding:8px;background:var(--surface-2);border:1px solid var(--hairline);border-radius:var(--r-sm)">${escapeHtml(m.summary)}</div>` : `<div class="muted" style="font-size:11px;margin-top:8px">（未配置编排 LLM，暂无 AI 摘要；仅确定性统计）</div>`}
    <div class="stat-grid" style="margin-top:10px">
      <div class="stat"><div class="k">接口契约</div><div class="v">${s.endpoints || 0}</div></div>
      <div class="stat"><div class="k">实体 / 列</div><div class="v">${s.entities || 0}</div></div>
      <div class="stat"><div class="k">测试用例</div><div class="v">${s.cases || 0}</div></div>
      <div class="stat"><div class="k">构建工具</div><div class="v" style="font-size:13px">${escapeHtml(m.buildTool || "-")}</div></div>
    </div>
    <div class="muted" style="font-size:12px;margin-top:8px">角色 ${escapeHtml(m.kindRole || "-")} · 最近测试 SHA <span class="mono">${escapeHtml((m.lastTestedSha || "").slice(0, 8) || "-")}</span> · 分析时间 ${m.analyzedAt ? new Date(m.analyzedAt).toLocaleString() : "-"}</div>
  </div>`;
}

async function reSummarizeIntelModule(moduleId) {
  const box = document.getElementById("intelModuleDetail");
  if (box) box.innerHTML = `<div class="muted" style="padding:8px">AI 重新生成中…</div>`;
  const res = await api("/api/intel/modules/" + moduleId + "/resummarize", { method: "POST", headers: appHeaders(), body: "{}" });
  const data = await res.json();
  if (!res.ok) { if (box) box.innerHTML = `<div class="muted" style="padding:8px">${escapeHtml(data.error || "重新生成失败")}</div>`; return; }
  showIntelModuleDetail(moduleId);
}

async function editIntelModuleOverrides(moduleId, role, summary) {
  const newRole = prompt("角色（留空保持不变）", role || "");
  if (newRole === null) return;
  const newSummary = prompt("摘要（留空保持不变；人工修改会存入覆写层，重新分析不被覆盖）", summary || "");
  if (newSummary === null) return;
  const body = {};
  if (newRole.trim() && newRole.trim() !== role) body.role = newRole.trim();
  if (newSummary.trim() && newSummary.trim() !== summary) body.summary = newSummary.trim();
  if (!Object.keys(body).length) return;
  const res = await api("/api/intel/modules/" + moduleId + "/overrides", { method: "PUT", headers: appHeaders(), body: JSON.stringify(body) });
  const data = await res.json();
  if (!res.ok) { alert(data.error || "保存失败"); return; }
  showIntelModuleDetail(moduleId);
}

// 项目概览：展示项目关键画像（来源/路径/分析时间、概览统计、命令白名单、最近运行）
async function loadIntelSummary(id) {
  const wrap = document.getElementById("intelSummaryList");
  if (!wrap) return;
  wrap.innerHTML = `<div class="muted" style="text-align:center;padding:16px">加载中…</div>`;
  const res = await api("/api/intel/projects/" + id, { headers: appHeaders() });
  const data = await res.json();
  if (!res.ok) { wrap.innerHTML = `<div class="muted" style="text-align:center;padding:16px">${escapeHtml(data.error || "加载失败")}</div>`; return; }
  const p = data.project || {};
  const loc = p.source === "git" ? p.gitUrl : p.localPath;
  intelSummaryCommands = [];
  try { intelSummaryCommands = JSON.parse(p.commandsJson || "[]"); } catch (_) {}
  intelSummaryDescription = p.description || "";

  // 并行拉取各维度统计
  const [epRes, entRes, featRes, caseRes, findRes, issueRes, fixRes, runRes, ovRes, pendRes] = await Promise.all([
    api("/api/intel/endpoints?projectId=" + id, { headers: appHeaders() }),
    api("/api/intel/entities?projectId=" + id, { headers: appHeaders() }),
    api("/api/intel/features?projectId=" + id, { headers: appHeaders() }),
    api("/api/intel/test-cases?projectId=" + id, { headers: appHeaders() }),
    api("/api/intel/findings?projectId=" + id, { headers: appHeaders() }),
    api("/api/intel/issues?projectId=" + id, { headers: appHeaders() }),
    api("/api/intel/fixes?projectId=" + id, { headers: appHeaders() }),
    api("/api/intel/runs?projectId=" + id, { headers: appHeaders() }),
    api("/api/intel/overview?projectId=" + id, { headers: appHeaders() }),
    api("/api/intel/pending?projectId=" + id, { headers: appHeaders() }),
  ]);
  const eps = (await epRes.json()).endpoints || [];
  const ents = (await entRes.json()).entities || [];
  const feats = (await featRes.json()).features || [];
  const cases = (await caseRes.json()).testCases || [];
  const findings = (await findRes.json()).findings || [];
  const issues = (await issueRes.json()).issues || [];
  const fixes = (await fixRes.json()).fixes || [];
  const runs = (await runRes.json()).runs || [];
  const ovData = await ovRes.json();
  const raw = ovData.overview || null;
  const pend = (await pendRes.json()).pending || [];
  const tableSet = new Set(ents.map(e => e.table).filter(Boolean));
  let envs = [], deps = [];
  if (raw) { try { envs = JSON.parse(raw.envJson || "[]"); } catch (_) {} try { deps = JSON.parse(raw.depsJson || "[]"); } catch (_) {} }

  const cmdsHtml = intelSummaryCommands.length
    ? intelSummaryCommands.map(c => `<span class="mono" style="font-size:11.5px;padding:1px 6px;background:var(--surface-2);border:1px solid var(--hairline);border-radius:4px">${escapeHtml(c)}</span>`).join(" ")
    : `<span class="muted" style="font-size:12px">（未配置，将使用各工具默认命令）</span>`;

  const openFind = findings.filter(f => (f.status || "open") === "open").length;
  const lastRun = runs.length ? runs[0] : null;
  const runStats = { passed: 0, failed: 0 };
  for (const r of runs) { if (r.status === "passed") runStats.passed++; else if (r.status === "failed") runStats.failed++; }

  wrap.innerHTML = `
    <div class="card" style="margin:0">
      <div class="row" style="justify-content:space-between">
        <strong style="font-size:13px">项目画像</strong>
        <div class="row" style="gap:6px">
          ${intelStatusBadge(p)}
          <button class="ghost sm" onclick="editIntelProjectDescription()">编辑描述</button>
        </div>
      </div>
      ${p.description ? `<div style="margin-top:8px;padding:10px 12px;background:var(--surface-2);border:1px solid var(--hairline);border-radius:var(--r-sm);font-size:12.5px;line-height:1.6;white-space:pre-wrap">${escapeHtml(p.description)}</div>` : `<div class="muted" style="font-size:12px;margin-top:8px">（暂无项目描述，点击「编辑描述」补充）</div>`}
      <div class="muted" style="font-size:12px;margin-top:8px;display:flex;flex-direction:column;gap:4px">
        <div>来源：${escapeHtml(p.source || "-")} · 路径：<span class="mono">${escapeHtml(loc || "-")}</span></div>
        <div>Git 分支：<span class="mono">${escapeHtml(p.gitRef || "默认")}</span> · 最近分析：${p.analyzedAt ? new Date(p.analyzedAt).toLocaleString() : "未分析"}</div>
        <div>最近测试 SHA：<span class="mono">${escapeHtml((p.lastTestedSha || "").slice(0, 8) || "-")}</span> · 快照 SHA：<span class="mono">${escapeHtml((p.snapshotSha || "").slice(0, 8) || "-")}</span></div>
      </div>
    </div>

    <div class="card" style="margin:0">
      <div class="row" style="justify-content:space-between">
        <strong style="font-size:13px">命令白名单（项目级）</strong>
        <button class="ghost sm" onclick="editIntelProjectCommands()">编辑</button>
      </div>
      <div style="margin-top:8px;display:flex;flex-wrap:wrap;gap:6px">${cmdsHtml}</div>
    </div>

    <div class="card" style="margin:0">
      <strong style="font-size:13px">概览统计</strong>
      <div class="stat-grid" style="margin-top:10px">
        <div class="stat"><div class="k">子模块</div><div class="v">${(data.modules || []).length}</div></div>
        <div class="stat primary"><div class="k">接口契约</div><div class="v">${eps.length}</div></div>
        <div class="stat"><div class="k">实体 / 表</div><div class="v">${tableSet.size}</div></div>
        <div class="stat"><div class="k">字段 / 列</div><div class="v">${ents.length}</div></div>
        <div class="stat"><div class="k">功能点</div><div class="v">${feats.length}</div></div>
        <div class="stat"><div class="k">测试用例</div><div class="v">${cases.length}</div></div>
        <div class="stat"><div class="k">合规发现</div><div class="v">${openFind} / ${findings.length}</div></div>
        <div class="stat"><div class="k">问题</div><div class="v">${issues.length}</div></div>
        <div class="stat"><div class="k">修复建议</div><div class="v">${fixes.length}</div></div>
        <div class="stat"><div class="k">待确认</div><div class="v">${pend.length}</div></div>
        <div class="stat"><div class="k">运行通过 / 失败</div><div class="v">${runStats.passed} / ${runStats.failed}</div></div>
        <div class="stat"><div class="k">依赖 / 环境</div><div class="v">${deps.length} / ${envs.length}</div></div>
      </div>
    </div>

    ${lastRun ? `
    <div class="card" style="margin:0">
      <div class="row" style="justify-content:space-between">
        <strong style="font-size:13px">最近运行</strong>
        <button class="ghost sm" onclick="switchIntelTab('runs')">查看全部</button>
      </div>
      <div style="margin-top:8px;display:flex;flex-direction:column;gap:4px;font-size:12px">
        <div class="row" style="justify-content:space-between">
          <span>#${lastRun.id} · ${escapeHtml(lastRun.scope || "module")}</span>
          <span class="intel-status ${lastRun.status === "passed" ? "analyzed" : ""}">${escapeHtml(lastRun.status || "-")}</span>
        </div>
        <div class="mono muted" style="font-size:11px">${escapeHtml(lastRun.command || "")}</div>
        <div class="muted" style="font-size:11px">${lastRun.startedAt ? new Date(lastRun.startedAt).toLocaleString() : "-"}</div>
      </div>
    </div>` : ""}
  `;
}

function switchIntelTab(tab) {
  const btn = document.querySelector(`.intel-tab[data-tab="${tab}"]`);
  if (btn) btn.click();
}

async function loadIntelContracts(id) {
  const [epRes, entRes] = await Promise.all([
    api("/api/intel/endpoints?projectId=" + id, { headers: appHeaders() }),
    api("/api/intel/entities?projectId=" + id, { headers: appHeaders() }),
  ]);
  const eps = (await epRes.json()).endpoints || [];
  const ents = (await entRes.json()).entities || [];

  // 统计
  document.getElementById("intelDetailStatEndpoints").textContent = eps.length;
  document.getElementById("intelDetailStatColumns").textContent = ents.length;
  const tableSet = new Set(ents.map(e => e.table).filter(Boolean));
  document.getElementById("intelDetailStatTables").textContent = tableSet.size;

  // 接口契约表
  intelEndpointsCache = eps;
  const etb = document.querySelector("#intelEndpointTable tbody");
  etb.innerHTML = "";
  eps.forEach((ep, idx) => {
    const mc = (ep.method || "").toLowerCase();
    const gw = (ep.gatewayRoutes || []).map(g => `<span class="mono" style="font-size:11px">${escapeHtml(g)}</span>`).join("、") || `<span class="muted" style="font-size:11px">直连</span>`;
    etb.insertAdjacentHTML("beforeend", `<tr>
      <td><span class="badge method-${mc}">${escapeHtml(ep.method)}</span></td>
      <td class="mono"${ep.summary ? ` title="${escapeHtml(ep.summary)}"` : ""}>${escapeHtml(ep.path)}${ep.summary ? `<div class="muted" style="font-size:11px;font-weight:400">${escapeHtml(ep.summary)}</div>` : ""}</td>
      <td class="clip">${gw}</td>
      <td class="clip">${escapeHtml(ep.responseType || "-")}</td>
      <td class="clip muted" title="${escapeHtml(ep.requestJson || "")}">${escapeHtml(ep.requestJson || "-")}</td>
      <td class="mono muted clip" title="${escapeHtml(ep.sourceFile)}">${escapeHtml(shortProv(ep.sourceFile, ep.sourceLine))}</td>
      <td><button class="ghost sm" onclick="openIntelMock(${idx})">模拟</button> <button class="ghost sm" onclick="editEndpointSummary(${ep.id}, '${escapeHtml(ep.summary || "")}')">修正</button></td>
    </tr>`);
  });
  if (!eps.length) etb.insertAdjacentHTML("beforeend", `<tr><td colspan="7" class="muted" style="text-align:center;padding:16px">暂无接口契约（分析后自动提取）</td></tr>`);

  // 实体按表分组：每张表一个卡片，列出全部字段（字段名/类型/主键/可空）
  renderIntelEntities(ents);
}

// 修正接口摘要：人工/LLM 修正写入覆写层，重新分析不被覆盖。
async function editEndpointSummary(endpointId, summary) {
  const next = prompt("修正该接口的业务描述（存入覆写层，重新分析不覆盖）", summary || "");
  if (next === null) return;
  const text = next.trim();
  if (!text || text === summary) return;
  const res = await api("/api/intel/endpoints/" + endpointId + "/overrides", { method: "PUT", headers: appHeaders(), body: JSON.stringify({ summary: text }) });
  const data = await res.json();
  if (!res.ok) { alert(data.error || "保存失败"); return; }
  if (intelCurrentProject) loadIntelContracts(intelCurrentProject);
}

// renderIntelEntities 按表分组渲染实体：一张表 = 一张卡片，展示列契约。
function renderIntelEntities(ents) {
  const wrap = document.getElementById("intelEntityList");
  wrap.innerHTML = "";
  if (!ents.length) {
    wrap.innerHTML = `<div class="muted" style="text-align:center;padding:16px">暂无实体映射（分析后自动提取）</div>`;
    return;
  }
  // group by table
  const groups = new Map();
  for (const e of ents) {
    const key = e.table || e.entity || "(未命名)";
    if (!groups.has(key)) groups.set(key, { table: e.table, entity: e.entity, columns: [] });
    groups.get(key).columns.push(e);
  }
  for (const [key, g] of groups) {
    const cols = g.columns.map(e => `<tr>
      <td class="mono">${escapeHtml(e.column)}${e.isPrimary ? ' <span class="badge warn" style="font-size:10px">PK</span>' : ''}</td>
      <td class="mono muted">${escapeHtml(e.fieldType || "-")}</td>
      <td>${e.nullable ? '<span class="intel-status pending">可空</span>' : '<span class="intel-status analyzed">非空</span>'}</td>
      <td class="mono muted clip" title="${escapeHtml(e.sourceFile)}">${escapeHtml(shortProv(e.sourceFile, e.sourceLine))}</td>
    </tr>`).join("");
    wrap.insertAdjacentHTML("beforeend", `<div class="card" style="margin:0">
      <div class="row" style="justify-content:space-between">
        <strong class="mono">${escapeHtml(g.table || key)}</strong>
        <span class="muted" style="font-size:12px">${escapeHtml(g.entity)} · ${g.columns.length} 列</span>
      </div>
      <div class="table-wrap">
        <table><thead><tr><th>列</th><th>类型</th><th>可空</th><th>来源</th></tr></thead>
        <tbody>${cols}</tbody></table>
      </div>
    </div>`);
  }
}

function shortProv(file, line) {
  if (!file) return "-";
  const name = file.split("/").pop().split("\\").pop();
  return line ? (name + ":" + line) : name;
}

/* ---------- 模拟请求（接口契约 → 自动填参 + curl/JSON） ---------- */
let mockMethod = "GET";
let mockPath = "/";
let mockParams = [];
let mockBodyType = "";
let mockEndpointId = 0;
let mockIsGraphQL = false;

function mockValue(type, name) {
  const t = (type || "").toLowerCase();
  if (/long|integer|int|short|byte|number|bigint/.test(t)) return 1;
  if (/double|float|decimal|bigdecimal/.test(t)) return 1.5;
  if (/boolean/.test(t)) return true;
  if (/date|time|timestamp/.test(t)) return "2026-09-16T00:00:00";
  if (/list|set|map|array|collection/.test(t)) return [];
  if (/id/i.test(name || "")) return 1;
  if (/name|title|label/i.test(name || "")) return "test";
  return "test";
}

function buildMockUrl(method, path, params) {
  let url = "http://localhost:8080" + (path || "/");
  const qs = [];
  for (const p of params || []) {
    if (p.source === "path") {
      url = url.split("{" + p.name + "}").join(encodeURIComponent(mockValue(p.type, p.name)));
    } else {
      qs.push(encodeURIComponent(p.name) + "=" + encodeURIComponent(mockValue(p.type, p.name)));
    }
  }
  if (qs.length) url += "?" + qs.join("&");
  return url;
}

const GQL_METHODS = { QUERY: "query", MUTATION: "mutation", SUBSCRIPTION: "subscription" };

function renderGraphQLDoc(verb, field, params) {
  const op = GQL_METHODS[verb] || "query";
  const args = (params || []).filter(p => p.source === "arg");
  let argStr = "";
  if (args.length) {
    argStr = "(" + args.map(p => `${p.name}: ${JSON.stringify(mockValue(p.type, p.name))}`).join(", ") + ")";
  }
  return `${op} ${field}${argStr} {\n  # 按需填写返回字段\n}`;
}

function renderGraphQLCurl(field, doc) {
  const body = JSON.stringify({ query: doc });
  return `curl -X POST 'http://localhost:8080/graphql' \\\n  -H 'Content-Type: application/json' \\\n  -d '${body.replace(/'/g, "'\\''")}'`;
}

function openIntelMock(idx) {
  const ep = intelEndpointsCache[idx];
  if (!ep) return;
  let params = [], bodyType = "";
  if (ep.requestJson) {
    try { const r = JSON.parse(ep.requestJson); params = r.params || []; bodyType = r.bodyType || ""; } catch (_) {}
  }
  mockMethod = (ep.method || "GET").toUpperCase();
  mockPath = ep.path || "/";
  mockParams = params;
  mockBodyType = bodyType;
  mockEndpointId = ep.id || 0;
  mockIsGraphQL = GQL_METHODS[mockMethod] ? true : false;
  document.getElementById("intelMockTitle").textContent =
    `${mockMethod} ${mockPath}${mockIsGraphQL ? " · GraphQL 操作" : ""}${bodyType ? "  ·  请求体类型 " + bodyType : ""}${ep.summary ? "  ·  " + ep.summary : ""}`;
  if (mockIsGraphQL) {
    const doc = renderGraphQLDoc(mockMethod, mockPath, params);
    document.getElementById("intelMockUrl").value = "POST http://localhost:8080/graphql";
    document.getElementById("intelMockBody").value = doc;
    document.getElementById("intelMockCurl").value = renderGraphQLCurl(mockPath, doc);
  } else {
    document.getElementById("intelMockUrl").value = buildMockUrl(mockMethod, mockPath, params);
    document.getElementById("intelMockBody").value = bodyType ? "{\n}" : "";
    document.getElementById("intelMockCurl").value = renderMockCurl();
  }
  document.getElementById("intelMockStatus").textContent = "";
  document.getElementById("intelContractJson").value = "";
  document.getElementById("intelContractStatus").textContent = "";
  document.getElementById("intelContractResult").innerHTML = "";
  document.getElementById("intelMockModal").classList.remove("hidden");
}

function renderMockCurl() {
  const url = document.getElementById("intelMockUrl").value;
  const body = document.getElementById("intelMockBody").value.trim();
  const hasBody = mockMethod === "POST" || mockMethod === "PUT" || mockMethod === "PATCH";
  let cmd = `curl -X ${mockMethod} '${url}'`;
  if (hasBody && body) cmd += ` \\\n  -H 'Content-Type: application/json' \\\n  -d '${body.replace(/'/g, "'\\''")}'`;
  else if (hasBody) cmd += ` -H 'Content-Type: application/json'`;
  return cmd;
}

function closeIntelMock() {
  document.getElementById("intelMockModal").classList.add("hidden");
}

async function copyIntelMock() {
  const curl = mockIsGraphQL
    ? renderGraphQLCurl(mockPath, document.getElementById("intelMockBody").value)
    : renderMockCurl();
  document.getElementById("intelMockCurl").value = curl;
  try {
    await navigator.clipboard.writeText(curl);
    document.getElementById("intelMockStatus").textContent = "已复制到剪贴板";
  } catch (_) {
    document.getElementById("intelMockStatus").textContent = "复制失败，请手动选择";
  }
}

async function checkIntelContract() {
  const st = document.getElementById("intelContractStatus");
  const box = document.getElementById("intelContractResult");
  const resp = document.getElementById("intelContractJson").value.trim();
  st.textContent = "";
  box.innerHTML = "";
  if (!mockEndpointId) { st.textContent = "该接口暂无 id，无法校验"; return; }
  if (!resp) { st.textContent = "请粘贴响应 JSON"; return; }
  const res = await api("/api/intel/contracts/check", { method: "POST", headers: appHeaders(), body: JSON.stringify({ endpointId: mockEndpointId, responseJson: resp }) });
  const data = await res.json();
  if (!res.ok) { st.textContent = data.error || "校验失败"; return; }
  const results = data.results || [];
  st.textContent = `通过 ${data.passed || 0} / 失败 ${data.failed || 0}` + (data.note ? " · " + data.note : "");
  if (!results.length) { box.innerHTML = `<div class="muted" style="padding:8px">${escapeHtml(data.note || "暂无字段契约")}</div>`; return; }
  const rows = results.map(r => {
    const ok = r.status === "passed";
    const label = { missing: "缺失", null: "为 null", type_mismatch: "类型不符", unexpected: "结构不符" }[r.status] || r.status;
    return `<div class="row" style="justify-content:space-between;gap:8px;padding:4px 0;border-bottom:1px solid var(--border)">
      <span class="mono" style="font-size:12px">${escapeHtml(r.field || "-")}</span>
      <span style="font-size:12px;color:${ok ? "var(--green,#16a34a)" : "var(--red,#dc2626)"}">${ok ? "✓" : "✗"} ${escapeHtml(label)}</span>
      <span class="muted" style="font-size:11px;flex:1;text-align:right">${escapeHtml(r.reason || "")}</span>
    </div>`;
  }).join("");
  box.innerHTML = rows;
}

async function checkIntelContractsBatch() {
  if (!intelCurrentProject) return;
  const base = prompt("被测服务地址（如 http://localhost:8080）", "http://localhost:8080");
  if (!base) return;
  const box = document.getElementById("intelContractBatchResult");
  if (box) box.textContent = "一键校验中（逐接口调用 + 契约比对）…";
  const res = await api("/api/intel/contracts/check-batch", { method: "POST", headers: appHeaders(), body: JSON.stringify({ projectId: intelCurrentProject, baseUrl: base }) });
  const data = await res.json();
  if (!res.ok) { if (box) box.textContent = data.error || "校验失败"; return; }
  if (!box) return;
  const lines = (data.results || []).map(r => {
    const ok = r.ok && (!r.contract || r.contract.failed === 0);
    const c = r.contract ? `契约 ${r.contract.passed}/${(r.contract.passed || 0) + (r.contract.failed || 0)}` : "无契约";
    return `<div class="mono" style="font-size:11px;color:${ok ? "var(--green,#16a34a)" : "var(--red,#dc2626)"}">${ok ? "✓" : "✗"} ${escapeHtml((r.method || "") + " " + (r.path || ""))} → ${r.status || "ERR"} ${escapeHtml(c)}${r.error ? " · " + escapeHtml(r.error) : ""}</div>`;
  }).join("");
  box.innerHTML = `<div style="margin-bottom:6px">接口 ${data.total || 0} · 可达 ${data.reachable || 0} · 契约通过 ${data.passed || 0} / 失败 ${data.failed || 0}</div>` + lines;
}

/* ---------- 分析结果 Tab：功能点 / 用例 / 审计 / 问题 / 修复 / 运行 ---------- */
const SEV_LABELS = { critical: "严重", high: "高", medium: "中", low: "低" };

async function loadIntelAiRules() {
  const res = await api("/api/intel/ai-rules", { headers: appHeaders() });
  const data = await res.json();
  const wrap = document.getElementById("aiRuleList");
  if (!wrap) return;
  wrap.innerHTML = "";
  for (const r of data.rules || []) {
    wrap.insertAdjacentHTML("beforeend", `<div class="row" style="justify-content:space-between;gap:8px;padding:6px;border:1px solid var(--hairline);border-radius:var(--r-sm)">
      <div style="flex:1;min-width:0">
        <strong style="font-size:13px">${escapeHtml(r.name || "-")}</strong>
        <div class="muted clip" style="font-size:11px;margin-top:2px" title="${escapeHtml(r.prompt || "")}">${escapeHtml((r.prompt || "").slice(0, 80))}</div>
        <div class="muted" style="font-size:10px;margin-top:2px">${escapeHtml(r.target || "-")} · ${escapeHtml(r.severity || "-")} · ${r.enabled ? "启用" : "停用"}</div>
      </div>
      <div class="row" style="gap:4px">
        <button class="ghost sm" onclick="polishAIRule(${r.id})">润色</button>
        <button class="ghost sm" onclick="toggleAIRule(${r.id}, ${r.enabled})">${r.enabled ? "停用" : "启用"}</button>
        <button class="ghost sm" onclick="deleteAIRule(${r.id})">删除</button>
      </div>
    </div>`);
  }
  if (!(data.rules || []).length) wrap.innerHTML = `<div class="muted" style="font-size:12px">暂无规则（新增后可在项目详情扫描）</div>`;
}

async function createAIRule() {
  const name = document.getElementById("aiRuleName").value.trim();
  const prompt = document.getElementById("aiRulePrompt").value.trim();
  const msg = document.getElementById("aiRuleMsg");
  if (!name || !prompt) { msg.textContent = "请填写名称与提示词"; return; }
  const res = await api("/api/intel/ai-rules", { method: "POST", headers: appHeaders(), body: JSON.stringify({ name, prompt }) });
  const data = await res.json();
  if (!res.ok) { msg.textContent = data.error || "新增失败"; return; }
  document.getElementById("aiRuleName").value = "";
  document.getElementById("aiRulePrompt").value = "";
  msg.textContent = "已新增";
  loadIntelAiRules();
}

async function toggleAIRule(id, enabled) {
  await api("/api/intel/ai-rules/" + id, { method: "PUT", headers: appHeaders(), body: JSON.stringify({ enabled: !enabled }) });
  loadIntelAiRules();
}

async function polishAIRule(id) {
  const msg = document.getElementById("aiRuleMsg");
  if (msg) msg.textContent = "LLM 润色中…";
  const res = await api("/api/intel/ai-rules/" + id + "/polish", { method: "PUT", headers: appHeaders(), body: "{}" });
  const data = await res.json();
  if (!res.ok) { if (msg) msg.textContent = data.error || "润色失败"; return; }
  const polished = data.polished || "";
  const ok = confirm("润色草稿：\n\n" + polished + "\n\n保存为规则提示词？");
  if (!ok) return;
  const sres = await api("/api/intel/ai-rules/" + id, { method: "PUT", headers: appHeaders(), body: JSON.stringify({ prompt: polished }) });
  if (!sres.ok) { if (msg) msg.textContent = (await sres.json()).error || "保存失败"; return; }
  if (msg) msg.textContent = "已保存润色提示词";
  loadIntelAiRules();
}

async function deleteAIRule(id) {
  if (!confirm("删除该规则？")) return;
  await api("/api/intel/ai-rules/" + id, { method: "DELETE", headers: appHeaders() });
  loadIntelAiRules();
}

async function loadIntelPending(id) {
  const wrap = document.getElementById("intelPendingList");
  if (!wrap) return;
  wrap.innerHTML = "";
  const res = await api("/api/intel/pending?projectId=" + id, { headers: appHeaders() });
  const data = await res.json();
  const pending = data.pending || [];
  if (!pending.length) { wrap.innerHTML = `<div class="muted" style="text-align:center;padding:16px">暂无待确认项</div>`; return; }
  for (const p of pending) {
    let auto = "";
    try { const av = JSON.parse(p.autoValueJson || "{}"); auto = JSON.stringify(av); } catch (_) { auto = p.autoValueJson || ""; }
    wrap.insertAdjacentHTML("beforeend", `<div class="card" style="margin:0">
      <div class="row" style="justify-content:space-between">
        <strong style="font-size:13px">${escapeHtml(p.target || "-")} · ${escapeHtml(p.field || "-")}</strong>
        <span class="badge sev-${p.confidence === "low" ? "high" : "medium"}">${escapeHtml(p.confidence || "medium")}</span>
      </div>
      <div class="muted" style="font-size:11px;margin-top:4px">自动值：<span class="mono">${escapeHtml(auto)}</span></div>
      <div class="row" style="gap:6px;margin-top:8px">
        <input type="text" id="intelPendingValue-${p.id}" placeholder="人工确认值" style="flex:1" value="${escapeHtml(p.manualValue || "")}">
        <button class="ghost sm" onclick="confirmIntelPending(${p.id})">确认</button>
        <button class="tertiary sm" onclick="rejectIntelPending(${p.id})">驳回</button>
      </div>
    </div>`);
  }
}

async function confirmIntelPending(id) {
  const value = document.getElementById("intelPendingValue-" + id).value.trim();
  await api("/api/intel/pending/" + id + "/confirm", { method: "POST", headers: appHeaders(), body: JSON.stringify({ manualValue: value, status: "applied" }) });
  loadIntelPending(intelCurrentProject);
}

async function rejectIntelPending(id) {
  await api("/api/intel/pending/" + id + "/confirm", { method: "POST", headers: appHeaders(), body: JSON.stringify({ status: "rejected" }) });
  loadIntelPending(intelCurrentProject);
}

async function suggestIntelOverride() {
  if (!intelCurrentProject) return;
  const instruction = prompt("描述校正方向（如：把管理端模块角色标为 admin）", "");
  if (instruction === null) return;
  const res = await api("/api/intel/overrides/suggest", { method: "POST", headers: appHeaders(), body: JSON.stringify({ projectId: intelCurrentProject, instruction }) });
  const data = await res.json();
  if (!res.ok) { alert(data.error || "建议失败"); return; }
  const drafts = data.drafts || [];
  if (!drafts.length) { alert("AI 未提出有明确依据的覆写建议"); return; }
  const ok = confirm("AI 建议草稿（仅预览）：\n\n" + JSON.stringify(drafts, null, 2) + "\n\n加入待确认队列，由人工确认后生效？");
  if (!ok) return;
  const r2 = await api("/api/intel/overrides/enqueue", { method: "POST", headers: appHeaders(), body: JSON.stringify({ projectId: intelCurrentProject, drafts }) });
  const d2 = await r2.json();
  if (!r2.ok) { alert(d2.error || "入队失败"); return; }
  alert(`已加入待确认队列 ${d2.queued || 0} 条（待确认 Tab 处理）`);
  loadIntelPending(intelCurrentProject);
}

async function scanIntelRules() {
  if (!intelCurrentProject) return;
  if (!confirm("用全部启用的 AI 建议规则扫描本项目的知识块（接口契约/文档/绑定），命中将落库为审计问题（ai-rule）？")) return;
  const res = await api("/api/intel/scan/rules", { method: "POST", headers: appHeaders(), body: JSON.stringify({ projectId: intelCurrentProject }) });
  const data = await res.json();
  if (!res.ok) { alert(data.error || "扫描失败"); return; }
  alert(`扫描完成：命中 ${data.created || 0} 条问题（审计 Tab 查看）`);
  if (typeof loadIntelFindings === "function") loadIntelFindings(intelCurrentProject);
}

async function loadIntelBindings(id) {
  const [aRes, wRes, iRes] = await Promise.all([
    api("/api/intel/android-bindings?projectId=" + id, { headers: appHeaders() }),
    api("/api/intel/web-bindings?projectId=" + id, { headers: appHeaders() }),
    api("/api/intel/ios-bindings?projectId=" + id, { headers: appHeaders() }),
  ]);
  const android = ((await aRes.json()).bindings || []).map(b => ({ ...b, client: "android", slot: b.widget || "-" }));
  const web = ((await wRes.json()).bindings || []).map(b => ({ ...b, client: "web", slot: b.slot || "-" }));
  const ios = ((await iRes.json()).bindings || []).map(b => ({ ...b, client: "ios", slot: b.slot || "-" }));
  const bindings = android.concat(web, ios);
  const tb = document.querySelector("#intelBindingTable tbody");
  tb.innerHTML = "";
  for (const b of bindings) {
    tb.insertAdjacentHTML("beforeend", `<tr>
      <td><span class="badge">${escapeHtml(b.client)}</span></td>
      <td class="mono">${escapeHtml(b.page || "-")}</td>
      <td class="mono">${escapeHtml(b.fieldPath || "-")}</td>
      <td><span class="badge">${escapeHtml(b.slot || "-")}</span></td>
      <td class="mono muted clip" title="${escapeHtml(b.sourceFile || "")}">${escapeHtml(shortProv(b.sourceFile, b.sourceLine))}</td>
    </tr>`);
  }
  if (!bindings.length) tb.insertAdjacentHTML("beforeend", `<tr><td colspan="5" class="muted" style="text-align:center;padding:16px">暂无客户端绑定（Android/Web 项目分析后自动提取字段）</td></tr>`);
}

async function loadIntelEnv(id) {
  const wrap = document.getElementById("intelEnvList");
  wrap.innerHTML = `<div class="muted" style="text-align:center;padding:16px">检测中…</div>`;
  const [res, devRes, nodeRes] = await Promise.all([
    api("/api/intel/env/status?projectId=" + id, { headers: appHeaders() }),
    api("/api/intel/env/devices?projectId=" + id, { headers: appHeaders() }),
    api("/api/intel/nodes", { headers: appHeaders() }),
  ]);
  const data = await res.json();
  if (!res.ok) { wrap.innerHTML = `<div class="muted" style="text-align:center;padding:16px">${escapeHtml(data.error || "检测失败")}</div>`; return; }
  renderIntelEnv(data);
  renderIntelDevices(((await devRes.json()).devices || []), id);
  renderIntelNodes(((await nodeRes.json()).nodes || []));
}

function renderIntelNodes(nodes) {
  const wrap = document.getElementById("intelNodeList");
  if (!wrap) return;
  wrap.innerHTML = "";
  if (!nodes.length) { wrap.innerHTML = `<div class="muted" style="font-size:12px">暂无节点（本机不满足门禁时可路由到远程节点执行）</div>`; return; }
  for (const n of nodes) {
    wrap.insertAdjacentHTML("beforeend", `<div class="card" style="margin:0">
      <div class="row" style="justify-content:space-between">
        <strong style="font-size:13px">${escapeHtml(n.name || "-")}</strong>
        <div class="row" style="gap:6px">
          <span class="intel-status ${n.reachable ? "analyzed" : ""}">${n.reachable ? "可达" : "不可达"}</span>
          <button class="ghost sm" onclick="checkIntelNode(${n.id})">检查</button>
          <button class="ghost sm" onclick="deleteIntelNode(${n.id})">删除</button>
        </div>
      </div>
      <div class="muted" style="font-size:11px;margin-top:4px">${escapeHtml(n.user || "")}@${escapeHtml(n.host || "-")}:${n.port || 22} · ${escapeHtml(n.capabilities || "-")}</div>
    </div>`);
  }
}

async function addIntelNode() {
  const name = prompt("节点名称（如 linux-ci）", "");
  if (!name) return;
  const host = prompt("主机（IP 或域名）", "");
  if (!host) return;
  const port = parseInt(prompt("端口", "22"), 10) || 22;
  const user = prompt("SSH 用户", "root");
  const caps = prompt("能力标签（逗号分隔，如 linux-docker,android-sdk）", "");
  const workDir = prompt("节点上仓库工作目录（如 /srv/repos/echo，可留空用 ~）", "");
  const auth = prompt("口令或密钥路径（仅入库本环境，不对外回显）", "");
  const res = await api("/api/intel/nodes", { method: "POST", headers: appHeaders(), body: JSON.stringify({ name, host, port, user, capabilities: caps, workDir, auth }) });
  const data = await res.json();
  if (!res.ok) { alert(data.error || "新增失败"); return; }
  loadIntelEnv(intelCurrentProject);
}

async function checkIntelNode(id) {
  await api("/api/intel/nodes/" + id + "/check", { method: "PUT", headers: appHeaders() });
  loadIntelEnv(intelCurrentProject);
}

async function deleteIntelNode(id) {
  await api("/api/intel/nodes/" + id, { method: "DELETE", headers: appHeaders() });
  loadIntelEnv(intelCurrentProject);
}

function renderIntelDevices(devices, projectId) {
  const wrap = document.getElementById("intelDeviceList");
  if (!wrap) return;
  wrap.innerHTML = "";
  if (!devices.length) { wrap.innerHTML = `<div class="muted" style="font-size:12px">暂无设备（无线连接后出现）</div>`; return; }
  for (const d of devices) {
    const bound = d.projectId === projectId && d.bound;
    const btn = bound
      ? `<button class="ghost sm" onclick="deleteIntelDevice(${d.id})">解绑</button>`
      : `<button class="ghost sm" onclick="bindIntelDevice(${d.id}, ${projectId})">绑定</button>`;
    wrap.insertAdjacentHTML("beforeend", `<div class="card" style="margin:0">
      <div class="row" style="justify-content:space-between">
        <strong style="font-size:13px">${escapeHtml(d.name || d.serial || "-")}</strong>
        <div class="row" style="gap:6px">
          <span class="intel-status ${d.status === "device" || d.status === "connected" ? "analyzed" : ""}">${escapeHtml(d.status || "disconnected")}</span>
          ${btn}
        </div>
      </div>
      <div class="muted" style="font-size:11px;margin-top:4px">${escapeHtml(d.serial || "-")} · ${escapeHtml(d.method || "-")}</div>
    </div>`);
  }
}

async function connectIntelDevice() {
  if (!intelCurrentProject) return;
  const addr = prompt("无线 adb 地址（IP:端口），如 192.0.2.9:5555", "");
  if (!addr) return;
  const m = addr.match(/^(.*):(\d+)$/);
  if (!m) { alert("格式应为 IP:端口"); return; }
  const res = await api("/api/intel/env/devices/connect", { method: "POST", headers: appHeaders(), body: JSON.stringify({ ip: m[1], port: parseInt(m[2], 10) }) });
  const data = await res.json();
  if (!res.ok) { alert(data.error || "连接失败"); return; }
  loadIntelEnv(intelCurrentProject);
}

async function bindIntelDevice(id, projectId) {
  await api("/api/intel/env/devices/" + id + "/bind", { method: "PUT", headers: appHeaders(), body: JSON.stringify({ projectId }) });
  loadIntelEnv(intelCurrentProject);
}

async function deleteIntelDevice(id) {
  await api("/api/intel/env/devices/" + id, { method: "DELETE", headers: appHeaders() });
  loadIntelEnv(intelCurrentProject);
}

async function ensureIntelEnv() {
  const wrap = document.getElementById("intelEnvList");
  if (!intelCurrentProject) return;
  wrap.innerHTML = `<div class="muted" style="text-align:center;padding:16px">检测中…</div>`;
  const res = await api("/api/intel/env/ensure", { method: "POST", headers: appHeaders(), body: JSON.stringify({ projectId: intelCurrentProject }) });
  const data = await res.json();
  if (!res.ok) { wrap.innerHTML = `<div class="muted" style="text-align:center;padding:16px">${escapeHtml(data.error || "检测失败")}</div>`; return; }
  renderIntelEnv(data);
}

function renderIntelEnv(data) {
  const wrap = document.getElementById("intelEnvList");
  const svcs = data.services || [];
  wrap.innerHTML = "";
  if (!svcs.length) {
    wrap.innerHTML = `<div class="muted" style="text-align:center;padding:16px">未检测到环境依赖（分析项目后自动识别中间件/工具链）</div>`;
    return;
  }
  if (data.dockerNotice) {
    wrap.insertAdjacentHTML("beforeend", `<div class="msg" style="margin:0 0 8px">${escapeHtml(data.dockerNotice)}</div>`);
  }
  wrap.insertAdjacentHTML("beforeend", `<div class="muted" style="font-size:12px;margin-bottom:6px">就绪 ${data.ready || 0} · 缺失 ${data.missing || 0} · 不支持 ${data.unsupported || 0}</div>`);
  for (const svc of svcs) {
    const ready = svc.status === "ready";
    const unsupported = svc.status === "unsupported";
    const provider = svc.provider === "container" ? "容器" : svc.provider === "installed" ? "已装" : "-";
    const ep = svc.port ? `127.0.0.1:${svc.port}` : (svc.endpoint || "-");
    let action = "";
    if (ready) {
      action = `<button class="ghost sm" onclick="stopIntelEnv('${escapeHtml(svc.service)}')">停止</button>`;
      if (svc.service === "mysql") action += ` <button class="ghost sm" onclick="initIntelEnvSchema('${escapeHtml(svc.service)}')">初始化库</button>`;
    } else if (svc.category === "middleware") {
      action = `<button class="ghost sm" onclick="installIntelEnv('${escapeHtml(svc.service)}')">安装</button>
        <button class="ghost sm" onclick="externalIntelEnv('${escapeHtml(svc.service)}')">外部配置</button>`;
    } else if (unsupported) {
      action = `<span class="muted" style="font-size:11px">需手动安装</span>`;
    }
    wrap.insertAdjacentHTML("beforeend", `<div class="card" style="margin:0">
      <div class="row" style="justify-content:space-between">
        <strong>${escapeHtml(svc.service)}</strong>
        <div class="row" style="gap:6px">
          <span class="intel-status ${ready ? "analyzed" : unsupported ? "pending" : ""}">${ready ? "就绪" : unsupported ? "不支持" : "缺失"}</span>
          ${action}
        </div>
      </div>
      <div class="muted" style="font-size:12px;margin-top:6px">
        类别 ${escapeHtml(svc.category)} · 版本 ${escapeHtml(svc.version || "latest")} · 供给 ${escapeHtml(provider)} · 连接 ${escapeHtml(ep)}
      </div>
    </div>`);
  }
}

async function installIntelEnv(service) {
  if (!intelCurrentProject) return;
  const res = await api("/api/intel/env/install", { method: "POST", headers: appHeaders(), body: JSON.stringify({ projectId: intelCurrentProject, service }) });
  const data = await res.json();
  if (!res.ok) { alert(data.error || "安装失败"); return; }
  loadIntelEnv(intelCurrentProject);
}

async function stopIntelEnv(service) {
  if (!intelCurrentProject) return;
  await api("/api/intel/env/stop", { method: "POST", headers: appHeaders(), body: JSON.stringify({ projectId: intelCurrentProject, service }) });
  loadIntelEnv(intelCurrentProject);
}

async function initIntelEnvSchema(service) {
  if (!intelCurrentProject) return;
  if (!confirm("将把项目内的 SQL 迁移脚本（Flyway/Liquibase/*.sql）执行到该 MySQL 容器，继续？")) return;
  const res = await api("/api/intel/env/schema-init", { method: "POST", headers: appHeaders(), body: JSON.stringify({ projectId: intelCurrentProject, service }) });
  const data = await res.json();
  if (!res.ok) { alert(data.error || "初始化失败"); return; }
  alert(`已执行脚本 ${data.executed}/${data.total}`);
  loadIntelEnv(intelCurrentProject);
}

async function externalIntelEnv(service) {
  const addr = prompt("外部中间件地址（host:port），如 192.0.2.150:3306，用户与口令可选", "");
  if (!addr) return;
  const m = addr.match(/^(.*):(\d+)$/);
  if (!m) { alert("格式应为 host:port"); return; }
  const user = prompt("用户名（可留空）", "");
  const pw = prompt("口令（可留空，仅落库本环境供测试连接）", "");
  if (user === null || pw === null) return;
  const res = await api("/api/intel/env/external", { method: "POST", headers: appHeaders(), body: JSON.stringify({ projectId: intelCurrentProject, service, host: m[1], port: parseInt(m[2], 10), username: user, password: pw }) });
  const data = await res.json();
  if (!res.ok) { alert(data.error || "配置失败"); return; }
  loadIntelEnv(intelCurrentProject);
}

async function loadIntelFeatures(id) {
  const res = await api("/api/intel/features?projectId=" + id, { headers: appHeaders() });
  const data = await res.json();
  const feats = data.features || [];
  const wrap = document.getElementById("intelFeatureList");
  wrap.innerHTML = "";
  if (!feats.length) { wrap.innerHTML = `<div class="muted" style="text-align:center;padding:16px">暂无功能点（分析后自动聚类）</div>`; return; }
  for (const f of feats) {
    let ends = [];
    try { ends = JSON.parse(f.endsJson || "[]"); } catch (_) {}
    const endBadges = (ends || []).map(e => `<span class="badge">${escapeHtml(e)}</span>`).join(" ") || `<span class="muted" style="font-size:11px">未知</span>`;
    wrap.insertAdjacentHTML("beforeend", `<div class="card" style="margin:0">
      <div class="row" style="justify-content:space-between">
        <strong>${escapeHtml(f.name || "-")}</strong>
        <div class="row" style="gap:6px">
          <span class="muted" style="font-size:12px">涉及端 ${endBadges}</span>
          <button class="ghost sm" onclick="testIntelFeature(${f.id})">单测</button>
          <button class="ghost sm" onclick="chatIntelFeature(${f.id})">对话</button>
          <button class="ghost sm" onclick="historyIntelFeatureChat(${f.id})">历史</button>
        </div>
      </div>
      <div id="intelFeatureResult-${f.id}" class="muted" style="font-size:12px;margin-top:6px"></div>
    </div>`);
  }
}

async function testIntelFeature(featureId) {
  const base = prompt("请输入被测服务地址（如 http://localhost:8080）", "http://localhost:8080");
  if (!base) return;
  const box = document.getElementById("intelFeatureResult-" + featureId);
  if (box) box.textContent = "单测中…";
  const res = await api("/api/intel/features/test", { method: "POST", headers: appHeaders(), body: JSON.stringify({ projectId: intelCurrentProject, featureId, baseUrl: base }) });
  const data = await res.json();
  if (!res.ok) { if (box) box.textContent = data.error || "单测失败"; return; }
  const rs = data.results || [];
  let okCount = 0, failCount = 0;
  const lines = rs.map(r => {
    if (r.ok && (!r.contract || r.contract.failed === 0)) okCount++; else failCount++;
    const c = r.contract ? `契约 ${r.contract.passed}/${r.contract.passed + r.contract.failed}` : "无契约";
    return `${r.method} ${r.path} → ${r.status || "ERR"} ${c}${r.error ? " · " + r.error : ""}`;
  });
  if (box) box.innerHTML = `<span class="mono" style="font-size:11px">${okCount} 通 / ${failCount} 败</span>` + lines.map(l => `<div class="mono" style="font-size:11px">${escapeHtml(l)}</div>`).join("");
}

async function chatIntelFeature(featureId) {
  const q = prompt("针对该功能点提问（自动携带接口契约 + 最近实测 + 挂载问题上下文）", "");
  if (q === null || !q.trim()) return;
  const box = document.getElementById("intelFeatureResult-" + featureId);
  if (box) box.textContent = "AI 归因中…";
  const res = await api("/api/intel/features/" + featureId + "/chat", { method: "POST", headers: appHeaders(), body: JSON.stringify({ projectId: intelCurrentProject, question: q }) });
  const data = await res.json();
  if (!res.ok) { if (box) box.textContent = data.error || "对话失败"; return; }
  const chat = data.chat || {};
  if (box) box.innerHTML = `<div class="card" style="margin:0;padding:8px">
      <div class="muted" style="font-size:11px;margin-bottom:4px">Q：${escapeHtml(chat.question || q)}</div>
      <div class="mono" style="font-size:12px">${escapeHtml(chat.answer || "（无回答）")}</div>
    </div>`;
}

async function historyIntelFeatureChat(featureId) {
  const box = document.getElementById("intelFeatureResult-" + featureId);
  if (box) box.textContent = "加载历史…";
  const res = await api("/api/intel/features/" + featureId + "/chats?projectId=" + intelCurrentProject, { headers: appHeaders() });
  const data = await res.json();
  if (!res.ok) { if (box) box.textContent = data.error || "加载失败"; return; }
  const chats = data.chats || [];
  if (!chats.length) { if (box) box.innerHTML = `<span class="muted" style="font-size:12px">暂无对话记录</span>`; return; }
  const rows = chats.map(c => `<div style="padding:6px 0;border-bottom:1px solid var(--hairline)">
      <div class="muted" style="font-size:11px">${new Date(c.createdAt).toLocaleString()}</div>
      <div class="mono" style="font-size:12px;margin-top:2px">Q：${escapeHtml(c.question || "")}</div>
      <div class="mono muted" style="font-size:11px;margin-top:2px">A：${escapeHtml(c.answer || "")}</div>
    </div>`).join("");
  if (box) box.innerHTML = `<div style="max-height:220px;overflow:auto">${rows}</div>`;
}

async function loadIntelCases(id) {
  const res = await api("/api/intel/test-cases?projectId=" + id, { headers: appHeaders() });
  const data = await res.json();
  const cases = data.testCases || [];
  const tb = document.querySelector("#intelCaseTable tbody");
  tb.innerHTML = "";
  for (const c of cases) {
    const ls = c.lastStatus;
    const statusHtml = ls ? `<span class="intel-status ${ls === "passed" ? "analyzed" : ""}">${escapeHtml(ls)}</span>` : `<span class="muted" style="font-size:11px">-</span>`;
    const flaky = c.flakyCount > 0;
    tb.insertAdjacentHTML("beforeend", `<tr>
      <td class="mono clip">${escapeHtml(c.module || "-")}</td>
      <td><span class="badge">${escapeHtml(c.kind || "-")}</span></td>
      <td>${escapeHtml(c.framework || "-")}</td>
      <td class="mono clip">${escapeHtml(c.class || "")}${c.method ? "." + escapeHtml(c.method) : ""}</td>
      <td class="mono muted clip" title="${escapeHtml(c.path || "")}">${escapeHtml(shortProv(c.path, 0))}</td>
      <td>${statusHtml}</td>
      <td>${flaky ? `<span class="intel-status" style="color:var(--red,#dc2626)">${c.flakyCount} 次</span>` : `<span class="muted" style="font-size:11px">-</span>`}</td>
    </tr>`);
  }
  if (!cases.length) tb.insertAdjacentHTML("beforeend", `<tr><td colspan="7" class="muted" style="text-align:center;padding:16px">暂无测试用例（分析后自动发现）</td></tr>`);
}

async function loadIntelFindings(id) {
  const res = await api("/api/intel/findings?projectId=" + id, { headers: appHeaders() });
  const data = await res.json();
  const findings = data.findings || [];
  const wrap = document.getElementById("intelFindingList");
  wrap.innerHTML = "";
  if (!findings.length) { wrap.innerHTML = `<div class="muted" style="text-align:center;padding:16px">暂无合规 / 审计发现（分析后自动扫描）</div>`; return; }
  for (const f of findings) {
    const sev = f.severity || "low";
    wrap.insertAdjacentHTML("beforeend", `<div class="card" style="margin:0">
      <div class="row" style="justify-content:space-between">
        <div class="row" style="gap:6px">
          <span class="badge sev-${sev}">${SEV_LABELS[sev] || sev}</span>
          <span class="badge">${escapeHtml(f.category || "-")}</span>
          <span class="mono muted" style="font-size:11px">${escapeHtml(f.cveOrRuleId || f.detector || "")}</span>
        </div>
        <span class="intel-status ${f.status === "open" ? "analyzed" : ""}">${escapeHtml(f.status || "open")}</span>
      </div>
      <div style="margin-top:6px">${escapeHtml(f.summary || "")}</div>
      <div class="row" style="justify-content:space-between;margin-top:4px">
        <span class="muted mono" style="font-size:11px">${escapeHtml(f.location || "")}</span>
        <button class="ghost sm" onclick="generateIntelFix(${f.id})">生成修复</button>
      </div>
    </div>`);
  }
}

async function generateIntelFix(findingId) {
  const id = intelCurrentProject;
  const res = await api("/api/intel/fixes/generate", { method: "POST", headers: appHeaders(), body: JSON.stringify({ projectId: id, findingId }) });
  const data = await res.json();
  if (!res.ok) { show(document.getElementById("intelMsg"), data.error || "生成失败"); return; }
  show(document.getElementById("intelMsg"), "已生成修复建议，见「修复建议」Tab");
  document.getElementById("intelMsg").classList.add("ok");
  loadIntelFixes(id);
}

async function loadIntelIssues(id) {
  const res = await api("/api/intel/issues?projectId=" + id, { headers: appHeaders() });
  const data = await res.json();
  const issues = data.issues || [];
  const tb = document.querySelector("#intelIssueTable tbody");
  tb.innerHTML = "";
  for (const it of issues) {
    tb.insertAdjacentHTML("beforeend", `<tr>
      <td><span class="badge">${escapeHtml(it.kind || "-")}</span></td>
      <td><span class="badge sev-${it.severity || "low"}">${SEV_LABELS[it.severity] || it.severity || "-"}</span></td>
      <td class="mono clip" title="${escapeHtml(it.location || "")}">${escapeHtml(it.location || "-")}</td>
      <td><span class="intel-status ${it.status === "open" ? "analyzed" : ""}">${escapeHtml(it.status || "open")}</span></td>
      <td class="muted" style="font-size:12px">${it.commitSeen ? escapeHtml(it.commitSeen.slice(0, 8)) : "-"}</td>
    </tr>`);
  }
  if (!issues.length) tb.insertAdjacentHTML("beforeend", `<tr><td colspan="5" class="muted" style="text-align:center;padding:16px">暂无问题（测试失败后自动登记）</td></tr>`);
}

async function loadIntelFixes(id) {
  const res = await api("/api/intel/fixes?projectId=" + id, { headers: appHeaders() });
  const data = await res.json();
  const fixes = data.fixes || [];
  const wrap = document.getElementById("intelFixList");
  wrap.innerHTML = "";
  if (!fixes.length) { wrap.innerHTML = `<div class="muted" style="text-align:center;padding:16px">暂无修复建议</div>`; return; }
  for (const fx of fixes) {
    wrap.insertAdjacentHTML("beforeend", `<div class="card" style="margin:0">
      <div class="row" style="justify-content:space-between">
        <strong>${escapeHtml(fx.title || fx.kind || "-")}</strong>
        <span class="intel-status ${fx.status === "applied" ? "analyzed" : ""}">${escapeHtml(fx.status || "pending")}</span>
      </div>
      ${renderIntelFixDiff(fx.diffJson)}
      <div class="row" style="gap:6px;margin-top:6px">
        ${fx.status === "proposed" ? `<button class="ghost sm" onclick="applyIntelFix(${fx.id}, 'apply')">应用</button>
        <button class="tertiary sm" onclick="applyIntelFix(${fx.id}, 'reject')">拒绝</button>` : ""}
        ${fx.status === "applied" ? `<button class="ghost sm" onclick="applyIntelFix(${fx.id}, 'rollback')">回滚</button>` : ""}
      </div>
    </div>`);
  }
}

function renderIntelFixDiff(diffJson) {
  if (!diffJson) return "";
  let suggs = [];
  try { suggs = JSON.parse(diffJson); } catch (_) { return `<pre class="mono" style="font-size:11px;max-height:180px;overflow:auto;background:var(--surface-2);border:1px solid var(--hairline);border-radius:var(--r-sm);padding:8px;margin:8px 0 0">${escapeHtml(diffJson)}</pre>`; }
  if (!Array.isArray(suggs)) return "";
  const blocks = suggs.map(s => {
    const oldTxt = escapeHtml(s.oldText || "");
    const newTxt = escapeHtml(s.newText || "");
    return `<div style="margin-top:8px">
      <div class="muted mono" style="font-size:11px">${escapeHtml(s.file || "-")}${s.line ? ":" + s.line : ""}</div>
      <div class="mono" style="font-size:11px;background:var(--surface-2);border:1px solid var(--hairline);border-radius:var(--r-sm);padding:6px;margin-top:4px;white-space:pre-wrap">
        <span style="color:var(--red,#dc2626)">- ${oldTxt}</span>
        <br><span style="color:var(--green,#16a34a)">+ ${newTxt}</span>
      </div>
    </div>`;
  }).join("");
  return `<div style="max-height:220px;overflow:auto">${blocks}</div>`;
}

async function applyIntelFix(fixId, action) {
  const res = await api("/api/intel/fixes/" + fixId + "/" + action, { method: "POST", headers: appHeaders() });
  const data = await res.json();
  if (!res.ok) { show(document.getElementById("intelMsg"), data.error || "操作失败"); return; }
  loadIntelFixes(intelCurrentProject);
}

async function loadIntelRuns(id) {
  const res = await api("/api/intel/runs?projectId=" + id, { headers: appHeaders() });
  const data = await res.json();
  const runs = data.runs || [];
  const tb = document.querySelector("#intelRunTable tbody");
  tb.innerHTML = "";
  let anyActive = false;
  for (const r of runs) {
    const dur = r.finishedAt && r.startedAt ? Math.round((new Date(r.finishedAt) - new Date(r.startedAt)) / 1000) + "s" : "-";
    const active = (r.status === "queued" || r.status === "running");
    if (active) anyActive = true;
    const statusCls = r.status === "passed" ? "analyzed" : (active ? "" : "");
    const progress = active && r.progress ? ` <span class="muted" style="font-size:11px">（${escapeHtml(r.progress)}）</span>` : "";
    const cancelBtn = active ? ` <button class="ghost sm" onclick="cancelIntelRun(${r.id})">取消</button>` : "";
    tb.insertAdjacentHTML("beforeend", `<tr data-active="${active ? "1" : "0"}">
      <td>#${r.id}</td>
      <td>${escapeHtml(r.scope || "module")}</td>
      <td class="mono clip" title="${escapeHtml(r.command || "")}">${escapeHtml(r.command || "-")}${progress}</td>
      <td><span class="intel-status ${statusCls}">${escapeHtml(r.status || "-")}</span></td>
      <td class="muted" style="font-size:12px">${r.startedAt ? new Date(r.startedAt).toLocaleString() : "-"}</td>
      <td>${dur}</td>
      <td><button class="ghost sm" onclick="showIntelRunDetail(${r.id})">详情</button>${cancelBtn}</td>
    </tr>`);
  }
  if (!runs.length) tb.insertAdjacentHTML("beforeend", `<tr><td colspan="7" class="muted" style="text-align:center;padding:16px">暂无运行记录（点击右上角「运行测试」触发）</td></tr>`);
  if (anyActive) {
    clearTimeout(intelRunPollTimer);
    intelRunPollTimer = setTimeout(pollIntelRunningRuns, 2000);
  }
}

async function showIntelRunDetail(runId) {
  const box = document.getElementById("intelRunDetail");
  if (!box) return;
  box.innerHTML = `<div class="muted" style="padding:8px">加载中…</div>`;
  const res = await api("/api/intel/runs/" + runId, { headers: appHeaders() });
  const data = await res.json();
  if (!res.ok) { box.innerHTML = `<div class="muted" style="padding:8px">${escapeHtml(data.error || "加载失败")}</div>`; return; }
  const run = data.run || {};
  const results = data.results || [];
  const active = (run.status === "queued" || run.status === "running");
  const rows = results.map(rt => {
    const ok = rt.passed;
    const flaky = ok && (rt.failuresJson || "").indexOf("flaky") !== -1;
    const stHtml = flaky
      ? `<span class="intel-status" style="color:var(--amber,#f59e0b)" title="${escapeHtml(rt.failuresJson || "")}">flaky</span>`
      : `<span class="intel-status ${ok ? "analyzed" : ""}">${ok ? "通过" : "失败"}</span>`;
    const rcBtn = ok ? "" : ` <button class="ghost sm" onclick="showResultRootcause(${rt.id})">归因</button>`;
    return `<tr>
      <td class="mono" style="font-size:12px">${escapeHtml(rt.endpoint || "-")}</td>
      <td>${stHtml}</td>
      <td class="mono muted" style="font-size:11px" id="rootcause-${rt.id}">${rcBtn}</td>
    </tr>`;
  }).join("");
  const outHtml = run.output
    ? `<div class="mono" style="margin-top:8px;font-size:11.5px;white-space:pre-wrap;background:var(--surface-2);border:1px solid var(--hairline);border-radius:var(--r-sm);padding:8px;max-height:280px;overflow:auto">${escapeHtml(run.output)}</div>`
    : "";
  const progressHtml = run.progress
    ? `<div class="muted" style="font-size:12px;margin-top:4px">${escapeHtml(run.progress)}</div>`
    : "";
  box.innerHTML = `<div class="card" style="margin:0">
    <div class="row" style="justify-content:space-between">
      <strong>运行 #${run.id || ""}</strong>
      <div class="row">
        <span class="intel-status ${run.status === "passed" ? "analyzed" : ""}">${escapeHtml(run.status || "-")}</span>
        ${active ? ` <button class="ghost sm" onclick="cancelIntelRun(${run.id})">取消</button>` : ""}
      </div>
    </div>
    <div class="mono muted" style="font-size:11px;margin-top:4px">${escapeHtml(run.command || "")}</div>
    ${progressHtml}
    <div class="table-wrap" style="margin-top:8px">
      <table><thead><tr><th>用例</th><th>结果</th><th>归因</th></tr></thead><tbody>${rows || `<tr><td colspan="3" class="muted" style="text-align:center;padding:8px">无结果</td></tr>`}</tbody></table>
    </div>
    ${outHtml}
  </div>`;
  // 运行中：定时刷新详情，展示最新 progress/output。
  if (active) {
    setTimeout(() => { if (document.getElementById("intelRunDetail")) showIntelRunDetail(run.id); }, 2000);
  }
}

async function showResultRootcause(resultId) {
  const box = document.getElementById("rootcause-" + resultId);
  if (!box) return;
  box.textContent = "加载归因…";
  const res = await api("/api/intel/results/" + resultId + "/rootcause", { headers: appHeaders() });
  const data = await res.json();
  if (!res.ok) { box.textContent = data.error || "归因失败"; return; }
  const rc = data.rootcause || {};
  box.innerHTML = `<div class="mono" style="font-size:11px;white-space:pre-wrap;background:var(--surface-2);border:1px solid var(--hairline);border-radius:var(--r-sm);padding:6px">${escapeHtml(JSON.stringify(rc, null, 2))}</div>`;
}

let intelRunPollTimer = null;

// refreshIntelRunsIfVisible reloads the runs table when the runs tab is open.
function refreshIntelRunsIfVisible() {
  if (!intelCurrentProject) return;
  const panel = document.getElementById("intelPanel-runs");
  if (!panel || panel.classList.contains("hidden")) return;
  loadIntelRuns(intelCurrentProject);
}

// pollIntelRunningRuns refreshes the runs table while any run is active, so
// progress stays live even if the push socket is not connected.
function pollIntelRunningRuns() {
  clearTimeout(intelRunPollTimer);
  const panel = document.getElementById("intelPanel-runs");
  if (!panel || panel.classList.contains("hidden")) return;
  const rows = document.querySelectorAll('#intelRunTable tbody tr[data-active="1"]');
  if (rows.length === 0) return;
  if (intelCurrentProject) loadIntelRuns(intelCurrentProject);
  intelRunPollTimer = setTimeout(pollIntelRunningRuns, 2000);
}

async function runIntelTests() {
  const id = intelCurrentProject;
  if (!id) return;
  if (!confirm("运行测试？\n（环境缺失时默认被门禁拦截，可下一步选择强制继续）")) return;
  const force = confirm("环境缺失时强制继续执行？\n确定=跳过门禁，取消=按门禁拦截");
  const st = document.getElementById("intelAnalyzeStatus");
  if (st) st.textContent = "已排队，等待执行…";
  const res = await api("/api/intel/run", { method: "POST", headers: appHeaders(), body: JSON.stringify({ projectId: id, force }) });
  const data = await res.json();
  if (!res.ok) { if (st) st.textContent = ""; show(document.getElementById("intelMsg"), data.error || "运行失败"); return; }
  if (st) st.textContent = "已入队（#运行异步执行中）";
  switchIntelTab("runs");
  loadIntelRuns(id);
}

async function runIntelTestsAll() {
  const id = intelCurrentProject;
  if (!id) return;
  if (!confirm("一键回归：并行运行项目全部模块的测试命令，继续？")) return;
  const st = document.getElementById("intelAnalyzeStatus");
  if (st) st.textContent = "已排队，等待一键回归…";
  const res = await api("/api/intel/run-all", { method: "POST", headers: appHeaders(), body: JSON.stringify({ projectId: id }) });
  const data = await res.json();
  if (!res.ok) { if (st) st.textContent = ""; show(document.getElementById("intelMsg"), data.error || "回归失败"); return; }
  if (st) st.textContent = "一键回归已入队（异步执行中）";
  switchIntelTab("runs");
  loadIntelRuns(id);
}

// runIntelPlan 生成并执行测试计划：后端按模块角色/最近失败/影响面规划顺序，
// 每模块先构建后测试，串行执行。执行前弹出计划预览让用户确认。
async function runIntelPlan() {
  const id = intelCurrentProject;
  if (!id) return;
  const planRes = await api("/api/intel/plan?projectId=" + id, { headers: appHeaders() });
  const planData = await planRes.json();
  if (!planRes.ok) { toast("生成计划失败", (planData.error || "")); return; }
  const plan = planData.plan || [];
  if (!plan.length) { toast("暂无测试计划", "项目尚未分析或没有可执行模块", "warn"); return; }
  const lines = plan.map((s, i) => `${i + 1}. [${s.kindRole || "?"}] ${escapeHtml(s.relPath)} → ${escapeHtml(s.testCommand) || "?"}`);
  if (!confirm("按测试计划执行（先构建后测试，串行）：\n\n" + lines.join("\n") + "\n\n继续？")) return;
  const st = document.getElementById("intelAnalyzeStatus");
  if (st) st.textContent = "已排队，等待测试计划执行…";
  const res = await api("/api/intel/plan", { method: "POST", headers: appHeaders(), body: JSON.stringify({ projectId: id }) });
  const data = await res.json();
  if (!res.ok) { if (st) st.textContent = ""; show(document.getElementById("intelMsg"), data.error || "计划执行失败"); return; }
  if (st) st.textContent = "测试计划已入队（异步执行中）";
  switchIntelTab("runs");
  loadIntelRuns(id);
}

async function cancelIntelRun(runId) {
  const data = await res.json();
  if (!res.ok) { toast("取消失败", data.error || "该运行已结束或不可取消", "warn"); return; }
  toast("已请求取消", "测试运行正在终止", "info");
  if (intelCurrentProject) loadIntelRuns(intelCurrentProject);
}

async function loadIntelImpact(id) {
  const wrap = document.getElementById("intelImpactList");
  wrap.innerHTML = `<div class="muted" style="text-align:center;padding:16px">加载中…</div>`;
  const res = await api("/api/intel/impact?projectId=" + id, { headers: appHeaders() });
  const data = await res.json();
  const raw = data.impact;
  if (!raw) { wrap.innerHTML = `<div class="muted" style="text-align:center;padding:16px">暂无影响面（git 项目重复分析后自动生成变更影响）</div>`; return; }
  let imp = {};
  try { imp = JSON.parse(raw.impactJson || "{}"); } catch (_) {}
  const base = raw.baseSha ? raw.baseSha.slice(0, 8) : "-";
  const head = raw.headSha ? raw.headSha.slice(0, 8) : "-";
  const list = (imp.files || []).length;
  wrap.innerHTML = "";
  wrap.insertAdjacentHTML("beforeend", `<div class="row muted" style="font-size:12px;margin-bottom:4px">
    基线 ${base} → ${head} · ${list} 个变更文件${imp.fullRescan ? " · <span class='badge warn'>需全量重扫</span>" : ""}
  </div>`);
  const sections = [
    ["受影响测试", imp.affectedTests, "mono"],
    ["受影响业务源码", imp.affectedSources, "mono"],
    ["变更测试文件", imp.changedTests, "mono"],
    ["需全量重扫模块", imp.widenedModules, "mono"],
  ];
  for (const [title, arr, cls] of sections) {
    const items = arr || [];
    if (!items.length) continue;
    wrap.insertAdjacentHTML("beforeend", `<div class="card" style="margin:0">
      <strong style="font-size:13px">${title}（${items.length}）</strong>
      <div style="margin-top:6px;display:flex;flex-direction:column;gap:2px">
        ${items.map(x => `<span class="${cls}" style="font-size:11.5px">${escapeHtml(x)}</span>`).join("")}
      </div>
    </div>`);
  }
  if (!list && !imp.fullRescan) {
    wrap.insertAdjacentHTML("beforeend", `<div class="muted" style="text-align:center;padding:16px">自上次分析以来无变更</div>`);
  }
}

async function loadIntelOverview(id) {
  const wrap = document.getElementById("intelOverviewList");
  wrap.innerHTML = `<div class="muted" style="text-align:center;padding:16px">加载中…</div>`;
  const res = await api("/api/intel/overview?projectId=" + id, { headers: appHeaders() });
  const data = await res.json();
  const raw = data.overview;
  if (!raw) { wrap.innerHTML = `<div class="muted" style="text-align:center;padding:16px">暂无依赖 / 环境画像（分析后自动生成）</div>`; return; }
  let envs = [], deps = [];
  try { envs = JSON.parse(raw.envJson || "[]"); } catch (_) {}
  try { deps = JSON.parse(raw.depsJson || "[]"); } catch (_) {}
  wrap.innerHTML = "";

  if (envs.length) {
    const envRows = envs.map(e => `<div class="row" style="justify-content:space-between;gap:8px">
      <span class="mono" style="font-size:12.5px">${escapeHtml(e.service)}${e.version ? "@" + escapeHtml(e.version) : ""}</span>
      <span class="muted" style="font-size:11px">${escapeHtml(e.category || "")} · ${escapeHtml(e.source || "")}</span>
    </div>`).join("");
    wrap.insertAdjacentHTML("beforeend", `<div class="card" style="margin:0">
      <strong style="font-size:13px">环境依赖（${envs.length}）</strong>
      <div style="margin-top:6px;display:flex;flex-direction:column;gap:4px">${envRows}</div>
    </div>`);
  }

  const byEco = new Map();
  for (const d of deps) {
    const k = d.ecosystem || "other";
    if (!byEco.has(k)) byEco.set(k, []);
    byEco.get(k).push(d);
  }
  if (deps.length) {
    let ecoHtml = "";
    for (const [eco, list] of byEco) {
      const rows = list.map(d => `<span class="mono muted" style="font-size:11.5px">${escapeHtml(d.group ? d.group + ":" : "")}${escapeHtml(d.name)}${d.version ? "@" + escapeHtml(d.version) : ""}</span>`).join("、");
      ecoHtml += `<div style="margin-top:6px"><span class="badge">${escapeHtml(eco)}</span> ${list.length} 项
        <div style="margin-top:4px;display:flex;flex-wrap:wrap;gap:4px 10px">${rows}</div></div>`;
    }
    wrap.insertAdjacentHTML("beforeend", `<div class="card" style="margin:0">
      <div class="row" style="justify-content:space-between">
        <strong style="font-size:13px">依赖清单（${deps.length}）</strong>
        <button class="ghost sm" onclick="downloadIntelSbom()">下载 SBOM</button>
      </div>${ecoHtml}
    </div>`);
  }
  if (!envs.length && !deps.length) {
    wrap.insertAdjacentHTML("beforeend", `<div class="muted" style="text-align:center;padding:16px">未检测到依赖或环境信息</div>`);
  }
}

let intelSbomCache = "";
async function downloadIntelSbom() {
  if (intelSbomCache) { saveTextFile("sbom.json", intelSbomCache); return; }
  const id = intelCurrentProject;
  const res = await api("/api/intel/overview?projectId=" + id, { headers: appHeaders() });
  const data = await res.json();
  const sbom = data.overview ? data.overview.sbomJson : "";
  if (!sbom) { show(document.getElementById("intelMsg"), "暂无 SBOM 数据"); return; }
  intelSbomCache = sbom;
  saveTextFile("sbom.json", sbom);
}

function saveTextFile(name, content) {
  const blob = new Blob([content], { type: "application/json" });
  const a = document.createElement("a");
  a.href = URL.createObjectURL(blob);
  a.download = name;
  a.click();
  URL.revokeObjectURL(a.href);
}

// 执行分析（不跳转）；在详情页时分析后原地刷新详情
async function runIntelAnalyze(projectId) {
  const id = projectId || intelCurrentProject;
  if (!id) return;
  const statusEl = document.getElementById("intelAnalyzeStatus");
  const onDetail = !!document.getElementById("page-intel-detail").classList.contains("hidden") === false;
  if (statusEl && onDetail) statusEl.textContent = "分析中…";
  const res = await api("/api/intel/analyze", { method: "POST", headers: appHeaders(), body: JSON.stringify({ projectId: id }) });
  const data = await res.json();
  if (!res.ok) {
    if (statusEl && onDetail) statusEl.textContent = "";
    show(document.getElementById("intelMsg"), data.error || "分析失败");
    return;
  }
  if (onDetail) {
    if (statusEl) statusEl.textContent = "分析完成";
    loadIntelDetail(id);
  } else {
    show(document.getElementById("intelMsg"), "分析完成");
    document.getElementById("intelMsg").classList.add("ok");
  }
  loadIntelProjects();
}

// 建立 / 重建知识库向量索引（实体/表 + 接口契约 → 向量化入库）
async function runIntelIndex() {
  const id = intelCurrentProject;
  if (!id) return;
  const btns = [document.getElementById("ragIndexBtn"), document.getElementById("ragIndexBtn2")].filter(Boolean);
  btns.forEach(b => { b.disabled = true; b.textContent = "索引中…"; });
  const res = await api("/api/intel/index", { method: "POST", headers: appHeaders(), body: JSON.stringify({ projectId: id }) });
  const data = await res.json();
  btns.forEach(b => { b.disabled = false; b.textContent = "重建索引"; });
  if (!res.ok) { show(document.getElementById("ragMsg"), data.error || "索引失败"); return; }
  show(document.getElementById("ragMsg"), "索引完成，共 " + (data.chunks || 0) + " 条（含项目概览、契约与文档）", true);
}

/* ---------- 知识库对话（项目级多轮，左侧会话列表可收起） ---------- */
let intelCurrentChat = 0;
let ragChatsCache = [];

function toggleRagSidebar() {
  const sb = document.getElementById("ragSidebar");
  sb.classList.toggle("collapsed");
}

async function initIntelChats() {
  const id = intelCurrentProject;
  if (!id) return;
  intelCurrentChat = 0;
  await loadRagChatList(0);
  document.getElementById("ragDelBtn").disabled = true;
  document.getElementById("ragCurrentTitle").textContent = "新对话";
  ragShowEmpty();
}

async function loadRagChatList(activeId) {
  const id = intelCurrentProject;
  const res = await api("/api/intel/chats?projectId=" + id, { headers: appHeaders() });
  const data = await res.json();
  ragChatsCache = data.chats || [];
  const list = document.getElementById("ragChatList");
  list.innerHTML = "";
  if (!ragChatsCache.length) {
    list.innerHTML = `<div class="muted" style="text-align:center;padding:12px;font-size:12px">暂无对话</div>`;
    return;
  }
  for (const c of ragChatsCache) {
    list.insertAdjacentHTML("beforeend",
      `<button class="rag-chat-item${c.id === activeId ? " active" : ""}" data-id="${c.id}" onclick="selectIntelChat(${c.id})">${escapeHtml(c.title || ("对话 " + c.id))}</button>`);
  }
}

function ragChatTitle(chatId) {
  const c = ragChatsCache.find(x => x.id === chatId);
  return c ? (c.title || ("对话 " + c.id)) : "新对话";
}

function newIntelChat() {
  intelCurrentChat = 0;
  document.getElementById("ragDelBtn").disabled = true;
  document.getElementById("ragCurrentTitle").textContent = "新对话";
  ragShowEmpty();
  document.querySelectorAll(".rag-chat-item").forEach(el => el.classList.remove("active"));
}

function ragShowEmpty() {
  document.getElementById("ragMessages").innerHTML =
    `<div class="rag-empty">向知识库提问，支持连续追问<br><span>基于已索引的项目概览、契约与文档</span></div>`;
}

function ragQuestionKeydown(e) {
  if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); askIntel(); }
}

function autoGrowRagInput(el) {
  el.style.height = "auto";
  el.style.height = Math.min(el.scrollHeight, 140) + "px";
}

async function selectIntelChat(chatId) {
  intelCurrentChat = chatId;
  document.getElementById("ragDelBtn").disabled = false;
  document.getElementById("ragCurrentTitle").textContent = ragChatTitle(chatId);
  document.querySelectorAll(".rag-chat-item").forEach(el => el.classList.toggle("active", Number(el.dataset.id) === chatId));
  const res = await api("/api/intel/chats/" + chatId, { headers: appHeaders() });
  const data = await res.json();
  if (!res.ok) { show(document.getElementById("ragMsg"), data.error || "加载失败"); return; }
  renderRagMessages(data.messages || []);
}

async function deleteIntelChat() {
  const chatId = intelCurrentChat;
  if (!chatId) return;
  const res = await api("/api/intel/chats/" + chatId, { method: "DELETE", headers: appHeaders() });
  if (!res.ok) { show(document.getElementById("ragMsg"), "删除失败"); return; }
  await initIntelChats();
}

function renderRagMessages(msgs) {
  const wrap = document.getElementById("ragMessages");
  wrap.innerHTML = "";
  for (const m of msgs) {
    appendRagBubble(m.role, m.content, m.sourcesJson ? safeParseJSON(m.sourcesJson) : null);
  }
  wrap.scrollTop = wrap.scrollHeight;
}

function appendRagBubble(role, content, sources) {
  const wrap = document.getElementById("ragMessages");
  const isUser = role === "user";
  let sourcesHtml = "";
  if (!isUser && sources && sources.length) {
    sourcesHtml = `<div class="rag-sources">引用：` +
      sources.map(s => escapeHtml(shortProv(s.sourceFile, s.sourceLine))).join("、") + `</div>`;
  }
  const label = isUser ? "你" : "AI 助手";
  wrap.insertAdjacentHTML("beforeend",
    `<div class="rag-msg ${isUser ? "user" : "assistant"}"><div class="rag-msg-label">${label}</div><div class="rag-bubble">${isUser ? escapeHtml(content) : `<div class="md">${mdRender(content)}</div>`}${sourcesHtml}</div></div>`);
  wrap.scrollTop = wrap.scrollHeight;
}

// 对项目提问：向量检索 + LLM 生成回答（多轮，携带 chatId）
async function askIntel() {
  const id = intelCurrentProject;
  const q = document.getElementById("ragQuestion").value.trim();
  if (!id || !q) { show(document.getElementById("ragMsg"), "请输入问题"); return; }
  const btn = document.getElementById("ragAskBtn");
  btn.disabled = true;
  const input = document.getElementById("ragQuestion");
  input.value = "";
  input.style.height = "auto";
  appendRagBubble("user", q, null);
  const res = await api("/api/intel/ask", { method: "POST", headers: appHeaders(), body: JSON.stringify({ projectId: id, question: q, chatId: intelCurrentChat }) });
  const data = await res.json();
  btn.disabled = false;
  if (!res.ok) { show(document.getElementById("ragMsg"), data.error || "提问失败"); return; }
  appendRagBubble("assistant", data.answer || "（未配置大模型，仅返回检索到的上下文）", data.sources || []);
  // 新会话：刷新列表并选中
  if (intelCurrentChat === 0 && data.chatId) {
    intelCurrentChat = data.chatId;
    document.getElementById("ragDelBtn").disabled = false;
    document.getElementById("ragCurrentTitle").textContent = ragChatTitle(data.chatId);
    await loadRagChatList(data.chatId);
  }
}

function safeParseJSON(s) { try { return JSON.parse(s); } catch (_) { return null; } }

/* ---------- 启动 ---------- */
document.getElementById("appToken").value = localStorage.getItem(APP_TOKEN_KEY) || "";
initThemePicker();

/* ---------- 实时动态列：宽度可拖拽 + 收起/展开 ---------- */
let wbEventsOpen = true;
let wbEventsW = 360;
function wbApplyEventsState() {
  const grid = document.getElementById("wbGrid");
  if (grid) {
    grid.style.setProperty("--wb-events-w", wbEventsW + "px");
    grid.classList.toggle("ev-collapsed", !wbEventsOpen);
  }
  const exp = document.getElementById("wbEventsExpand");
  if (exp) exp.style.display = wbEventsOpen ? "none" : "flex";
  const tg = document.getElementById("wbEventsToggle");
  if (tg) { tg.textContent = wbEventsOpen ? "» 收起" : "« 展开"; tg.title = wbEventsOpen ? "收起实时动态" : "展开实时动态"; }
}
function wbSetEventsOpen(open) {
  wbEventsOpen = !!open;
  try { localStorage.setItem("ocb_wb_events_open", wbEventsOpen ? "1" : "0"); } catch (_) {}
  wbApplyEventsState();
}
function wbInitEventsPanel() {
  try { wbEventsOpen = localStorage.getItem("ocb_wb_events_open") !== "0"; } catch (_) {}
  try { wbEventsW = Math.min(640, Math.max(200, Number(localStorage.getItem("ocb_wb_events_w")) || 360)); } catch (_) {}
  const tg = document.getElementById("wbEventsToggle");
  if (tg) tg.onclick = () => wbSetEventsOpen(!wbEventsOpen);
  const exp = document.getElementById("wbEventsExpand");
  if (exp) exp.onclick = () => wbSetEventsOpen(true);
  wbApplyEventsState();
  const h = document.getElementById("wbEventsHandle");
  const grid = document.getElementById("wbGrid");
  if (h && grid) {
    let dragging = false;
    h.addEventListener("mousedown", (e) => {
      dragging = true;
      document.body.style.cursor = "col-resize";
      document.body.style.userSelect = "none";
      e.preventDefault();
    });
    document.addEventListener("mousemove", (e) => {
      if (!dragging) return;
      const g = document.getElementById("wbGrid");
      if (!g) return;
      const rect = g.getBoundingClientRect();
      const w = Math.min(640, Math.max(200, Math.round(rect.right - e.clientX - 6)));
      wbEventsW = w;
      g.style.setProperty("--wb-events-w", w + "px");
    });
    document.addEventListener("mouseup", () => {
      if (!dragging) return;
      dragging = false;
      document.body.style.cursor = "";
      document.body.style.userSelect = "";
      try { localStorage.setItem("ocb_wb_events_w", String(wbEventsW)); } catch (_) {}
    });
  }
}
wbInitEventsPanel();
renderAuth();
connectTaskWS();