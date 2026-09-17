/* ============================================================================
 * StarBurst Console — app logic
 * 布局：左栏导航 + 顶栏 + 内容区；所有 API 端点与后端保持稳定契约。
 * ========================================================================== */
"use strict";

const SESSION_KEY = "ocb_web_session";
const APP_TOKEN_KEY = "ocb_app_token";
const PAGE_KEY = "ocb_last_page";
let session = localStorage.getItem(SESSION_KEY) || "";

const TASK_STATUS = {
  queued: "排队中", running: "执行中", succeeded: "已完成",
  failed: "失败", retrying: "重试中", canceled: "已取消",
  pending: "等待前置", blocked: "被阻塞",
};
const KIND_LABEL = { cron: "cron 定时", git: "git 监听", http: "webhook" };

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
function fmtDate(v) { return v ? new Date(v).toLocaleString() : "-"; }

async function api(path, opts) {
  opts = opts || {};
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
}

/* ---------- 路由 ---------- */
const TITLES = {
  workbench: "AI 工作台", tasks: "任务", workflow: "编排", stream: "实时流", projects: "项目 / 会话",
  rules: "自动化规则", archives: "会话归档", audit: "审计日志",
  tokens: "Token 管理", settings: "设置", intel: "智能测试",
};
function switchPage(name) {
  // 记住当前页面，刷新后回到同一页而不是跳回首页。
  localStorage.setItem(PAGE_KEY, name);
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
  if (name === "workbench") { loadWorkbench(); loadWbEvents(); renderWbFilters(); ensureWbProviders(); }
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
  renderAuth();
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
    // 恢复上次所在页面（刷新不跳回首页）；无效/无记录则默认进 AI 工作台。
    const last = localStorage.getItem(PAGE_KEY);
    switchPage(TITLES[last] ? last : "workbench");
  }
}

/* ---------- App token ---------- */
function saveAppToken() {
  const v = document.getElementById("appToken").value.trim();
  localStorage.setItem(APP_TOKEN_KEY, v);
  document.getElementById("appToken").value = v;
  if (taskWS) taskWS.close();
  connectTaskWS();
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
  const np = document.getElementById("newpw").value;
  const res = await api("/api/web/password", { method: "POST", headers: hdr(), body: JSON.stringify({ newPassword: np }) });
  const data = await res.json();
  show(document.getElementById("pwMsg"), res.ok ? "密码已更新" : (data.error || "修改失败"), res.ok);
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
const WB_HIGH_FREQ = new Set(["heartbeat", "server.heartbeat", "sync", "server.connected", "message.part.delta", "message.part.updated", "message.part.removed", "message.updated", "session.updated"]);
const WB_MAX_EVENTS = 50;
// 大屏浏览器可容纳更完整的内容：多拉消息、多显示几条、少截断。
const WB_PANEL_LIMIT = 200;
const WB_RECENT = 40;
const WB_MSG_CLIP = 3000;
const WB_RECENT_SHOW = 24;
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
// 决策面板「最近对话」是否展开显示全部（默认收起，只显示最新 N 条，更早的从顶部展开）。
let wbMoreMsgs = false;
// 决策面板是否处于「改名」编辑态。
let wbRenaming = false;
// 会话是否正在压缩（压缩为同步长耗时操作，期间禁用操作按钮）。
let wbCompacting = false;
// 有未读新消息的会话集合（后端 session_unread 为准，Web/App 共享）。
let wbNewSet = new Set();
// 待授权操作：sessionId → [PermissionRequest{id, permission, patterns, always, tool}]。
let wbPermissions = {};
// 待决问题的作答暂存：wbQAnswers[requestId][questionIndex] = [selectedLabels...]。
// 一个 request 可包含多个 question，需全部作答后一次性提交（answers 数组按序对应）。
let wbQAnswers = {};
// 渲染后是否把对话区贴底（打开会话 / 刚发送回复时用），展示最新消息。
let wbPanelScrollBottom = false;
// 正在拉取并渲染的决策面板会话 id（同会话去重，避免 5s 轮询与 WS 推送重复拉取）。
let wbPanelFetching = null;
// 快捷回复输入框自动撑高：随输入行数增长（最多到 max-height，超出内部滚动）。
const WB_REPLY_MAX_H = 180;
function wbAutosizeReply(ta) {
  if (!ta) return;
  ta.style.height = "auto";
  ta.style.height = Math.min(ta.scrollHeight, WB_REPLY_MAX_H) + "px";
  ta.style.overflowY = ta.scrollHeight > WB_REPLY_MAX_H ? "auto" : "hidden";
}
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

// 决策面板输入快照：重绘前记住快捷回复 / 待决问题自定义输入的内容、焦点与滚动位置。
function wbSnapshotPanel() {
  const snap = { q: {}, scroll: null };
  const ta = document.getElementById("wbReplyInput");
  if (ta) {
    snap.reply = ta.value;
    snap.focused = document.activeElement === ta;
  }
  document.querySelectorAll('#wbPanel input[id^="wbQInput_"]').forEach(inp => {
    snap.q[inp.id] = inp.value;
  });
  const sc = document.querySelector("#wbPanel .wb-scroll");
  if (sc) snap.scroll = sc.scrollTop;
  const mw = document.querySelector("#wbPanel .wb-msgwrap");
  if (mw) snap.mw = { top: mw.scrollTop, height: mw.scrollHeight };
  return snap;
}
function wbRestorePanel(snap) {
  if (!snap) return;
  const ta = document.getElementById("wbReplyInput");
  if (ta && snap.reply != null) {
    ta.value = snap.reply;
    wbAutosizeReply(ta);
    if (snap.focused) {
      ta.focus();
      const len = ta.value.length;
      try { ta.setSelectionRange(len, len); } catch (_) {}
    }
  }
  Object.keys(snap.q || {}).forEach(id => {
    const inp = document.getElementById(id);
    if (inp) inp.value = snap.q[id];
  });
  const sc = document.querySelector("#wbPanel .wb-scroll");
  if (sc && snap.scroll != null) sc.scrollTop = snap.scroll;
  // 对话区滚动：默认贴底看最新；已在底部（或要求贴底）时跟随新内容，否则保持原位置。
  // 注意：首次打开时快照里没有旧 msgwrap（snap.mw 为空），此时必须按 wbPanelScrollBottom 贴底。
  const mw = document.querySelector("#wbPanel .wb-msgwrap");
  if (mw) {
    let stick = wbPanelScrollBottom;
    if (snap.mw) {
      const nearBottom = snap.mw.height <= 0 || snap.mw.top + 120 >= snap.mw.height;
      stick = stick || nearBottom;
    }
    if (stick) {
      mw.scrollTop = mw.scrollHeight;
      requestAnimationFrame(() => {
        const m2 = document.querySelector("#wbPanel .wb-msgwrap");
        if (m2) m2.scrollTop = m2.scrollHeight;
      });
    } else if (snap.mw) {
      mw.scrollTop = snap.mw.top;
    }
  }
  wbPanelScrollBottom = false;
}
// 重绘决策面板但不丢输入/焦点/滚动（snapshot → render → restore）。
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
  const model = (d.session && d.session.model && d.session.model.id) || "";
  return [d.id, st, model,
    (d.pending || []).map(q => q.id).join(","),
    (d.permissions || []).map(p => p.id).join(","),
    (d.recent || []).map(m => m.role + ":" + m.text.slice(0, 60)).join("|"),
  ].join("\u0001");
}

