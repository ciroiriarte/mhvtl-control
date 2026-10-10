/*
 * mhvtld dashboard client.
 *
 * Plain ES modules-free JS (no build step). Responsibilities:
 *   - theme switching (modern | classic), persisted in localStorage
 *   - remote-target config: API base URL + bearer token, persisted, so the
 *     dashboard can manage a LOCAL or REMOTE mhvtld node (remote-first)
 *   - a fetch wrapper that applies the base URL + Authorization: Bearer token
 *   - rendering the library rack, system panel and raw-disk dock from the API
 */
(function () {
  "use strict";

  var LS_THEME = "mhvtld.theme";
  var LS_BASE = "mhvtld.apiBase";
  var LS_TOKEN = "mhvtld.apiToken";

  function lsGet(k, d) { try { var v = localStorage.getItem(k); return v === null ? d : v; } catch (e) { return d; } }
  function lsSet(k, v) { try { localStorage.setItem(k, v); } catch (e) {} }

  /* ---- theme ---------------------------------------------------------- */
  function applyTheme(t) {
    if (t !== "modern" && t !== "classic") t = "modern";
    document.documentElement.setAttribute("data-theme", t);
    var sel = document.getElementById("theme-select");
    if (sel) sel.value = t;
    lsSet(LS_THEME, t);
  }

  /* ---- API client (remote-aware) -------------------------------------- */
  function apiBase() { return lsGet(LS_BASE, "").replace(/\/+$/, ""); }
  function apiToken() { return lsGet(LS_TOKEN, ""); }

  function api(path, opts) {
    opts = opts || {};
    var headers = opts.headers || {};
    var tok = apiToken();
    if (tok) headers["Authorization"] = "Bearer " + tok;
    if (opts.body && !headers["Content-Type"]) headers["Content-Type"] = "application/json";
    return fetch(apiBase() + path, {
      method: opts.method || "GET",
      headers: headers,
      body: opts.body ? JSON.stringify(opts.body) : undefined,
    }).then(function (r) {
      if (!r.ok) {
        return r.json().catch(function () { return {}; }).then(function (j) {
          var msg = (j && j.error && j.error.message) || (r.status + " " + r.statusText);
          throw new Error(msg);
        });
      }
      return r.status === 204 ? null : r.json();
    });
  }

  function esc(s) {
    return String(s == null ? "" : s).replace(/[&<>"]/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c];
    });
  }
  function fmtBytes(n) {
    n = Number(n) || 0;
    var u = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"], i = 0;
    while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
    return n.toFixed(i ? 1 : 0) + " " + u[i];
  }
  function showError(msg) {
    var el = document.getElementById("global-error");
    if (el) el.innerHTML = '<div class="error">' + esc(msg) + "</div>";
  }
  function clearError() { var el = document.getElementById("global-error"); if (el) el.innerHTML = ""; }

  /* ---- renderers ------------------------------------------------------ */
  function renderSystem(s) {
    s = s || {};
    document.getElementById("sys-stat").innerHTML =
      "host <b>" + esc(s.hostname || "?") + "</b> · " +
      "<b>" + esc(s.cpu_count || "?") + "</b> cpu · " +
      "mem <b>" + fmtBytes(s.mem_total_bytes) + "</b>";
    var la = s.load_average || [];
    document.getElementById("system-status").innerHTML =
      '<table><tbody>' +
      row("Hostname", esc(s.hostname)) +
      row("OS / Kernel", esc(s.os) + " " + esc(s.kernel_version)) +
      row("Arch", esc(s.architecture)) +
      row("CPUs", esc(s.cpu_count)) +
      row("Memory", fmtBytes(s.mem_available_bytes) + " free / " + fmtBytes(s.mem_total_bytes)) +
      row("Load", (la.join ? la.join("  ") : "")) +
      row("Transports", (s.active_transports || []).join(", ") || "—") +
      "</tbody></table>";
  }
  function row(k, v) { return "<tr><th>" + k + "</th><td>" + v + "</td></tr>"; }

  function renderLibraries(data) {
    var libs = (data && data.libraries) || [];
    document.getElementById("lib-count").textContent = libs.length ? "(" + libs.length + ")" : "";
    if (!libs.length) { document.getElementById("library-rack").innerHTML = '<p class="muted">No libraries configured.</p>'; return; }
    var html = "";
    libs.forEach(function (lib) {
      html += '<div class="rack"><div><b>' + esc(lib.vendor) + " " + esc(lib.product) +
        '</b> <span class="muted mono">#' + esc(lib.id) + " · " + esc(lib.serial) + "</span></div>";
      var drives = lib.drives || [];
      html += '<div class="drive-row">';
      drives.forEach(function (d) {
        var loaded = d.slot && d.slot > 0;
        html += '<div class="drive' + (loaded ? " loaded" : "") + '">' +
          '<span class="id">drive ' + esc(d.id) + "</span><br>" +
          '<span class="pid">' + esc(d.product) + "</span>" +
          '<div><button class="btn sm" data-mount="' + esc(lib.id) + ":" + esc(d.id) + '">Mount</button> ' +
          '<button class="btn sm" data-unmount="' + esc(lib.id) + ":" + esc(d.id) + '">Unmount</button></div>' +
          "</div>";
      });
      html += "</div>";
      var slots = lib.slots || [];
      if (slots.length) {
        html += '<div class="slot-grid">';
        slots.forEach(function (sl) {
          var full = sl.barcode && sl.barcode.length;
          html += '<div class="slot' + (full ? " full" : "") + '">' +
            '<span class="n">slot ' + esc(sl.number) + "</span>" +
            '<span class="bc">' + (full ? esc(sl.barcode) : "—") + "</span></div>";
        });
        html += "</div>";
      }
      html += "</div>";
    });
    document.getElementById("library-rack").innerHTML = html;
  }

  function renderDisks(data) {
    var disks = (data && data.disks) || [];
    document.getElementById("disk-count").textContent = disks.length ? "(" + disks.length + ")" : "";
    if (!disks.length) { document.getElementById("disk-dock").innerHTML = '<p class="muted">No candidate disks found.</p>'; return; }
    var html = "";
    disks.forEach(function (d) {
      var mvtl = d.mvtl_formatted && d.mvtl;
      html += '<div class="disk"><div class="head">' +
        '<span class="name">' + esc(d.name || d.path) + "</span>" +
        '<span class="tag' + (mvtl ? " mvtl" : "") + '">' + (mvtl ? "MVTL" : "unformatted") + "</span>" +
        '<span class="muted">' + fmtBytes(d.size_bytes) + "</span>";
      if (mvtl) {
        html += '<span class="muted mono">' + esc(d.mvtl.barcode) + " · " +
          esc(d.mvtl.vendor_id) + " " + esc(d.mvtl.product_id) + "</span>";
      }
      html += "</div>";
      var pct = d.size_bytes && mvtl ? Math.min(100, Math.round((d.mvtl.total_capacity_bytes / d.size_bytes) * 100)) : 0;
      html += '<div class="bar"><span style="width:' + pct + '%"></span></div></div>';
    });
    document.getElementById("disk-dock").innerHTML = html;
  }

  /* ---- actions -------------------------------------------------------- */
  function onRackClick(e) {
    var m = e.target.getAttribute && (e.target.getAttribute("data-mount") || e.target.getAttribute("data-unmount"));
    if (!m) return;
    var isMount = !!e.target.getAttribute("data-mount");
    var parts = m.split(":"), lib = parts[0], drive = parts[1];
    clearError();
    api("/api/v1/libraries/" + encodeURIComponent(lib) + "/" + (isMount ? "mount" : "unmount"),
      { method: "POST", body: { drive: Number(drive) } })
      .then(refresh)
      .catch(function (err) { showError((isMount ? "Mount" : "Unmount") + " failed: " + err.message); });
  }

  function refresh() {
    clearError();
    api("/api/v1/system").then(renderSystem).catch(function (e) { showError("system: " + e.message); });
    api("/api/v1/libraries").then(renderLibraries).catch(function (e) { showError("libraries: " + e.message); });
    api("/api/v1/storage/disks").then(renderDisks).catch(function (e) { showError("disks: " + e.message); });
  }

  /* ---- wiring --------------------------------------------------------- */
  document.addEventListener("DOMContentLoaded", function () {
    applyTheme(lsGet(LS_THEME, "modern"));
    document.getElementById("theme-select").addEventListener("change", function (e) { applyTheme(e.target.value); });
    document.getElementById("refresh-btn").addEventListener("click", refresh);
    document.getElementById("library-rack").addEventListener("click", onRackClick);

    var dlg = document.getElementById("target-dialog");
    document.getElementById("target-btn").addEventListener("click", function () {
      document.getElementById("target-url").value = lsGet(LS_BASE, "");
      document.getElementById("target-token").value = lsGet(LS_TOKEN, "");
      if (dlg.showModal) dlg.showModal();
    });
    document.getElementById("target-form").addEventListener("submit", function (e) {
      if (e.submitter && e.submitter.value === "save") {
        lsSet(LS_BASE, document.getElementById("target-url").value.trim());
        lsSet(LS_TOKEN, document.getElementById("target-token").value);
        setTimeout(refresh, 0);
      }
    });

    refresh();
  });
})();
