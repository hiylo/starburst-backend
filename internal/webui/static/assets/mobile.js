/* ============================================================================
 * StarBurst Mobile — 移动端会话工作台逻辑（隐藏入口 /mobile）
 * 复用 chat.js 的 SBChat.ChatView（聊天页）与 SBChat.Bus（SSE），本文件负责：
 * Admin 密码登录、会话列表、ctx 适配（对齐 app.js 的 wbChatCtx 契约）。
 * ========================================================================== */
"use strict";

const SESSION_KEY = "ocb_web_session";
const APP_TOKEN_KEY = "ocb_app_token";
const THEME_KEY = "sb.theme";
let session = localStorage.getItem(SESSION_KEY) || "";

const THEME_MODES = [
  { mode: "system", label: "跟随系统" },
  { mode: "light", label: "浅色" },
  { mode: "dark", label: "深色" },
  { mode: "amoled", label: "纯黑 AMOLED" },
  { mode: "dim", label: "柔和 Dim" },
];

/* ============================ Markdown 渲染 ============================ */
/* 从 app.js 平移：零依赖轻量渲染 + 代码高亮，供 chat.js 的 global.mdRender 使用。 */
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
function buildNestedList(items, inlineFn) {
  let html = "";
  const stack = [];
  for (const it of items) {
    const tag = it.ordered ? "ol" : "ul";
    while (stack.length && stack[stack.length - 1].depth > it.depth) html += `</${stack.pop().tag}>`;
    const top = stack[stack.length - 1];
    if (top && top.depth === it.depth && top.tag === tag) {
    } else {
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
  return /^\s*[-*+]\s+/.test(l) || /^\s*\d+[.)]\s/.test(l);
}
function isTaskContent(content) {
  const m = /^\[([ xX])\]\s+/.exec(content);
  return m ? { done: m[1].toLowerCase() === "x", rest: content.slice(m[0].length) } : null;
}
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
    const hm = line.match(/^(#{1,6})\s+(.*)$/);
    if (hm) {
      const lvl = hm[1].length;
      html += `<h${lvl}>${inline(hm[2])}</h${lvl}>\n`;
      i++;
      continue;
    }
    if (isHr(line)) { html += "<hr>\n"; i++; continue; }
    if (/^\s*>/.test(line)) {
      const buf = [];
      while (i < lines.length && /^\s*>/.test(lines[i])) buf.push(lines[i++].replace(/^\s*>\s?/, ""));
      html += `<blockquote>${mdRender(buf.join("\n"))}</blockquote>\n`;
      continue;
    }
    if (line.includes("|") && i + 1 < lines.length && /^\s*\|?[\s:|-]+\|[\s:|-]*$/.test(lines[i + 1])) {
      const parseRow = (r) => r.trim().replace(/^\|/, "").replace(/\|$/, "").split("|").map(c => c.trim());
      const header = parseRow(line);
      i += 2;
      const rows = [];
      while (i < lines.length && lines[i].includes("|")) rows.push(parseRow(lines[i++]));
      html += `<div class="md-table-wrap"><table><thead><tr>${header.map(h => `<th>${inline(h)}</th>`).join("")}</tr></thead><tbody>${rows.map(r => `<tr>${r.map(c => `<td>${inline(c)}</td>`).join("")}</tr>`).join("")}</tbody></table></div>\n`;
      continue;
    }
    if (isListLine(line)) {
      const collected = [];
      // 列表项之间允许空行（LLM 常见输出），空行后仍是列表项则合并为同一列表；
      // 空行后是普通段落则终止，避免把后续段落吞进列表。
      while (i < lines.length) {
        if (isListLine(lines[i])) collected.push(lines[i++]);
        else if (!lines[i].trim()) {
          let j = i;
          while (j < lines.length && !lines[j].trim()) j++;
          if (j < lines.length && isListLine(lines[j])) i = j;
          else break;
        } else break;
      }
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
    const buf = [];
    while (i < lines.length && lines[i].trim() && !isListLine(lines[i]) && !/^\s*```/.test(lines[i]) && !/^\s*>/.test(lines[i]) && !/^#{1,6}\s/.test(lines[i]) && !isHr(lines[i])) {
      buf.push(lines[i++]);
    }
    html += `<p>${inline(buf.join("\n")).replace(/\n/g, "<br>")}</p>\n`;
  }
  return html;
}
window.mdRender = mdRender;
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

/* ============================ 主题 ============================ */
const prefersLight = window.matchMedia("(prefers-color-scheme: light)");
function resolveTheme(mode) { return mode === "system" ? (prefersLight.matches ? "light" : "dark") : mode; }
function currentThemeMode() {
  const m = localStorage.getItem(THEME_KEY) || "system";
  return THEME_MODES.some(t => t.mode === m) ? m : "system";
}
function applyTheme() {
  document.documentElement.dataset.theme = resolveTheme(currentThemeMode());
  const sel = document.getElementById("themeMode");
  if (sel) sel.value = currentThemeMode();
}
function initThemePicker() {
  const sel = document.getElementById("themeMode");
  if (sel) {
    sel.innerHTML = THEME_MODES.map(t => `<option value="${t.mode}">${t.label}</option>`).join("");
    sel.addEventListener("change", () => { localStorage.setItem(THEME_KEY, sel.value); applyTheme(); });
  }
  prefersLight.addEventListener("change", () => { if (currentThemeMode() === "system") applyTheme(); });
  applyTheme();
}

/* ============================ 鉴权 / 网络 ============================ */
function hdr(extra) {
  const h = { "Content-Type": "application/json" };
  if (session) h["X-Web-Session"] = session;
  return Object.assign(h, extra || {});
}
function appHeaders() {
  const t = localStorage.getItem(APP_TOKEN_KEY) || "";
  const h = { "Content-Type": "application/json" };
  if (t) h["Authorization"] = "Bearer " + t;
  if (session) h["X-Web-Session"] = session;
  return h;
}
async function api(path, opts) {
  opts = opts || {};
  const res = await fetch(path, opts);
  const usesWebSession = !!(opts.headers && (opts.headers["X-Web-Session"] || opts.headers["x-web-session"]));
  if (res.status === 401 && session && usesWebSession) {
    session = "";
    localStorage.removeItem(SESSION_KEY);
    renderAuth();
    throw new Error("unauthorized");
  }
  return res;
}
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
function show(el, msg, ok) {
  el.textContent = msg || "";
  el.className = "msg " + (ok ? "ok" : "err");
}
function copyText(text, okTitle) {
  const done = () => toast(okTitle || "已复制", text.slice(0, 80), "info");
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(text).then(done).catch(() => copyFallback(text, done));
  } else copyFallback(text, done);
}
function copyFallback(text, done) {
  const ta = document.createElement("textarea");
  ta.value = text;
  ta.style.position = "fixed"; ta.style.opacity = "0";
  document.body.appendChild(ta);
  ta.select();
  try { document.execCommand("copy"); done(); } catch (_) { toast("复制失败", "无法访问剪贴板", "warn"); }
  document.body.removeChild(ta);
}

/* ============================ 登录 ============================ */
async function doLogin() {
  const pw = document.getElementById("pw").value;
  const res = await fetch("/api/web/session", { method: "POST", headers: hdr(), body: JSON.stringify({ password: pw }) });
  const data = await res.json();
  if (!res.ok) { show(document.getElementById("loginMsg"), data.error || "登录失败"); return; }
  session = data.session;
  localStorage.setItem(SESSION_KEY, session);
  document.getElementById("pw").value = "";
  renderAuth();
}
function doLogout() {
  session = "";
  localStorage.removeItem(SESSION_KEY);
  if (chatUnsub) { chatUnsub(); chatUnsub = null; }
  if (window.SBChat) SBChat.Bus.close();
  ["settingsModal", "moreSheet", "newSheet", "modal"].forEach(id => {
    const el = document.getElementById(id);
    if (el) el.classList.add("hidden");
  });
  closePanel(true);
  renderAuth();
}
function renderAuth() {
  const logged = !!session;
  document.getElementById("loginView").classList.toggle("hidden", logged);
  document.getElementById("listView").classList.toggle("hidden", !logged);
  if (!logged) {
    document.getElementById("chatView").classList.add("hidden");
  } else {
    loadWorkbench();
  }
}
function saveAppToken() {
  const v = document.getElementById("appToken").value.trim();
  localStorage.setItem(APP_TOKEN_KEY, v);
  document.getElementById("appToken").value = v;
  toast("已保存", v ? "APP Token 已保存" : "APP Token 已清除", "info");
  if (window.SBChat && chatUnsub) SBChat.Bus.restart();
}
function settingsOpen() {
  document.getElementById("appToken").value = localStorage.getItem(APP_TOKEN_KEY) || "";
  document.getElementById("settingsModal").classList.remove("hidden");
}
function settingsClose() { document.getElementById("settingsModal").classList.add("hidden"); }

/* ============================ 状态 ============================ */
let sessions = [], statuses = {}, pending = {}, permissions = {}, items = [];
let selected = null, panelData = null, sending = false, loading = false;
let newSet = new Set(), stars = new Set(), sessTags = {};
let filterOption = "all", tagFilter = null;
let chat = null, chatUnsub = null;
let providers = null, providersState = "idle";
let agentsList = [];
let qAnswers = {}, permBusy = new Set(), qBusy = new Set();
let agentChoice = {};
let listRenderTimer = null, panelRefreshTimer = null, workbenchTimer = null, panelFetching = null;

function wbRank(st) { return st === "question" ? 0 : (st === "busy" || st === "retry") ? 1 : 2; }
function statusLabel(st) { return { question: "提问中", busy: "处理中", retry: "重试中", idle: "空闲" }[st] || "空闲"; }
function modelId(m) {
  if (!m) return "";
  if (typeof m === "object") return m.id || "";
  return String(m);
}

function loadLocal() {
  try { stars = new Set(JSON.parse(localStorage.getItem("ocb_session_stars") || "[]")); } catch (_) { stars = new Set(); }
  try { const t = JSON.parse(localStorage.getItem("ocb_session_tags") || "{}"); sessTags = (t && typeof t === "object") ? t : {}; } catch (_) { sessTags = {}; }
}
function saveStars() { try { localStorage.setItem("ocb_session_stars", JSON.stringify([...stars])); } catch (_) {} }
function saveTags() { try { localStorage.setItem("ocb_session_tags", JSON.stringify(sessTags)); } catch (_) {} }
function allTags() {
  const m = new Map();
  for (const sid of Object.keys(sessTags)) for (const t of (sessTags[sid] || [])) m.set(t, (m.get(t) || 0) + 1);
  return [...m.entries()].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]));
}

/* ============================ 会话列表 ============================ */
function buildItems() {
  const childBusy = {};
  const parentQ = new Set();
  const parentPending = {};
  const parentPerm = {};
  for (const s of sessions) {
    const pid = s.parentID;
    if (!pid) continue;
    const pq = pending[s.id];
    if (pq && pq.length) { parentQ.add(pid); (parentPending[pid] = parentPending[pid] || []).push(...pq); }
    const pps = permissions[s.id];
    if (pps && pps.length) (parentPerm[pid] = parentPerm[pid] || []).push(...pps);
    const st = statuses[s.id];
    if (st && (st.type === "busy" || st.type === "retry")) childBusy[pid] = st.type;
  }
  const roots = sessions.filter(s => !s.parentID && !(s.time && s.time.archived));
  return roots.map(s => {
    const self = statuses[s.id];
    const hasQ = (pending[s.id] && pending[s.id].length) || parentQ.has(s.id);
    const perms = [...(permissions[s.id] || []), ...(parentPerm[s.id] || [])];
    const p = [...(pending[s.id] || []), ...(parentPending[s.id] || [])];
    let st;
    if (hasQ || (perms && perms.length)) st = "question";
    else if (self && (self.type === "busy" || self.type === "retry")) st = self.type;
    else st = childBusy[s.id] || (self && self.type) || "idle";
    return { session: s, status: st, pending: p, permissions: perms };
  }).sort((a, b) => {
    const r = wbRank(a.status) - wbRank(b.status);
    if (r) return r;
    const ta = (a.session.time && a.session.time.updated) || 0;
    const tb = (b.session.time && b.session.time.updated) || 0;
    if (ta !== tb) return tb - ta;
    return a.session.id < b.session.id ? 1 : -1;
  });
}

function renderFilters() {
  const box = document.getElementById("filters");
  if (!box) return;
  const opts = [["all", "全部"], ["question", "提问中"], ["busy", "处理中"], ["idle", "空闲"]];
  box.innerHTML = opts.map(([k, l]) =>
    `<button class="wb-filter ${filterOption === k ? "active" : ""}" data-f="${k}">${l}</button>`).join("");
  box.onclick = (e) => {
    const btn = e.target.closest("button[data-f]");
    if (!btn) return;
    filterOption = btn.dataset.f;
    renderFilters();
    renderList();
  };
}
function renderTagFilters() {
  const box = document.getElementById("tagFilters");
  if (!box) return;
  const chips = [["", "全部"]];
  if (stars.size) chips.push(["star", `⭐ 书签 ${stars.size}`]);
  for (const [t, n] of allTags()) chips.push(["tag:" + t, `#${t} ${n}`]);
  box.innerHTML = chips.map(([k, l]) =>
    `<button class="wb-tagfilter ${tagFilter === k ? "active" : ""}" data-tf="${escapeHtml(k)}">${escapeHtml(l)}</button>`).join("");
  box.onclick = (e) => {
    const btn = e.target.closest("button[data-tf]");
    if (!btn) return;
    tagFilter = btn.dataset.tf || null;
    renderTagFilters();
    renderList();
  };
}

function fmtListTime(ts) {
  if (!ts) return "";
  const d = new Date(ts);
  const now = new Date();
  if (d.toDateString() === now.toDateString()) {
    return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  }
  const y = d.getFullYear() === now.getFullYear() ? "" : "/" + d.getFullYear();
  return (d.getMonth() + 1) + "/" + d.getDate() + y;
}

function itemHtml(it) {
  const s = it.session;
  const id = s.id;
  const title = (s.title || s.slug || id).toString();
  const dir = (s.directory || "").toString();
  const model = modelId(s.model);
  const q = (it.pending || []).length;
  const pc = (it.permissions || []).length;
  const active = selected === id ? "active" : "";
  const badges = (q ? `<span class="qbadge">提问 ${q}</span>` : "") +
    (pc ? `<span class="qbadge perm" title="待授权操作">授权 ${pc}</span>` : "");
  const newDot = newSet.has(id) ? `<span class="newdot" title="有新消息"></span>` : "";
  const starred = stars.has(id);
  const tags = (sessTags[id] || []);
  const tagsHtml = tags.length
    ? `<div class="m-item-tags">${tags.map(t => `<span class="wb-tag" data-tag-jump="${escapeHtml(t)}">#${escapeHtml(t)}</span>`).join("")}</div>`
    : "";
  return `<div class="wb-item m-item ${active}" data-sid="${escapeHtml(id)}">
    <span class="dot ${escapeHtml(it.status)}"></span>
    <div class="m-item-body">
      <div class="m-item-line1">
        <span class="m-item-title">${escapeHtml(title)}</span>${newDot}
        <span class="m-item-time">${fmtListTime(s.time && s.time.updated)}</span>
      </div>
      <div class="m-item-line2">
        <span class="m-item-dir">${escapeHtml(dir) || "—"}</span>
        ${model ? `<span class="m-item-model" title="会话模型">${escapeHtml(model)}</span>` : ""}
        ${badges}
        <button class="wb-star${starred ? " on" : ""}" type="button" data-star="${escapeHtml(id)}" title="${starred ? "取消收藏" : "收藏会话"}" aria-label="收藏">${starred ? "★" : "☆"}</button>
      </div>
      ${tagsHtml}
    </div>
  </div>`;
}

function renderList() {
  items = buildItems();
  const box = document.getElementById("list");
  const kw = (document.getElementById("filter").value || "").trim().toLowerCase();
  let matched = items.filter(it => {
    if (filterOption === "question" && wbRank(it.status) !== 0) return false;
    if (filterOption === "busy" && wbRank(it.status) !== 1) return false;
    if (filterOption === "idle" && wbRank(it.status) !== 2) return false;
    if (tagFilter === "star" && !stars.has(it.session.id)) return false;
    if (tagFilter && tagFilter.indexOf("tag:") === 0 && !(sessTags[it.session.id] || []).includes(tagFilter.slice(4))) return false;
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
  for (const it of matched) (groups[it.status] || groups.idle).push(it);
  const labels = { question: "提问中", busy: "处理中", retry: "重试中", idle: "空闲" };
  let html = "";
  for (const g of ["question", "busy", "retry", "idle"]) {
    const list = groups[g];
    if (!list.length) continue;
    html += `<div class="wb-group-title">${labels[g]}（${list.length}）</div>`;
    for (const it of list) html += itemHtml(it);
  }
  if (html) {
    box.innerHTML = html;
  } else if (!sessions.length) {
    box.innerHTML = `<div class="wb-placeholder"><div class="ico" aria-hidden="true"><svg width="30" height="30" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"><path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/></svg></div>还没有会话<br><span class="muted" style="font-size:12px">点击右上角 ＋ 新建会话</span></div>`;
  } else {
    box.innerHTML = `<div class="wb-placeholder">无匹配会话<br><span class="muted" style="font-size:12px">调整关键词或筛选条件</span></div>`;
  }
  box.onclick = (e) => {
    const star = e.target.closest("[data-star]");
    if (star) {
      const sid = star.dataset.star;
      if (stars.has(sid)) stars.delete(sid); else stars.add(sid);
      saveStars();
      renderTagFilters();
      renderList();
      return;
    }
    const tj = e.target.closest("[data-tag-jump]");
    if (tj) { tagFilter = "tag:" + tj.dataset.tagJump; renderTagFilters(); renderList(); return; }
    const el = e.target.closest(".wb-item");
    if (el) openPanel(el.dataset.sid);
  };
}

async function loadWorkbench() {
  if (loading) return;
  loading = true;
  try {
    const [sr, stR] = await Promise.all([
      api("/api/opencode/experimental/session", { headers: appHeaders() }),
      api("/api/opencode/session/status", { headers: appHeaders() }),
    ]);
    if (!sr.ok) { toast("会话列表加载失败", "HTTP " + sr.status, "crit"); return; }
    const data = await sr.json();
    sessions = Array.isArray(data) ? data : (data.sessions || data.data || []);
    let st = {};
    if (stR.ok) { try { st = await stR.json(); } catch (_) {} }
    statuses = st || {};
    pending = {};
    const dirs = [...new Set(sessions.map(s => s.directory).filter(Boolean))];
    const qResps = await Promise.all(dirs.map(d =>
      api("/api/opencode/question?directory=" + encodeURIComponent(d), { headers: appHeaders() })
        .then(r => (r.ok ? r.json() : []))
        .catch(() => [])
    ));
    for (const qs of qResps) for (const q of (Array.isArray(qs) ? qs : [])) {
      if (q && q.sessionID) (pending[q.sessionID] = pending[q.sessionID] || []).push(q);
    }
    permissions = {};
    const permGlobal = await api("/api/opencode/permission", { headers: appHeaders() })
      .then(r => (r.ok ? r.json() : []))
      .catch(() => []);
    const permResps = await Promise.all(dirs.map(d =>
      api("/api/opencode/permission?directory=" + encodeURIComponent(d), { headers: appHeaders() })
        .then(r => (r.ok ? r.json() : []))
        .catch(() => [])
    ));
    const allPerms = [...(Array.isArray(permGlobal) ? permGlobal : [])];
    for (const ps of permResps) if (Array.isArray(ps)) allPerms.push(...ps);
    const permSeen = new Set();
    for (const p of allPerms) {
      if (!p || !p.sessionID || permSeen.has(p.id)) continue;
      permSeen.add(p.id);
      (permissions[p.sessionID] = permissions[p.sessionID] || []).push(p);
    }
    renderFilters();
    renderTagFilters();
    renderList();
    if (selected) refreshPanel(selected);
    syncUnread();
  } catch (e) {
    if (e.message !== "unauthorized") toast("加载失败", e.message || "未知错误", "crit");
  } finally {
    loading = false;
  }
}

function openNewSheet() {
  const sheet = document.getElementById("newSheet");
  if (!sheet) return;
  sheet.classList.remove("hidden");
  const dirs = [...new Set(sessions.map(s => s.directory).filter(Boolean))];
  document.getElementById("dirList").innerHTML = dirs.map(d => `<option value="${escapeHtml(d)}">`).join("");
  show(document.getElementById("newMsg"), "");
  setTimeout(() => { const d = document.getElementById("newDir"); if (d) d.focus(); }, 250);
}
function closeNewSheet() {
  const sheet = document.getElementById("newSheet");
  if (sheet) sheet.classList.add("hidden");
}
async function createSession() {
  const dir = document.getElementById("newDir").value.trim();
  const title = document.getElementById("newTitle").value.trim();
  const msg = document.getElementById("newMsg");
  if (!dir) { show(msg, "请填写工作目录"); return; }
  const body = {};
  if (title) body.title = title;
  const url = `/api/opencode/session?directory=${encodeURIComponent(dir)}&x-opencode-directory=${encodeURIComponent(dir)}`;
  const headers = appHeaders();
  headers["x-starburst-directory"] = dir;
  headers["x-opencode-directory"] = dir;
  const res = await api(url, { method: "POST", headers, body: JSON.stringify(body) });
  if (!res.ok) { show(msg, "创建失败 (" + res.status + ")"); return; }
  const data = await res.json();
  toast("已创建会话", data.id, "info");
  show(msg, "已创建 " + data.id, true);
  document.getElementById("newTitle").value = "";
  document.getElementById("newDir").value = "";
  closeNewSheet();
  await loadWorkbench();
  if (data && data.id) openPanel(data.id);
}

/* ============================ 未读 ============================ */
function unreadTrigger(t) {
  return ["message.complete", "message.created", "message.updated",
    "question.asked", "question.updated", "permission.asked",
    "session.idle", "session.status", "session.error", "session.failed"].includes(t);
}
async function syncUnread() {
  try {
    const res = await api("/api/unread", { headers: appHeaders() });
    if (!res.ok) return;
    const d = await res.json();
    const m = d.unread || {};
    const next = new Set(Object.keys(m).filter(k => m[k]));
    if (next.size !== newSet.size || [...next].some(x => !newSet.has(x))) {
      newSet = next;
      renderList();
    }
  } catch (_) {}
}
async function markRead(sid) {
  if (!sid) return;
  if (newSet.has(sid)) { newSet.delete(sid); renderList(); }
  try { await api(`/api/unread/${encodeURIComponent(sid)}`, { method: "POST", headers: appHeaders() }); } catch (_) {}
}
function handleActivity(sid) {
  if (!sid) return;
  if (selected === sid) { markRead(sid); return; }
  if (!newSet.has(sid)) { newSet.add(sid); scheduleRenderList(); }
}

/* ============================ 面板 / 聊天 ============================ */
function ensureChat() {
  const host = document.getElementById("chat");
  if (!host || !window.SBChat) return null;
  if (chat && chat.host === host) return chat;
  chat = new SBChat.ChatView();
  chat.mount(host, ctx());
  syncStream();
  return chat;
}
function openPanel(id) {
  const switched = selected !== id;
  if (!id) { closePanel(); return; }
  selected = id;
  markRead(id);
  document.getElementById("listView").classList.add("hidden");
  document.getElementById("chatView").classList.remove("hidden");
  if (switched) {
    panelData = { id, loading: true, pending: [], permissions: [], status: "", session: null };
    qAnswers = {};
    renderShell();
    const c = ensureChat();
    if (c) {
      // 移动端不恢复草稿：ChatView.open 会从 localStorage 恢复上次未发送的文本，
      // 若残留旧文本会与本次新输入混在一起发出（「新消息混入旧文本」）。前缀与
      // chat.js 的 DRAFT_PREFIX / ATT_DRAFT_PREFIX 一致，打开会话前先清除。
      try { localStorage.removeItem("ocb_chat_draft_" + id); localStorage.removeItem("ocb_chat_draft_atts_" + id); } catch (_) {}
      ensureAgents().then(() => c.refreshSelectors());
      c.open(id).then(() => {
        if (chat && chat.sessionId === id) {
          const it = items.find(x => x.session.id === id);
          const st = it ? it.status : "";
          if (st === "busy" || st === "retry") { chat.setWorking(st === "retry" ? "重试中…" : "处理中…"); chat.syncSendIcon(); }
        }
      });
    }
  } else if (chat) {
    chat.renderSelectors();
  }
  renderList();
  refreshPanel(id, true);
  syncStream();
}
function closePanel(skipList) {
  selected = null;
  panelData = null;
  if (chat) { try { chat.unmount(); } catch (_) {} chat = null; }
  syncStream();
  panelFetching = null;
  document.getElementById("chatView").classList.add("hidden");
  document.getElementById("shell").innerHTML = "";
  if (!skipList && session) document.getElementById("listView").classList.remove("hidden");
  renderList();
}

function owningDir(reqId) {
  for (const map of [pending, permissions]) {
    for (const sid of Object.keys(map)) {
      if ((map[sid] || []).some(x => x.id === reqId)) {
        const s = sessions.find(x => x.id === sid);
        if (s && s.directory) return s.directory;
      }
    }
  }
  return "";
}

function renderShell() {
  const shell = document.getElementById("shell");
  const d = panelData;
  if (!d) { shell.innerHTML = ""; return; }
  if (d.loading || !d.session) {
    document.getElementById("chatTitle").textContent = "加载中…";
    shell.innerHTML = "";
    return;
  }
  const s = d.session || {};
  document.getElementById("chatTitle").textContent = (s.title || s.slug || d.id).toString();
  const stDot = document.getElementById("chatStatusDot");
  if (stDot) {
    const st = d.status || "idle";
    stDot.className = "dot m-status-dot " + st;
    stDot.style.display = "";
    stDot.title = statusLabel(st);
  }
  const qhtml = renderQuestions();
  const permHtml = renderPermissions(d.permissions || []);
  const meta = [];
  const model = modelId(s.model);
  const agent = s.agent ? String(s.agent) : "";
  const dir = (s.directory || "").toString();
  if (model) meta.push(`<span class="chip"><i>模型</i>${escapeHtml(model)}</span>`);
  if (agent) meta.push(`<span class="chip"><i>Agent</i>${escapeHtml(agent)}</span>`);
  if (dir) meta.push(`<span class="chip"><i>目录</i>${escapeHtml(dir)}</span>`);
  shell.innerHTML = (meta.length ? `<div class="m-meta">${meta.join("")}</div>` : "")
    + `<div class="wb-blocks${(permHtml || qhtml) ? "" : " empty"}">`
    + (permHtml ? `<div class="wb-block"><h4>待授权操作<span class="info">${(d.permissions || []).length} 项</span></h4>${permHtml}</div>` : "")
    + (qhtml ? `<div class="wb-block"><h4>待决问题<span class="info">${(d.pending || []).reduce((n, q) => n + (q.questions || []).length, 0)} 个</span></h4>${qhtml}</div>` : "")
    + `</div>`;
  shell.onclick = (e) => {
    const pbp = e.target.closest("[data-wb-perm]");
    if (pbp) { permReply(pbp.dataset.wbPerm, pbp.dataset.reply); return; }
    const opt = e.target.closest("[data-qopt]");
    if (opt) { qToggle(opt.dataset.qopt, Number(opt.dataset.qidx), opt.dataset.label); return; }
    const rej = e.target.closest("[data-qreject]");
    if (rej) { rejectQ(rej.dataset.qreject); return; }
    const sub = e.target.closest("[data-qsubmit]");
    if (sub) { qSubmit(sub.dataset.qsubmit); return; }
    const cus = e.target.closest("[data-qcustom]");
    if (cus) { qCustom(cus.dataset.qcustom, Number(cus.dataset.qidx)); return; }
  };
  shell.onkeydown = (e) => {
    if (e.key !== "Enter" || e.isComposing) return;
    const inp = e.target.closest("[id^='wbQInput_']");
    if (!inp) return;
    e.preventDefault();
    const m = inp.id.match(/^wbQInput_(.+)_(\d+)$/);
    if (!m) return;
    qCustom(m[1], Number(m[2]));
  };
}

async function refreshPanel(id, force) {
  if (id !== selected) return;
  if (!force && panelFetching === id) return;
  panelFetching = id;
  try {
    if (id !== selected) return;
    if (!items.length) items = buildItems();
    const item = items.find(x => x.session.id === id);
    panelData = {
      id,
      pending: (item && item.pending) || [],
      permissions: (item && item.permissions) || [],
      status: item ? item.status : "idle",
      session: item ? item.session : (panelData && panelData.session) || null,
    };
    renderShell();
    if (chat) chat.renderSelectors();
    if (providersState === "idle" || providersState === "failed") ensureProviders().then(() => { if (chat) chat.renderSelectors(); });
  } finally {
    if (panelFetching === id) panelFetching = null;
  }
}

/* ============================ 问题 / 授权 ============================ */
function renderPermissions(perms) {
  return (perms || []).map(p => {
    const patterns = (p.patterns || []).join("、") || "-";
    const tool = (p.tool && (p.tool.type || p.tool.id || p.tool.name)) ? ` · ${escapeHtml(p.tool.type || p.tool.id || p.tool.name)}` : "";
    const always = (p.always || []).join("、");
    const busy = permBusy.has(p.id);
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
async function permReply(reqId, reply) {
  if (!reqId || permBusy.has(reqId)) return;
  if (reply === "always") {
    const item = (permissions[selected] || []).find(x => x.id === reqId)
      || Object.values(permissions).flat().find(x => x.id === reqId) || {};
    const always = (item.always || []).join("、");
    const body = "将来匹配操作可能会自动批准。" + (always ? `\n适用于：${always}` : "");
    if (!window.confirm("始终允许此权限？\n" + body)) return;
  }
  permBusy.add(reqId);
  renderShell();
  try {
    const item = items.find(x => x.session.id === selected);
    const dir = owningDir(reqId) || (item && item.session && item.session.directory) || "";
    const url = `/api/opencode/permission/${encodeURIComponent(reqId)}/reply` + (dir ? "?directory=" + encodeURIComponent(dir) : "");
    const res = await api(url, { method: "POST", headers: appHeaders(), body: JSON.stringify({ reply }) });
    if (!res.ok) { toast("授权失败", "权限回复未送达 (" + res.status + ")", "crit"); return; }
    toast("已处理", reply === "reject" ? "已拒绝该操作" : (reply === "always" ? "已始终允许" : "已允许一次"), "info");
    for (const sid of Object.keys(permissions)) permissions[sid] = (permissions[sid] || []).filter(p => p.id !== reqId);
    if (panelData && panelData.permissions) panelData.permissions = panelData.permissions.filter(p => p.id !== reqId);
    renderShell();
    loadWorkbench();
  } finally {
    permBusy.delete(reqId);
  }
}

function renderQuestions() {
  const pend = (panelData && panelData.pending) || [];
  const valid = new Set(pend.map(x => x.id));
  Object.keys(qAnswers).forEach(k => { if (!valid.has(k)) delete qAnswers[k]; });
  return pend.map(q => {
    const qs = q.questions || [];
    if (!qs.length) return "";
    const isSingle = qs.length === 1 && qs[0].multiple !== true;
    const busy = qBusy.has(q.id);
    const sel = (qAnswers[q.id] = qAnswers[q.id] || qs.map(() => []));
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
function qToggle(reqId, qidx, label) {
  const q = ((panelData && panelData.pending) || []).find(x => x.id === reqId);
  if (!q || qBusy.has(reqId)) return;
  const qs = q.questions || [];
  const qu = qs[qidx];
  if (!qu) return;
  const sel = (qAnswers[reqId] = qAnswers[reqId] || qs.map(() => []));
  const cur = sel[qidx] || [];
  if (qu.multiple === true) {
    sel[qidx] = cur.includes(label) ? cur.filter(x => x !== label) : [...cur, label];
  } else {
    sel[qidx] = [label];
    if (qs.length === 1) { postAnswer(reqId, [[label]]); return; }
  }
  renderShell();
}
function qCustom(reqId, qidx) {
  const input = document.getElementById("wbQInput_" + reqId + "_" + qidx);
  const v = input ? input.value.trim() : "";
  if (!v) { toast("答复失败", "请先输入回答内容"); return; }
  const q = ((panelData && panelData.pending) || []).find(x => x.id === reqId);
  if (!q || qBusy.has(reqId)) return;
  const qs = q.questions || [];
  const sel = (qAnswers[reqId] = qAnswers[reqId] || qs.map(() => []));
  sel[qidx] = [v];
  if (qs.length === 1 && (qs[0].multiple !== true)) { postAnswer(reqId, [[v]]); return; }
  renderShell();
}
function qSubmit(reqId) {
  const q = ((panelData && panelData.pending) || []).find(x => x.id === reqId);
  if (!q || qBusy.has(reqId)) return;
  const qs = q.questions || [];
  const sel = (qAnswers[reqId] = qAnswers[reqId] || qs.map(() => []));
  const answers = qs.map((_, i) => (sel[i] || []).slice());
  if (!answers.some(a => a.length)) { toast("提交失败", "请至少回答一个问题"); return; }
  postAnswer(reqId, answers);
}
async function postAnswer(reqId, answers) {
  if (qBusy.has(reqId)) return;
  qBusy.add(reqId);
  renderShell();
  try {
    const dir = owningDir(reqId) || (panelData && panelData.session && panelData.session.directory) || "";
    const url = `/api/opencode/question/${encodeURIComponent(reqId)}/reply` + (dir ? "?directory=" + encodeURIComponent(dir) : "");
    const res = await api(url, { method: "POST", headers: appHeaders(), body: JSON.stringify({ answers }) });
    if (!res.ok) { toast("答复失败", "问题回复未送达 (" + res.status + ")", "crit"); return; }
    toast("已答复", "问题已回复", "info");
    if (panelData) panelData.pending = panelData.pending.filter(x => x.id !== reqId);
    for (const sid of Object.keys(pending)) pending[sid] = (pending[sid] || []).filter(x => x.id !== reqId);
    delete qAnswers[reqId];
    renderShell();
    loadWorkbench();
  } finally {
    qBusy.delete(reqId);
  }
}
async function rejectQ(reqId) {
  if (qBusy.has(reqId)) return;
  qBusy.add(reqId);
  renderShell();
  try {
    const dir = owningDir(reqId) || (panelData && panelData.session && panelData.session.directory) || "";
    const url = `/api/opencode/question/${encodeURIComponent(reqId)}/reject` + (dir ? "?directory=" + encodeURIComponent(dir) : "");
    const res = await api(url, { method: "POST", headers: appHeaders() });
    if (!res.ok) { toast("拒绝失败", "拒绝未送达 (" + res.status + ")", "crit"); return; }
    toast("已拒绝", "问题已拒绝", "info");
    if (panelData) panelData.pending = panelData.pending.filter(q => q.id !== reqId);
    for (const sid of Object.keys(pending)) pending[sid] = (pending[sid] || []).filter(x => x.id !== reqId);
    delete qAnswers[reqId];
    renderShell();
    loadWorkbench();
  } finally {
    qBusy.delete(reqId);
  }
}

/* ============================ 模型 / Agent ============================ */
async function ensureProviders(force) {
  if (providers && !force) return providers;
  if (!force && (providersState === "loading" || providersState === "loaded")) return providers;
  providersState = "loading";
  try {
    const res = await api("/api/opencode/config/providers", { headers: appHeaders() });
    if (!res.ok) { providersState = "failed"; if (chat) chat.renderSelectors(); return providers; }
    const d = await res.json();
    const list = (d.providers || []).map(p => ({
      id: p.id,
      name: p.name || p.id,
      models: Object.values(p.models || {}).map(m => ({
        id: m.id,
        name: m.name || m.id,
        variant: m.variant || "default",
        context: (m.limit && m.limit.context) || 0,
        attachment: !!(m.capabilities && m.capabilities.attachment),
      })),
    })).filter(p => p.models.length);
    providers = list;
    providersState = "loaded";
    return list;
  } catch (_) {
    providersState = "failed";
    return providers;
  } finally {
    if (chat) chat.renderSelectors();
  }
}
function sessionModelValue() {
  const m = (panelData && panelData.session && panelData.session.model) || {};
  if (!m.id) return "";
  return (m.providerID || "") + "\u0001" + m.id + "\u0001" + (m.variant || "default");
}
function modelOptions() {
  const list = providers;
  if (!list) return `<option value="" disabled selected>${providersState === "failed" ? "模型列表加载失败，自动重试中…" : "加载模型列表…"}</option>`;
  const want = sessionModelValue();
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
function contextBudget() {
  const s = (panelData && panelData.session) || {};
  const mid = modelId(s.model);
  const pid = (s.model && s.model.providerID) || "";
  for (const p of (providers || [])) {
    if (pid && p.id !== pid) continue;
    const m = (p.models || []).find(x => x.id === mid);
    if (m && m.context) return m.context;
  }
  return 0;
}
async function ensureAgents() {
  if (agentsList.length) return agentsList;
  const res = await api("/api/opencode/agent", { headers: appHeaders() }).catch(() => null);
  if (!res || !res.ok) return [];
  const data = await res.json().catch(() => []);
  agentsList = (Array.isArray(data) ? data : []).filter(a => a && a.name && !a.hidden && a.mode !== "subagent");
  return agentsList;
}
let commandsCache = null;
async function listCommands() {
  if (commandsCache) return commandsCache;
  const res = await api("/api/opencode/command", { headers: appHeaders() }).catch(() => null);
  if (!res || !res.ok) return [];
  const data = await res.json().catch(() => []);
  commandsCache = (Array.isArray(data) ? data : []).filter(c => c && c.name);
  return commandsCache;
}
async function findFiles(q) {
  const dir = currentDir();
  const url = `/api/opencode/find/file?query=${encodeURIComponent(q)}` + (dir ? "&directory=" + encodeURIComponent(dir) : "");
  const res = await api(url, { headers: appHeaders() });
  if (!res.ok) return [];
  const data = await res.json().catch(() => []);
  return (Array.isArray(data) ? data : []).filter(x => typeof x === "string");
}

/* ============================ 发送 / 动作 ============================ */
function currentDir() {
  const item = items.find(x => x.session.id === selected);
  return ((item && item.session && item.session.directory) || (panelData && panelData.session && panelData.session.directory) || "").toString();
}
function dirHeaders(dir) {
  const headers = appHeaders();
  if (dir) headers["x-opencode-directory"] = dir;
  return headers;
}
const SEND_TIMEOUT_MS = 15000;
async function sendPrompt(body) {
  const id = selected;
  if (!id) return false;
  if (sending) return false;
  sending = true;
  const dir = currentDir();
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
      toast("已提交", "指令已发送，若长时间无响应请检查会话状态", "info");
      statuses[id] = { type: "busy" };
      renderList();
      return true;
    }
    if (!res || !res.ok) {
      toast("发送失败", `指令未送达 (${res ? res.status : "网络错误"})，内容已保留在输入框可重试`, "crit");
      return false;
    }
    dismissPendingQuestions(id);
    statuses[id] = { type: "busy" };
    renderList();
    return true;
  } catch (e) {
    if (e.message !== "unauthorized") toast("发送失败", (e.message || "未知错误") + "，内容已保留在输入框可重试", "crit");
    return false;
  } finally {
    sending = false;
  }
}
function dismissPendingQuestions(id) {
  const childIds = sessions.filter(s => s.parentID === id).map(s => s.id);
  for (const sid of [id, ...childIds]) {
    const dir = (sessions.find(s => s.id === sid) || {}).directory || "";
    for (const q of (pending[sid] || [])) {
      api(`/api/opencode/question/${encodeURIComponent(q.id)}/reject` + (dir ? "?directory=" + encodeURIComponent(dir) : ""),
        { method: "POST", headers: appHeaders() }).catch(() => {});
    }
    pending[sid] = [];
  }
}
async function abortSession(id) {
  if (!id) return;
  const dir = currentDir();
  const res = await api(`/api/opencode/session/${encodeURIComponent(id)}/abort`, { method: "POST", headers: dirHeaders(dir) });
  if (!res.ok) { toast("停止失败", "abort (" + res.status + ")", "crit"); return; }
  toast("已停止", "已发送停止信号", "info");
  statuses[id] = { type: "idle" };
  renderList();
  if (chat && chat.sessionId === id) { chat.setWorking(""); chat.syncSendIcon(); }
  refreshPanel(id, true);
}
async function runCommand(id, command, args) {
  const dir = currentDir();
  const res = await api(`/api/opencode/session/${encodeURIComponent(id)}/command`,
    { method: "POST", headers: dirHeaders(dir), body: JSON.stringify({ command, arguments: args || "" }) }).catch(() => null);
  if (!res || !res.ok) { toast("命令失败", `/${command} 未生效 (${res ? res.status : "网络错误"})`, "crit"); return false; }
  toast(`已运行 /${command}`, "命令已下发到会话", "info");
  statuses[id] = { type: "busy" };
  renderList();
  if (chat) { chat.setWorking("命令执行中…"); chat.syncSendIcon(); }
  return true;
}
async function revertMessage(id, mid, dir) {
  const doRev = () => api(`/api/opencode/session/${encodeURIComponent(id)}/revert`,
    { method: "POST", headers: dirHeaders(dir), body: JSON.stringify({ messageID: mid }) }).catch(() => null);
  let res = await doRev();
  if (res && res.status === 409) {
    toast("会话处理中", "等待空闲后自动重试…", "info", 2500);
    for (let i = 0; i < 15 && selected === id; i++) {
      await new Promise(r => setTimeout(r, 2000));
      res = await doRev();
      if (!res || res.status !== 409) break;
    }
  }
  return res;
}
async function revertTo(turn) {
  const id = selected;
  const mid = turn && (turn.id || (turn.msgIds || [])[0]);
  if (!id || !mid) return;
  const dir = currentDir();
  const res = await revertMessage(id, mid, dir);
  if (!res || !res.ok) { toast("回退失败", `revert (${res ? res.status : "网络错误"})，请先停止处理或稍后再试`, "crit"); return; }
  toast("已回退", "该消息之后的内容已撤销，可用「更多 ▾ → 重做」恢复", "info");
  if (chat) { chat.dropAfter(mid); chat.revertTo = mid; }
  loadWorkbench();
}
async function regenerate(turn) {
  const id = selected;
  if (!id || !chat) return;
  const turns = chat.turns;
  const ix = turns.findIndex(t => t.id === turn.id);
  let userTurn = null;
  for (let i = ix - 1; i >= 0; i--) {
    if (turns[i].role === "user" && turns[i].parts.some(p => p.type === "text" && !p.synthetic)) { userTurn = turns[i]; break; }
  }
  if (!userTurn) { toast("重新生成失败", "未找到对应的用户指令", "crit"); return; }
  const dir = currentDir();
  const res = await revertMessage(id, userTurn.id, dir);
  if (!res || !res.ok) { toast("重新生成失败", `revert (${res ? res.status : "网络错误"})，请先停止处理或稍后再试`, "crit"); return; }
  chat.revertTo = userTurn.id;
  chat.dropAfter(userTurn.id);
  chat.resendTurn(userTurn.id);
}
async function modelChange(v) {
  const id = selected;
  if (!id || !v) return;
  const [providerID, modelID, variant] = v.split("\u0001");
  if (!providerID || !modelID) return;
  const dir = currentDir();
  const url = `/api/opencode/api/session/${encodeURIComponent(id)}/model` + (dir ? "?directory=" + encodeURIComponent(dir) : "");
  const res = await api(url, { method: "POST", headers: appHeaders(), body: JSON.stringify({ model: { id: modelID, providerID, variant: variant || "default" } }) }).catch(() => null);
  if (!res || !res.ok) {
    toast("切换失败", `模型切换未生效 (${res ? res.status : "网络错误"})`, "crit");
    if (chat) chat.refreshSelectors();
    loadWorkbench();
    return;
  }
  toast("已切换", `已切换到 ${modelID}`, "info");
  const s = (items.find(x => x.session.id === id) || {}).session;
  if (s) s.model = { id: modelID, providerID, variant: variant || "default" };
  if (panelData && panelData.session) panelData.session.model = { id: modelID, providerID, variant: variant || "default" };
  if (chat) chat.refreshSelectors();
  renderShell();
}
async function agentChange(name) {
  const id = selected;
  if (!id || !name) return;
  agentChoice[id] = name;
  const dir = currentDir();
  const url = `/api/opencode/api/session/${encodeURIComponent(id)}/agent` + (dir ? "?directory=" + encodeURIComponent(dir) : "");
  const res = await api(url, { method: "POST", headers: appHeaders(), body: JSON.stringify({ agent: name }) }).catch(() => null);
  if (!res || !res.ok) {
    toast("切换未生效", `Agent ${name} 未能切换（发送时仍会带上）`, "warn");
    return;
  }
  const s = (items.find(x => x.session.id === id) || {}).session;
  if (s) s.agent = name;
  if (panelData && panelData.session) panelData.session.agent = name;
  renderShell();
}

/* ============================ 更多菜单动作 ============================ */
async function forkSession(id) {
  const dir = currentDir();
  const res = await api(`/api/opencode/session/${encodeURIComponent(id)}/fork`, { method: "POST", headers: dirHeaders(dir), body: JSON.stringify({}) });
  let data = {};
  try { data = await res.json(); } catch (_) {}
  if (!res.ok) { toast("Fork 失败", "未创建新会话 (" + res.status + ")", "crit"); return; }
  const nid = data.id || data.sessionID || "";
  toast("已 Fork", "新会话 " + nid, "info");
  await loadWorkbench();
  if (nid) openPanel(nid);
}
async function undoSession(id) {
  const dir = currentDir();
  let msgs = [];
  try {
    const mRes = await api(`/api/opencode/session/${encodeURIComponent(id)}/message?limit=50`, { headers: dirHeaders(dir) });
    if (mRes.ok) { const d = await mRes.json(); msgs = Array.isArray(d) ? d : (d.messages || []); }
  } catch (_) {}
  const lastUser = [...msgs].reverse().find(m => m.info && m.info.role === "user");
  if (!lastUser) { toast("撤销失败", "未找到可撤销的用户消息", "warn"); return; }
  const mid = lastUser.info.id || lastUser.id || "";
  if (!mid) { toast("撤销失败", "消息缺少 ID", "warn"); return; }
  const res = await api(`/api/opencode/session/${encodeURIComponent(id)}/revert`, { method: "POST", headers: dirHeaders(dir), body: JSON.stringify({ messageID: mid }) });
  if (!res.ok) { toast("撤销失败", "revert (" + res.status + ")", "crit"); return; }
  toast("已撤销", "已回退到上一条消息", "info");
  if (chat) { chat.revertTo = mid; chat.reload(); }
  loadWorkbench();
}
async function redoSession(id) {
  const dir = currentDir();
  const res = await api(`/api/opencode/session/${encodeURIComponent(id)}/unrevert`, { method: "POST", headers: dirHeaders(dir) });
  if (!res.ok) { toast("重做失败", "unrevert (" + res.status + ")", "crit"); return; }
  toast("已重做", "已恢复上一条撤销", "info");
  if (chat) { chat.revertTo = null; chat.reload(); }
  loadWorkbench();
}
async function renameSession(id) {
  const title = prompt("修改会话标题", (panelData && panelData.session && (panelData.session.title || panelData.session.slug)) || "");
  if (title === null) return;
  const t = (title || "").trim();
  if (!t) { toast("改名失败", "标题不能为空"); return; }
  const item = items.find(x => x.session.id === id);
  const dir = (item && item.session && item.session.directory) || "";
  const headers = appHeaders();
  if (dir) headers["x-opencode-directory"] = dir;
  const res = await api(`/api/opencode/session/${encodeURIComponent(id)}`, { method: "PATCH", headers, body: JSON.stringify({ title: t }) });
  if (!res.ok) { toast("改名失败", "改名失败 (" + res.status + ")"); return; }
  const s = sessions.find(x => x.id === id);
  if (s) s.title = t;
  if (panelData && panelData.session && panelData.session.id === id) panelData.session.title = t;
  toast("已改名", t, "info");
  renderShell();
  renderList();
}
async function deleteSession(id) {
  if (!confirm("确认删除该会话？此操作不可恢复。")) return;
  const res = await api(`/api/opencode/session/${encodeURIComponent(id)}`, { method: "DELETE", headers: appHeaders() });
  if (!res.ok) { toast("删除失败", "删除会话失败 (" + res.status + ")", "crit"); return; }
  sessions = sessions.filter(s => s.id !== id);
  delete statuses[id];
  delete pending[id];
  closePanel();
  renderList();
  toast("已删除", "会话已删除", "info");
}
async function exportSession(id, fmt) {
  const dir = currentDir();
  try {
    const [sRes, mRes] = await Promise.all([
      api(`/api/opencode/session/${encodeURIComponent(id)}`, { headers: dirHeaders(dir) }),
      api(`/api/opencode/session/${encodeURIComponent(id)}/message`, { headers: dirHeaders(dir) }),
    ]);
    const sess = sRes.ok ? await sRes.json() : { id };
    let msgs = [];
    if (mRes.ok) { const d = await mRes.json(); msgs = Array.isArray(d) ? d : (d.messages || []); }
    const title = (sess.title || sess.slug || id).toString();
    const safe = title.replace(/[^\w\u4e00-\u9fa5-]+/g, "_").slice(0, 60) || "session";
    if (fmt === "json") {
      downFile(safe + ".json", JSON.stringify({ info: sess, messages: msgs }, null, 2), "application/json");
    } else {
      let md = `# ${title}\n\n`;
      for (const m of msgs) {
        const role = (m.info && m.info.role) || "user";
        const label = role === "assistant" ? "Assistant" : role === "tool" ? "Tool" : "User";
        const text = (m.parts || []).filter(p => p && p.type === "text" && p.text && !p.synthetic && !p.ignored).map(p => p.text).join("\n").trim();
        if (!text) continue;
        md += `## ${label}\n\n${text}\n\n`;
      }
      downFile(safe + ".md", md.trim() + "\n", "text/markdown");
    }
    toast("已导出", safe + "." + (fmt === "json" ? "json" : "md"), "info");
  } catch (e) {
    if (e.message !== "unauthorized") toast("导出失败", e.message || "未知错误", "crit");
  }
}
function downFile(name, content, type) {
  const blob = new Blob([content], { type });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url; a.download = name;
  a.click();
  URL.revokeObjectURL(url);
}

/* ============================ 更多菜单（底部抽屉） ============================ */
function moreSheetOpen() {
  const id = selected;
  if (!id) return;
  const item = items.find(x => x.session.id === id);
  const status = item ? item.status : "idle";
  const busy = status === "busy" || status === "retry";
  document.getElementById("moreSheetTitle").textContent = (panelData && panelData.session && (panelData.session.title || panelData.session.slug)) || id;
  const acts = [
    ["star", stars.has(id) ? "取消收藏" : "收藏会话"],
    ["tag", "设置标签"],
    ["rename", "修改标题"],
    ["reload", "重新加载"],
    ["fork", "Fork 会话"],
    ["undo", "撤销上一条", busy],
    ["redo", "重做", busy],
    ["abort", "停止处理", !busy],
    ["export-md", "导出 Markdown"],
    ["export-json", "导出 JSON"],
    ["delete", "删除会话", false, "m-sheet-danger"],
  ];
  document.getElementById("moreSheetBody").innerHTML = acts.map(([a, l, dis, cls]) =>
    `<button class="${cls || "ghost"}" data-more="${a}" ${dis ? "disabled" : ""}>${l}</button>`).join("");
  document.getElementById("moreSheet").classList.remove("hidden");
}
function moreSheetClose() { document.getElementById("moreSheet").classList.add("hidden"); }
async function moreAction(action) {
  const id = selected;
  if (!id) return;
  moreSheetClose();
  if (action === "star") {
    if (stars.has(id)) stars.delete(id); else stars.add(id);
    saveStars();
    renderTagFilters();
    renderList();
    toast(stars.has(id) ? "已收藏" : "已取消收藏", (sessions.find(x => x.id === id) || {}).title || id, "info");
    return;
  }
  if (action === "tag") { editTags(id); return; }
  if (action === "rename") { await renameSession(id); return; }
  if (action === "reload") { if (chat) chat.reload(); refreshPanel(id, true); toast("已重新加载", "会话信息已刷新", "info"); return; }
  if (action === "fork") { await forkSession(id); return; }
  if (action === "undo") { await undoSession(id); return; }
  if (action === "redo") { await redoSession(id); return; }
  if (action === "abort") { await abortSession(id); return; }
  if (action === "export-md") { await exportSession(id, "markdown"); return; }
  if (action === "export-json") { await exportSession(id, "json"); return; }
  if (action === "delete") { await deleteSession(id); return; }
}
function editTags(sid) {
  const s = sessions.find(x => x.id === sid) || {};
  const title = (s.title || s.slug || sid).toString();
  const tags = sessTags[sid] || [];
  const tagRow = (t) => `<span class="wb-edit-tag" data-rm-sess-tag="${escapeHtml(sid)}|${escapeHtml(t)}">#${escapeHtml(t)} ×</span>`;
  wbModalOpen(`会话标签 · ${title}`,
    `<div class="wb-tag-edit">
      <div class="muted" style="font-size:12px;margin-bottom:8px">给会话打标签，用于列表分组筛选；点击标签可移除。</div>
      <div class="wb-edit-tags" id="wbEditTags">${tags.length ? tags.map(tagRow).join("") : `<span class="muted" style="font-size:12px">暂无标签</span>`}</div>
      <div class="row" style="gap:6px;margin-top:10px">
        <input type="text" id="wbTagInput" placeholder="输入标签后回车，如：发布" style="flex:1" onkeydown="if(event.key==='Enter'){addSessionTag('${escapeHtml(sid)}');}">
        <button class="ghost" onclick="addSessionTag('${escapeHtml(sid)}')">添加</button>
      </div>
    </div>`);
  const body = document.getElementById("modalBody");
  body.onclick = (e) => {
    const rm = e.target.closest("[data-rm-sess-tag]");
    if (!rm) return;
    const [rid, rtag] = rm.dataset.rmSessTag.split("|");
    sessTags[rid] = (sessTags[rid] || []).filter(t => t !== rtag);
    if (!sessTags[rid].length) delete sessTags[rid];
    saveTags();
    renderTagFilters();
    renderList();
    editTags(rid);
  };
  const inp = document.getElementById("wbTagInput");
  if (inp) inp.focus();
}
function addSessionTag(sid) {
  const inp = document.getElementById("wbTagInput");
  let tag = (inp && inp.value || "").trim().replace(/^#/, "").replace(/[^\w\u4e00-\u9fa5\-. ]+/g, "-").replace(/-+/g, "-").trim();
  if (!tag) return;
  const cur = sessTags[sid] || [];
  if (!cur.includes(tag)) cur.push(tag);
  sessTags[sid] = cur;
  if (inp) inp.value = "";
  saveTags();
  renderTagFilters();
  renderList();
  editTags(sid);
}

/* ============================ 弹窗 ============================ */
function wbModalOpen(title, html) {
  document.getElementById("modalTitle").textContent = title;
  document.getElementById("modalBody").innerHTML = html;
  document.getElementById("modal").classList.remove("hidden");
}
function modalClose() { document.getElementById("modal").classList.add("hidden"); }
window.wbModalOpen = wbModalOpen;

/* ============================ 语音（/api/stt，需 APP Token） ============================ */
const STT_RATE = 16000;
const STT_CHUNK_SAMPLES = 3200;
const STT_MAX_FAILURES = 3;
async function recognize(onText) {
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
  let actx = null;
  try { actx = new AC({ sampleRate: STT_RATE }); } catch (_) { actx = new AC(); }
  const ratio = actx.sampleRate / STT_RATE;

  let tail = [];
  let failures = 0;
  let stopped = false;
  let lastText = "";
  let chain = Promise.resolve();

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

  const src = actx.createMediaStreamSource(stream);
  const proc = actx.createScriptProcessor(4096, 1, 1);
  let pos = 0;
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
    pos = p - input.length;
    tail = tail.concat(out);
    while (tail.length >= STT_CHUNK_SAMPLES) {
      sendChunk(tail.slice(0, STT_CHUNK_SAMPLES));
      tail = tail.slice(STT_CHUNK_SAMPLES);
    }
  };
  src.connect(proc); proc.connect(actx.destination);

  const teardown = async () => {
    stopped = true;
    try { proc.disconnect(); src.disconnect(); } catch (_) {}
    stream.getTracks().forEach(t => t.stop());
    try { await actx.close(); } catch (_) {}
  };

  return {
    stop: async () => {
      if (tail.length) { sendChunk(tail); tail = []; }
      await chain;
      const wasBroken = failures >= STT_MAX_FAILURES;
      await teardown();
      if (wasBroken) return;
      try {
        const fr = await fetch("/api/stt/sessions/" + encodeURIComponent(sid) + "/finish", { method: "POST", headers: bearer });
        if (!fr.ok) throw new Error("finish " + fr.status);
        let text = String(((await fr.json().catch(() => ({}))) || {}).text || "").trim();
        if (!text) { toast("未识别到内容", "请靠近麦克风再试一次", "warn"); return; }
        onText(text, true);
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

/* ============================ 流（SSE） ============================ */
function streamCred() {
  if (session) return "session=" + encodeURIComponent(session);
  const t = localStorage.getItem(APP_TOKEN_KEY) || "";
  return t ? "token=" + encodeURIComponent(t) : "";
}
function syncStream() {
  const view = document.getElementById("chatView");
  const want = !!window.SBChat && !!selected && !!chat && view && !view.classList.contains("hidden");
  if (want && !chatUnsub) {
    SBChat.Bus.cred = streamCred;
    chatUnsub = SBChat.Bus.subscribe(onStream);
  } else if (!want && chatUnsub) {
    chatUnsub();
    chatUnsub = null;
  }
}
function onStream(obj) {
  if (chat) { try { chat.onEvent(obj); } catch (_) {} }
  handleStreamEvent(obj);
}
function statusFromProps(props) {
  const st = props && props.status;
  if (!st) return "";
  return typeof st === "object" ? (st.type || "") : String(st);
}
function handleStreamEvent(obj) {
  const inner = (obj && obj.payload && obj.payload.type) ? obj.payload : obj;
  const type = (inner && inner.type) || (obj && obj.type) || "";
  const props = (inner && inner.properties) || (inner && inner.data) || {};
  const sid = props.sessionID || props.sessionId || (inner && inner.sessionID) || (obj && obj.sessionId) || "";
  if (!sid || !type) return;
  if (type === "session.created" || type === "session.deleted" || type === "session.compacted") {
    scheduleWorkbench();
    return;
  }
  if (type === "session.status" || type === "session.idle" || type === "session.updated") {
    if (!sessions.some(x => x.id === sid)) { scheduleWorkbench(); return; }
    let st = "idle";
    if (type !== "session.idle") {
      const stt = statusFromProps(props);
      st = ["busy", "idle", "retry"].includes(stt) ? stt : (statuses[sid] && statuses[sid].type) || "idle";
    }
    statuses[sid] = { type: st };
    handleActivity(sid);
    scheduleRenderList();
    if (selected === sid && (type === "session.status" || type === "session.updated")) schedulePanelRefresh(sid);
  }
  if (["question.asked", "question.updated", "permission.asked", "message.complete", "session.error", "session.failed"].includes(type)) {
    handleActivity(sid);
    scheduleWorkbench();
  }
}

function scheduleRenderList() {
  clearTimeout(listRenderTimer);
  listRenderTimer = setTimeout(() => { renderList(); }, 350);
}
function schedulePanelRefresh(id) {
  clearTimeout(panelRefreshTimer);
  panelRefreshTimer = setTimeout(() => { if (selected === id) refreshPanel(id); }, 600);
}
function scheduleWorkbench() {
  clearTimeout(workbenchTimer);
  workbenchTimer = setTimeout(() => { loadWorkbench(); }, 500);
}

/* ============================ ctx 契约 ============================ */
function ctx() {
  const sid = () => selected;
  const curSession = () => {
    const it = items.find(x => x.session.id === selected);
    return (it && it.session) || (panelData && panelData.session) || {};
  };
  return {
    api,
    toast,
    dirHeaders: () => dirHeaders(currentDir()),
    session: curSession,
    isBusy: () => {
      const it = items.find(x => x.session.id === selected);
      const st = it ? it.status : (panelData && panelData.status) || "idle";
      return st === "busy" || st === "retry";
    },
    isSending: () => sending,
    confirm: (title, body) => window.confirm(title + "\n" + (body || "")),
    copyText: (text) => copyText(text, "已复制到剪贴板"),
    openSession: (id) => openPanel(id),
    agents: () => agentsList.map(a => a.name),
    modelOptions: () => modelOptions(),
    contextBudget: () => contextBudget(),
    send: (body) => sendPrompt(body),
    sent: () => { statuses[sid()] = { type: "busy" }; scheduleRenderList(); if (chat) chat.syncSendIcon(); },
    abort: () => abortSession(sid()),
    command: (name, args) => runCommand(sid(), name, args),
    revert: (turn) => revertTo(turn),
    regenerate: (turn) => regenerate(turn),
    switchModel: (value) => modelChange(value),
    switchAgent: (name) => agentChange(name),
    findFiles: (q) => findFiles(q),
    listCommands: () => listCommands(),
    recognize: localStorage.getItem(APP_TOKEN_KEY) ? (onText) => recognize(onText) : null,
    statusChanged: (st) => {
      const id = sid();
      if (!id) return;
      statuses[id] = { type: st === "retry" ? "retry" : (st === "busy" ? "busy" : "idle") };
      scheduleRenderList();
      if (chat && chat.sessionId === id) chat.syncSendIcon();
    },
  };
}

/* ============================ 轮询 / 初始化 ============================ */
function initListeners() {
  document.getElementById("settingsBtn").addEventListener("click", settingsOpen);
  document.getElementById("moreBtn").addEventListener("click", moreSheetOpen);
  document.getElementById("moreSheetBody").addEventListener("click", (e) => {
    const b = e.target.closest("[data-more]");
    if (b) moreAction(b.dataset.more);
  });
  document.getElementById("moreSheet").addEventListener("click", (e) => {
    if (e.target.id === "moreSheet") moreSheetClose();
  });
  document.getElementById("settingsModal").addEventListener("click", (e) => {
    if (e.target.id === "settingsModal") settingsClose();
  });
  document.getElementById("newSheet").addEventListener("click", (e) => {
    if (e.target.id === "newSheet") closeNewSheet();
  });
  document.getElementById("modal").addEventListener("click", (e) => {
    if (e.target.id === "modal") modalClose();
  });
  document.getElementById("filter").addEventListener("input", () => renderList());
  // 列表轮询：保持状态/未读/新会话近实时（SSE 只在打开聊天时连接）。
  setInterval(() => {
    if (session && !document.getElementById("listView").classList.contains("hidden")) loadWorkbench();
  }, 4000);
}

function boot() {
  loadLocal();
  initThemePicker();
  initListeners();
  renderAuth();
}
boot();