// 拉取可切换的 provider + 模型列表（懒加载并缓存；失败只尝试一次，避免每次刷新重复打接口）。
async function ensureWbProviders(force) {
  if (wbProviders && !force) return wbProviders;
  if (!force && wbProvidersState !== "idle") return wbProviders;
  wbProvidersState = "loading";
  try {
    const res = await api("/api/opencode/config/providers", { headers: appHeaders() });
    if (!res.ok) { wbProvidersState = "failed"; return wbProviders; }
    const d = await res.json();
    const list = (d.providers || []).map(p => ({
      id: p.id,
      name: p.name || p.id,
      models: Object.values(p.models || {}).map(m => ({
        id: m.id,
        name: m.name || m.id,
        variant: m.variant || "default",
      })),
    })).filter(p => p.models.length);
    wbProviders = list;
    wbProvidersState = "loaded";
    return list;
  } catch (_) {
    wbProvidersState = "failed";
    return wbProviders;
  }
}
// 渲染「模型」下拉选项：按 provider 分组；当前会话模型不在列表时额外补一项。
function wbModelOptions(curModelId) {
  const list = wbProviders;
  if (!list) return `<option value="" disabled>加载模型列表…</option>`;
  let html = "";
  const curPid = (wbPanelData && wbPanelData.session && wbPanelData.session.model && wbPanelData.session.model.providerID) || "";
  const curMid = curModelId || (wbPanelData && wbPanelData.session && wbPanelData.session.model && wbPanelData.session.model.id) || "";
  let found = false;
  for (const p of list) {
    const opts = p.models.map(m => {
      if (m.id === curMid && p.id === curPid) found = true;
      const label = `${m.name} (${p.id})`;
      return `<option value="${escapeHtml(p.id + "\u0001" + m.id + "\u0001" + m.variant)}">${escapeHtml(label)}</option>`;
    }).join("");
    html += `<optgroup label="${escapeHtml(p.name)}">${opts}</optgroup>`;
  }
  if (!found && curMid) {
    const variant = (wbPanelData && wbPanelData.session && wbPanelData.session.model && wbPanelData.session.model.variant) || "default";
    html = `<option value="${escapeHtml(curPid + "\u0001" + curMid + "\u0001" + variant)}" selected>${escapeHtml(curMid)}（当前，未在列表）</option>` + html;
  }
  return html;
}
// 模型下拉 change：POST /api/session/{id}/model 切换并刷新面板。
async function wbModelChange(sel) {
  const v = sel.value;
  if (!v) return;
  const [providerID, modelID, variant] = v.split("\u0001");
  if (!providerID || !modelID) return;
  const dir = (wbPanelData && wbPanelData.session && wbPanelData.session.directory) || "";
  // 模型切换的 directory 以 query 参数传递（与 App switchSessionModelV2 一致）。
  const url = `/api/opencode/api/session/${encodeURIComponent(wbSelected)}/model` + (dir ? "?directory=" + encodeURIComponent(dir) : "");
  const body = JSON.stringify({ model: { id: modelID, providerID, variant: variant || "default" } });
  const res = await api(url, { method: "POST", headers: appHeaders(), body });
  if (!res.ok) {
    toast("切换失败", `模型切换未生效 (${res.status})`, "crit");
    // 重绘面板把下拉恢复到实际生效的模型（指纹包含 model，refreshWbPanel 会跟进）。
    wbRerenderPanel();
    loadWorkbench();
    return;
  }
  toast("已切换", `已切换到 ${modelID}`, "info");
  if (wbPanelData && wbPanelData.session) {
    wbPanelData.session.model = { id: modelID, providerID, variant: variant || "default" };
  }
  wbRerenderPanel();
  loadWorkbench();
}

