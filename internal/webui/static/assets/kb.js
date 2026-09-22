// Web 知识库管理（/kb.html）：集合 CRUD + 文档上传/删除 + 检索测试。
// 鉴权复用主控制台的 web 会话（localStorage ocb_web_session + X-Web-Session 头）。
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

  async function kb(path, opts) {
    var o = opts || {};
    o.headers = authHeaders(o.headers);
    try {
      var res = await fetch("/api/kb" + path, o);
      if (res.status === 401) { setAuthed(false); throw new Error("unauthorized"); }
      if (!res.ok) {
        var detail = "";
        try { detail = (await res.json()).error || ""; } catch (e) {}
        throw new Error("HTTP " + res.status + (detail ? ": " + detail : ""));
      }
      return res.status === 204 ? null : res.json();
    } catch (e) {
      if (e.message === "unauthorized") throw e;
      throw new Error(e.message);
    }
  }

  var authed = !!session();
  function setAuthed(v) {
    authed = v;
    document.getElementById("authMsg").style.display = v ? "none" : "block";
    document.getElementById("collections").style.display = v ? "" : "none";
    document.getElementById("searchSec").style.display = v ? "" : "none";
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

  /* ---------- 集合 ---------- */
  var collectionsBox = document.getElementById("collections");

  async function loadCollections() {
    if (!authed) return;
    collectionsBox.innerHTML = '<p class="hint">加载中…</p>';
    try {
      var data = await kb("/collections");
      var cols = data.collections || [];
      var searchSel = document.getElementById("searchCollection");
      if (searchSel) {
        searchSel.innerHTML = '<option value="">全部集合</option>' +
          cols.map(function (c) {
            return '<option value="' + c.id + '">' + esc(c.name) + "</option>";
          }).join("");
      }
      if (!cols.length) {
        collectionsBox.innerHTML = '<p class="hint">还没有知识库集合。点击右上角「新建集合」，然后上传文档即可在会话中自动检索。</p>';
        return;
      }
      collectionsBox.innerHTML = '<h2 style="font-size:14px;margin:0 0 10px">集合（' + cols.length + "）</h2>" +
        '<div class="kb-grid">' + cols.map(function (c) {
          return '<div class="kb-card"><h3>' + esc(c.name) + "</h3>" +
            '<p class="desc">' + esc(c.description || "—") + "</p>" +
            '<span class="stat">文档 ' + (c.documentCount || 0) + " · 片段 " + (c.chunkCount || 0) + "</span>" +
            '<div class="ops"><button class="ghost xs" type="button" data-view="' + c.id + '">管理</button>' +
            '<button class="ghost xs" type="button" data-edit-col="' + c.id + '" data-name="' + esc(c.name) + '" data-desc="' + esc(c.description || "") + '">编辑</button>' +
            '<button class="ghost xs danger" type="button" data-del-col="' + c.id + '" data-name="' + esc(c.name) + '">删除</button></div></div>';
        }).join("") + "</div>";
      bindCollections(cols);
    } catch (e) {
      collectionsBox.innerHTML = '<p class="hint" style="color:var(--err)">' + esc(e.message) + "</p>";
    }
  }

  function bindCollections(cols) {
    collectionsBox.querySelectorAll("[data-view]").forEach(function (b) {
      b.addEventListener("click", function () { openCollection(Number(b.dataset.view)); });
    });
    collectionsBox.querySelectorAll("[data-edit-col]").forEach(function (b) {
      b.addEventListener("click", function () {
        var name = prompt("集合名称（必填）：", b.dataset.name);
        if (name == null) return;
        name = name.trim();
        if (!name) { alert("名称不能为空"); return; }
        var desc = prompt("描述（可选）：", b.dataset.desc) || "";
        kb("/collections/" + b.dataset.editCol, {
          method: "PATCH",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ name: name, description: desc.trim() })
        }).then(loadCollections).catch(function (e) { alert(e.message); });
      });
    });
    collectionsBox.querySelectorAll("[data-del-col]").forEach(function (b) {
      b.addEventListener("click", async function () {
        if (!confirm("删除集合「" + b.dataset.name + "」及其全部文档？")) return;
        try {
          await kb("/collections/" + b.dataset.delCol, { method: "DELETE" });
          loadCollections();
        } catch (e) { alert(e.message); }
      });
    });
  }

  document.getElementById("btnNewCollection").addEventListener("click", function () {
    var name = prompt("集合名称（必填）：");
    if (name == null) return;
    name = name.trim();
    if (!name) { alert("名称不能为空"); return; }
    var desc = prompt("描述（可选）：") || "";
    kb("/collections", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name: name, description: desc.trim() })
    }).then(function () { loadCollections(); }).catch(function (e) { alert(e.message); });
  });

  /* ---------- 集合详情 / 文档 ---------- */
  var detailBox = document.getElementById("detail");
  var documentsBox = document.getElementById("documents");
  var currentCollection = null;
  var docOffset = 0;

  function openCollection(id) {
    currentCollection = id;
    docOffset = 0;
    detailBox.style.display = "block";
    document.getElementById("detailTitle").textContent = "文档列表";
    loadDocuments();
    window.scrollTo({ top: 0 });
  }
  document.getElementById("btnBack").addEventListener("click", function () {
    currentCollection = null;
    detailBox.style.display = "none";
    documentsBox.innerHTML = "";
  });

  function loadDocuments() {
    documentsBox.innerHTML = '<p class="hint">加载中…</p>';
    kb("/documents?collectionId=" + currentCollection + "&limit=50&offset=" + docOffset).then(function (data) {
      var docs = data.documents || [];
      var total = data.total || 0;
      if (!docs.length) {
        documentsBox.innerHTML = '<p class="hint">该集合暂无文档。点「上传文档」入库，成功后即可在会话中检索。</p>';
        return;
      }
      documentsBox.innerHTML = docs.map(function (d) {
        var stc = d.status === "indexed" ? '<span class="chip" style="color:var(--accent)">已索引</span>'
          : d.status === "failed" ? '<span class="chip" style="color:var(--err)">失败</span>'
          : '<span class="chip">' + esc(d.status) + "</span>";
        return '<div class="kb-row"><span class="name">' + esc(d.name) + "</span>" +
          '<span class="meta">' + esc((d.mime || "").split("/").pop() || "—") + " · " + fmtBytes(d.sizeBytes) + " · 片段 " + (d.chunkCount || 0) + "</span> " +
          stc +
          '<button class="ghost xs danger" type="button" data-del-doc="' + d.id + '" data-name="' + esc(d.name) + '">删除</button></div>';
      }).join("");
      if (total > 50) {
        documentsBox.insertAdjacentHTML("beforeend",
          '<div class="kb-row" style="justify-content:center"><span class="hint">共 ' + total + " 篇，显示 " +
          (docOffset + 1) + "-" + (docOffset + docs.length) + '</span>' +
          '<button class="ghost xs" type="button" id="btnDocMore">加载更多</button></div>');
        var more = document.getElementById("btnDocMore");
        if (more) more.addEventListener("click", function () { docOffset += 50; loadDocuments(); });
      }
      documentsBox.querySelectorAll("[data-del-doc]").forEach(function (b) {
        b.addEventListener("click", function () {
          if (!confirm("删除文档「" + b.dataset.name + "」？")) return;
          kb("/documents/" + b.dataset.delDoc, { method: "DELETE" }).then(loadDocuments).catch(function (e) { alert(e.message); });
        });
      });
    }).catch(function (e) { documentsBox.innerHTML = '<p class="hint" style="color:var(--err)">' + esc(e.message) + "</p>"; });
  }

  document.getElementById("btnUpload").addEventListener("click", function () {
    document.getElementById("fileInput").click();
  });
  document.getElementById("fileInput").addEventListener("change", function (e) {
    var f = e.target.files && e.target.files[0];
    e.target.value = "";
    if (!f) return;
    if (f.size > 10 * 1024 * 1024) { alert("文件超过 10 MiB，暂不支持"); return; }
    var fd = new FormData();
    fd.append("collectionId", String(currentCollection));
    fd.append("file", f, f.name);
    fd.append("replace", "true");
    var btn = document.getElementById("btnUpload");
    btn.disabled = true;
    btn.textContent = "上传中…";
    kb("/ingest", { method: "POST", body: fd })
      .then(function (r) { alert("已入库：" + r.name + "，共 " + r.chunks + " 个片段" + (r.replaced ? "（已替换同名的旧版本）" : "")); loadDocuments(); })
      .catch(function (x) { alert("上传失败：" + x.message); })
      .finally(function () { btn.disabled = false; btn.textContent = "上传文档"; });
  });

  /* ---------- 检索测试 ---------- */
  document.getElementById("btnSearch").addEventListener("click", doSearch);
  document.getElementById("searchQ").addEventListener("keydown", function (e) {
    if (e.key === "Enter") doSearch();
  });
  function doSearch() {
    var q = document.getElementById("searchQ").value.trim();
    var box = document.getElementById("searchResults");
    if (!q) return;
    box.innerHTML = '<p class="hint">检索中…</p>';
    var sel = document.getElementById("searchCollection");
    var cid = Number((sel && sel.value) || 0);
    var body = { query: q, topK: 5, minScore: 0.5 };
    if (cid > 0) body.collectionIds = [cid];
    kb("/search", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body)
    }).then(function (data) {
      var r = data.results || [];
      if (!r.length) { box.innerHTML = '<p class="hint">未命中相关片段（或知识库为空）。</p>'; return; }
      box.innerHTML = "<p class=\"hint\">命中 " + r.length + " 条</p>" + r.map(function (x) {
        return '<div class="kb-search-result"><span class="src">' + esc(x.source) +
          (x.section ? " · " + esc(x.section) : "") + "</span>" +
          '<span class="chip" style="margin-left:6px">' + x.score.toFixed(3) + "</span>" +
          '<div class="body">' + esc(x.content) + "</div></div>";
      }).join("");
    }).catch(function (e) { box.innerHTML = '<p class="hint" style="color:var(--err)">' + esc(e.message) + "</p>"; });
  }

  document.getElementById("btnHome").addEventListener("click", function () { location.href = "/"; });
  setAuthed(authed);
  if (authed) loadCollections();
  if (authed) { loadRagStats(); }

