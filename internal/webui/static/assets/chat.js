/* ============================================================================
 * StarBurst Console — 聊天引擎（AI 工作台）
 *
 * 与安卓客户端 ChatScreen 对齐的 Web 实现：part 级富渲染、SSE 增量流式、
 * 附件 / @提及 / 斜杠命令输入区、轮次级操作。
 *
 * app.js 只负责外壳（标题栏、待授权 / 待决问题卡片、会话列表），对话区整体
 * 交给本模块托管——面板级 snapshot/重绘与逐 token 增量刷新会争夺同一个 DOM。
 * ========================================================================== */
(function (global) {
"use strict";

const esc = global.escapeHtml || function (s) {
  return String(s ?? "").replace(/[&<>"']/g, c =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
};

const PAGE_SIZE = 30;          // 首屏消息条数
const OLD_PAGE = 30;           // 「加载更早」每次增量
const TOOL_OUT_CLIP = 3000;    // 工具输出折叠阈值（对齐 App ChatToolCards）
const NEAR_BOTTOM = 90;        // 距底多少像素内算「贴着最新」
const DRAFT_PREFIX = "ocb_chat_draft_";
const ATT_DRAFT_PREFIX = "ocb_chat_draft_atts_";
const ATTACH_MAX_BYTES = 10 * 1024 * 1024;
const ATTACH_TEXT_MAX = 2 * 1024 * 1024;

/* ---------------- 会话 / Prompt 模板（对齐 App ChatTemplateDialogs） ---------------- */
const TPL_KEY = "ocb_chat_templates";
const TPL_EXPAND_KEY = "ocb_chat_tpl_expanded";
// 内置模板：与 App builtinPromptTemplates 同款（标题 + 预设 prompt，不可编辑/删除，始终置顶）。
const BUILTIN_TPL = [
  { name: "代码审查", prompt: "请对当前项目的代码做一次全面的代码审查，重点关注：潜在 bug、安全问题、性能瓶颈、代码风格一致性。请指出具体的文件和位置，并给出可执行的修复建议。" },
  { name: "生成测试", prompt: "请为项目中的核心功能生成单元测试，覆盖主要的正常流程和边界情况，遵循项目现有的测试框架和风格。" },
  { name: "解释代码", prompt: "请解释当前项目的核心架构和关键代码逻辑，帮助我快速理解项目。如有相关文件，请结合具体代码说明。" },
  { name: "修复 Bug", prompt: "请帮我排查并修复项目中的 bug。先定位问题的根因，再给出修复方案和具体的代码修改。" },
  { name: "继续任务", prompt: "请继续之前中断或失败的任务。检查当前状态和已完成的部分，从断点处继续处理，不要重复已完成的工作。" },
];
function loadTpls() {
  try { return Array.isArray(JSON.parse(localStorage.getItem(TPL_KEY))) ? JSON.parse(localStorage.getItem(TPL_KEY)) : []; } catch (_) { return []; }
}
function saveTpls(list) {
  try { localStorage.setItem(TPL_KEY, JSON.stringify(list)); } catch (_) {}
}
function tplId() {
  return "tpl_" + Date.now().toString(36) + Math.random().toString(36).slice(2, 8);
}

/* ---------------- 通用小工具 ---------------- */

// 上游 bash 输出常带 ANSI 转义序列，原样进 DOM 会显示成乱码。
const ANSI_RE = /\x1B\[[0-9;?]*[ -/]*[@-~]|\x1B\][^\x07]*\x07/g;
function stripAnsi(s) { return String(s || "").replace(ANSI_RE, ""); }

function fmtDur(ms) {
  if (!ms || ms < 0) return "";
  if (ms < 1000) return ms + "ms";
  if (ms < 60000) return (ms / 1000).toFixed(1) + "s";
  const m = Math.floor(ms / 60000), s = Math.round((ms % 60000) / 1000);
  return s ? m + "m" + s + "s" : m + "m";
}
function fmtTok(n) {
  n = Number(n) || 0;
  if (n < 1000) return String(n);
  if (n < 1000000) return (n / 1000).toFixed(n < 10000 ? 1 : 0) + "k";
  return (n / 1000000).toFixed(2) + "M";
}
function fmtCost(c) {
  c = Number(c) || 0;
  if (!c) return "";
  return "$" + (c < 0.01 ? c.toFixed(4) : c.toFixed(2));
}
function fmtTime(ts) {
  if (!ts) return "";
  const d = new Date(ts), now = new Date();
  const pad = n => String(n).padStart(2, "0");
  const hm = pad(d.getHours()) + ":" + pad(d.getMinutes());
  if (d.toDateString() === now.toDateString()) return hm;
  return pad(d.getMonth() + 1) + "-" + pad(d.getDate()) + " " + hm;
}
function clipText(s, n) {
  s = String(s || "");
  return s.length > n ? { text: s.slice(0, n), truncated: true } : { text: s, truncated: false };
}
function safeJson(v, spaces) {
  if (v == null) return "";
  if (typeof v === "string") return v;
  try { return JSON.stringify(v, null, spaces || 1); } catch (_) { return String(v); }
}
function uid(p) {
  return (p || "msg") + "-" + Date.now().toString(36) + Math.random().toString(36).slice(2, 8);
}
// 与安卓 MessageIdGenerator 同格式：msg_ + 12 位十六进制时间戳 + 14 位随机。
// 上游按 id 前缀排序消息，随意 id（如 wb-xxx）会打乱历史顺序。
const B62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz";
let _midLastTs = 0, _midCounter = 0;
function nextMessageId() {
  const ts = Date.now();
  if (ts !== _midLastTs) { _midLastTs = ts; _midCounter = 0; }
  _midCounter = Math.min(_midCounter + 1, 0xfff);
  const hex = ((ts * 0x1000 + _midCounter).toString(16) + "").padStart(13, "0").slice(-12);
  let s = "";
  for (let i = 0; i < 14; i++) s += B62[Math.floor(Math.random() * 62)];
  return "msg_" + hex + s;
}
function base(name) {
  const s = String(name || "").replace(/\/+$/, "");
  const i = s.lastIndexOf("/");
  return i >= 0 ? s.slice(i + 1) : s;
}

/* ---------------- part 归一化：消息 → 轮次 ---------------- */
// 上游 /session/{id}/message 返回 [{info, parts:[...]}]。与安卓一致：连续的
// assistant 消息并入同一轮（一次提问可产生多条 assistant 消息），并丢弃仍在
// 流式中、还没有任何 parts 的空消息。
function normalizeTurns(raw) {
  const out = [];
  for (const m of Array.isArray(raw) ? raw : []) {
    if (!m || !m.info) continue;
    const info = m.info;
    const role = info.role === "assistant" ? "assistant" : "user";
    const parts = (Array.isArray(m.parts) ? m.parts : []).filter(p => p && typeof p === "object");
    const last = out[out.length - 1];
    if (role === "assistant" && last && last.role === "assistant") {
      last.msgIds.push(info.id);
      if (parts.length) last.parts = last.parts.concat(parts);
      absorbMeta(last, info);
      continue;
    }
    out.push({
      id: info.id || uid("t"),
      msgIds: [info.id],
      role,
      parts: parts.slice(),
      ts: (info.time && info.time.created) || 0,
      doneTs: (info.time && info.time.completed) || 0,
      model: info.modelID || (info.model && (info.model.id || info.model.modelID)) || "",
      provider: info.providerID || (info.model && info.model.providerID) || "",
      agent: info.agent || info.mode || "",
      variant: info.variant || "",
      cost: info.cost || 0,
      tokens: info.tokens || null,
      error: info.error || null,
      finish: info.finish || "",
    });
  }
  return out.filter(t => t.role === "user"
    ? t.parts.length
    : (t.parts.length || t.error || t.model || t.tokens));
}
function absorbMeta(turn, info) {
  if (info.modelID) turn.model = info.modelID;
  if (info.providerID) turn.provider = info.providerID;
  if (info.agent) turn.agent = info.agent;
  if (info.cost) turn.cost = (turn.cost || 0) + info.cost;
  if (info.tokens) {
    const a = turn.tokens || {};
    turn.tokens = {
      input: (a.input || 0) + (info.tokens.input || 0),
      output: (a.output || 0) + (info.tokens.output || 0),
      reasoning: (a.reasoning || 0) + (info.tokens.reasoning || 0),
      cache: { read: ((a.cache || {}).read || 0) + ((info.tokens.cache || {}).read || 0) },
    };
  }
  if (info.time && info.time.completed) turn.doneTs = info.time.completed;
  if (info.error) turn.error = info.error;
}

// 一轮里可见的纯文本（复制 / 引用 / 编辑重发用）。
function turnText(turn) {
  return (turn.parts || [])
    .filter(p => p && p.type === "text" && !p.synthetic && !p.ignored && p.text)
    .map(p => p.text).join("\n\n").trim();
}

/* ---------------- 图标与工具元数据 ---------------- */
const ICONS = {
  term: '<path d="M4 17l6-5-6-5M12 19h8"/>',
  file: '<path d="M14 3v5h5M14 3H6a2 2 0 00-2 2v14a2 2 0 002 2h12a2 2 0 002-2V8z"/>',
  edit: '<path d="M12 20h9M16.5 3.5a2.1 2.1 0 013 3L7 19l-4 1 1-4z"/>',
  search: '<circle cx="11" cy="11" r="7"/><path d="M20 20l-3.5-3.5"/>',
  web: '<circle cx="12" cy="12" r="9"/><path d="M3 12h18M12 3c2.5 3 2.5 15 0 18M12 3c-2.5 3-2.5 15 0 18"/>',
  agent: '<rect x="4" y="7" width="16" height="12" rx="2"/><path d="M12 3v4M9 13h.01M15 13h.01M9 16h6"/>',
  todo: '<path d="M9 6h11M9 12h11M9 18h11M4 6l1.5 1.5L8 5M4 12l1.5 1.5L8 11M4 18l1.5 1.5L8 17"/>',
  think: '<path d="M9 18h6M10 21h4M12 3a6 6 0 00-3 11v2h6v-2a6 6 0 00-3-11z"/>',
  paperclip: '<path d="M21 11l-9 9a5 5 0 01-7-7l9-9a3.5 3.5 0 015 5l-9 9a2 2 0 01-3-3l8-8"/>',
  send: '<path d="M22 2L11 13M22 2l-7 20-4-9-9-4 20-7z"/>',
  stop: '<rect x="6" y="6" width="12" height="12" rx="2"/>',
  mic: '<rect x="9" y="2" width="6" height="12" rx="3"/><path d="M5 11a7 7 0 0014 0M12 18v4"/>',
  down: '<path d="M12 5v14M19 12l-7 7-7-7"/>',
  bolt: '<path d="M13 2L4 14h7l-1 8 9-12h-7z"/>',
  template: '<rect x="4" y="3" width="16" height="18" rx="2"/><path d="M9 8h6M9 12h6M9 16h4"/>',
  star: '<path d="M12 2l3.09 6.26 6.91 1-5 4.87 1.18 6.88L12 17.77l-6.18 3.24L7 14.13l-5-4.87 6.91-1z"/>',
  x: '<path d="M18 6L6 18M6 6l12 12"/>',
  chevron_up: '<path d="M18 15l-6-6-6 6"/>',
  chevron_down: '<path d="M6 9l6 6 6-6"/>',
  info: '<circle cx="12" cy="12" r="9"/><path d="M12 8h.01M12 12v4"/>',
  copy: '<rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 01-2-2V4a2 2 0 012-2h9a2 2 0 012 2v1"/>',
  quote: '<path d="M8 7a3 3 0 00-3 3v3h4v4H4v-7a5 5 0 015-5zm9 0a3 3 0 00-3 3v3h4v4h-5v-7a5 5 0 015-5z"/>',
  refresh: '<path d="M21 12a9 9 0 11-2.64-6.36L21 8"/><path d="M21 3v5h-5"/>',
  undo: '<path d="M9 14L4 9l5-5"/><path d="M4 9h10a6 6 0 016 6v1"/>',
};
function icon(name, size) {
  const s = size || 13;
  return `<svg width="${s}" height="${s}" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${ICONS[name] || ""}</svg>`;
}
const TOOLS = {
  bash: { icon: "term", label: "终端" },
  read: { icon: "file", label: "读取文件" },
  write: { icon: "edit", label: "写入文件" },
  edit: { icon: "edit", label: "编辑文件" },
  multiedit: { icon: "edit", label: "批量编辑" },
  patch: { icon: "edit", label: "补丁" },
  apply_patch: { icon: "edit", label: "应用补丁" },
  glob: { icon: "search", label: "查找文件" },
  grep: { icon: "search", label: "搜索内容" },
  search: { icon: "search", label: "搜索" },
  webfetch: { icon: "web", label: "抓取网页" },
  websearch: { icon: "web", label: "联网搜索" },
  task: { icon: "agent", label: "子代理" },
  subtask: { icon: "agent", label: "子任务" },
  todowrite: { icon: "todo", label: "更新计划" },
  todo_write: { icon: "todo", label: "更新计划" },
  list: { icon: "file", label: "列目录" },
  lsp: { icon: "search", label: "代码分析" },
};
function toolMeta(name) { return TOOLS[String(name || "").toLowerCase()] || null; }
function toolTitle(part, state) {
  const meta = toolMeta(part.tool);
  const label = meta ? meta.label : (part.tool || "工具");
  // 上游在 state.title 给出人类可读目标（文件名 / 命令），优先采用。
  const input = state.input || {};
  const target = (state.title && String(state.title).trim())
    || input.filePath || input.path || input.command || input.pattern || input.url || input.query || "";
  const t = String(target).replace(/\s+/g, " ").trim();
  return { label, target: t.length > 90 ? t.slice(0, 87) + "…" : t, icon: meta ? meta.icon : "bolt" };
}
// 上游用 state.status 判别工具生命周期（不是 state.type）。
function toolStatus(state) {
  const s = String((state && (state.status || state.type)) || "pending").toLowerCase();
  if (s === "completed" || s === "success") return "completed";
  if (s === "error" || s === "failed") return "error";
  if (s === "running") return "running";
  return "pending";
}

/* ---------------- diff 渲染 ---------------- */
function renderUnifiedDiff(patch) {
  return String(patch || "").split("\n").map(ln => {
    let cls = "ctx";
    if (ln.startsWith("+++") || ln.startsWith("---")) cls = "file";
    else if (ln.startsWith("@@")) cls = "hunk";
    else if (ln.startsWith("+")) cls = "add";
    else if (ln.startsWith("-")) cls = "del";
    return `<span class="dl ${cls}">${esc(ln) || "&nbsp;"}</span>`;
  }).join("");
}
// 上游没给 patch 时用 before/after 逐行对齐兜底（够用且零依赖）。
function lineDiff(before, after) {
  const a = String(before || "").split("\n"), b = String(after || "").split("\n");
  const max = Math.max(a.length, b.length);
  let html = "";
  for (let i = 0; i < max; i++) {
    const x = a[i], y = b[i];
    if (x === y) html += `<span class="dl ctx">${esc(x == null ? "" : x) || "&nbsp;"}</span>`;
    else {
      if (x != null) html += `<span class="dl del">- ${esc(x)}</span>`;
      if (y != null) html += `<span class="dl add">+ ${esc(y)}</span>`;
    }
  }
  return html;
}

/* ---------------- 折叠状态（按 会话:partId 记忆） ---------------- */
let openState = {};
try { openState = JSON.parse(localStorage.getItem("ocb_chat_open") || "{}") || {}; } catch (_) { openState = {}; }
function isOpen(key, byDefault) {
  if (Object.prototype.hasOwnProperty.call(openState, key)) return openState[key] === true;
  return !!byDefault;
}
function setOpen(key, byDefault, want) {
  const cur = isOpen(key, byDefault);
  openState[key] = want === undefined ? !cur : !!want;
  try { localStorage.setItem("ocb_chat_open", JSON.stringify(openState)); } catch (_) {}
  return openState[key];
}
function collapseHead(key, titleHtml, byDefault, extra, openByUser) {
  const open = openByUser !== undefined ? !!openByUser : isOpen(key, byDefault);
  return `<button class="ct-head" type="button" data-ct-toggle="${esc(key)}" data-def="${byDefault ? 1 : 0}" aria-expanded="${open ? "true" : "false"}">
    <span class="ct-chev${open ? " open" : ""}">${icon("down", 12)}</span>
    <span class="ct-title">${titleHtml}</span>${extra || ""}</button>`;
}

/* ---------------- part 渲染 ---------------- */
function renderPart(p, ctx) {
  const key = ctx.key + ":" + (p.id || "i" + ctx.i);
  const open = isOpen(key, false);
  switch (p.type) {
    case "text": {
      const txt = String(p.text || "");
      if (!txt.trim()) return "";
      if (p.synthetic || p.ignored) {
        return `<div class="ct-card sys">${collapseHead(key,
          `<span class="ct-label">系统注入上下文</span><span class="ct-sub">${txt.length} 字</span>`, false)}
          <div class="ct-body${open ? " open" : ""}"><pre class="ct-pre">${esc(clipText(txt, 8000).text)}</pre></div></div>`;
      }
      // 流式进行中的文本按纯文本渲染（配光标），避免每个 token 重跑 Markdown；
      // part 收尾后由 message.part.updated 触发整轮重绘，届时才出富文本。
      if (ctx.liveId && ctx.liveId === p.id) {
        return `<div class="ct-text streaming"${p.id ? ` data-part="${esc(p.id)}"` : ""}>${esc(txt)}<span class="cursor"></span></div>`;
      }
      return `<div class="ct-text md"${p.id ? ` data-part="${esc(p.id)}"` : ""}>${global.mdRender ? global.mdRender(txt) : esc(txt)}</div>`;
    }
    case "reasoning": {
      const txt = String(p.text || "").trim();
      if (!txt) return "";
      const dur = p.time && p.time.start && p.time.end ? fmtDur(p.time.end - p.time.start) : "";
      const head = pickReasonTitle(txt) || (dur ? `思考用时 ${dur}` : "思考过程");
      return `<div class="ct-card think" data-part="${esc(p.id || "")}">${collapseHead(key,
        `<span class="ct-ico">${icon("think", 13)}</span><span class="ct-label">${esc(head)}</span>${dur ? `<span class="ct-sub">${dur}</span>` : ""}`, false)}
        <div class="ct-body think-body${open ? " open" : ""}">${esc(clipText(txt, 12000).text)}</div></div>`;
    }
    case "tool":
      return renderToolPart(p, key);
    case "patch": {
      const files = Array.isArray(p.files) ? p.files : [];
      if (!files.length) return "";
      return `<div class="ct-card patch">${collapseHead(key,
        `<span class="ct-ico">${icon("edit", 13)}</span><span class="ct-label">改动了 ${files.length} 个文件</span>`, false)}
        <div class="ct-body${open ? " open" : ""}"><ul class="ct-files">${
          files.map(f => `<li><code>${esc(f)}</code></li>`).join("")}</ul></div></div>`;
    }
    case "file":
      return renderFilePart(p);
    case "subtask": {
      const sid = p.sessionID || p.id || "";
      return `<div class="ct-card sub">
        <div class="sub-row"><span class="ct-ico">${icon("agent", 13)}</span>
          <b>${esc(p.description || "子任务")}</b>
          ${p.agent ? `<span class="chip">${esc(p.agent)}</span>` : ""}
          ${p.model && p.model.modelID ? `<span class="chip mono">${esc(p.model.modelID)}</span>` : ""}
          ${sid ? `<button class="ghost xs" type="button" data-open-child="${esc(sid)}">查看子会话</button>` : ""}
        </div>${p.prompt ? `<div class="sub-prompt">${esc(clipText(p.prompt, 300).text)}</div>` : ""}</div>`;
    }
    case "compaction":
      return `<div class="ct-divider"><span>${esc(p.auto ? "上下文已自动压缩" : "上下文已压缩")}</span></div>`;
    case "retry": {
      const msg = (p.error && (p.error.message || (p.error.data && p.error.data.message))) || "上游错误";
      return `<div class="ct-retry"><b>第 ${Number(p.attempt) || 1} 次重试</b><span>${esc(String(msg).slice(0, 200))}</span></div>`;
    }
    case "abort":
      return `<div class="ct-abort">已停止处理${p.reason ? "：" + esc(String(p.reason).slice(0, 120)) : ""}</div>`;
    case "step-finish":
      return "";   // token / 费用汇总到轮次元信息行，气泡内不重复（与 App 一致）
    default:
      return "";
  }
}
function pickReasonTitle(txt) {
  const first = String(txt).replace(/^\s*#{1,6}\s*/, "").split(/[\n。！？.!?]/)[0] || "";
  const t = first.trim();
  return t.length >= 4 && t.length <= 60 ? t : "";
}

function renderToolPart(p, key) {
  const state = p.state || {};
  const st = toolStatus(state);
  const tt = toolTitle(p, state);
  const open = isOpen(key, false) && (st === "completed" || st === "error");
  const input = state.input || {};
  const head = `<span class="ct-ico">${icon(tt.icon, 13)}</span><span class="ct-label">${esc(tt.label)}</span>${
    tt.target ? `<code class="ct-target">${esc(tt.target)}</code>` : ""}`;
  const badge = st === "running" ? '<span class="st running">执行中</span>'
    : st === "error" ? '<span class="st error">失败</span>'
    : st === "pending" ? '<span class="st pending">等待</span>'
    : (state.time && state.time.start && state.time.end ? `<span class="st done">${fmtDur(state.time.end - state.time.start)}</span>` : "");
  let body;
  if (st === "error") {
    body = `<pre class="ct-pre err">${esc(stripAnsi(state.error || ""))}</pre>`;
  } else if (String(p.tool || "").toLowerCase() === "bash") {
    const cmd = String(input.command || state.title || "").trim();
    const out = clipText(stripAnsi(state.output || ""), TOOL_OUT_CLIP);
    body = `${cmd ? `<div class="ct-cmd"><span class="pmt">$</span><code>${esc(cmd)}</code><button class="ghost xs" type="button" data-copy-raw="${esc(cmd)}">复制</button></div>` : ""}<pre class="ct-pre">${esc(out.text)}</pre>`;
  } else if (isEditTool(p.tool)) {
    body = renderEditBody(state);
  } else if (st === "completed") {
    const out = clipText(stripAnsi(String(state.output || "")), TOOL_OUT_CLIP);
    body = `<pre class="ct-pre${out.text ? "" : " muted"}">${esc(out.text || "（无输出）")}</pre>`;
  } else {
    body = `<pre class="ct-pre json">${esc(clipText(safeJson(input), 1200).text)}</pre>`;
  }
  return `<div class="ct-card tool ${st}"${p.id ? ` data-part="${esc(p.id)}"` : ""}>
    ${collapseHead(key, head, false, `<span class="ct-right">${badge}</span>`, open)}
    <div class="ct-body${open ? " open" : ""}">${body}</div></div>`;
}
function isEditTool(name) {
  const n = String(name || "").toLowerCase();
  return n === "edit" || n === "write" || n === "multiedit" || n === "apply_patch" || n === "patch";
}
function renderEditBody(state) {
  const md = state.metadata || {};
  const fd = md.filediff || md.fileDiff || null;
  const patch = (fd && fd.patch) || md.patch || "";
  if (patch) {
    const adds = (fd && fd.additions) || md.additions || 0;
    const dels = (fd && fd.deletions) || md.deletions || 0;
    return `<div class="ct-stat"><span class="add">+${adds}</span><span class="del">-${dels}</span></div><div class="ct-diff">${renderUnifiedDiff(patch)}</div>`;
  }
  const before = (fd && fd.before) || md.before || "";
  const after = (fd && fd.after) || md.after || "";
  if (before || after) return `<div class="ct-diff">${lineDiff(before, after)}</div>`;
  const input = state.input || {};
  const content = input.content || "";
  if (content) {
    const n = content.split("\n").length;
    return `<div class="ct-stat"><span class="add">+${n} 行</span></div><div class="ct-diff">${
      renderUnifiedDiff(content.split("\n").map(l => "+ " + l).join("\n"))}</div>`;
  }
  return `<pre class="ct-pre json muted">${esc(input.filePath || input.path || "（无内容）")}</pre>`;
}
function renderFilePart(p) {
  const mime = String(p.mime || "");
  const name = p.filename || base(p.url || "") || "附件";
  if (mime.indexOf("image/") === 0 && p.url) {
    return `<div class="ct-imgs"><button class="ct-img" type="button" data-lightbox="${esc(p.url)}" aria-label="${esc(name)}">
      <img src="${esc(p.url)}" alt="${esc(name)}" loading="lazy"><span class="cap">${esc(name)}</span></button></div>`;
  }
  return `<div class="ct-file"><span class="ct-ico">${icon("file", 13)}</span><code>${esc(name)}</code>${
    mime ? `<span class="chip">${esc(mime.replace("application/", "").replace("text/", ""))}</span>` : ""}</div>`;
}

/* ---------------- 轮次渲染 ---------------- */
function renderTurn(turn, ctx) {
  const partsHtml = (turn.parts || []).map((p, i) => renderPart(p, { key: ctx.key, i, liveId: ctx.liveId })).join("");
  const errHtml = turn.error
    ? `<div class="ct-error"><b>${esc(turn.error.name || "错误")}</b><div>${esc(
        String((turn.error.data && (turn.error.data.message || turn.error.data)) || turn.error.message || "").slice(0, 400))}</div></div>`
    : "";
  // 空回复占位：assistant 轮没有任何 parts 且无错误时（如仅回传了 token/模型信息），
  // 给个弱提示，避免用户看到「只有一个空白的 AI 气泡」。
  const emptyReply = turn.role === "assistant" && !turn.error && !(turn.parts && turn.parts.length) && !turn.pending
    ? `<div class="ct-empty-reply">（AI 未生成文本）</div>` : "";
  return `<div class="cmsg ${turn.role}${turn.pending ? " pending" : ""}${turn.failed ? " failed" : ""}" data-turn="${esc(turn.id)}">
    <div class="cbub">
      <div class="cwho">${turn.role === "assistant" ? "AI" : "我"}${turn.agent ? `<span class="chip">${esc(turn.agent)}</span>` : ""}${
        turn.pending ? '<span class="cstate">发送中</span>' : ""}${turn.failed ? '<span class="cstate err">发送失败</span>' : ""}</div>
      ${renderTodoCard(turn, ctx.key)}
      ${partsHtml}${errHtml}${emptyReply}
      ${turn.role === "user" ? renderUserMetaActions(turn) : (renderTurnMeta(turn) + renderTurnActions(turn))}
    </div>
  </div>`;
}
// 用户消息脚注：时间与操作（复制/编辑重发/引用）合并为一行，省垂直空间。
function renderUserMetaActions(turn) {
  const hasText = !!turnText(turn);
  const bits = [];
  const t = fmtTime(turn.ts);
  if (t) bits.push(`<span class="m time">${t}</span>`);
  if (hasText) {
    bits.push(messageAct("copy", "复制", turn.id));
    bits.push(messageAct("edit", "编辑重发", turn.id));
    bits.push(messageAct("quote", "引用", turn.id));
  }
  if (!bits.length) return "";
  return `<div class="cmeta cmeta-user">${bits.join('<span class="sep">·</span>')}</div>`;
}
function messageAct(act, title, tid) {
  const ic = { copy: "copy", edit: "edit", quote: "quote" }[act] || "bolt";
  return `<button class="iact-inline" type="button" data-act="${act}" data-tid="${esc(tid)}" title="${title}" aria-label="${title}">${icon(ic, 11)}</button>`;
}
// 计划（todo）卡片：进度条 + n/m，默认展开（对齐 App TodoListCard）。
function renderTodoCard(turn, key) {
  const todos = turn.todos;
  if (!Array.isArray(todos) || !todos.length) return "";
  const done = todos.filter(t => t.status === "completed" || t.status === "done" || t.completed).length;
  const pct = Math.round(done / todos.length * 100);
  const rows = todos.map(t => {
    const st = (t.status === "completed" || t.status === "done" || t.completed) ? "done"
      : (t.status === "in_progress" || t.status === "active") ? "doing" : "todo";
    return `<li class="${st}">${esc(t.content || t.title || t.text || "")}</li>`;
  }).join("");
  return `<div class="ct-plan">${collapseHead(key + ":plan",
    `<span class="ct-ico">${icon("todo", 13)}</span><span class="ct-label">执行计划</span><span class="ct-sub">${done}/${todos.length}</span>`, true,
    `<span class="ct-right"><span class="plan-bar"><i style="width:${pct}%"></i></span></span>`)}
    <div class="ct-body${isOpen(key + ":plan", true) ? " open" : ""}"><ul class="plan-list">${rows}</ul></div></div>`;
}
function renderTurnMeta(turn) {
  if (turn.role !== "assistant") {
    const t = fmtTime(turn.ts);
    return t ? `<div class="cmeta"><span class="m time">${t}</span></div>` : "";
  }
  const bits = [];
  const tk = turn.tokens || {};
  const inTok = (tk.input || 0) + ((tk.cache || {}).read || 0);
  if (inTok || tk.output) bits.push(`<span class="m" title="输入 / 输出 token">↑${fmtTok(inTok)} ↓${fmtTok(tk.output)}</span>`);
  if (tk.reasoning) bits.push(`<span class="m">↳${fmtTok(tk.reasoning)}</span>`);
  const c = fmtCost(turn.cost);
  if (c) bits.push(`<span class="m">${c}</span>`);
  if (turn.model) bits.push(`<span class="m mono">${esc(turn.model)}</span>`);
  const dur = turn.doneTs && turn.ts ? fmtDur(turn.doneTs - turn.ts) : "";
  if (dur) bits.push(`<span class="m">${dur}</span>`);
  const t = fmtTime(turn.ts);
  if (t) bits.push(`<span class="m time">${t}</span>`);
  return bits.length ? `<div class="cmeta">${bits.join('<span class="sep">·</span>')}</div>` : "";
}
function renderTurnActions(turn) {
  const hasText = !!turnText(turn);
  if (turn.role === "user") {
    return `<div class="cacts">${hasText ? `
      <button class="ghost xs iact" type="button" data-act="copy" data-tid="${esc(turn.id)}" title="复制" aria-label="复制这个提问">${icon("copy", 12)}</button>
      <button class="ghost xs iact" type="button" data-act="edit" data-tid="${esc(turn.id)}" title="编辑重发" aria-label="编辑重发">${icon("edit", 12)}</button>
      <button class="ghost xs iact" type="button" data-act="quote" data-tid="${esc(turn.id)}" title="引用" aria-label="引用到输入框">${icon("quote", 12)}</button>` : ""}
    </div>`;
  }
  return `<div class="cacts">${hasText ? `
      <button class="ghost xs iact" type="button" data-act="copy" data-tid="${esc(turn.id)}" title="复制" aria-label="复制这段回复">${icon("copy", 12)}</button>
      <button class="ghost xs iact" type="button" data-act="quote" data-tid="${esc(turn.id)}" title="引用" aria-label="引用到输入框">${icon("quote", 12)}</button>` : ""}
      <button class="ghost xs iact" type="button" data-act="regen" data-tid="${esc(turn.id)}" title="重新生成" aria-label="重新生成这段回复">${icon("refresh", 12)}</button>
      <button class="ghost xs iact" type="button" data-act="revert" data-tid="${esc(turn.id)}" title="回退到此处" aria-label="回退到此处，丢弃其后内容">${icon("undo", 12)}</button>
    </div>`;
}

/* ============================================================================
 * ChatView —— 对话区控制器
 *
 * ctx 由 app.js 注入：
 *   api / dirHeaders / toast / session() / isBusy() / isSending()
 *   send(body) → Promise<boolean>            （prompt_async）
 *   abort() / command(name,args) / revert(msgId) / copyText(text)
 *   agents() / modelOptions(cur) / switchModel(value) / switchAgent(name)
 *   findFiles(q) / listCommands() / openSession(id) / contextUsage()
 * ========================================================================== */
function ChatView() { this.reset(); }

ChatView.prototype.reset = function () {
  this.ctx = null; this.host = null;
  this.turns = []; this.byMsg = new Map(); this.partIx = new Map();
  this.cursor = null; this.loading = false; this.sessionId = null;
  this.revertTo = null;
  this.liveId = null; this.working = "";
  this.stick = true; this.unread = 0;
  this.attachments = []; this.attDrafts = {}; this.popup = null; this.voice = null; this.voiceBase = "";
  this._deltaBuf = null; this._deltaTimer = null; this._flushTimer = null;
};

ChatView.prototype.mount = function (host, ctx) {
  this.reset();
  this.ctx = ctx; this.host = host;
  host.innerHTML = `
    <div class="chat-list" data-role="list"></div>
    <div class="chat-fab hidden" data-role="fab" title="回到最新消息"><button type="button">${icon("down", 15)}</button><span class="ub hidden"></span></div>
    <div class="chat-working hidden" data-role="working"></div>
    <div class="chat-compose" data-role="compose">
      <div class="cc-pop hidden" data-role="pop"></div>
      <div class="cc-tpl hidden" data-role="tpl"></div>
      <div class="cc-atts hidden" data-role="atts"></div>
      <div class="cc-row">
        <button class="cc-btn" type="button" data-cc="tpl" title="会话模板" aria-label="会话模板">${icon("template", 16)}</button>
        <button class="cc-btn" type="button" data-cc="attach" title="添加附件" aria-label="添加附件">${icon("paperclip", 16)}</button>
        <textarea class="cc-input" data-role="input" rows="1" spellcheck="false" placeholder="给该会话发送指令…（Enter 发送 · Shift+Enter 换行 · @ 引用文件 · / 命令 · 模板）" aria-label="消息输入框"></textarea>
        <button class="cc-btn" type="button" data-cc="voice" title="语音输入" aria-label="语音输入">${icon("mic", 16)}</button>
        <button class="cc-send" type="button" data-cc="send" title="发送" aria-label="发送">${icon("send", 16)}</button>
      </div>
      <div class="cc-bar">
        <select class="cc-sel" data-role="agent" title="Agent"></select>
        <select class="cc-sel" data-role="model" title="模型"></select>
        <span class="cc-ctx" data-role="ctx"></span>
      </div>
      <input type="file" class="hidden" data-role="file" multiple>
    </div>`;
  host.addEventListener("click", (e) => this.onClick(e));
  this.el("list").addEventListener("scroll", () => this.onScroll(), { passive: true });
  const ta = this.el("input");
  ta.addEventListener("keydown", (e) => this.onKeydown(e));
  ta.addEventListener("input", () => this.onInput());
  ta.addEventListener("paste", (e) => this.onPaste(e));
  ta.addEventListener("dragover", (e) => e.preventDefault());
  ta.addEventListener("drop", (e) => this.onDrop(e));
  this.el("file").addEventListener("change", (e) => { this.addFiles(e.target.files); e.target.value = ""; });
  this.el("agent").addEventListener("change", (e) => { this.saveDraft(); if (this.ctx.switchAgent) this.ctx.switchAgent(e.target.value); });
  this.el("model").addEventListener("change", (e) => { if (this.ctx.switchModel) this.ctx.switchModel(e.target.value); this.renderCtxBar(); });
  return this;
};
ChatView.prototype.el = function (role) { return this.host.querySelector('[data-role="' + role + '"]'); };
ChatView.prototype.unmount = function () {
  clearTimeout(this._deltaTimer); clearTimeout(this._flushTimer);
  this._deltaTimer = this._flushTimer = null;
  if (this.voice) { if (this.voice.cancel) this.voice.cancel(); this.voice = null; }
};

/* ---- 会话切换与加载 ---- */
ChatView.prototype.open = async function (sessionId) {
  if (!this.host) return;
  const c = this.ctx;
  if (!c || !c.api || !sessionId) return;
  // 切会话时丢弃进行中的录音：识别结果不该落到新会话的输入框里。
  if (this.voice) { if (this.voice.cancel) this.voice.cancel(); this.voice = null; this.voiceBase = ""; }
  this.hidePopup();
  const tplBox = this.el("tpl");
  if (tplBox) tplBox.classList.add("hidden");
  // 切走前把当前会话未发送的附件固化进内存草稿（文本草稿已随输入实时落库）。
  if (this.sessionId && this.sessionId !== sessionId) this.saveAtts();
  this.sessionId = sessionId;
  this.turns = []; this.byMsg.clear(); this.partIx.clear();
  this.cursor = null; this.liveId = null; this.working = ""; this.unread = 0;
  this.revertTo = null;
  this.stick = true;
  this.hidePopup();
  this.restoreAtts();
  this.el("list").innerHTML = `<div class="wb-placeholder">加载对话…</div>`;
  this.el("working").classList.add("hidden");
  this.restoreDraft();
  this.renderSelectors();
  await this.fetchPage(PAGE_SIZE, false);
  this.renderCtxBar();
};

ChatView.prototype.fetchPage = async function (limit, older) {
  const c = this.ctx, sid = this.sessionId;
  if (!sid || this.loading) return;
  this.loading = true;
  try {
    let url = `/api/opencode/session/${encodeURIComponent(sid)}/message?limit=${limit}`;
    if (older && this.cursor) url += "&before=" + encodeURIComponent(this.cursor);
    const res = await c.api(url, { headers: c.dirHeaders() });
    if (this.sessionId !== sid) return;
    if (!res.ok) {
      if (!older) this.el("list").innerHTML = `<div class="wb-placeholder">对话加载失败 (${res.status})</div>`;
      return;
    }
    const next = res.headers.get("X-Next-Cursor");
    const data = await res.json();
    const raw = Array.isArray(data) ? data : (data.messages || []);
    const page = normalizeTurns(raw);
    // 会话处在「已回退」状态时（session.revert.messageID），上游 /message 仍会返回
    // 全部历史；按回退点裁剪，避免把已撤销的内容重新拉回来（对齐 App 的做法）。
    if (this.revertTo) clipToRevert(page, this.revertTo);
    if (older) {
      const seen = new Set(page.map(t => t.id));
      const mine = this.turns.filter(t => !seen.has(t.id));
      this.turns = page.concat(mine);
      if (next) this.cursor = next;
      this.reindex(); this.render();
    } else {
      // 全量刷新：以服务端为权威，仅保留仍在等待回执的乐观消息。
      const pending = this.turns.filter(t => t.pending);
      const known = new Set();
      for (const t of page) { known.add(t.id); for (const m of t.msgIds) known.add(m); }
      let keep = pending.filter(t => !known.has(t.id) && !t.echoOf);
      // 乐观气泡与快照里已落库的用户消息按可见文本去重：上游不保证回显我们下发的
      // messageID（服务端自建 id），且 SSE 可能在 POST 返回前就投递 session.idle
      // 触发本快照——此时 echo 仍处于 pending，若不清掉就会和快照里的同一条用户
      // 消息各渲染一个气泡（「发一条出两条」）。
      if (keep.some(t => t.role === "user")) {
        const serverUser = new Set();
        for (const t of page) if (t.role === "user") serverUser.add(normalizedText(turnText(t)));
        keep = keep.filter(t => {
          if (t.role !== "user") return true;
          const txt = normalizedText(turnText(t));
          // 仅对「有非空文本」的乐观气泡做文本对齐去重：空文本（纯附件/图片）消息
          // 身份不可靠，宁可保留也不误删。
          return !txt || !serverUser.has(txt);
        });
      }
      this.turns = page.concat(keep);
      this.cursor = next || (raw.length >= limit ? msgIdOf(raw[0]) : null);
      this.reindex(); this.render(); this.scrollToBottom(true);
    }
  } finally {
    if (this.sessionId === sid) this.loading = false;
  }
};
// 一条用户消息的规范化文本（去空白），用于乐观气泡与服务端消息的身份对齐。
function normalizedText(s) {
  return String(s || "").replace(/\s+/g, " ").trim();
}
// 按回退点裁剪轮次：保留「包含回退点消息」的那一轮及之前的轮次，其后全部丢弃。
function clipToRevert(turns, revertMessageId) {
  if (!revertMessageId) return;
  for (let i = 0; i < turns.length; i++) {
    const t = turns[i];
    if (t.id === revertMessageId || (t.msgIds || []).includes(revertMessageId)) {
      turns.splice(i + 1, turns.length - i - 1);
      return;
    }
  }
}
ChatView.prototype.reindex = function () {
  this.byMsg.clear(); this.partIx.clear();
  for (const t of this.turns) {
    if (t.id) this.byMsg.set(t.id, t);
    for (const mid of t.msgIds) if (mid) this.byMsg.set(mid, t);
    for (const p of t.parts || []) if (p.id) this.partIx.set(p.id, { turn: t, part: p });
  }
};
function msgIdOf(rawMsg) {
  return (rawMsg && rawMsg.info && rawMsg.info.id) || (rawMsg && rawMsg.id) || "";
}

/* ---- 渲染 ---- */
ChatView.prototype.render = function () {
  if (!this.host) return;
  const list = this.el("list");
  if (!this.turns.length) {
    list.innerHTML = `<div class="wb-placeholder">还没有对话<br><span class="muted" style="font-size:12px">在消息输入区开始</span></div>`;
    this.renderWorking(); return;
  }
  const head = `<div class="c-load${this.cursor ? "" : " hidden"}"><button class="ghost sm" type="button" data-act="older">加载更早的消息</button></div>`;
  list.innerHTML = head + renderTimeline(this.turns, { key: this.sessionId, liveId: this.liveId });
  this.renderWorking();
};
ChatView.prototype.renderWorking = function () {
  const w = this.el("working");
  if (!w) return;
  w.classList.toggle("hidden", !this.working);
  if (this.working) w.innerHTML = `<span class="dots"><i></i><i></i><i></i></span>${esc(this.working)}`;
};
ChatView.prototype.rerenderTurn = function (turn) {
  const node = this.host.querySelector('[data-turn="' + cssEscape(turn.id) + '"]');
  if (!node) { this.render(); return; }
  const tmp = document.createElement("div");
  tmp.innerHTML = renderTurn(turn, { key: this.sessionId, liveId: this.liveId });
  node.replaceWith(tmp.firstElementChild);
};
function cssEscape(s) {
  return global.CSS && CSS.escape ? CSS.escape(String(s)) : String(s).replace(/[^a-zA-Z0-9_-]/g, ch => "\\" + ch);
}

/* ---- 日期分组时间线（对齐 App ChatTimeline 的 DateDivider） ---- */
function dayKey(ts) {
  if (!ts) return null;
  const d = new Date(ts);
  return d.getFullYear() + "-" + (d.getMonth() + 1) + "-" + d.getDate();
}
function sameDay(a, b) {
  return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
}
function dayLabel(ts) {
  const d = new Date(ts), now = new Date();
  if (sameDay(d, now)) return "今天";
  const y = new Date(now); y.setDate(now.getDate() - 1);
  if (sameDay(d, y)) return "昨天";
  const pad = n => String(n).padStart(2, "0");
  const today = new Date(now);
  today.setHours(0, 0, 0, 0);
  const day = new Date(d); day.setHours(0, 0, 0, 0);
  const delta = Math.round((today - day) / 86400000);
  if (delta < 7) return `${["周一", "周二", "周三", "周四", "周五", "周六", "周日"][d.getDay() === 0 ? 6 : d.getDay() - 1]}`;
  if (d.getFullYear() === now.getFullYear()) return pad(d.getMonth() + 1) + "-" + pad(d.getDate());
  return d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate());
}
// 在轮次之间插入「今天 / 昨天 / 周几 / 日期」分组条；乐观消息 ts 为当前时间，落到最新日期组。
function renderTimeline(turns, ctx) {
  let html = "", lastKey = null;
  for (const t of turns) {
    const k = dayKey(t.ts);
    if (k && k !== lastKey) html += `<div class="chat-day"><span>${esc(dayLabel(t.ts))}</span></div>`;
    if (k) lastKey = k;
    html += renderTurn(t, ctx);
  }
  return html;
}

/* ============================================================================
 * 实时事件（由 app.js 的 /api/stream 多路复用器喂入）
 * ========================================================================== */
// 上游 v1.18 把事件包在 {directory, payload:{id,type,properties}} 里；同时兼容
// 裸 {type,properties} 与新一代 session.next.* 命名。
ChatView.prototype.onEvent = function (obj) {
  if (!this.sessionId || !this.host || !obj) return false;
  const inner = (obj.payload && obj.payload.type) ? obj.payload : obj;
  const type = inner.type || obj.type || "";
  const props = inner.properties || inner.data || {};
  const sid = props.sessionID || props.sessionId || inner.sessionID || obj.sessionId || "";
  if (sid && sid !== this.sessionId) return false;
  switch (type) {
    case "message.part.delta": this.queueDelta(props); return true;
    case "session.next.text.delta": this.queueDelta(props); return true;
    case "session.next.reasoning.delta": this.queueDelta(Object.assign({ field: "reasoning" }, props)); return true;
    case "message.part.updated": this.upsertPart(props.part || inner.part, props.messageID); return true;
    case "session.next.tool.input.ended":
    case "session.next.tool.progress":
    case "session.next.tool.success":
    case "session.next.tool.failed":
      this.upsertTool(type, props); return true;
    case "message.part.removed": this.removePart(props.partID); return true;
    case "message.updated": this.updateMessageInfo(props.info); return true;
    case "message.removed": this.removeMessage(props.messageID); return true;
    case "session.idle": case "session.status": this.statusEvent(type, props); return true;
    case "session.error": case "session.failed": this.errorEvent(props); return true;
    case "todo.updated": this.todoEvent(props); return true;
    case "session.compacted": this.compactedEvent(); return true;
    default: return false;
  }
};
ChatView.prototype.queueDelta = function (props) {
  this._deltaBuf = this._deltaBuf || [];
  this._deltaBuf.push(props);
  if (this._deltaTimer) return;
  // 50ms 合并一批 token，既跟得上阅读速度又不逐 token 触发 layout。
  this._deltaTimer = setTimeout(() => {
    this._deltaTimer = null;
    const batch = this._deltaBuf || [];
    this._deltaBuf = null;
    for (const p of batch) this.applyDelta(p);
    this.scrollToBottom();
  }, 50);
};
ChatView.prototype.applyDelta = function (props) {
  const messageId = props.messageID || props.messageId;
  const partId = props.partID || props.partId;
  const field = props.field || "text";
  const delta = props.delta || "";
  if (!delta) return;
  const known = partId ? this.partIx.get(partId) : null;
  const turn = known ? known.turn : this.ensureTurn(messageId, "assistant");
  if (!turn) return;
  // 用户自己的消息不回放流式增量：乐观气泡已完整显示原文，上游回放会把同一段
  // 文本再叠一遍（一条变两条）。等 fetchPage 用服务端权威版本收敛即可。
  if (turn.role === "user") return;
  let part = known ? known.part : null;
  if (!part) {
    part = { id: partId || uid("p"), type: field === "reasoning" ? "reasoning" : "text", text: "" };
    turn.parts.push(part);
    if (partId) this.partIx.set(partId, { turn, part });
  }
  part.text = (part.text || "") + delta;
  this.liveId = part.id;
  if (!this.working) { this.working = "生成中…"; this.renderWorking(); }
  // 目标节点存在 → 只改文本；否则重绘该轮。
  const node = this.host.querySelector('[data-part="' + cssEscape(part.id) + '"]');
  if (node && node.classList.contains("ct-text") && node.classList.contains("streaming")) {
    node.firstChild && node.firstChild.nodeType === 3
      ? (node.firstChild.nodeValue = part.text)
      : (node.textContent = part.text);
    if (!node.querySelector(".cursor")) {
      const cur = document.createElement("span"); cur.className = "cursor"; node.appendChild(cur);
    }
  } else {
    this.rerenderTurn(turn);
  }
};
ChatView.prototype.ensureTurn = function (messageId, role) {
  if (!messageId) {
    // 少数上游事件不带 messageID，落到最后一个 assistant 轮上。
    for (let i = this.turns.length - 1; i >= 0; i--) if (this.turns[i].role === "assistant") return this.turns[i];
    const t = blankTurn(); this.turns.push(t); this.byMsg.set(t.id, t); return t;
  }
  let t = this.byMsg.get(messageId);
  if (t) return t;
  t = blankTurn(messageId, role);
  this.turns.push(t); this.byMsg.set(messageId, t);
  return t;
};
ChatView.prototype.upsertPart = function (part, messageId) {
  if (!part || !part.id) return;
  const known = this.partIx.get(part.id);
  const turn = known ? known.turn : this.ensureTurn(messageId || part.messageID, "assistant");
  if (!turn) return;
  // 用户自己发出的消息：内容已由乐观气泡原样展示，服务端回放该消息的 parts 时
  // 只会把同一段内容再 append 一份（一条变两条）。跳过回放，等 fetchPage 收敛。
  if (turn.role === "user") return;
  if (known) Object.assign(known.part, part);
  else {
    turn.parts.push(part);
    this.partIx.set(part.id, { turn, part });
  }
  if (part.type === "tool") this.syncToolStatus(part);
  if (this.liveId === part.id) this.liveId = null;
  this.rerenderTurn(turn);
  this.scrollToBottom();
};
ChatView.prototype.upsertTool = function (type, props) {
  const status = type.indexOf("success") >= 0 ? "completed"
    : type.indexOf("failed") >= 0 ? "error" : "running";
  const part = {
    id: props.partID || props.id || uid("tool"),
    type: "tool",
    tool: props.tool || props.name || "tool",
    state: Object.assign({ status: status }, props.state || {}),
  };
  this.upsertPart(part, props.messageID);
};
ChatView.prototype.syncToolStatus = function (part) {
  const st = toolStatus(part.state || {});
  if (st === "running") { this.working = `正在${toolTitle(part, part.state || {}).label}…`; this.renderWorking(); }
  else if (st === "pending") { this.working = "准备工具…"; this.renderWorking(); }
};
ChatView.prototype.removePart = function (partId) {
  const known = partId && this.partIx.get(partId);
  if (!known) return;
  known.turn.parts = known.turn.parts.filter(p => p.id !== partId);
  this.partIx.delete(partId);
  this.rerenderTurn(known.turn);
};
ChatView.prototype.updateMessageInfo = function (info) {
  if (!info || !info.id) return;
  const t = this.byMsg.get(info.id) || (info.role === "user" ? this.takePendingEcho(info) : null);
  if (!t) return;
  absorbMeta(t, info);
  if (info.error) t.error = info.error;
  if (t.role === "user" && info.time && info.time.created) t.ts = info.time.created;
  this.rerenderTurn(t);
  this.renderCtxBar();
};
// 服务端回执到达时认领乐观消息，避免同一条用户消息显示两次。
ChatView.prototype.takePendingEcho = function (info) {
  const pend = this.turns.filter(t => t.pending && t.role === "user");
  if (!pend.length) return null;
  const hit = pend.find(t => t.echoOf === info.id) || pend[0];
  hit.pending = false; hit.id = info.id; hit.msgIds.push(info.id);
  this.byMsg.set(info.id, hit);
  return hit;
};
ChatView.prototype.removeMessage = function (messageId) {
  const t = messageId && this.byMsg.get(messageId);
  if (!t) return;
  this.turns = this.turns.filter(x => x !== t);
  this.reindex(); this.render();
};
ChatView.prototype.statusEvent = function (type, props) {
  const raw = props.status;
  const st = type === "session.idle" ? "idle" : ((raw && (raw.type || raw)) || "idle");
  if (st === "idle") {
    this.working = ""; this.liveId = null; this.renderWorking();
    // 增量只负责显示加速；轮次结束按服务端权威快照收敛一次。
    this.fetchPage(Math.max(PAGE_SIZE, this.turns.length), false);
  } else if (st === "retry") {
    this.working = "重试中…"; this.renderWorking();
  } else {
    if (!this.working) { this.working = "处理中…"; this.renderWorking(); }
  }
  if (this.ctx.statusChanged) this.ctx.statusChanged(st);
};
ChatView.prototype.errorEvent = function (props) {
  const err = props.error || props;
  const msg = (err && (err.message || (err.data && err.data.message))) || "会话出错";
  this.working = ""; this.renderWorking();
  if (this.ctx.toast) this.ctx.toast("会话出错", String(msg).slice(0, 160), "crit");
};
ChatView.prototype.todoEvent = function (props) {
  const todos = props.todos || (props.payload && props.payload.todos);
  if (!Array.isArray(todos) || !todos.length) return;
  let turn = null;
  for (let i = this.turns.length - 1; i >= 0; i--) if (this.turns[i].role === "assistant") { turn = this.turns[i]; break; }
  if (!turn) turn = this.ensureTurn(null, "assistant");
  turn.todos = todos;
  this.render(); this.scrollToBottom();
};
ChatView.prototype.compactedEvent = function () {
  this.fetchPage(PAGE_SIZE, false);
};
function blankTurn(id, role) {
  return { id: id || uid("t"), msgIds: id ? [id] : [], role: role || "assistant", parts: [],
    ts: Date.now(), doneTs: 0, model: "", provider: "", agent: "", variant: "", cost: 0,
    tokens: null, error: null, finish: "" };
}

/* ---- 滚动 ---- */
ChatView.prototype.onScroll = function () {
  const list = this.el("list");
  const near = list.scrollHeight - list.scrollTop - list.clientHeight < NEAR_BOTTOM;
  this.stick = near;
  this.el("fab").classList.toggle("hidden", near);
  if (near) { this.unread = 0; this.updateUnreadBadge(); }
  if (this.cursor && list.scrollTop < 140 && !this.loading) this.loadOlder();
};
ChatView.prototype.loadOlder = function () {
  const list = this.el("list");
  const anchor = { top: list.scrollTop, h: list.scrollHeight };
  return this.fetchPage(OLD_PAGE, true).then(() => {
    requestAnimationFrame(() => { list.scrollTop = list.scrollHeight - anchor.h + anchor.top; });
  });
};
// 按服务端权威快照重载当前已看过的轮次（撤销 / 重做 / 重新加载后调用）。
ChatView.prototype.reload = function () {
  if (!this.sessionId) return Promise.resolve();
  return this.fetchPage(Math.max(PAGE_SIZE, this.turns.length), false);
};
ChatView.prototype.setWorking = function (text) {
  this.working = text || "";
  this.renderWorking();
};
ChatView.prototype.scrollToBottom = function (force) {
  const list = this.el("list");
  if (!list) return;
  if (force || this.stick) {
    list.scrollTop = list.scrollHeight;
    if (force) this.el("fab").classList.add("hidden");
    return;
  }
  const fab = this.el("fab");
  if (fab.classList.contains("hidden")) fab.classList.remove("hidden");
  this.unread++;
  this.updateUnreadBadge();
};
ChatView.prototype.updateUnreadBadge = function () {
  const b = this.host.querySelector('[data-role="fab"] .ub');
  if (!b) return;
  b.classList.toggle("hidden", !this.unread);
  b.textContent = this.unread > 99 ? "99+" : String(this.unread);
};

/* ---- 点击分发 ---- */
ChatView.prototype.onClick = function (e) {
  const c = this.ctx, t = e.target;
  const head = t.closest("[data-ct-toggle]");
  if (head) {
    const want = setOpen(head.dataset.ctToggle, head.dataset.def === "1");
    head.setAttribute("aria-expanded", want ? "true" : "false");
    head.querySelector(".ct-chev").classList.toggle("open", want);
    const body = head.parentElement.querySelector(".ct-body");
    if (body) body.classList.toggle("open", want);
    return;
  }
  const pop = t.closest("[data-pop]");
  if (pop) { this.popup.index = Number(pop.dataset.pop); this.popupPick(); return; }
  const tpl = t.closest("[data-tpl-act]");
  if (tpl) { this.onTplAction(tpl.dataset.tplAct, tpl); return; }
  const rm = t.closest("[data-rm-att]");
  if (rm) { this.attachments.splice(Number(rm.dataset.rmAtt), 1); this.renderAtts(); this.saveAtts(); return; }
  const cc = t.closest("[data-cc]");
  if (cc) { this.onToolbutton(cc.dataset.cc); return; }
  const ctx = t.closest('[data-role="ctx"]');
  if (ctx) { this.showContextUsage(); return; }
  if (t.closest('[data-role="fab"]')) { this.stick = true; this.scrollToBottom(true); return; }
  const act = t.closest("[data-act]");
  if (act) { this.onTurnAction(act.dataset.act, act); return; }
  const cp = t.closest("[data-copy-raw]");
  if (cp) { if (c.copyText) c.copyText(cp.dataset.copyRaw); return; }
  const lb = t.closest("[data-lightbox]");
  if (lb) { Lightbox.show(lb.dataset.lightbox); return; }
  const child = t.closest("[data-open-child]");
  if (child && child.dataset.openChild && c.openSession) c.openSession(child.dataset.openChild);
};
ChatView.prototype.onToolbutton = function (which) {
  if (which === "tpl") this.toggleTpl();
  else if (which === "attach") this.el("file").click();
  else if (which === "voice") this.toggleVoice();
  else if (which === "send") this.onSendKey();
};
ChatView.prototype.onTurnAction = function (act, btn) {
  const c = this.ctx;
  if (act === "older") { this.loadOlder(); return; }
  const turn = this.turnById(btn.dataset.tid);
  if (!turn) return;
  const text = turnText(turn);
  if (act === "copy") { if (c.copyText) c.copyText(text); return; }
  if (act === "quote") { this.insertPrompt("> " + text.split("\n").join("\n> ") + "\n\n"); return; }
  if (act === "edit") { this.insertPrompt(text); return; }
  if (act === "regen") {
    if (c.isBusy && c.isBusy()) { if (c.toast) c.toast("会话处理中", "先等处理完成（或点发送区停止），再执行该操作", "warn"); return; }
    if (c.confirm && !c.confirm("重新生成", "回退到本轮之前并重发上一条指令，该轮之后的内容会被丢弃。")) return;
    if (c.regenerate) c.regenerate(turn);
    return;
  }
  if (act === "revert") {
    if (c.isBusy && c.isBusy()) { if (c.toast) c.toast("会话处理中", "先等处理完成（或点发送区停止），再执行该操作", "warn"); return; }
    if (c.confirm && !c.confirm("回退到此处", "撤销该消息之后的内容（可通过「更多 ▾ → 重做」恢复）。")) return;
    if (c.revert) c.revert(turn);
  }
};
ChatView.prototype.turnById = function (id) { return this.turns.find(t => t.id === id); };
ChatView.prototype.lastUserTurn = function () {
  for (let i = this.turns.length - 1; i >= 0; i--) {
    if (this.turns[i].role === "user" && turnText(this.turns[i])) return this.turns[i];
  }
  return null;
};

/* ============================================================================
 * 输入区
 * ========================================================================== */
ChatView.prototype.text = function () { return this.el("input").value; };
ChatView.prototype.onKeydown = function (e) {
  if (this.popup && this.popup.visible) {
    if (e.key === "ArrowDown") { e.preventDefault(); this.popupMove(1); return; }
    if (e.key === "ArrowUp") { e.preventDefault(); this.popupMove(-1); return; }
    if (e.key === "Enter" || e.key === "Tab") { e.preventDefault(); this.popupPick(); return; }
    if (e.key === "Escape") { e.preventDefault(); this.hidePopup(); return; }
  }
  // isComposing：中文输入法选词回车不应触发发送。
  if (e.key === "Enter" && !e.shiftKey && !e.isComposing) { e.preventDefault(); this.onSendKey(); }
};
ChatView.prototype.onInput = function () {
  const ta = this.el("input");
  ta.style.height = "auto";
  ta.style.height = Math.min(ta.scrollHeight, 200) + "px";
  this.saveDraft();
  this.syncSendIcon();
  // 开始输入说明模板已选定/不再需要，收起模板面板避免遮挡消息区
  this.el("tpl").classList.add("hidden");
  this.detectTrigger(ta.value, ta.selectionStart);
};
// 发送 / 停止语义与 App ComposerAction 一致：处理中且输入框为空 → 停止；
// 有草稿 → 照常发送（允许排队追加指令）。
ChatView.prototype.onSendKey = function () {
  const busy = !!(this.ctx.isBusy && this.ctx.isBusy());
  if (!this.text().trim() && !this.attachments.length) {
    if (busy && this.ctx.abort) this.ctx.abort();
    return;
  }
  this.send();
};
ChatView.prototype.syncSendIcon = function () {
  const btn = this.host && this.host.querySelector('[data-cc="send"]');
  if (!btn) return;
  const busy = !!(this.ctx && this.ctx.isBusy && this.ctx.isBusy());
  const hasDraft = !!(this.text().trim() || this.attachments.length);
  // 处理中且输入框为空 → 按钮变成「停止」；有草稿 → 仍是「发送」（允许排队追加指令）。
  const stopping = busy && !hasDraft;
  btn.innerHTML = icon(stopping ? "stop" : "send", 16);
  btn.title = stopping ? "停止处理" : (btn.disabled ? "输入内容后可发送" : "发送");
  btn.classList.toggle("stopping", stopping);
  // 输入框空且会话空闲 → 禁用（无可发内容）；忙碌时空输入=停止按钮、有草稿=继续发送，均保持可用。
  btn.disabled = !hasDraft && !busy;
};
ChatView.prototype.onPaste = function (e) {
  const items = e.clipboardData && e.clipboardData.items;
  if (!items) return;
  const files = [];
  for (const it of items) { if (it.kind === "file") { const f = it.getAsFile(); if (f) files.push(f); } }
  if (files.length) { e.preventDefault(); this.addFiles(files); }
};
ChatView.prototype.onDrop = function (e) {
  if (e.dataTransfer && e.dataTransfer.files && e.dataTransfer.files.length) {
    e.preventDefault(); this.addFiles(e.dataTransfer.files);
  }
};

/* ---- @提及 / 斜杠命令弹层 ---- */
ChatView.prototype.detectTrigger = function (value, caret) {
  const head = value.slice(0, caret);
  if (head.startsWith("/") && !/\s/.test(head)) { this.showCommands(head.slice(1)); return; }
  const m = /(?:^|\s)@([^\s@]*)$/.exec(head);
  if (m) { this.showMentions(m[1]); return; }
  this.hidePopup();
};
ChatView.prototype.showCommands = async function (q) {
  const c = this.ctx;
  if (!c.listCommands) return;
  const cmds = await c.listCommands();
  if (!this.host || !cmds || !cmds.length) { this.hidePopup(); return; }
  const needle = q.toLowerCase();
  const items = cmds.filter(x => !needle || String(x.name || "").toLowerCase().indexOf(needle) >= 0).slice(0, 40);
  if (!items.length) { this.hidePopup(); return; }
  this.popup = { visible: true, items, index: 0, kind: "cmd" };
  this.renderPopup(items.map(x => ({ icon: "bolt", title: "/" + x.name, sub: x.description || x.hint || "" })));
};
ChatView.prototype.showMentions = function (q) {
  const c = this.ctx;
  if (!c.findFiles) return;
  const seq = (this._mentionSeq = (this._mentionSeq || 0) + 1);
  clearTimeout(this._mentionTimer);
  this._mentionTimer = setTimeout(async () => {
    const files = await c.findFiles(q);
    if (seq !== this._mentionSeq || !this.host) return;
    if (!files || !files.length) { this.hidePopup(); return; }
    const items = files.slice(0, 12);
    this.popup = { visible: true, items, index: 0, kind: "file" };
    this.renderPopup(items.map(f => ({ icon: "file", title: base(f), sub: f })));
  }, 150);
};
ChatView.prototype.renderPopup = function (rows) {
  const pop = this.el("pop");
  pop.classList.remove("hidden");
  pop.innerHTML = rows.map((r, i) => `<button class="cc-item${i === this.popup.index ? " active" : ""}" type="button" data-pop="${i}">
    <span class="pi">${icon(r.icon, 13)}</span><span class="pt">${esc(r.title)}</span><span class="ps">${esc(r.sub)}</span></button>`).join("");
};
ChatView.prototype.hidePopup = function () {
  if (this.popup) this.popup.visible = false;
  const pop = this.host && this.host.querySelector('[data-role="pop"]');
  if (pop) { pop.classList.add("hidden"); pop.innerHTML = ""; }
};
ChatView.prototype.popupMove = function (d) {
  if (!this.popup || !this.popup.items) return;
  const n = this.popup.items.length;
  this.popup.index = (this.popup.index + d + n) % n;
  this.host.querySelectorAll("[data-pop]").forEach((el, i) => {
    el.classList.toggle("active", i === this.popup.index);
    if (i === this.popup.index) el.scrollIntoView({ block: "nearest" });
  });
};
ChatView.prototype.popupPick = function () {
  const pop = this.popup;
  if (!pop || !pop.visible) return;
  const it = pop.items[pop.index];
  pop.visible = false;
  this.hidePopup();
  if (it == null) return;
  const ta = this.el("input");
  ta.value = pop.kind === "cmd" ? ("/" + it.name + " ") : ta.value.replace(/@([^\s@]*)$/, "@" + it + " ");
  ta.focus();
  this.onInput();
};

/* ---- 会话模板选择器 / 管理（对齐 App ChatTemplateDialogs） ---- */
ChatView.prototype.toggleTpl = function () {
  const box = this.el("tpl");
  if (!box) return;
  box.classList.toggle("hidden");
  if (!box.classList.contains("hidden")) { this.tplEditing = null; this.renderTplPanel(); }
  else { this.hidePopup(); }
};
ChatView.prototype.onTplAction = function (act, btn) {
  const c = this.ctx;
  if (act === "close") { this.el("tpl").classList.add("hidden"); return; }
  if (act === "add") { this.tplEditing = { mode: "add" }; this.renderTplPanel(); return; }
  if (act === "edit") {
    this.tplEditing = { mode: "edit", id: btn.dataset.tplId };
    this.renderTplPanel(); return;
  }
  if (act === "back" || act === "cancel") { this.tplEditing = null; this.renderTplPanel(); return; }
  if (act === "save") {
    const name = String(this.el("tpl").querySelector('[data-tpl-field="name"]').value || "").trim();
    const prompt = String(this.el("tpl").querySelector('[data-tpl-field="prompt"]').value || "").trim();
    if (!name || !prompt) { if (c.toast) c.toast("无法保存", "名称与内容都不能为空", "warn"); return; }
    const list = loadTpls();
    if (this.tplEditing && this.tplEditing.mode === "edit") {
      const t = list.find(x => x.id === this.tplEditing.id);
      if (t) { t.name = name; t.prompt = prompt; }
    } else {
      list.push({ id: tplId(), name, prompt });
    }
    saveTpls(list);
    this.tplEditing = null;
    this.renderTplPanel();
    return;
  }
  if (act === "delete") {
    const id = btn.dataset.tplId;
    if (c.confirm && !c.confirm("删除模板", "删除后不可恢复。")) return;
    saveTpls(loadTpls().filter(t => t.id !== id));
    this.renderTplPanel();
    return;
  }
  if (act === "move") {
    const id = btn.dataset.tplId, dir = Number(btn.dataset.tplDir) || 0;
    const list = loadTpls();
    const ix = list.findIndex(t => t.id === id);
    const nx = ix + dir;
    if (ix < 0 || nx < 0 || nx >= list.length) return;
    const [it] = list.splice(ix, 1);
    list.splice(nx, 0, it);
    saveTpls(list);
    this.renderTplPanel();
    return;
  }
  if (act === "use") {
    const prompt = btn.dataset.tplVal || "";
    this.el("tpl").classList.add("hidden");
    this.tplEditing = null;
    if (prompt) this.insertPrompt(prompt);
    return;
  }
  if (act === "expand") {
    const key = btn.dataset.tplKey;
    const s = this.tplExpanded || (this.tplExpanded = new Set());
    if (s.has(key)) s.delete(key); else s.add(key);
    try { localStorage.setItem(TPL_EXPAND_KEY, JSON.stringify([...s])); } catch (_) {}
    this.renderTplPanel();
    return;
  }
};
ChatView.prototype.renderTplPanel = function () {
  const box = this.el("tpl");
  if (!box) return;
  const c = this.ctx;
  if (this.tplEditing) {
    const editing = this.tplEditing;
    let name = "", prompt = "";
    if (editing.mode === "edit") {
      const t = loadTpls().find(x => x.id === editing.id);
      if (t) { name = t.name; prompt = t.prompt; }
    }
    box.innerHTML = `
      <div class="tpl-head"><span class="tpl-title">${editing.mode === "add" ? "新建模板" : "编辑模板"}</span>
        <div><button class="ghost xs" type="button" data-tpl-act="back">← 返回</button></div></div>
      <div class="tpl-edit">
        <input data-tpl-field="name" placeholder="模板名称" value="${esc(name)}">
        <textarea data-tpl-field="prompt" placeholder="模板内容（Prompt）" rows="4">${esc(prompt)}</textarea>
        <div class="tpl-edit-actions">
          <button class="sm" type="button" data-tpl-act="save">保存</button>
          <button class="ghost sm" type="button" data-tpl-act="cancel">取消</button>
        </div>
      </div>`;
    const inp = box.querySelector('[data-tpl-field="name"]');
    if (inp) inp.focus();
    return;
  }
  const user = loadTpls();
  if (!this.tplExpanded) {
    try { this.tplExpanded = new Set(JSON.parse(localStorage.getItem(TPL_EXPAND_KEY) || "[]")); } catch (_) { this.tplExpanded = new Set(); }
  }
  const row = (t, builtin, i, n) => {
    const key = builtin ? "builtin:" + t.name : t.id;
    const exp = this.tplExpanded.has(key);
    const acts = builtin ? `<button class="ghost xs" type="button" data-tpl-act="expand" data-tpl-key="${esc(key)}" title="${exp ? "收起" : "展开内容"}">${icon(exp ? "chevron_up" : "chevron_down", 12)}</button>`
      : `<button class="ghost xs" type="button" data-tpl-act="expand" data-tpl-key="${esc(key)}" title="${exp ? "收起" : "展开内容"}">${icon(exp ? "chevron_up" : "chevron_down", 12)}</button>
         <button class="ghost xs" type="button" data-tpl-act="move" data-tpl-id="${esc(t.id)}" data-tpl-dir="-1" title="上移" ${i === 0 ? "disabled" : ""}>↑</button>
         <button class="ghost xs" type="button" data-tpl-act="move" data-tpl-id="${esc(t.id)}" data-tpl-dir="1" title="下移" ${i === n - 1 ? "disabled" : ""}>↓</button>
         <button class="ghost xs" type="button" data-tpl-act="edit" data-tpl-id="${esc(t.id)}" title="编辑">编辑</button>
         <button class="ghost xs danger" type="button" data-tpl-act="delete" data-tpl-id="${esc(t.id)}" title="删除">删除</button>`;
    return `<div class="tpl-item${builtin ? " builtin" : ""}">
      <button class="tpl-use" type="button" data-tpl-act="use" data-tpl-val="${esc(t.prompt)}"><span class="tpl-name">${esc(t.name)}</span><span class="tpl-tag">${builtin ? "内置" : ""}</span></button>
      <span class="tpl-acts">${acts}</span>
      ${exp ? `<div class="tpl-prompt">${esc(clipText(t.prompt, 600).text)}</div>` : ""}
    </div>`;
  };
  const builtinHtml = BUILTIN_TPL.map((t, i) => row(t, true, i, BUILTIN_TPL.length)).join("");
  const userHtml = user.map((t, i) => row(t, false, i, user.length)).join("");
  box.innerHTML = `
    <div class="tpl-head"><span class="tpl-title">会话模板</span>
      <div><button class="ghost xs" type="button" data-tpl-act="add">＋ 新建</button>
        <button class="ghost xs" type="button" data-tpl-act="close" title="关闭">${icon("x", 12)}</button></div></div>
    <div class="tpl-scroll">
      <div class="tpl-group">内置</div>${builtinHtml || ""}
      <div class="tpl-group">我的模板</div>
      ${userHtml || `<div class="tpl-empty">还没有自定义模板，点「＋ 新建」添加</div>`}
    </div>`;
};

/* ---- 附件 ---- */
// 保存当前会话的未发送附件：先落内存（切会话立刻有），小体积再落 localStorage（跨刷新）。
// 附件是 base64 data URL，很容易超 localStorage 配额，超过 3MB 就只留内存。
ChatView.prototype.saveAtts = function () {
  if (!this.sessionId) return;
  this.attDrafts[this.sessionId] = this.attachments.slice();
  const total = this.attachments.reduce((n, a) => n + (a.size || 0), 0);
  try {
    if (total > 3 * 1024 * 1024 || !this.attachments.length) localStorage.removeItem(ATT_DRAFT_PREFIX + this.sessionId);
    else localStorage.setItem(ATT_DRAFT_PREFIX + this.sessionId, JSON.stringify(this.attachments));
  } catch (_) { /* 配额满则放弃持久化，只保留内存草稿 */ }
};
// 恢复指定会话的未发送附件（内存优先，localStorage 兜底）。
ChatView.prototype.restoreAtts = function () {
  if (!this.sessionId) { this.attachments = []; return; }
  const mem = this.attDrafts[this.sessionId];
  if (Array.isArray(mem)) { this.attachments = mem.slice(); }
  else {
    let stored = null;
    try { const raw = localStorage.getItem(ATT_DRAFT_PREFIX + this.sessionId); stored = raw ? JSON.parse(raw) : null; } catch (_) {}
    if (Array.isArray(stored)) this.attachments = stored;
    else this.attachments = [];
    if (stored) this.attDrafts[this.sessionId] = stored.slice();
  }
  this.renderAtts();
};
ChatView.prototype.addFiles = function (files) {
  const c = this.ctx;
  for (const f of Array.from(files || [])) {
    if (f.size > ATTACH_MAX_BYTES) {
      if (c.toast) c.toast("附件过大", f.name + " 超过 10 MB", "warn");
      continue;
    }
    const isText = /^text\//.test(f.type) || /\.(md|txt|json|ya?ml|log|csv|go|py|js|ts|tsx|java|kt|rs|c|h|cpp|sh|sql|xml|toml|ini)$/i.test(f.name);
    if (isText && f.size > ATTACH_TEXT_MAX) {
      if (c.toast) c.toast("文本过大", f.name + " 超过 2 MB", "warn");
      continue;
    }
    const reader = new FileReader();
    reader.onload = () => {
      this.attachments.push({ name: f.name, mime: f.type || "application/octet-stream", size: f.size, url: String(reader.result || "") });
      this.renderAtts();
      this.saveAtts();
    };
    reader.onerror = () => { if (c.toast) c.toast("读取失败", f.name, "crit"); };
    reader.readAsDataURL(f);
  }
};
ChatView.prototype.renderAtts = function () {
  const box = this.host && this.host.querySelector('[data-role="atts"]');
  if (!box) return;
  box.classList.toggle("hidden", !this.attachments.length);
  box.innerHTML = this.attachments.map((a, i) => `<span class="att">
    ${a.mime.indexOf("image/") === 0 ? `<img src="${esc(a.url)}" alt="">` : `<span class="ai">${icon("file", 12)}</span>`}
    <b>${esc(a.name)}</b><span class="sz">${fmtTok(a.size)}B</span>
    <button type="button" data-rm-att="${i}" aria-label="移除">×</button></span>`).join("");
  this.syncSendIcon();
};

/* ---- 组 parts 与发送 ---- */
// 与 App buildPromptParts 同策略：@path 提及展开成 file part，其余按文本分段。
function buildPartsFromText(raw, attachments) {
  const parts = [];
  const mentions = [];
  const re = /(?:^|\s)@([^\s@]+)/g;
  let mm;
  while ((mm = re.exec(raw))) {
    const at = mm.index + mm[0].indexOf("@");
    mentions.push({ start: at, end: at + mm[0].trimStart().length, path: mm[1] });
  }
  if (!mentions.length) {
    if (raw) parts.push({ type: "text", text: raw });
  } else {
    let cur = 0;
    for (const m of mentions) {
      if (m.start > cur) {
        const seg = raw.slice(cur, m.start).trim();
        if (seg) parts.push({ type: "text", text: seg });
      }
      // 绝对路径 / 含 .. 的一律不展开为文件引用，退化为普通文本（防路径穿越）。
      if (m.path.indexOf("..") >= 0 || m.path.charAt(0) === "/") parts.push({ type: "text", text: "@" + m.path });
      else parts.push({ type: "file", path: m.path });
      cur = m.end;
    }
    const tail = raw.slice(cur).trim();
    if (tail) parts.push({ type: "text", text: tail });
  }
  for (const a of attachments || []) {
    parts.push({ type: a.mime.indexOf("image/") === 0 ? "image" : "file", mime: a.mime, url: a.url, filename: a.name });
  }
  return parts;
}
ChatView.prototype.buildParts = function () { return buildPartsFromText(this.text().trim(), this.attachments); };
ChatView.prototype.send = async function () {
  const c = this.ctx;
  if (!c || !this.sessionId) return;
  if (c.isSending && c.isSending()) {
    // 上一条请求仍在途：不重复发，但给出明确反馈，避免「按了 Enter 没反应/内容没清」。
    if (c.toast) c.toast("正在发送", "上一条指令仍在发送，请稍候再试", "info");
    return;
  }
  const parts = this.buildParts();
  if (!parts.length) return;
  const ta = this.el("input");
  const keep = ta.value;
  const keepAtts = this.attachments.slice();
  this.el("tpl").classList.add("hidden");
  ta.value = ""; ta.style.height = "auto";
  this.attachments = []; this.renderAtts(); this.clearDraft(); this.saveAtts();
  const ok = await this.sendParts(parts, this.selections());
  if (!ok) {
    // 明确失败：把草稿还给用户（安卓同处理），乐观气泡在 sendParts 内已撤回。
    ta.value = keep; this.attachments = keepAtts; this.renderAtts();
    this.saveDraft(); this.saveAtts(); this.syncSendIcon();
  }
};
// 发送一组 parts：先落乐观气泡，再按 /命令 或 prompt_async 分派；失败返回 false。
ChatView.prototype.sendParts = async function (parts, sel) {
  const c = this.ctx;
  sel = sel || {};
  // 发新指令 = 离开回退状态：上游会在写入新消息时裁掉被回退的尾部。
  this.revertTo = null;
  // 上游契约：messageID（大写 ID）+ model{providerID,modelID} + variant 顶层字段，
  // 与安卓 PromptRequest 完全一致；写错键名会让服务端自建 id、乐观消息无法认领。
  const body = { messageID: nextMessageId(), parts };
  if (sel.agent) body.agent = sel.agent;
  if (sel.model) body.model = sel.model;
  if (sel.variant) body.variant = sel.variant;
  // 单条纯文本且形如 /命令 → 走 /command 端点（与 App executeCommand 一致）。
  const slash = parts.length === 1 && parts[0].type === "text" ? /^\/([\w:-]+)\s*([\s\S]*)$/.exec(parts[0].text) : null;
  const echo = Object.assign(blankTurn(body.messageID, "user"), {
    parts, pending: true, agent: sel.agent || (c.session && (c.session() || {}).agent) || "",
  });
  this.turns.push(echo);
  this.reindex(); this.render(); this.scrollToBottom(true);
  let ok = true;
  if (slash && c.command) ok = await c.command(slash[1], slash[2] || "");
  else if (c.send) ok = await c.send(body);
  if (ok === false) {
    this.turns = this.turns.filter(t => t !== echo);
    this.reindex(); this.render();
    return false;
  }
  echo.pending = false;
  this.rerenderTurn(echo);
  if (c.sent) c.sent();
  return true;
};
// 「重新生成」：用同一轮用户指令的 parts 再发一次（parts 需还原成请求体形态）。
ChatView.prototype.resendTurn = function (turnId) {
  const t = this.turns.find(x => x.id === turnId);
  if (!t || t.role !== "user") return false;
  const parts = (t.parts || []).filter(p => p && !p.synthetic && !p.ignored).map(p => {
    if (p.type === "text") return { type: "text", text: p.text || "" };
    if (p.type === "file" || p.type === "image") {
      return { type: p.type, path: p.path, mime: p.mime, url: p.url, filename: p.filename };
    }
    return null;
  }).filter(Boolean);
  if (!parts.length) return false;
  this.sendParts(parts, { agent: t.agent || "" });
  return true;
};
// 丢弃某轮之后的所有本地轮次（回退 / 重新生成后与服务端保持一致）。
ChatView.prototype.dropAfter = function (turnId) {
  const ix = this.turns.findIndex(t => t.id === turnId);
  if (ix < 0) return;
  this.turns = this.turns.slice(0, ix + 1);
  this.reindex(); this.render();
};
ChatView.prototype.selections = function () {
  const a = this.el("agent"), m = this.el("model");
  const out = { agent: a ? a.value : "", model: null, variant: "" };
  if (m && m.value) {
    const seg = m.value.split("\u0001");
    if (seg.length >= 2 && seg[0] && seg[1]) {
      out.model = { providerID: seg[0], modelID: seg[1] };
      if (seg[2] && seg[2] !== "default") out.variant = seg[2];
    }
  }
  return out;
};

/* ---- 草稿 ---- */
ChatView.prototype.saveDraft = function () {
  if (!this.sessionId) return;
  try { localStorage.setItem(DRAFT_PREFIX + this.sessionId, this.text()); } catch (_) {}
};
ChatView.prototype.restoreDraft = function () {
  if (!this.sessionId || !this.host) return;
  let d = "";
  try { d = localStorage.getItem(DRAFT_PREFIX + this.sessionId) || ""; } catch (_) {}
  const ta = this.el("input");
  ta.value = d;
  ta.style.height = "auto";
  ta.style.height = Math.min(ta.scrollHeight, 200) + "px";
  this.syncSendIcon();
};
ChatView.prototype.clearDraft = function () {
  try { localStorage.removeItem(DRAFT_PREFIX + this.sessionId); } catch (_) {}
};
ChatView.prototype.insertPrompt = function (text, focus) {
  const ta = this.el("input");
  if (!ta) return;
  ta.value = text;
  this.saveDraft(); this.syncSendIcon();
  if (focus !== false) { ta.focus(); ta.setSelectionRange(ta.value.length, ta.value.length); }
};

/* ---- Agent / 模型选择器（内容由 app.js 提供） ---- */
ChatView.prototype.renderSelectors = function () {
  const c = this.ctx, s = (c.session && c.session()) || {};
  const ag = this.el("agent");
  const agents = c.agents ? (c.agents() || []) : [];
  const cur = s.agent || "";
  ag.innerHTML = agents.length
    ? agents.map(a => `<option value="${esc(a)}"${a === cur ? " selected" : ""}>Agent ${esc(a)}</option>`).join("")
    : `<option value="">Agent ${esc(cur || "默认")}</option>`;
  ag.disabled = agents.length < 2;
  this.el("model").innerHTML = c.modelOptions ? c.modelOptions() : "";
};
ChatView.prototype.refreshSelectors = function () {
  this.renderSelectors();
  this.renderCtxBar();
  this.syncSendIcon();
};
// 上下文占用与花费（App 的 context ring 同款信息，这里用文字 + 进度条表达）。
// 整条可点：点开详情弹窗（对齐 App ContextUsageDialog）。无数据时也给出可点的
// 「详情」入口，保证会话统计面板可被发现。
ChatView.prototype.renderCtxBar = function () {
  const c = this.ctx, box = this.el("ctx");
  if (!box) return;
  const st = this.contextStats();
  const budget = (c.contextBudget && c.contextBudget()) || 0;
  const bits = [];
  if (st.used) bits.push(`<span>${fmtTok(st.used)} tok</span>`);
  if (st.cost) bits.push(`<span>${esc(fmtCost(st.cost))}</span>`);
  let body;
  if (budget && st.used) {
    const pct = Math.min(100, Math.round(st.used / budget * 100));
    body = `<button type="button" class="ctx-click"><span class="ctx-bar"><i style="width:${pct}%"></i></span><span class="ctx-n">${pct}%</span>`
      + (bits.length ? `<span class="ctx-sub">${bits.join(" · ")}</span>` : "")
      + `<span class="ctx-caret">${icon("info", 11)}</span></button>`;
    box.title = `上下文占用约 ${pct}%（预算 ${fmtTok(budget)}）— 点击查看详情`;
  } else if (st.used) {
    body = `<button type="button" class="ctx-click"><span class="ctx-sub">${bits.join(" · ")}</span><span class="ctx-caret">${icon("info", 11)}</span></button>`;
    box.title = "查看本会话 token 用量详情";
  } else {
    body = `<button type="button" class="ctx-click"><span class="ctx-sub" style="opacity:.55">详情</span><span class="ctx-caret">${icon("info", 11)}</span></button>`;
    box.title = "查看本会话统计（token / 花费 / 模型）";
  }
  box.innerHTML = body;
};
// 聚合当前会话的 token / 花费 / 消息统计（上下文详情弹窗的数据源）。
ChatView.prototype.contextStats = function () {
  let used = 0, out = 0, reason = 0, cache = 0, cost = 0;
  let user = 0, assistant = 0;
  let firstTs = 0, endTs = 0;
  for (const t of this.turns) {
    if (t.role === "user") user++; else assistant++;
    if (t.ts && (!firstTs || t.ts < firstTs)) firstTs = t.ts;
    if (t.doneTs && t.doneTs > endTs) endTs = t.doneTs;
    const tk = t.tokens || {};
    const c = (tk.cache || {}).read || 0;
    used += (tk.input || 0) + c + (tk.output || 0);
    out += tk.output || 0;
    reason += tk.reasoning || 0;
    cache += c;
    cost += t.cost || 0;
  }
  const inOnly = used - out - cache;
  const s = (this.ctx && this.ctx.session && this.ctx.session()) || {};
  return { used, out, inOnly, reason, cache, cost, user, assistant, firstTs, endTs,
    model: this.turns.length ? (this.turns[this.turns.length - 1].model || "") : modelOf(s.model),
    agent: s.agent || "", sessionId: this.sessionId, created: (s.time && s.time.created) || firstTs };
};
function modelOf(m) {
  if (!m) return "";
  if (typeof m === "object") return m.id || m.modelID || "";
  return String(m);
}
// 上下文占用详情弹窗：预算进度、token 分项、花费、消息数、会话元信息（对齐 App ContextUsageDialog）。
ChatView.prototype.showContextUsage = function () {
  const c = this.ctx, st = this.contextStats();
  const budget = (c.contextBudget && c.contextBudget()) || 0;
  const pct = budget && st.used ? Math.min(100, Math.round(st.used / budget * 100)) : 0;
  const rows = [];
  rows.push(["模型", esc(st.model || "—")]);
  if (st.agent) rows.push(["Agent", esc(st.agent)]);
  if (st.sessionId) rows.push(["会话 ID", esc(String(st.sessionId).slice(0, 26))]);
  if (st.created) rows.push(["创建时间", esc(fmtTime(st.created))]);
  const dur = st.firstTs && st.endTs ? fmtDur(st.endTs - st.firstTs) : "";
  if (dur) rows.push(["总历时", esc(dur)]);
  let html = `
    <div class="ctx-dlg">
      ${budget ? `<div class="ctx-dlg-progress">
        <div class="ctx-dlg-row"><span>上下文占用</span><b>${pct}%</b></div>
        <div class="ctx-dlg-bar"><i style="width:${pct}%"></i></div>
        <div class="ctx-dlg-hint">已用 ${fmtTok(st.used)} / 预算 ${fmtTok(budget)}，剩余 ${fmtTok(Math.max(0, budget - st.used))}</div>
      </div>` : st.used ? `<div class="ctx-dlg-hint">本会话累计 ${fmtTok(st.used)} token（未配置模型上下文预算，无法计算百分比）</div>` : ""}
      <div class="ctx-dlg-grid">
        <div class="ctx-dlg-cell"><b>${fmtTok(st.used)}</b><span>总 token</span></div>
        <div class="ctx-dlg-cell"><b>${fmtTok(st.inOnly)}</b><span>输入</span></div>
        <div class="ctx-dlg-cell"><b>${fmtTok(st.out)}</b><span>输出</span></div>
        <div class="ctx-dlg-cell"><b>${fmtTok(st.reason)}</b><span>推理</span></div>
        <div class="ctx-dlg-cell"><b>${fmtTok(st.cache)}</b><span>缓存读取</span></div>
        <div class="ctx-dlg-cell"><b>${esc(fmtCost(st.cost)) || "—"}</b><span>花费</span></div>
      </div>
      <div class="ctx-dlg-row"><span>消息</span><b>${st.user + st.assistant} 条 <span class="muted">(用户 ${st.user} / AI ${st.assistant})</span></b></div>
      <div class="ctx-dlg-meta">${rows.map(r => `<span>${r[0]}：${r[1]}</span>`).join("")}</div>
    </div>`;
  if (global.wbModalOpen) global.wbModalOpen("上下文占用详情", html);
  else if (this.ctx && this.ctx.toast) this.ctx.toast("上下文占用", `${fmtTok(st.used)} token · 已用 ${pct}%`, "info");
};

/* ---- 语音输入（后端 /api/stt 流式识别） ---- */
ChatView.prototype.toggleVoice = async function () {
  const c = this.ctx;
  if (this.voice) { this.stopVoice(); return; }
  if (!c.recognize) {
    if (c.toast) c.toast("语音输入不可用", "后端未配置语音识别服务", "warn");
    return;
  }
  let rec;
  this.voiceBase = this.el("input").value || "";
  try { rec = await c.recognize((text) => this.onVoiceText(text)); }
  catch (e) {
    if (c.toast) c.toast("无法开始录音", String(e && e.message || e), "crit");
    return;
  }
  this.voice = rec;
  const btn = this.host.querySelector('[data-cc="voice"]');
  if (btn) btn.classList.add("recording");
  if (c.toast) c.toast("录音中", "再次点击麦克风结束", "info", 3000);
};
// /api/stt 每个分片返回的是「累积全文」，因此整体替换识别区间，不能追加。
// voiceBase 是按下麦克风时输入框里已有的内容（与 App 的 pre-roll 一致）。
ChatView.prototype.onVoiceText = function (text) {
  const ta = this.el("input");
  if (!ta) return;
  const base = this.voiceBase || "";
  ta.value = base + (base && !/\s$/.test(base) ? " " : "") + text;
  ta.style.height = "auto";
  ta.style.height = Math.min(ta.scrollHeight, 200) + "px";
  this.saveDraft();
  this.syncSendIcon();
};
ChatView.prototype.stopVoice = function () {
  if (this.voice && this.voice.stop) this.voice.stop();
  this.voice = null;
  const btn = this.host && this.host.querySelector('[data-cc="voice"]');
  if (btn) btn.classList.remove("recording");
};

/* ============================================================================
 * 图片灯箱
 * ========================================================================== */
const Lightbox = {
  el: null,
  show(src) {
    if (!this.el) {
      this.el = document.createElement("div");
      this.el.className = "lightbox hidden";
      this.el.innerHTML = `<button type="button" aria-label="关闭"><img alt=""></button>`;
      this.el.addEventListener("click", () => this.hide());
      document.body.appendChild(this.el);
    }
    this.el.querySelector("img").src = src;
    this.el.classList.remove("hidden");
  },
  hide() { if (this.el) this.el.classList.add("hidden"); },
};
document.addEventListener("keydown", (e) => { if (e.key === "Escape") Lightbox.hide(); });

/* ============================================================================
 * SSE 多路复用器
 *
 * 后端 /api/stream 把上游 OpenCode 的全量事件逐条转发（含 .delta）。这里只开
 * 一条连接，把事件分发给所有订阅者，避免「实时流」页和工作台各占一条连接、
 * 也避免切会话时反复重连。/api/stream 支持 ?session= 查询参数鉴权，因此可以
 * 直接用 EventSource。
 * ========================================================================== */
const Bus = {
  es: null,
  subs: new Set(),
  retry: null,
  attempts: 0,
  status: "off",
  onStatus: null,
  // app.js 注入：返回 EventSource 可用的鉴权查询串（不含 "?"）。EventSource 无法
  // 自定义请求头，后端 /api/stream 因此支持 ?session= / ?token= 查询参数。
  cred: null,
  subscribe(fn) {
    this.subs.add(fn);
    this.connect();
    return () => this.unsubscribe(fn);
  },
  unsubscribe(fn) {
    this.subs.delete(fn);
    if (!this.subs.size) this.close();
  },
  close() {
    if (this.es) { try { this.es.close(); } catch (_) {} this.es = null; }
    clearTimeout(this.retry); this.retry = null;
    this.setStatus("off");
  },
  connect() {
    if (this.es) return;
    const q = this.cred ? (this.cred() || "") : "";
    if (!q) { this.setStatus("off"); return; }
    let es;
    try { es = new EventSource("/api/stream?" + q); } catch (_) { return; }
    this.es = es;
    this.setStatus("connecting");
    es.onopen = () => { this.attempts = 0; this.setStatus("on"); };
    es.onmessage = (ev) => {
      let obj = null;
      try { obj = JSON.parse(ev.data); } catch (_) { return; }
      for (const fn of Array.from(this.subs)) {
        try { fn(obj); } catch (_) { /* 单个订阅者出错不应中断分发 */ }
      }
    };
    es.onerror = () => {
      this.setStatus("err");
      es.close();
      this.es = null;
      if (!this.subs.size) return;
      // 指数退避封顶 30s：后端重启 / 网络抖动时不要疯狂重连。
      this.attempts++;
      const wait = Math.min(30000, 1000 * Math.pow(2, Math.min(this.attempts, 5)));
      clearTimeout(this.retry);
      this.retry = setTimeout(() => { if (this.subs.size) this.connect(); }, wait);
    };
  },
  setStatus(s) { this.status = s; if (this.onStatus) this.onStatus(s); },
  // 登录态变化后换用新凭据重连（保留订阅者）。
  restart() {
    const alive = this.subs.size;
    this.close();
    if (alive) this.connect();
  },
};

/* ============================================================================
 * 导出
 * ========================================================================== */
global.SBChat = {
  ChatView,
  normalizeTurns,
  turnText,
  renderTurn,
  buildPartsFromText,
  stripAnsi,
  renderUnifiedDiff,
  toolStatus,
  Bus,
  Lightbox,
  nextMessageId,
  helpers: { fmtDur, fmtTok, fmtCost, fmtTime, icon },
};
})(window);