// 归并子会话的忙/待决问题到父会话（口径与 App 工作台一致）。
function buildWbItems() {
  const childBusy = {};
  const parentQ = new Set();
  for (const s of wbSessions) {
    const pid = s.parentID;
    if (!pid) continue;
    if (wbPending[s.id] && wbPending[s.id].length) parentQ.add(pid);
    const st = wbStatuses[s.id];
    if (st && (st.type === "busy" || st.type === "retry")) childBusy[pid] = st.type;
  }
  const roots = wbSessions.filter(s => !s.parentID && !(s.time && s.time.archived));
  return roots.map(s => {
    const self = wbStatuses[s.id];
    const hasQ = (wbPending[s.id] && wbPending[s.id].length) || parentQ.has(s.id);
    const perms = wbPermissions[s.id] || [];
    let st;
    if (hasQ || (perms && perms.length)) st = "question";
    else if (self && (self.type === "busy" || self.type === "retry")) st = self.type;
    else st = childBusy[s.id] || (self && self.type) || "idle";
    return { session: s, status: st, pending: wbPending[s.id] || [], permissions: perms };
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
  const metaParts = [];
  if (s.time && s.time.updated) metaParts.push(new Date(s.time.updated).toLocaleTimeString());
  const q = (it.pending || []).length;
  const pc = (it.permissions || []).length;
  const active = wbSelected === id ? "active" : "";
  const wrap = (q ? `<span class="qbadge">提问 ${q}</span>` : "") +
    (pc ? `<span class="qbadge" title="待授权操作">授权 ${pc}</span>` : "");
  const newDot = wbNewSet.has(id) ? `<span class="newdot" title="有新消息"></span>` : "";
  return `<div class="wb-item ${active}" data-sid="${escapeHtml(id)}">
    <div class="t"><span class="dot ${escapeHtml(it.status)}"></span><span class="ttl">${escapeHtml(title)}</span>${newDot}${wrap}</div>
    <div class="dir">${escapeHtml(dir) || "-"}</div>
    ${metaParts.length ? `<div class="meta">${escapeHtml(metaParts.join(" · "))}</div>` : ""}
  </div>`;
}

function renderWbList() {
  wbItems = buildWbItems();
  refreshWbStats();
  const box = document.getElementById("wbList");
  const kw = (document.getElementById("wbFilter").value || "").trim().toLowerCase();
  const filter = wbFilterOption;
  // 内容指纹：状态/排序/关键字/选中项都没变就不重建 DOM，避免滚动条跳动；
  // 选中项必须参与比对，否则点选其他会话时高亮不会更新。
  const sig = filter + "\u0001" + kw + "\u0001" + (wbSelected || "") + "\u0001" + wbItems.map(it =>
    it.session.id + ":" + it.status + ":" + (it.pending || []).length + ":" + (it.session.time && it.session.time.updated || 0) + ":U" + (wbNewSet.has(it.session.id) ? 1 : 0)
  ).join(",");
  if (sig === wbListSig) return;
  wbListSig = sig;
  const scroller = box.closest(".wb-body") || box;
  const prevScroll = scroller.scrollTop || 0;
  let matched = wbItems.filter(it => {
    if (filter === "question" && wbRank(it.status) !== 0) return false;
    if (filter === "busy" && wbRank(it.status) !== 1) return false;
    if (filter === "idle" && wbRank(it.status) !== 2) return false;
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
      api("/api/opencode/experimental/session?roots=true", { headers: appHeaders() }),
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
    // 待授权操作同样按目录查询（App listPendingPermissions 同契约）。
    wbPermissions = {};
    const permResps = await Promise.all(dirs.map(d =>
      api("/api/opencode/permission?directory=" + encodeURIComponent(d), { headers: appHeaders() })
        .then(r => (r.ok ? r.json() : []))
        .catch(() => [])
    ));
    for (const ps of permResps) {
      for (const p of (Array.isArray(ps) ? ps : [])) {
        if (p && p.sessionID) (wbPermissions[p.sessionID] = wbPermissions[p.sessionID] || []).push(p);
      }
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
  if (type.includes("status") || type.includes("updated") || type.includes("created")) return "状态更新";
  return "事件更新";
}
// 从原始事件 payload 提取展示信息，兼容 v1.18 wrapper（{"payload":{...}}）与非 wrapper 形态。
function wbPayloadMeta(payload, eventType) {
  let obj = payload;
  if (!obj || typeof obj !== "object") obj = {};
  let inner = obj.properties || obj.data || (obj.payload && (obj.payload.properties || obj.payload.data)) || {};
  let type = obj.type || (obj.payload && obj.payload.type) || eventType || "";
  const sessionObj = inner.session || obj.session;
  const title = (sessionObj && sessionObj.title) || "";
  const file = wbFilePath(obj, inner);
  const directory = obj.directory || (sessionObj && sessionObj.directory) || (file ? file.substring(0, file.lastIndexOf("/")) : "");
  let summary = "";
  if (type === "session.status" || type === "session.updated") {
    const st = inner.status;
    const stt = st && typeof st === "object" ? st.type : st;
    summary = stt === "busy" ? "开始处理" : stt === "idle" ? "处理完成" : stt === "retry" ? "重试中" : stt === "error" ? "出错" : "";
  }
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
    if (tool && tool.type) summary = "正在执行工具：" + tool.type;
  }
  if (!summary && file) summary = "修改文件：" + file.split("/").pop();
  if (!summary) {
    const msg = inner.message || obj.message;
    if (msg && msg.content) {
      summary = (Array.isArray(msg.content) ? msg.content.map(c => (c && c.text) || "").join(" ") : String(msg.content)).trim().replace(/\s+/g, " ").slice(0, 50);
    }
  }
  if (!summary) {
    const part = inner.part || obj.part;
    if (part && part.text) summary = String(part.text).trim().replace(/\s+/g, " ").slice(0, 50);
  }
  if (!summary) summary = wbFallbackEvent(type);
  return { title, directory, file, summary, type };
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
// 事件点颜色：错误红 / 完成绿 / 提问黄 / 默认紫罗兰。
function wbEventDot(ev) {
  const t = ev.eventType || "";
  if (t.includes("error") || t.includes("failed")) return "busy";
  if (t === "message.complete" || t.includes("idle") || t.includes("finish")) return "idle";
  if (t.startsWith("question.") || t.startsWith("permission.")) return "question";
  return "idle";
}
// 把一条有意义的事件追加进动态历史（不按会话折叠，最多保留最近 WB_MAX_EVENTS 条）。
// 轮询带事件 id 用 id 去重；页面首次加载的历史事件按时间排，让动态栏更丰富。
function wbAppendEvent(ev) {
  if (!ev || !ev.sessionId) return;
  const item = wbEventItem(ev);
  const dot = wbEventDot(ev);
  const key = (ev.id !== undefined && ev.id !== null) ? "id:" + ev.id : "raw:" + ev.eventType + ":" + ev.sessionId + ":" + item.ts;
  if (wbEvents.some(x => x.key === key)) return;
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

/* ---- 决策面板 ---- */
function parseWbMessages(msgs) {
  const list = Array.isArray(msgs) ? msgs : [];
  // 最近 N 条有文本的消息，接口返回按时间从旧到新；收集时倒序遍历取「最新在前」。
  const newestFirst = [];
  for (let i = list.length - 1; i >= 0; i--) {
    const m = list[i];
    if (!m) continue;
    const text = wbMsgText(m);
    if (!text) continue;
    newestFirst.push({
      role: (m.info && m.info.role === "assistant") ? "assistant" : "user",
      text,
      ts: (m.info && m.info.time && m.info.time.created) || 0,
    });
    if (newestFirst.length >= WB_RECENT) break;
  }
  // 展示时再倒回来，保证聊天按时间从上往下（旧在上、新在下）。
  const newestAsstIdx = newestFirst.findIndex(x => x.role === "assistant");
  const chronological = newestFirst.slice().reverse();
  const newestAsstChronoIdx = newestAsstIdx >= 0 ? chronological.length - 1 - newestAsstIdx : -1;
  const recent = chronological.map((m, idx) => {
    const full = m.role === "assistant" && idx === newestAsstChronoIdx;
    return full
      ? { ...m, full: true, clipped: false }
      : { ...m, full: false, clipped: m.text.length > WB_MSG_CLIP, text: m.text.slice(0, WB_MSG_CLIP) };
  });
  return { recent };
}
function wbMsgText(m) {
  return (m.parts || []).filter(p => p && p.type === "text" && !p.synthetic && !p.ignored && p.text).map(p => p.text).join("\n").trim();
}
function openWbPanel(id) {
  const switched = wbSelected !== id;
  wbSelected = id;
  wbMarkRead(id);
  document.getElementById("wbPanelPlaceholder").classList.add("hidden");
  document.getElementById("wbPanel").classList.remove("hidden");
  if (switched) {
    // 切会话时先清空为加载态，避免显示上一个会话的内容。
    wbPanelData = { id, loading: true, recent: [], pending: [], permissions: [], status: "", session: null };
    // 切换会话时清掉上一会话的待决问题作答暂存。
    wbQAnswers = {};
    wbMoreMsgs = false;
    wbRenaming = false;
    wbCompacting = false;
    wbPanelScrollBottom = true;
    renderWbPanel();
  }
  renderWbList();
  refreshWbPanel(id, true);
}
function closeWbPanel() {
  wbSelected = null;
  wbPanelData = null;
  document.getElementById("wbPanel").classList.add("hidden");
  document.getElementById("wbPanel").innerHTML = "";
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
    let msgs = [];
    try {
      const res = await api(`/api/opencode/session/${encodeURIComponent(id)}/message?limit=${WB_PANEL_LIMIT}`, { headers: appHeaders() });
      if (res.ok) {
        const data = await res.json();
        msgs = Array.isArray(data) ? data : (data.messages || []);
      }
    } catch (e) { if (e.message === "unauthorized") return; }
    if (id !== wbSelected) return;
    if (!wbItems.length) wbItems = buildWbItems();
    const item = wbItems.find(x => x.session.id === id);
    const parsed = parseWbMessages(msgs);
    const newData = {
      id,
      recent: parsed.recent,
      pending: (item && item.pending) || [],
      permissions: (item && item.permissions) || [],
      status: item ? item.status : "idle",
      session: item ? item.session : null,
    };
    // 拉取后比较：内容没变化才跳过重绘。先拉后比才能发现「上游新增了消息」
    // （旧逻辑在拉取前用旧面板数据比较，新增消息永远无法触发刷新）。
    if (!force && wbPanelData && wbPanelData.id === id && wbPanelSigFor(wbPanelData) === wbPanelSigFor(newData)) return;
    // 用户在快捷回复输入框里打字时暂缓整块重绘，避免抢走焦点/中断输入；
    // 等输入结束（失焦）后下一次轮询再渲染新内容。强制刷新（如发送后）不受影响。
    if (!force) {
      const ta = document.getElementById("wbReplyInput");
      if (ta && document.activeElement === ta) return;
    }
    wbPanelData = newData;
    const snap = wbSnapshotPanel();
    renderWbPanel();
    wbRestorePanel(snap);
    // 模型下拉首次打开时后台加载 provider 列表，加载完仅重绘一次（保留输入）。
    if (wbProvidersState === "idle") {
      ensureWbProviders().then(() => {
        if (wbSelected && wbPanelData && wbPanelData.id === wbSelected) {
          const s2 = wbSnapshotPanel();
          renderWbPanel();
          wbRestorePanel(s2);
        }
      });
    }
  } finally {
    if (wbPanelFetching === id) wbPanelFetching = null;
  }
}
function renderWbPanel() {
  wbMoreOpen = false;
  const panel = document.getElementById("wbPanel");
  const d = wbPanelData;
  if (!d) { panel.innerHTML = ""; return; }
  if (d.loading || !d.session) {
    panel.innerHTML = `<div class="wb-placeholder">加载中…</div>`;
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
  if (model) metaBits.push(`模型 ${escapeHtml(model)}`);
  if (agent) metaBits.push(`Agent ${escapeHtml(agent)}`);
  if (s.id) metaBits.push(`<span class="mono" style="font-size:10px">${escapeHtml(String(s.id).slice(0, 18))}</span>`);
  if (s.time && s.time.created) metaBits.push(`创建 ${escapeHtml(new Date(s.time.created).toLocaleString())}`);
  if (s.time && s.time.updated) metaBits.push(escapeHtml(new Date(s.time.updated).toLocaleString()));
  const statusBadgeCls = status === "question" ? "question" : (status === "busy" || status === "retry") ? "busy" : "idle";
  const isBusyForAct = status === "busy" || status === "retry";
  const shareUrl = (s.share && s.share.url) || "";
  const qhtml = renderWbQuestions();
  const permHtml = renderWbPermissions(d.permissions || []);
  const total = d.recent.length;
  const visCount = wbMoreMsgs ? total : Math.min(WB_RECENT_SHOW, total);
  const hiddenCount = total - visCount;
  // 聊天旧→新（上→下），默认展示最新几条；更早的在顶部展开。
  const visMsgs = d.recent.slice(total - visCount);
  const msgs = visMsgs.map(m => {
    const t = wbTimeFormat(m.ts);
    return `<div class="wb-msg ${m.role}${m.clipped ? " clipped" : ""}"><div class="who">${m.role === "assistant" ? "AI" : "我"}</div>${escapeHtml(m.text)}${m.clipped ? " …（已截断）" : ""}${t ? `<div class="time">${t}</div>` : ""}</div>`;
  }).join("") ||
    `<div class="wb-placeholder" style="padding:16px">暂无对话内容</div>`;
  const moreBtn = hiddenCount > 0
    ? `<div class="wb-more"><button class="ghost sm" data-wb-more="1">加载更早的对话记录（还有 ${hiddenCount} 条）</button></div>`
    : "";
  panel.innerHTML = `
    <div class="wb-panel-head">
      <span class="dot ${escapeHtml(status)}" style="margin-top:7px"></span>
      <div class="t">${escapeHtml(title)}
        <div class="dir">${escapeHtml(dir) || "-"}</div>
        ${metaBits.length ? `<div class="meta">${metaBits.join(" · ")}</div>` : ""}
      </div>
      <div style="display:flex;flex-direction:column;gap:6px;align-items:flex-end;flex-shrink:0">
        <span class="badge ${statusBadgeCls}">${wbStatusLabel(status)}</span>
        <div style="display:flex;gap:6px;align-items:center">
          <button class="ghost sm" data-wb-rename="1" title="修改会话标题">改名</button>
          <button class="ghost sm" data-wb-compact="1" title="压缩会话：把历史对话汇总为摘要以释放上下文（耗时较长）" ${wbCompacting ? "disabled" : ""}>${wbCompacting ? "压缩中…" : "压缩"}</button>
          <button class="danger sm" data-wb-del="${escapeHtml(d.id)}">删除</button>
          <div class="wb-more-wrap">
            <button class="ghost sm" data-wb-morebtn="1" title="更多操作（同 App 聊天详情菜单）">更多 ▾</button>
            <div class="wb-more-menu hidden" data-wb-more-menu>
              <button data-wb-action="reload">重新加载</button>
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
    </div>
    ${wbRenaming ? `<div class="wb-rename-row">
      <input id="wbRenameInput" maxlength="120" value="${escapeHtml(title)}">
      <button class="sm" data-wb-rensave="1">保存</button>
      <button class="ghost sm" data-wb-rencancel="1">取消</button>
      <div id="wbRenameMsg" class="msg"></div>
    </div>` : ""}
    <div class="wb-scroll">
      <div class="wb-model-row"><span class="lbl">模型</span>
        <select id="wbModelSelect" onchange="wbModelChange(this)">${wbModelOptions()}</select>
      </div>
            ${permHtml ? `<div class="wb-block"><h4>待授权操作<span class="info">${(d.permissions || []).length} 项</span></h4>${permHtml}</div>` : ""}
      ${qhtml ? `<div class="wb-block"><h4>待决问题<span class="info">${(d.pending || []).reduce((n, q) => n + (q.questions || []).length, 0)} 个</span></h4>${qhtml}</div>` : ""}
      <div class="wb-block wb-msg-block"><h4>最近对话<span class="info">${wbMoreMsgs ? `全部 ${d.recent.length} 条` : `最新 ${visCount} / ${d.recent.length} 条`}</span></h4>
        ${moreBtn}
        <div class="wb-msgwrap">${msgs}</div>
      </div>
      <div class="wb-block">
        <h4>快捷回复</h4>
        <div class="wb-reply">
          <textarea id="wbReplyInput" placeholder="给该会话发送指令…（Enter 发送，Shift+Enter 换行）"></textarea>
          <button class="send" id="wbSendBtn" onclick="sendWbReply()">发送</button>
        </div>
      </div>
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
    const more = e.target.closest("[data-wb-more]");
    if (more) {
      // 更早的消息插入在顶部：记录旧滚动锚点，展开后视角保持在原位置。
      const mw = document.querySelector("#wbPanel .wb-msgwrap");
      const anchor = mw ? { top: mw.scrollTop, oldH: mw.scrollHeight } : null;
      wbMoreMsgs = true;
      wbRerenderPanel();
      if (anchor) {
        const mw2 = document.querySelector("#wbPanel .wb-msgwrap");
        if (mw2) mw2.scrollTop = anchor.top + (mw2.scrollHeight - anchor.oldH);
      }
      return;
    }
    const opt = e.target.closest("[data-qopt]");
    if (opt) { wbQToggle(opt.dataset.qopt, Number(opt.dataset.qidx), opt.dataset.label); return; }
    const rej = e.target.closest("[data-qreject]");
    if (rej) { wbRejectQ(rej.dataset.qreject); return; }
    const sub = e.target.closest("[data-qsubmit]");
    if (sub) { wbQSubmit(sub.dataset.qsubmit); return; }
    const cus = e.target.closest("[data-qcustom]");
    if (cus) { wbQCustom(cus.dataset.qcustom, Number(cus.dataset.qidx)); return; }
  };
  const ta = document.getElementById("wbReplyInput");
  if (ta) ta.addEventListener("keydown", (ev) => {
    // isComposing 判断：中文输入法选词时按 Enter 不应发送。
    if (ev.key === "Enter" && !ev.shiftKey && !ev.isComposing) { ev.preventDefault(); sendWbReply(); }
  });
  if (ta) ta.addEventListener("input", () => wbAutosizeReply(ta));
  if (ta) wbAutosizeReply(ta);
  const msel = document.getElementById("wbModelSelect");
  if (msel && s.model) {
    const want = (s.model.providerID || "") + "\u0001" + (s.model.id || "") + "\u0001" + (s.model.variant || "default");
    if (want !== "\u0001\u0001") msel.value = want;
  }
  wbPanelSig = wbPanelSigFor(wbPanelData);
}
// 渲染待决问题。一个 request 可含多个 question，answers 数组须按序一一对应，
// 因此把同一 request 的问题聚合成一组，逐题选择后一次性提交全部作答。
// 点选项只做本地暂存/高亮，绝不立即提交（避免误触导致 request 提前被消费）。
// 渲染待授权操作。每条权限显示 permission 类型 + patterns，提供 允许一次 /
// 始终允许 / 拒绝 三个动作（与 App replyToPermission 契约一致）。
function renderWbPermissions(perms) {
  return (perms || []).map(p => {
    const patterns = (p.patterns || []).join("、") || "-";
    const tool = (p.tool && (p.tool.type || p.tool.id || p.tool.name)) ? ` · ${escapeHtml(p.tool.type || p.tool.id || p.tool.name)}` : "";
    const always = (p.always || []).join("、");
    return `<div class="wb-qcard" data-wb-permcard>
      <div class="qhead">待授权 · ${escapeHtml(p.permission || "操作")}${tool}<span class="info" style="float:right;text-transform:none">${escapeHtml(p.id || "")}</span></div>
      <div class="q">${escapeHtml(patterns)}</div>
      ${always ? `<div class="muted" style="font-size:11px;margin-top:4px">已始终允许：${escapeHtml(always)}</div>` : ""}
      <div class="wb-qopts">
        <button data-wb-perm="${escapeHtml(p.id)}" data-reply="once">允许一次</button>
        <button data-wb-perm="${escapeHtml(p.id)}" data-reply="always">始终允许</button>
        <button class="reject" data-wb-perm="${escapeHtml(p.id)}" data-reply="reject">拒绝</button>
      </div>
    </div>`;
  }).join("");
}
// 处理待授权操作：allowed once / always / reject。
async function wbPermReply(reqId, reply) {
  const item = wbItems.find(x => x.session.id === wbSelected);
  const dir = (item && item.session && item.session.directory) || "";
  const url = `/api/opencode/permission/${encodeURIComponent(reqId)}/reply` + (dir ? "?directory=" + encodeURIComponent(dir) : "");
  const res = await api(url, { method: "POST", headers: appHeaders(), body: JSON.stringify({ reply }) });
  if (!res.ok) { toast("授权失败", "权限回复未送达 (" + res.status + ")", "crit"); return; }
  toast("已处理", reply === "reject" ? "已拒绝该操作" : (reply === "always" ? "已始终允许" : "已允许一次"), "info");
  if (wbPanelData && wbPanelData.permissions) {
    wbPanelData.permissions = wbPanelData.permissions.filter(p => p.id !== reqId);
  }
  wbRerenderPanel();
  loadWorkbench();
}

function renderWbQuestions() {
  const pending = wbPanelData.pending || [];
  // 清理已不在待决列表里的 request 的暂存作答，避免旧选择残留。
  const valid = new Set(pending.map(x => x.id));
  Object.keys(wbQAnswers).forEach(k => { if (!valid.has(k)) delete wbQAnswers[k]; });
  return pending.map(q => {
    const qs = q.questions || [];
    if (!qs.length) return "";
    const sel = (wbQAnswers[q.id] = wbQAnswers[q.id] || qs.map(() => []));
    const cards = qs.map((qu, qi) => {
      const opts = (qu.options || []).map(o => {
        const active = (sel[qi] || []).includes(o.label);
        return `<button class="wb-qopt${active ? " active" : ""}" data-qopt="${escapeHtml(q.id)}" data-qidx="${qi}" data-label="${escapeHtml(o.label)}">${escapeHtml(o.label)}${o.description ? `<br><span class="muted" style="font-size:11px;font-weight:400">${escapeHtml(o.description)}</span>` : ""}</button>`;
      }).join("");
      const custom = qu.custom !== false
        ? `<div class="wb-qcustom"><input id="wbQInput_${escapeHtml(q.id)}_${qi}" placeholder="自定义回答（可选）"><button class="ghost sm" data-qcustom="${escapeHtml(q.id)}" data-qidx="${qi}">填入</button></div>`
        : "";
      return `<div class="wb-qcard"><div class="qhead">${escapeHtml(qu.header || "AI 提问")}${qs.length > 1 ? ` <span class="muted" style="font-weight:400">${qi + 1}/${qs.length}</span>` : ""}</div><div class="q">${escapeHtml(qu.question)}</div>
        <div class="wb-qopts">${opts}</div>${custom}</div>`;
    }).join("");
    const answered = sel.filter(a => a && a.length).length;
    const footer = `<div class="wb-qsubmit"><button class="sm" data-qsubmit="${escapeHtml(q.id)}" ${answered ? "" : "disabled"} title="已作答 ${answered}/${qs.length}">提交回答${qs.length > 1 ? `（${answered}/${qs.length}）` : ""}</button><button class="reject" data-qreject="${escapeHtml(q.id)}">拒绝</button></div>`;
    return `<div class="wb-qgroup">${cards}${footer}</div>`;
  }).join("");
}
// 点击问题选项：单选/多选切换本地暂存（不会立即提交）。
function wbQToggle(reqId, qidx, label) {
  const q = (wbPanelData.pending || []).find(x => x.id === reqId);
  if (!q) return;
  const qu = (q.questions || [])[qidx];
  if (!qu) return;
  const sel = (wbQAnswers[reqId] = wbQAnswers[reqId] || (q.questions || []).map(() => []));
  const cur = sel[qidx] || [];
  if (qu.multiple === true) {
    sel[qidx] = cur.includes(label) ? cur.filter(x => x !== label) : [...cur, label];
  } else {
    sel[qidx] = [label];
  }
  wbRerenderPanel();
}
// 自定义回答「填入」：仅暂存，随「提交回答」一起发出。
async function wbQCustom(reqId, qidx) {
  const input = document.getElementById("wbQInput_" + reqId + "_" + qidx);
  const v = input ? input.value.trim() : "";
  if (!v) { toast("答复失败", "请先输入回答内容"); return; }
  const q = (wbPanelData.pending || []).find(x => x.id === reqId);
  if (!q) return;
  const qs = q.questions || [];
  const sel = (wbQAnswers[reqId] = wbQAnswers[reqId] || qs.map(() => []));
  sel[qidx] = [v];
  wbRerenderPanel();
}
async function wbQSubmit(reqId) {
  const q = (wbPanelData.pending || []).find(x => x.id === reqId);
  if (!q) return;
  const qs = q.questions || [];
  const sel = (wbQAnswers[reqId] = wbQAnswers[reqId] || qs.map(() => []));
  // answers 数组长度必须与 questions 一致（未作答的给空数组）。
  const answers = qs.map((_, i) => (sel[i] || []).slice());
  if (!answers.some(a => a.length)) { toast("提交失败", "请至少回答一个问题"); return; }
  await postWbAnswer(reqId, answers);
}
async function postWbAnswer(reqId, answers) {
  const dir = (wbPanelData && wbPanelData.session && wbPanelData.session.directory) || "";
  // 与 App 一致：question reply 的目录以 query 参数传递（x-starburst-directory 头只对 prompt_async 生效）。
  const url = `/api/opencode/question/${encodeURIComponent(reqId)}/reply` + (dir ? "?directory=" + encodeURIComponent(dir) : "");
  const res = await api(url, { method: "POST", headers: appHeaders(), body: JSON.stringify({ answers }) });
  if (!res.ok) { toast("答复失败", "问题回复未送达 (" + res.status + ")", "crit"); return; }
  toast("已答复", "问题已回复", "info");
  wbPanelData.pending = wbPanelData.pending.filter(x => x.id !== reqId);
  delete wbQAnswers[reqId];
  wbRerenderPanel();
  loadWorkbench();
}
async function wbRejectQ(reqId) {
  const dir = (wbPanelData && wbPanelData.session && wbPanelData.session.directory) || "";
  const url = `/api/opencode/question/${encodeURIComponent(reqId)}/reject` + (dir ? "?directory=" + encodeURIComponent(dir) : "");
  const res = await api(url, { method: "POST", headers: appHeaders() });
  if (!res.ok) { toast("拒绝失败", "拒绝未送达 (" + res.status + ")", "crit"); return; }
  toast("已拒绝", "问题已拒绝", "info");
  wbPanelData.pending = wbPanelData.pending.filter(q => q.id !== reqId);
  delete wbQAnswers[reqId];
  wbRerenderPanel();
  loadWorkbench();
}
async function sendWbReply() {
  const ta = document.getElementById("wbReplyInput");
  const text = ta ? ta.value.trim() : "";
  if (!text || wbSending || !wbSelected) return;
  wbSending = true;
  const btn = document.getElementById("wbSendBtn");
  if (btn) btn.disabled = true;
  const item = wbItems.find(x => x.session.id === wbSelected);
  const dir = (item && item.session && item.session.directory) || "";
  const body = {
    messageId: "wb-" + Date.now().toString(36) + Math.random().toString(36).slice(2, 8),
    parts: [{ type: "text", text }],
  };
  const headers = appHeaders();
  if (dir) headers["x-starburst-directory"] = dir;
  try {
    const res = await api(`/api/opencode/session/${encodeURIComponent(wbSelected)}/prompt_async`, { method: "POST", headers, body: JSON.stringify(body) });
    if (!res.ok) { toast("发送失败", "快捷回复未送达 (" + res.status + ")", "crit"); return; }
    if (ta) ta.value = "";
    // 乐观更新状态：直接写 wbStatuses（列表/面板都从它派生），
    // 避免被 renderWbList 的重建丢弃，让「处理中」立即生效直到 WS 事件到达。
    wbStatuses[wbSelected] = { type: "busy" };
    renderWbList();
    wbPanelScrollBottom = true;
    refreshWbPanel(wbSelected, true);
  } catch (e) {
    if (e.message !== "unauthorized") toast("发送失败", e.message || "未知错误", "crit");
  } finally {
    wbSending = false;
    if (btn) btn.disabled = false;
  }
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
    refreshWbPanel(id, true);
  } catch (e) {
    if (e.message !== "unauthorized") toast("压缩失败", e.message || "未知错误", "crit");
  } finally {
    wbCompacting = false;
    if (wbSelected === id) {
      const snap = wbSnapshotPanel();
      renderWbPanel();
      wbRestorePanel(snap);
    }
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
  if (action === "reload") { refreshWbPanel(id, true); toast("已重新加载", "会话信息已刷新", "info"); return; }
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
  refreshWbPanel(id, true);
  loadWorkbench();
}
async function wbRedoWbSession(id) {
  const dir = wbCurrentDir();
  const res = await api(`/api/opencode/session/${encodeURIComponent(id)}/unrevert`, { method: "POST", headers: wbDirHeaders(dir) });
  if (!res.ok) { toast("重做失败", "unrevert (" + res.status + ")", "crit"); return; }
  toast("已重做", "已恢复上一条撤销", "info");
  refreshWbPanel(id, true);
  loadWorkbench();
}
// 运行服务器端命令（如 /review），与 App executeCommand 契约一致。
async function wbRunWbCommand(id, command) {
  const dir = wbCurrentDir();
  const res = await api(`/api/opencode/session/${encodeURIComponent(id)}/command`, { method: "POST", headers: wbDirHeaders(dir), body: JSON.stringify({ command, arguments: "" }) });
  if (!res.ok) { toast("命令失败", `/${command} 未生效 (${res.status})`, "crit"); return; }
  toast(`已运行 /${command}`, "命令已下发到会话", "info");
  wbStatuses[id] = { type: "busy" };
  renderWbList();
  wbPanelScrollBottom = true;
  refreshWbPanel(id, true);
}
async function wbAbortWbSession(id) {
  const dir = wbCurrentDir();
  const res = await api(`/api/opencode/session/${encodeURIComponent(id)}/abort`, { method: "POST", headers: wbDirHeaders(dir) });
  if (!res.ok) { toast("停止失败", "abort (" + res.status + ")", "crit"); return; }
  toast("已停止", "已发送停止信号", "info");
  wbStatuses[id] = { type: "idle" };
  renderWbList();
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
      return `<details class="wb-diff-item" ${i === 0 ? "open" : ""}>
        <summary><b>${escapeHtml(file)}</b><span class="meta"><span class="add">+${adds}</span> <span class="del">-${dels}</span> <span class="badge">${escapeHtml(df.status || "modified")}</span></span></summary>
        <div class="wb-diff-body">
          <div class="col before"><h5>改动前</h5><pre>${escapeHtml(before) || "-"}</pre></div>
          <div class="col after"><h5>改动后</h5><pre>${escapeHtml(after) || "-"}</pre></div>
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
        const st = state.type || "completed";
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
  document.getElementById("wbModalBody").innerHTML = html;
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
let intelEndpointsCache = [];
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
    features: loadIntelFeatures, cases: loadIntelCases, findings: loadIntelFindings,
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
    if (p.analyzedAt) analyzed++;
    const isAnalyzed = !!p.analyzedAt;
    tb.insertAdjacentHTML("beforeend", `<tr>
      <td class="clip" title="${escapeHtml(p.name)}"><strong>${escapeHtml(p.name)}</strong></td>
      <td><span class="badge">${escapeHtml(p.source)}</span></td>
      <td class="mono clip muted" title="${escapeHtml(loc)}">${escapeHtml(loc || "-")}</td>
      <td><span class="intel-status ${isAnalyzed ? "analyzed" : "pending"}">${isAnalyzed ? "已分析" : "待分析"}</span></td>
      <td>
        <button class="ghost sm" data-id="${p.id}">详情</button>
        <button class="ghost sm" data-analyze="${p.id}">分析</button>
        <button class="tertiary sm" data-del="${p.id}">删除</button>
      </td>
    </tr>`);
  }
  tb.onclick = (e) => {
    const btn = e.target.closest("button[data-id]");
    if (btn) { openIntelDetail(Number(btn.dataset.id)); return; }
    const az = e.target.closest("button[data-analyze]");
    if (az) { runIntelAnalyze(Number(az.dataset.analyze)); return; }
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
  };
  if (src === "local" && !body.localPath) { show(document.getElementById("intelMsg"), "请填写本地路径"); return; }
  if (src === "git" && !body.gitUrl) { show(document.getElementById("intelMsg"), "请填写 Git URL"); return; }
  const res = await api("/api/intel/projects", { method: "POST", headers: appHeaders(), body: JSON.stringify(body) });
  const data = await res.json();
  if (!res.ok) { show(document.getElementById("intelMsg"), data.error || "添加失败"); return; }
  show(document.getElementById("intelMsg"), "已添加：" + escapeHtml(data.name));
  document.getElementById("intelMsg").classList.add("ok");
  document.getElementById("intelName").value = "";
  document.getElementById("intelPath").value = "";
  document.getElementById("intelGitRef").value = "";
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
  document.getElementById("intelDetailName").textContent = p.name || "";
  const loc = p.source === "git" ? p.gitUrl : p.localPath;
  document.getElementById("intelDetailMeta").textContent =
    `来源：${p.source || "-"}  ·  路径：${loc || "-"}  ·  最近分析：${p.analyzedAt ? new Date(p.analyzedAt).toLocaleString() : "未分析"}`;

  const mtb = document.querySelector("#intelModuleTable tbody");
  mtb.innerHTML = "";
  for (const m of data.modules || []) {
    mtb.insertAdjacentHTML("beforeend", `<tr>
      <td class="mono clip" title="${escapeHtml(m.relPath)}">${escapeHtml(m.relPath || ".")}</td>
      <td><span class="badge type-badge">${escapeHtml(INTEL_TYPE_LABELS[m.kindType] || m.kindType || "-")}</span></td>
      <td>${escapeHtml(m.kindRole || "-")}</td>
      <td>${escapeHtml(m.buildTool || "-")}</td>
      <td class="muted clip" title="${escapeHtml(m.commandsJson || "")}" style="font-size:11px">${escapeHtml((m.commandsJson || "[]").slice(0, 40))}</td>
      <td><button class="ghost sm" onclick="editIntelModuleCommands(${m.id}, '${escapeHtml(m.commandsJson || "[]")}')">命令</button></td>
    </tr>`);
  }
  if (!(data.modules || []).length) mtb.insertAdjacentHTML("beforeend", `<tr><td colspan="6" class="muted" style="text-align:center;padding:16px">暂无子模块（分析后自动识别）</td></tr>`);
  document.getElementById("intelDetailStatModules").textContent = (data.modules || []).length;
  await loadIntelContracts(id);
  initIntelChats();
}

function editIntelModuleCommands(moduleId, commandsJson) {
  const existing = prompt("每行一条命令（命令白名单，运行测试时将按此执行）", (() => {
    try { return JSON.parse(commandsJson).join("\n"); } catch (_) { return commandsJson.replace(/"/g, "").slice(1, -1); }
  })());
  if (existing === null) return;
  const list = existing.split("\n").map(s => s.trim()).filter(s => s);
  api("/api/intel/modules/" + moduleId + "/commands", { method: "PUT", headers: appHeaders(), body: JSON.stringify({ commands: list }) })
    .then(r => r.json())
    .then(d => { if (d.error) alert(d.error); else if (intelCurrentProject) loadIntelDetail(intelCurrentProject); });
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
      <td><button class="ghost sm" onclick="openIntelMock(${idx})">模拟</button></td>
    </tr>`);
  });
  if (!eps.length) etb.insertAdjacentHTML("beforeend", `<tr><td colspan="7" class="muted" style="text-align:center;padding:16px">暂无接口契约（分析后自动提取）</td></tr>`);

  // 实体按表分组：每张表一个卡片，列出全部字段（字段名/类型/主键/可空）
  renderIntelEntities(ents);
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
  return name + ":" + (line || 0);
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
  for (const r of runs) {
    const dur = r.finishedAt && r.startedAt ? Math.round((new Date(r.finishedAt) - new Date(r.startedAt)) / 1000) + "s" : "-";
    tb.insertAdjacentHTML("beforeend", `<tr>
      <td>#${r.id}</td>
      <td>${escapeHtml(r.scope || "module")}</td>
      <td class="mono clip" title="${escapeHtml(r.command || "")}">${escapeHtml(r.command || "-")}</td>
      <td><span class="intel-status ${r.status === "passed" ? "analyzed" : ""}">${escapeHtml(r.status || "-")}</span></td>
      <td class="muted" style="font-size:12px">${r.startedAt ? new Date(r.startedAt).toLocaleString() : "-"}</td>
      <td>${dur}</td>
      <td><button class="ghost sm" onclick="showIntelRunDetail(${r.id})">详情</button></td>
    </tr>`);
  }
  if (!runs.length) tb.insertAdjacentHTML("beforeend", `<tr><td colspan="7" class="muted" style="text-align:center;padding:16px">暂无运行记录（点击右上角「运行测试」触发）</td></tr>`);
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
  box.innerHTML = `<div class="card" style="margin:0">
    <div class="row" style="justify-content:space-between">
      <strong>运行 #${run.id || ""}</strong>
      <span class="intel-status ${run.status === "passed" ? "analyzed" : ""}">${escapeHtml(run.status || "-")}</span>
    </div>
    <div class="mono muted" style="font-size:11px;margin-top:4px">${escapeHtml(run.command || "")}</div>
    <div class="table-wrap" style="margin-top:8px">
      <table><thead><tr><th>用例</th><th>结果</th><th>归因</th></tr></thead><tbody>${rows || `<tr><td colspan="3" class="muted" style="text-align:center;padding:8px">无结果</td></tr>`}</tbody></table>
    </div>
  </div>`;
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

async function runIntelTests() {
  const id = intelCurrentProject;
  if (!id) return;
  const st = document.getElementById("intelAnalyzeStatus");
  if (st) st.textContent = "运行测试中…";
  const res = await api("/api/intel/run", { method: "POST", headers: appHeaders(), body: JSON.stringify({ projectId: id }) });
  const data = await res.json();
  if (!res.ok) { if (st) st.textContent = ""; show(document.getElementById("intelMsg"), data.error || "运行失败"); return; }
  if (st) st.textContent = "运行完成：" + (data.run ? data.run.status : "");
  loadIntelRuns(id);
}

async function runIntelTestsAll() {
  const id = intelCurrentProject;
  if (!id) return;
  if (!confirm("一键回归：顺序运行项目全部模块的测试命令，继续？")) return;
  const st = document.getElementById("intelAnalyzeStatus");
  if (st) st.textContent = "一键回归中…";
  const res = await api("/api/intel/run-all", { method: "POST", headers: appHeaders(), body: JSON.stringify({ projectId: id }) });
  const data = await res.json();
  if (!res.ok) { if (st) st.textContent = ""; show(document.getElementById("intelMsg"), data.error || "回归失败"); return; }
  if (st) st.textContent = `回归完成：${data.passed || 0} 通 / ${(data.failed || 0)} 败（共 ${data.total || 0} 模块）`;
  show(document.getElementById("intelMsg"), `一键回归：${data.passed || 0} 模块通过 / ${(data.failed || 0)} 失败`);
  loadIntelRuns(id);
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
  show(document.getElementById("ragMsg"), "索引完成，共 " + (data.chunks || 0) + " 条（含契约与文档）", true);
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
    `<div class="rag-empty">向知识库提问，支持连续追问<br><span>基于已索引的项目契约与代码</span></div>`;
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
    `<div class="rag-msg ${isUser ? "user" : "assistant"}"><div class="rag-msg-label">${label}</div><div class="rag-bubble">${escapeHtml(content)}${sourcesHtml}</div></div>`);
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
renderAuth();
connectTaskWS();