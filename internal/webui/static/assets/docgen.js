// Web 文档生成（/docgen.html）：类型/模板 + 描述 → /api/documents/generate →
// 下载/预览/重新生成/删除；可选同步进知识库。鉴权复用主控制台 web 会话。
// CSP 禁内联脚本/事件，所有交互统一 addEventListener。
(function () {
  "use strict";
  var SESSION_KEY = "ocb_web_session";

  function session() { return localStorage.getItem(SESSION_KEY) || ""; }

  function authHeaders(extra) {
    var h = Object.assign({}, extra || {});
    var s = session();
    if (s) h["X-Web-Session"] = s;
    return h;
  }

  var authed = !!session();
  function setAuthed(v) {
    authed = v;
    document.getElementById("authMsg").style.display = v ? "none" : "block";
    document.getElementById("genForm").style.display = v ? "" : "none";
  }

  function esc(s) {
    return String(s == null ? "" : s).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }
  function fmtBytes(n) {
    n = Number(n) || 0;
    if (n > 1048576) return (n / 1048576).toFixed(1) + " MB";
    if (n > 1024) return (n / 1024).toFixed(0) + " KB";
    return n + " B";
  }

  async function docReq(path, opts) {
    var o = opts || {};
    o.headers = authHeaders(o.headers);
    try {
      var res = await fetch("/api/documents" + path, o);
      if (res.status === 401) { setAuthed(false); throw new Error("unauthorized"); }
      if (!res.ok) {
        var detail = "";
        try { detail = (await res.json()).error || ""; } catch (e) {}
        throw new Error("HTTP " + res.status + (detail ? ": " + detail : ""));
      }
      return res.json();
    } catch (e) { if (e.message === "unauthorized") throw e; throw new Error(e.message); }
  }

  // 反向入库目标集合下拉：选中后生成产物自动进入知识库。
  function populateDocKbCollections() {
    if (!authed) return;
    var sel = document.getElementById("docKbCollection");
    if (!sel) return;
    fetch("/api/kb/collections", { headers: authHeaders() }).then(function (res) {
      if (res.status === 401) { setAuthed(false); return; }
      return res.json();
    }).then(function (data) {
      if (!data) return;
      var cols = data.collections || [];
      sel.innerHTML = '<option value="">不同步知识库</option>' +
        cols.map(function (c) {
          return '<option value="' + c.id + '">' + esc(c.name) + "</option>";
        }).join("");
    }).catch(function () {});
  }

  /* ---------- 内置模板库（选中即填充类型与需求描述） ---------- */
  var DOC_TEMPLATES = [
    { name: "产品发布会 PPT", type: "pptx", prompt: "做一份 8 页产品发布会 PPT：封面、痛点、产品亮点（含参数对比表）、现场演示流程、定价、路线图、团队、结尾。亮点页放一张柱状图。" },
    { name: "周报 PPT", type: "pptx", prompt: "做一份 5 页周报 PPT：本周重点工作、目标达成（含一张条形图）、风险与阻塞、下周计划。" },
    { name: "立项汇报 PPT", type: "pptx", prompt: "做一份 6 页立项汇报 PPT：背景与问题、目标、方案对比（表格）、里程碑、资源需求、风险与应对。" },
    { name: "商务报价单 xlsx", type: "xlsx", prompt: "做一份商务报价单，两个工作表：报价明细（产品/单价/数量/小计，最终有合计行）和收款计划。" },
    { name: "销售漏斗 xlsx", type: "xlsx", prompt: "做一份季度销售漏斗表，一个工作表含：阶段/客户数/转化率列，末尾带一张漏斗图（饼图），并填示例数据。" },
    { name: "需求排期 xlsx", type: "xlsx", prompt: "做一份项目需求排期表：需求/负责人/优先级/预估工时/排期/状态列，填 6 条示例需求。" },
    { name: "会议纪要 docx", type: "docx", prompt: "写一份会议纪要：会议信息、议题与结论（含一张表格：议题/结论/负责人/截止时间）、行动项列表。" },
    { name: "周报 docx", type: "docx", prompt: "写一份周报：本周主要工作（分点）、数据概览（一张 2 列表格：指标/数值）、下周计划、需要支持的请求。" },
    { name: "业务汇报 docx", type: "docx", prompt: "写一份业务季度汇报：执行摘要、核心指标（表格：指标/数值/环比）、进展、挑战与风险、下季度目标。" },
  ];

  function populateDocTemplates() {
    var sel = document.getElementById("docTemplate");
    if (!sel) return;
    sel.innerHTML = '<option value="">内置模板…</option>' +
      DOC_TEMPLATES.map(function (t, i) {
        return '<option value="' + i + '">' + esc(t.name) + "</option>";
      }).join("");
    sel.addEventListener("change", function () {
      var t = DOC_TEMPLATES[Number(sel.value)];
      if (!t) return;
      document.getElementById("docType").value = t.type;
      document.getElementById("docPrompt").value = t.prompt;
    });
  }

  var docResultsBox = document.getElementById("docResults");
  var docHistoryBox = document.getElementById("docHistory");

  function currentKbCollectionID() {
    var sel = document.getElementById("docKbCollection");
    if (!sel) return 0;
    var v = Number(sel.value);
    return v > 0 ? v : 0;
  }

  document.getElementById("btnGenerate").addEventListener("click", generateDoc);
  document.getElementById("docPrompt").addEventListener("keydown", function (e) {
    if (e.key === "Enter") generateDoc();
  });

  function generateDoc() {
    var type = document.getElementById("docType").value;
    var prompt = document.getElementById("docPrompt").value.trim();
    if (!prompt) { alert("请描述要生成的文档"); return; }
    var kbID = currentKbCollectionID();
    var btn = document.getElementById("btnGenerate");
    btn.disabled = true; btn.textContent = "生成中（LLM 出骨架 + 渲染，约 10-60s）…";
    docReq("/generate", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ type: type, prompt: prompt, kbCollectionId: kbID, kbCollectionIds: kbID ? [kbID] : [] })
    }).then(function (g) {
      docResultsBox.innerHTML = renderDocCard(g);
      bindDocGenActions(docResultsBox);
      loadDocHistory();
    }).catch(function (e) {
      docResultsBox.innerHTML = '<p class="hint" style="color:var(--err)">生成失败：' + esc(e.message) + "</p>";
    }).finally(function () { btn.disabled = false; btn.textContent = "生成"; });
  }

  function renderDocCard(g) {
    var origin = window.location.origin;
    var src = encodeURIComponent(origin + g.downloadUrl);
    return '<div class="kb-card"><h3>' + esc(g.name) + "</h3>" +
      '<span class="stat">' + esc(g.docType) + " · " + fmtBytes(g.sizeBytes || 0) +
      (g.kbIngested ? " · 已同步进知识库" : "") + "</span>" +
      '<div class="ops"><a class="btn xs" href="' + origin + g.downloadUrl + '" download>下载</a>' +
      '<a class="btn xs" href="/doc/preview.html?src=' + src + '" target="_blank" rel="noopener">预览</a>' +
      '<button class="ghost xs" type="button" data-regen="' + g.id + '" data-name="' + esc(g.name) + '">重新生成</button>' +
      '<button class="ghost xs danger" type="button" data-del-docgen="' + g.id + '" data-name="' + esc(g.name) + '">删除</button></div></div>';
  }

  // 重新生成：留空指令 = 原需求重跑；填指令 = 按意见修改。
  function regenerateDoc(id, name) {
    var instruction = prompt("按意见修改（留空 = 原需求重新生成）：\n当前文档：" + name, "");
    if (instruction === null) return;
    var kbID = currentKbCollectionID();
    docReq("/regenerate", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ docId: id, instruction: instruction, kbCollectionId: kbID, kbCollectionIds: kbID ? [kbID] : [] })
    }).then(function (g) {
      docResultsBox.innerHTML = renderDocCard(g);
      bindDocGenActions(docResultsBox);
      loadDocHistory();
    }).catch(function (e) {
      docResultsBox.innerHTML = '<p class="hint" style="color:var(--err)">重新生成失败：' + esc(e.message) + "</p>";
    });
  }

  function deleteGeneratedDoc(id, name) {
    if (!confirm("删除生成的文档「" + name + "」及其产物文件？")) return;
    docReq("/" + id, { method: "DELETE" }).then(function () { loadDocHistory(); })
      .catch(function (e) { alert("删除失败：" + e.message); });
  }

  function bindDocGenActions(root) {
    if (!root) return;
    root.querySelectorAll("[data-regen]").forEach(function (b) {
      b.addEventListener("click", function () { regenerateDoc(Number(b.dataset.regen), b.dataset.name); });
    });
    root.querySelectorAll("[data-del-docgen]").forEach(function (b) {
      b.addEventListener("click", function () { deleteGeneratedDoc(Number(b.dataset.delDocgen), b.dataset.name); });
    });
  }

  function loadDocHistory() {
    if (!authed) return;
    docReq("/?limit=8").then(function (data) {
      var docs = data.documents || [];
      if (!docs.length) { docHistoryBox.innerHTML = ""; return; }
      docHistoryBox.innerHTML = '<h3 style="font-size:13px;margin:0 0 6px">最近生成</h3>' +
        docs.map(function (d) {
          var origin = window.location.origin;
          var src = encodeURIComponent(origin + "/api/documents/" + d.id + "/download");
          return '<div class="kb-row"><span class="name">' + esc(d.name) + "</span>" +
            '<span class="meta">' + esc(d.docType) + " · " + fmtBytes(d.sizeBytes) + "</span>" +
            '<button class="ghost xs" type="button" data-regen="' + d.id + '" data-name="' + esc(d.name) + '">重新生成</button>' +
            '<a class="ghost xs" href="/doc/preview.html?src=' + src + '" target="_blank" rel="noopener">预览</a>' +
            '<a class="ghost xs" href="' + origin + "/api/documents/" + d.id + '/download" download>下载</a>' +
            '<button class="ghost xs danger" type="button" data-del-docgen="' + d.id + '" data-name="' + esc(d.name) + '">删除</button></div>';
        }).join("");
      bindDocGenActions(docHistoryBox);
    }).catch(function () {});
  }

  document.getElementById("btnHome").addEventListener("click", function () { location.href = "/"; });
  setAuthed(authed);
  if (authed) { populateDocTemplates(); populateDocKbCollections(); loadDocHistory(); }
})();
/* 侧边栏导航（data-href → 整页跳转） */
document.querySelectorAll("#nav button[data-href]").forEach(function (b) {
  b.addEventListener("click", function () { location.href = b.dataset.href || "/"; });
});