/* ---------- RAG 命中统计（/api/kb/stats） ---------- */
async function loadRagStats() {
  var body = document.getElementById("ragStatsBody");
  if (!body) return;
  try {
    var res = await fetch("/api/kb/stats", { headers: authHeaders() });
    if (res.status === 401) { setAuthed(false); return; }
    if (!res.ok) { body.textContent = "统计不可用（" + res.status + "）"; return; }
    var data = await res.json();
    var r = data.rag || {};
    var total = ["spliced", "skipNoEmbedding", "skipNoVector", "skipTimeout", "skipNoResult", "skipBelowThreshold"]
      .reduce(function (acc, k) { return acc + (r[k] || 0); }, 0);
    if (!total) { body.textContent = "尚无 RAG-in-Prompt 调用记录。发一条会触发检索的消息后这里会出现命中/跳过分布。"; return; }
    var pct = function (v) { return total ? (100 * v / total).toFixed(1) + "%" : "0%"; };
    var rows = [
      ["已拼接上下文", r.spliced || 0], ["未配置嵌入模型", r.skipNoEmbedding || 0],
      ["无 pgvector(SQLite)", r.skipNoVector || 0], ["检索超时", r.skipTimeout || 0],
      ["未命中/检索失败", r.skipNoResult || 0], ["低于阈值/预算不足", r.skipBelowThreshold || 0]
    ];
    body.innerHTML = rows.map(function (row) {
      return '<span class="stat" style="margin-right:14px">' + esc(row[0]) + "：<b>" + row[1] + "</b>（" + pct(row[1]) + "）</span>";
    }).join("");
  } catch (e) { body.textContent = "统计加载失败"; }
}

})();
