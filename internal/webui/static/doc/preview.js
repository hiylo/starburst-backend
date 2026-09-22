// 文档预览 renderer page scripts (served same-origin; CSP forbids inline JS).
(function () {
  "use strict";

  var origin = window.location.origin;
  var stage = document.getElementById("stage");
  var tipEl = document.getElementById("tip");
  var fnameEl = document.getElementById("fname");
  var dlBtn = document.getElementById("btn-dl");
  var pdfPager = document.getElementById("pdf-pager");

  function fail(msg) {
    tipEl.style.display = "none";
    stage.innerHTML = '<div id="error">预览失败：' + esc(msg) + "</div>";
  }
  function esc(s) {
    return String(s).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }
  function baseName(u) {
    try { u = decodeURIComponent(u || ""); } catch (e) {}
    var s = u.split("#")[0].split("?")[0];
    var parts = (s || "").split("/");
    var last = parts[parts.length - 1] || "";
    return last ? decodeURIComponent(last) : "文档";
  }
  function fileExt(name) {
    var i = name.lastIndexOf(".");
    return i < 0 ? "" : name.slice(i + 1).toLowerCase();
  }
  function done(text) {
    tipEl.style.display = "none";
    stage.innerHTML = '<div class="doc-out">' + text + "</div>";
  }

  function renderPDF(url, arr) {
    pdfjsLib.GlobalWorkerOptions.workerSrc = origin + "/doc/vendor/pdf.worker.min.js";
    return pdfjsLib.getDocument({ data: arr, withCredentials: true }).promise.then(function (pdf) {
      var pageNum = 1, total = pdf.numPages;
      pdfPager.style.display = "flex";
      function showPage(n) {
        return pdf.getPage(n).then(function (page) {
          var base = Math.min(2, Math.max(0.7, 1240 / page.getViewport({ scale: 1 }).width));
          var vp = page.getViewport({ scale: base });
          var canvas = document.createElement("canvas");
          canvas.width = Math.floor(vp.width);
          canvas.height = Math.floor(vp.height);
          var ctx = canvas.getContext("2d");
          var out = document.createElement("div");
          out.className = "doc-out";
          out.style.textAlign = "center";
          out.appendChild(canvas);
          stage.replaceChildren(out);
          return page.render({ canvasContext: ctx, viewport: vp }).promise.then(function () {
            document.getElementById("pg-info").textContent = n + " / " + total;
          });
        });
      }
      function nav(d) {
        var n = pageNum + d;
        if (n < 1 || n > total) return;
        pageNum = n;
        showPage(n).catch(function (e) { fail(e && e.message || "PDF 渲染失败"); });
      }
      document.getElementById("pg-prev").addEventListener("click", function () { nav(-1); });
      document.getElementById("pg-next").addEventListener("click", function () { nav(1); });
      return showPage(1);
    });
  }

  function renderDOCX(arr) {
    return mammoth.convertToHtml({ arrayBuffer: arr }).then(function (res) {
      done(res.value || "（文档无文字内容）");
    });
  }

  function renderXLSX(arr, name) {
    var wb = XLSX.read(arr, { type: "array", cellDates: true });
    var out = [];
    for (var i = 0; i < wb.SheetNames.length; i++) {
      var sn = wb.SheetNames[i];
      out.push("<h2>" + esc(sn) + "</h2>");
      out.push(XLSX.utils.sheet_to_html(wb.Sheets[sn], { editable: false }));
    }
    done(out.join("<br>"));
  }

  function renderPPTX(arr) {
    return JSZip.loadAsync(arr).then(function (zip) {
      var names = Object.keys(zip.files)
        .filter(function (n) { return /^ppt\/slides\/slide\d+\.xml$/.test(n); })
        .sort(function (a, b) { return numOf(a) - numOf(b); });
      if (!names.length) { fail("PPT 中没有可渲染的幻灯片"); return; }
      var NS = "http://schemas.openxmlformats.org/drawingml/2006/main";
      return Promise.all(names.map(function (n, idx) {
        return zip.file(n).async("string").then(function (xml) {
          var doc = new DOMParser().parseFromString(xml, "application/xml");
          var ps = doc.getElementsByTagNameNS(NS, "p");
          var lines = [];
          for (var i = 0; i < ps.length; i++) {
            var ts = ps[i].getElementsByTagNameNS(NS, "t");
            var line = "";
            for (var j = 0; j < ts.length; j++) line += ts[j].textContent;
            if (line.trim()) lines.push(line.trim());
          }
          var title = lines[0] || ("第 " + (idx + 1) + " 页");
          var bullets = lines.slice(1);
          return '<div class="slide"><h3>第 ' + (idx + 1) + " 页 · " + esc(title) + "</h3>" +
            (bullets.length ? "<ul>" + bullets.map(function (b) { return "<li>" + esc(b) + "</li>"; }).join("") + "</ul>" : "") +
            "</div>";
        });
      })).then(function (slides) {
        stage.replaceChildren(); tipEl.style.display = "none";
        slides.forEach(function (h) { stage.insertAdjacentHTML("beforeend", h); });
      });
    });
  }

  function numOf(name) {
    var m = name.match(/slide(\d+)\.xml$/);
    return m ? Number(m[1]) : 0;
  }

  function load() {
    var params = new URLSearchParams(window.location.search);
    var src = params.get("src") || "";
    if (!src) { fail("缺少 src 参数"); return; }
    var name = baseName(src);
    var ext = fileExt(name);
    fnameEl.textContent = name;
    dlBtn.href = src;
    dlBtn.style.display = "inline-block";
    dlBtn.setAttribute("download", name);

    if (["docx", "pptx"].indexOf(ext) < 0 && ["pdf", "xlsx", "xls", "csv"].indexOf(ext) < 0) {
      fail("暂不支持预览 ." + ext);
      return;
    }
    fetch(src, { credentials: "include" })
      .then(function (resp) {
        if (!resp.ok) throw new Error("HTTP " + resp.status);
        return resp.arrayBuffer();
      })
      .then(function (arr) {
        if (ext === "pdf") return renderPDF(name, arr);
        if (ext === "docx") return renderDOCX(arr);
        if (ext === "pptx") return renderPPTX(arr);
        return renderXLSX(arr, name);
      })
      .catch(function (e) { fail(e && e.message || "加载失败"); });
  }

  document.getElementById("btn-back").addEventListener("click", function () {
    if (window.history.length > 1) window.history.back(); else window.close();
  });
  load();
})();