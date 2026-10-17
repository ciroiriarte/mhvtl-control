/*
 * mhvtl-control dashboard client (framework-free, no build step).
 *
 * Security + operational-correctness rewrite (P0 hardening):
 *   - XSS-safe: ALL API-derived DOM is built with createElement + textContent /
 *     setAttribute. No API value is ever assigned to innerHTML. Where a template
 *     string is unavoidable it is routed through VTL.esc / VTL.attr.
 *   - Bearer token kept IN MEMORY by default; persisted only with the explicit
 *     "Remember on this device" opt-in; a Forget control clears it. The token is
 *     never written to the console/logs.
 *   - Node isolation: switching the managed node bumps a generation counter,
 *     aborts in-flight requests, greys the panels and disables mutating controls
 *     until a health probe confirms the new node. Every response and every
 *     mutation is dropped if the generation advanced while it was in flight.
 *
 * Pure helpers live in core.js and are exposed as window.VTL.
 */
(function () {
  "use strict";

  var core = window.VTL;
  if (!core) {
    // core.js failed to load; fail loud rather than silently running unsafe.
    document.addEventListener("DOMContentLoaded", function () {
      var m = document.getElementById("library-rack");
      if (m) { m.textContent = "Dashboard failed to load (core.js missing)."; }
    });
    return;
  }
  var esc = core.esc, buildMountBody = core.buildMountBody, isStale = core.isStale;
  var canSendMutation = core.canSendMutation;
  var coerceArray = core.coerceArray, coerceNumber = core.coerceNumber, fmtBytes = core.fmtBytes;
  var filterSlots = core.filterSlots, countSlots = core.countSlots, slotIsOccupied = core.slotIsOccupied;
  var driveState = core.driveState, firstEmptyDrive = core.firstEmptyDrive, mountCandidates = core.mountCandidates;
  var emptyLocations = core.emptyLocations, fmtRatio = core.fmtRatio, validBarcode = core.validBarcode;
  var expandBarcodes = core.expandBarcodes, parseSizeMB = core.parseSizeMB, buildMediaPatch = core.buildMediaPatch;
  var mediaLocText = core.mediaLocText, filterMedia = core.filterMedia;
  var nodeRows = core.nodeRows;
  var capabilityNotice = core.capabilityNotice, findingRows = core.findingRows, problemText = core.problemText;
  var buildValidateBody = core.buildValidateBody;
  var writesEditable = core.writesEditable, stageFileRows = core.stageFileRows, planSummary = core.planSummary;
  var forceRemaining = core.forceRemaining, fmtCountdown = core.fmtCountdown, applyErrorAction = core.applyErrorAction;
  var stageOutcome = core.stageOutcome;
  var libraryAttrFields = core.libraryAttrFields, driveAttrFields = core.driveAttrFields, buildAttrPatch = core.buildAttrPatch;
  var buildSlotPatch = core.buildSlotPatch, buildSlotDelete = core.buildSlotDelete, buildSlotsAdd = core.buildSlotsAdd;

  /* ---- library view state (US-F6: search / filter / table / multi-lib) --
   * These persist across refreshes so a running search/filter is not reset by
   * background polling. lastLibData caches the last successful /libraries
   * payload so the toolbar can re-render without a refetch.                  */
  var libView = { query: "", state: "all", table: false, selected: null };
  var lastLibData = null;
  var openNodeLinks = {}; // device key -> true while its /dev/tape links are expanded

  /* ---- media view state: search / location filter, and the last /media
   * payload with the library and generation it was fetched for.           */
  var mediaView = { query: "", where: "all" };
  var lastMedia = null; // { libId, gen, data }

  /* ---- configuration view state: the library shown, the application it
   * is checked for, and the last payloads, so the selectors re-render
   * without a refetch.                                                   */
  var cfgView = { selected: null, app: "" };
  var lastCfg = null;    // { caps, config, personalities }

  /* ---- staged configuration change (plan 4.2): the daemon holds the
   * stage; the dashboard keeps its id, its last answer, the node generation
   * it belongs to, and the last dry run.                                 */
  var cfgStage = null;   // { id, data, gen, plan (planSummary), result }

  /* ---- localStorage (errors surfaced, never silently implying success) -- */
  var LS_THEME = "mhvtl-control.theme";
  var LS_ORIGIN = "mhvtl-control.origin";
  var LS_TOKEN = "mhvtl-control.token";
  var LS_REMEMBER = "mhvtl-control.remember";

  function lsGet(k) {
    try { return localStorage.getItem(k); } catch (e) { return null; }
  }
  // lsSet THROWS on failure so callers can surface it (private-mode, quota).
  function lsSet(k, v) { localStorage.setItem(k, v); }
  function lsDel(k) { try { localStorage.removeItem(k); } catch (e) { /* best effort */ } }

  /* ---- node state (in memory) ------------------------------------------ */
  var NodeState = {
    origin: "",      // "" == same origin (Local)
    token: "",       // kept in memory unless remember
    remember: false,
    nodeId: "",
    generation: 0,   // bumped on every target switch
    health: "unknown", // "unknown" | "ok" | "err"
  };

  var mutationsEnabled = false; // gated on a successful health probe
  var currentAbort = null;      // AbortController for the active refresh cycle

  /* ---- small DOM builders ---------------------------------------------- */
  function mk(tag, opts) {
    opts = opts || {};
    var n = document.createElement(tag);
    if (opts.class) { n.className = opts.class; }
    if (opts.text != null) { n.textContent = String(opts.text); }
    if (opts.attrs) {
      Object.keys(opts.attrs).forEach(function (k) {
        n.setAttribute(k, String(opts.attrs[k]));
      });
    }
    if (opts.children) {
      opts.children.forEach(function (c) { if (c) { n.appendChild(c); } });
    }
    return n;
  }
  function clear(node) { while (node && node.firstChild) { node.removeChild(node.firstChild); } }
  function $(id) { return document.getElementById(id); }

  /* ---- theme ----------------------------------------------------------- */
  function applyTheme(t) {
    if (t !== "modern" && t !== "classic") { t = "modern"; }
    document.documentElement.setAttribute("data-theme", t);
    var sel = $("theme-select");
    if (sel) { sel.value = t; }
    try { lsSet(LS_THEME, t); } catch (e) { /* theme persistence is non-critical */ }
  }

  /* ---- toast (transient success feedback) ------------------------------ */
  function toast(msg) {
    var wrap = $("toast-wrap");
    if (!wrap) { return; }
    var t = mk("div", { class: "toast", text: msg });
    wrap.appendChild(t);
    setTimeout(function () {
      t.classList.add("leaving");
      setTimeout(function () { if (t.parentNode) { t.parentNode.removeChild(t); } }, 300);
    }, 3500);
  }

  /* ---- per-panel status region ----------------------------------------- *
   * Each panel owns its region so one failing refresh never overwrites
   * another's error. Errors are dismissable and are only replaced by a
   * successful refresh of the SAME panel — never auto-cleared before shown.  */
  function PanelStatus(regionId) {
    var region = $(regionId);
    // sticky is a warning about a partly failed action (placing stopped, an
    // import not stored). It outlives refreshes, which follow every action and
    // would otherwise wipe it before it is read, until dismissed or the node
    // changes.
    var sticky = null;
    function errorBox(msg, onDismiss) {
      var box = mk("div", { class: "panel-error" });
      box.appendChild(mk("span", { class: "msg", text: String(msg) }));
      var x = mk("button", { class: "dismiss", text: "✕", attrs: { type: "button", "aria-label": "Dismiss" } });
      x.addEventListener("click", function () {
        if (box.parentNode) { box.parentNode.removeChild(box); }
        if (onDismiss) { onDismiss(); }
      });
      box.appendChild(x);
      return box;
    }
    function keepSticky() { if (sticky) { region.appendChild(sticky); } }
    return {
      loading: function () {
        if (!region) { return; }
        // Keep any standing error block; just mark that a refresh is running.
        var line = region.querySelector(".status-line");
        if (!line) { line = mk("span", { class: "status-line" }); region.insertBefore(line, region.firstChild); }
        line.className = "status-line loading";
        line.textContent = "Refreshing…";
      },
      ok: function () {
        if (!region) { return; }
        clear(region);
        var now = new Date();
        region.appendChild(mk("span", {
          class: "status-line",
          text: "Updated " + now.toLocaleTimeString(),
        }));
        keepSticky();
      },
      grey: function () {
        if (!region) { return; }
        sticky = null;
        clear(region);
        region.appendChild(mk("span", { class: "status-line loading", text: "Switching node…" }));
      },
      error: function (msg) {
        if (!region) { return; }
        clear(region);
        region.appendChild(errorBox(msg, function () { clear(region); keepSticky(); }));
        keepSticky();
      },
      warn: function (msg) {
        if (!region) { return; }
        if (sticky && sticky.parentNode) { sticky.parentNode.removeChild(sticky); }
        var box = errorBox(msg, function () { if (sticky === box) { sticky = null; } });
        sticky = box;
        region.appendChild(box);
      },
    };
  }
  var sysStatus = PanelStatus("system-status-region");
  var libStatus = PanelStatus("lib-status");
  var diskStatus = PanelStatus("disk-status");
  var mediaStatus = PanelStatus("media-status");
  var configStatus = PanelStatus("config-status");
  var findingsStatus = PanelStatus("findings-status");

  // busy toggles aria-busy on a panel <section> for assistive tech during loads.
  function busy(panelId, on) {
    var p = $(panelId);
    if (p) { p.setAttribute("aria-busy", on ? "true" : "false"); }
  }

  /* ---- API client (node-aware) ----------------------------------------- *
   * Applies NodeState.origin as base URL and a Bearer token. Parses the
   * {error:{code,message}} envelope and surfaces .status / .code on the Error.
   * The token is never logged.                                              */
  function api(path, opts) {
    opts = opts || {};
    var headers = {};
    if (NodeState.token) { headers["Authorization"] = "Bearer " + NodeState.token; }
    var body;
    if (opts.body !== undefined && opts.body !== null) {
      headers["Content-Type"] = "application/json";
      body = JSON.stringify(opts.body);
    }
    var base = NodeState.origin ? NodeState.origin.replace(/\/+$/, "") : "";
    return fetch(base + path, {
      method: opts.method || "GET",
      headers: headers,
      body: body,
      signal: opts.signal,
    }).then(function (r) {
      var ct = r.headers.get("content-type") || "";
      var isJSON = ct.indexOf("json") >= 0;
      if (!r.ok) {
        return (isJSON ? r.json().catch(function () { return {}; }) : Promise.resolve({}))
          .then(function (j) {
            var code = (j && j.error && j.error.code) || "";
            var msg = (j && j.error && j.error.message) || (r.status + " " + r.statusText);
            var err = new Error(msg);
            err.status = r.status;
            err.code = code;
            err.body = j || {};
            throw err;
          });
      }
      if (r.status === 204) { return null; }
      return isJSON ? r.json() : r.text();
    });
  }

  /* ---- node badge ------------------------------------------------------ */
  function updateBadge() {
    var label = $("node-label"), dot = $("node-health-dot"), text = $("node-health-text");
    if (label) { label.textContent = NodeState.origin ? NodeState.origin : "Local"; }
    if (dot) { dot.className = "dot " + NodeState.health; }
    if (text) {
      text.textContent = NodeState.health === "ok" ? "ok"
        : NodeState.health === "err" ? "unreachable"
        : "connecting…";
    }
  }

  /* ---- renderers (XSS-safe, no innerHTML of API data) ------------------ */
  function kvRow(k, v) {
    return mk("tr", { children: [
      mk("th", { text: k }),
      mk("td", { text: v }),
    ] });
  }

  function renderSystem(s) {
    s = s || {};
    // Topbar host summary (textContent only).
    var stat = $("sys-stat");
    if (stat) {
      clear(stat);
      stat.appendChild(document.createTextNode("host "));
      stat.appendChild(mk("b", { text: s.hostname || "?" }));
      stat.appendChild(document.createTextNode(" · "));
      stat.appendChild(mk("b", { text: String(s.cpu_count != null ? s.cpu_count : "?") }));
      stat.appendChild(document.createTextNode(" cpu · mem "));
      stat.appendChild(mk("b", { text: fmtBytes(s.mem_total_bytes) }));
    }
    var la = coerceArray(s.load_average).map(function (x) { return coerceNumber(x); });
    var transports = coerceArray(s.active_transports).map(function (x) { return String(x); });
    var body = $("system-status");
    clear(body);
    var tbody = mk("tbody", { children: [
      kvRow("Hostname", s.hostname || "—"),
      kvRow("OS / Kernel", (s.os || "") + " " + (s.kernel_version || "")),
      kvRow("Arch", s.architecture || "—"),
      kvRow("CPUs", String(s.cpu_count != null ? s.cpu_count : "—")),
      kvRow("Memory", fmtBytes(s.mem_available_bytes) + " free / " + fmtBytes(s.mem_total_bytes)),
      kvRow("Load", la.length ? la.join("  ") : "—"),
      kvRow("Transports", transports.length ? transports.join(", ") : "—"),
    ] });
    body.appendChild(mk("table", { children: [tbody] }));
  }

  // scsiAddr returns a drive's own SCSI address and NAA. It never borrows the
  // library's: that would label every drive with the changer's identity.
  // Returns {addr, naa} where either may be "" when unknown.
  function scsiAddr(d) {
    var c = d ? d.channel : null, t = d ? d.target : null, l = d ? d.lun : null;
    var addr = (c != null && t != null && l != null)
      ? (coerceNumber(c) + ":" + coerceNumber(t) + ":" + coerceNumber(l)) : "";
    var naa = (d && d.naa) ? String(d.naa) : "";
    return { addr: addr, naa: naa };
  }

  // telemetryBadges builds the compression + SCSI-addressing badges for a drive.
  // All values go through textContent (mk `text`), so API data is never HTML.
  function telemetryBadges(lib, d) {
    var wrap = mk("div", { class: "badges" });
    if (d.compression_enabled) {
      var ctype = d.compression_type ? String(d.compression_type) : "on";
      var factor = coerceNumber(d.compression_factor);
      wrap.appendChild(mk("span", {
        class: "badge comp on",
        text: "compression " + ctype + (factor > 0 ? " ×" + factor : ""),
        attrs: { title: "Data compression enabled (" + ctype + ")" },
      }));
    } else {
      wrap.appendChild(mk("span", { class: "badge comp off", text: "compression off" }));
    }
    var sa = scsiAddr(d);
    if (sa.addr) {
      wrap.appendChild(mk("span", { class: "badge scsi", text: "scsi " + sa.addr, attrs: { title: "SCSI channel:target:lun" } }));
    }
    if (sa.naa) {
      wrap.appendChild(mk("span", { class: "badge naa mono", text: "naa " + sa.naa, attrs: { title: "SCSI NAA identifier" } }));
    }
    return wrap;
  }

  // copyText puts text on the clipboard. navigator.clipboard needs a secure
  // context (HTTPS or localhost); elsewhere fall back to a hidden textarea.
  function copyText(text) {
    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(text).then(
        function () { toast("Copied " + text); },
        function () { toast("Copy failed; select the path instead"); });
      return;
    }
    var ta = mk("textarea", { attrs: { readonly: "", "aria-hidden": "true" } });
    ta.value = text;
    ta.style.position = "fixed";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    var ok = false;
    try { ok = document.execCommand("copy"); } catch (e) { ok = false; }
    document.body.removeChild(ta);
    toast(ok ? "Copied " + text : "Copy failed; select the path instead");
  }

  // nodeRow shows one node path (shortened to `shown` when given) with a
  // button that copies the full path.
  function nodeRow(label, path, shown) {
    var btn = mk("button", { class: "btn xs ghost copy", text: "copy", attrs: { type: "button", "aria-label": "Copy " + path } });
    btn.addEventListener("click", function () { copyText(path); });
    return mk("div", { class: "node-row", children: [
      mk("span", { class: "node-k", text: label }),
      mk("code", { class: "node-v", text: shown || path, attrs: { title: path } }),
      btn,
    ] });
  }

  // nodesBlock shows a device's nodes (st/nst/sg/sch) with copy buttons and
  // its /dev/tape links in a collapsed list. Returns null when the device
  // wasn't found on this host (no "nodes" in the API entry). `key` keeps
  // the link list open across re-renders.
  function nodesBlock(nodes, what, key) {
    var nr = nodeRows(nodes);
    if (!nr.rows.length && !nr.links.length) { return null; }
    var box = mk("div", { class: "nodes", attrs: { role: "group", "aria-label": what + " device nodes" } });
    nr.rows.forEach(function (r) { box.appendChild(nodeRow(r.label, r.path)); });
    if (nr.links.length) {
      var det = mk("details", { class: "node-links" });
      det.open = openNodeLinks[key] === true;
      det.addEventListener("toggle", function () { openNodeLinks[key] = det.open; });
      det.appendChild(mk("summary", { text: "/dev/tape links (" + nr.links.length + ")" }));
      nr.links.forEach(function (l) {
        det.appendChild(nodeRow(l.label, l.path, l.path.replace(/^.*\//, "")));
      });
      box.appendChild(det);
    }
    return box;
  }

  // locText renders a {kind, number} source location for display.
  function locText(loc) {
    return (loc.kind === "map" ? "MAP " : "slot ") + coerceNumber(loc.number);
  }

  // Build a drive card. `lib` is the raw library object. Load state comes
  // ONLY from the live changer view (core.driveState); without it the card
  // shows the configured topology and claims nothing about the drive's media.
  function driveCard(lib, d) {
    // Generation this control is built under. Every mutation launched from this
    // card captures it so canSendMutation() can reject a click that lands after
    // the node was switched (belt to the grey-until-render braces).
    var renderGen = NodeState.generation;
    var driveId = coerceNumber(d.id);
    var st = driveState(lib, d);
    var card = mk("div", { class: "drive" + (st.state === "loaded" ? " loaded" : "") });

    card.appendChild(mk("span", { class: "id", text: "drive " + driveId }));
    card.appendChild(mk("br"));
    card.appendChild(mk("span", { class: "pid", text: d.product || "" }));

    // drive.slot is the drive's configured POSITION in the library (its data
    // transfer element number), not a storage slot.
    if (coerceNumber(d.slot) > 0) {
      card.appendChild(mk("div", {
        class: "loaded-info mono",
        text: "configured position " + coerceNumber(d.slot),
        attrs: { title: "Drive position in the library from device.conf (Library ID … Slot)" },
      }));
    }
    if (st.state === "loaded") {
      card.appendChild(mk("div", {
        class: "loaded-info mono",
        text: (st.barcode || "(no barcode)") + (st.source ? " · from " + locText(st.source) : ""),
      }));
    } else if (st.state === "empty") {
      card.appendChild(mk("div", { class: "loaded-info", text: "empty" }));
    } else {
      card.appendChild(mk("span", {
        class: "badge unknown",
        text: "unknown (changer not reachable)",
        attrs: { title: lib.live_error ? String(lib.live_error) : "Live changer state unavailable" },
      }));
    }

    card.appendChild(telemetryBadges(lib, d));
    var dn = nodesBlock(d.nodes, "drive " + driveId, "lib" + coerceNumber(lib.id) + "/drive" + driveId);
    if (dn) { card.appendChild(dn); }

    var actions = mk("div", { class: "drive-actions" });
    if (st.state === "loaded") {
      var unmountBtn = mk("button", { class: "btn sm", text: "Unmount", attrs: { type: "button", "aria-label": "Unmount drive " + driveId } });
      unmountBtn.disabled = !mutationsEnabled;
      unmountBtn.addEventListener("click", function () { doUnmount(lib.id, driveId, unmountBtn, renderGen); });
      actions.appendChild(unmountBtn);
      var expBtn = mk("button", { class: "btn sm", text: "Export", attrs: { type: "button", "aria-label": "Export the cartridge in drive " + driveId + " to the MAP" } });
      expBtn.disabled = !mutationsEnabled || lib.map_open === true;
      expBtn.title = lib.map_open === true ? "the MAP is open: close it first" : "Unload and move the cartridge to the first empty MAP slot";
      expBtn.addEventListener("click", function () {
        mutate({
          renderGen: renderGen, btn: expBtn, what: "export",
          path: libPath(lib.id, "/map/export"), body: { drive: driveId },
          done: function (res) { return "Exported" + moveText(res && res.move); },
        });
      });
      actions.appendChild(expBtn);
    } else {
      var mountBtn = mk("button", { class: "btn sm", text: "Mount", attrs: { type: "button", "aria-label": "Mount a cartridge into drive " + driveId } });
      mountBtn.disabled = !mutationsEnabled || st.state !== "empty";
      if (st.state !== "empty") { mountBtn.title = "drive state unknown: changer not reachable"; }
      mountBtn.addEventListener("click", function () { openMountPicker(lib, driveId, renderGen); });
      actions.appendChild(mountBtn);
    }
    var statsBtn = mk("button", { class: "btn sm ghost", text: "Stats", attrs: { type: "button", "aria-label": "Show statistics for drive " + driveId } });
    statsBtn.addEventListener("click", function () { openStats(lib, d); });
    actions.appendChild(statsBtn);
    card.appendChild(actions);
    return card;
  }

  // mapArea renders a library's Import/Export (MAP) slots as a DISTINCT region
  // (US-F7), visually separate from storage slots, with the MAP door state
  // and its controls: open/close, eject everything to the shelf, and import a
  // shelf cartridge. "Load from MAP" mounts that MAP slot ({drive, map}) into
  // the first drive the changer reports empty; "Move to slot" stores it in the
  // first empty storage slot.
  function mapArea(lib) {
    var renderGen = NodeState.generation;
    var maps = coerceArray(lib.maps);
    var live = lib.live === true;
    var isOpen = lib.map_open === true;
    var box = mk("div", { class: "map-area" });
    var top = mk("div", { class: "map-top" });
    top.appendChild(mk("div", { class: "map-head", text: "Import / Export (MAP)" }));
    box.appendChild(top);
    if (!maps.length) {
      box.appendChild(mk("p", { class: "muted", text: "No import/export (MAP) slots." }));
      return box;
    }
    if (typeof lib.map_open === "boolean") {
      top.appendChild(mk("span", {
        class: "map-state" + (isOpen ? " open" : ""),
        text: isOpen ? "MAP OPEN — the robot cannot reach these slots" : "MAP closed",
      }));
    } else if (!live) {
      top.appendChild(mk("span", { class: "map-state", text: "state unknown (changer not reachable)" }));
    }
    top.appendChild(mk("span", { class: "spacer" }));

    var mapCounts = countSlots(maps);
    var libId = coerceNumber(lib.id);
    var doorBtn = mk("button", { class: "btn sm", text: isOpen ? "Close" : "Open", attrs: { type: "button", "aria-label": (isOpen ? "Close" : "Open") + " the MAP" } });
    doorBtn.disabled = !mutationsEnabled || !live;
    doorBtn.title = !live ? "changer not reachable"
      : isOpen ? "Close the MAP so the robot can reach its slots again"
        : "Open the MAP (the robot cannot reach its slots while it is open)";
    doorBtn.addEventListener("click", function () {
      mutate({
        renderGen: renderGen, btn: doorBtn, what: isOpen ? "close MAP" : "open MAP",
        path: libPath(libId, isOpen ? "/map/close" : "/map/open"),
        done: function () { return "MAP " + (isOpen ? "closed" : "opened"); },
      });
    });
    top.appendChild(doorBtn);

    var ejectBtn = mk("button", { class: "btn sm", text: "Eject all to shelf", attrs: { type: "button" } });
    ejectBtn.disabled = !mutationsEnabled || !live || mapCounts.occupied === 0;
    ejectBtn.title = !live ? "changer not reachable"
      : mapCounts.occupied === 0 ? "the MAP is empty"
        : "Take every cartridge in the MAP out of the library (media stays on disk)";
    ejectBtn.addEventListener("click", function () {
      var bcs = [];
      maps.forEach(function (m) { if (slotIsOccupied(m)) { bcs.push(String(m.barcode || "(no barcode)")); } });
      openConfirm("Eject MAP to shelf",
        "Take " + bcs.length + " cartridge" + (bcs.length === 1 ? "" : "s") + " (" + bcs.join(", ") +
        ") out of library #" + libId + ". Their media stays on disk; import them again from the Media panel.",
        "Eject", function () {
          mutate({
            renderGen: renderGen, btn: ejectBtn, what: "eject MAP",
            path: libPath(libId, "/map/eject"),
            done: function (res) {
              var out = coerceArray(res && res.ejected).map(String);
              return out.length ? "Ejected to shelf: " + out.join(", ") : "MAP emptied";
            },
          });
        });
    });
    top.appendChild(ejectBtn);

    var importBtn = mk("button", { class: "btn sm", text: "Import…", attrs: { type: "button", "aria-label": "Import a shelf cartridge through the MAP" } });
    importBtn.disabled = !mutationsEnabled || !live || mapCounts.empty === 0;
    importBtn.title = !live ? "changer not reachable"
      : mapCounts.empty === 0 ? "the MAP is full" : "Put a shelf cartridge into the MAP";
    importBtn.addEventListener("click", function () { openImportDialog(libId, renderGen, null); });
    top.appendChild(importBtn);

    var emptyDrive = firstEmptyDrive(lib);
    var freeSlot = null;
    emptyLocations(lib).forEach(function (loc) { if (freeSlot === null && loc.kind === "slot") { freeSlot = loc.number; } });
    var list = mk("div", { class: "map-list" });
    maps.forEach(function (m) {
      var occ = slotIsOccupied(m);
      var num = coerceNumber(m.number);
      var cell = mk("div", { class: "map-slot" + (occ ? " full" : "") });
      cell.appendChild(mk("span", { class: "n mono", text: "MAP " + num }));
      cell.appendChild(mk("span", { class: "bc mono", text: occ ? String(m.barcode || "(no barcode)") : "—" }));

      var acts = mk("div", { class: "map-actions" });
      var imp = mk("button", { class: "btn sm", text: "Load from MAP", attrs: { type: "button", "aria-label": "Import (load) the cartridge in MAP " + num + " into a drive" } });
      imp.disabled = !mutationsEnabled || !live || !occ || emptyDrive === null || isOpen;
      imp.title = !live ? "changer not reachable"
        : !occ ? "MAP slot is empty"
          : isOpen ? "the MAP is open: close it first"
            : (emptyDrive === null ? "no empty drive available to load into"
              : "Load the cartridge in this MAP slot into drive " + emptyDrive);
      imp.addEventListener("click", function () {
        doMount({ libId: libId, drive: emptyDrive }, { kind: "map", number: num }, imp, renderGen);
      });
      acts.appendChild(imp);

      if (occ) {
        var mv = mk("button", { class: "btn sm ghost", text: "Move to slot", attrs: { type: "button", "aria-label": "Move the cartridge in MAP " + num + " to the first empty storage slot" } });
        mv.disabled = !mutationsEnabled || !live || isOpen || freeSlot === null;
        mv.title = !live ? "changer not reachable"
          : isOpen ? "the MAP is open: close it first"
            : freeSlot === null ? "no empty storage slot" : "Move to slot " + freeSlot;
        mv.addEventListener("click", function () {
          var from = { kind: "map", number: num }, to = { kind: "slot", number: freeSlot };
          mutate({
            renderGen: renderGen, btn: mv, what: "move",
            path: libPath(libId, "/move"), body: { from: from, to: to },
            done: function () { return "Moved " + String(m.barcode || "") + " from " + locText(from) + " to " + locText(to); },
          });
        });
        acts.appendChild(mv);
      }
      cell.appendChild(acts);
      list.appendChild(cell);
    });
    box.appendChild(list);
    return box;
  }

  // slotGridView renders the visual slot grid for an already-filtered slot set.
  function slotGridView(slots) {
    var grid = mk("div", { class: "slot-grid" });
    slots.forEach(function (sl) {
      var full = slotIsOccupied(sl);
      var cell = mk("div", { class: "slot" + (full ? " full" : "") });
      cell.appendChild(mk("span", { class: "n", text: "slot " + coerceNumber(sl.number) }));
      cell.appendChild(mk("span", { class: "bc", text: full ? String(sl.barcode || "(no barcode)") : "—" }));
      grid.appendChild(cell);
    });
    return grid;
  }

  // slotTableView renders the compact table (slot, barcode, status, actions)
  // alternative for an already-filtered slot set. Mount loads an occupied slot
  // into the first empty drive; Export takes it to the first empty MAP slot;
  // Move… picks an empty storage or MAP slot. All need live state.
  function slotTableView(lib, slots) {
    var renderGen = NodeState.generation;
    var emptyDrive = firstEmptyDrive(lib);
    var live = lib.live === true;
    var libId = coerceNumber(lib.id);
    var rows = slots.map(function (sl) {
      var full = slotIsOccupied(sl);
      var num = coerceNumber(sl.number);
      var bc = full ? String(sl.barcode || "") : "";
      var actionCell = mk("td", { class: "acts" });
      if (full) {
        var b = mk("button", { class: "btn sm", text: "Mount", attrs: { type: "button", "aria-label": "Mount slot " + num + " into a drive" } });
        b.disabled = !mutationsEnabled || emptyDrive === null;
        b.title = !live ? "changer not reachable"
          : (emptyDrive === null ? "no empty drive available" : "Mount this slot into drive " + emptyDrive);
        b.addEventListener("click", function () { doMount({ libId: libId, drive: emptyDrive }, { kind: "slot", number: num }, b, renderGen); });
        actionCell.appendChild(b);

        var ex = mk("button", { class: "btn sm", text: "Export", attrs: { type: "button", "aria-label": "Export slot " + num + " to the MAP" } });
        ex.disabled = !mutationsEnabled || !live || lib.map_open === true;
        ex.title = !live ? "changer not reachable"
          : lib.map_open === true ? "the MAP is open: close it first" : "Move to the first empty MAP slot";
        ex.addEventListener("click", function () {
          mutate({
            renderGen: renderGen, btn: ex, what: "export",
            path: libPath(libId, "/map/export"), body: { slot: num },
            done: function (res) { return "Exported" + moveText(res && res.move); },
          });
        });
        actionCell.appendChild(ex);

        var mv = mk("button", { class: "btn sm ghost", text: "Move…", attrs: { type: "button", "aria-label": "Move slot " + num + " to another slot" } });
        mv.disabled = !mutationsEnabled || !live;
        if (!live) { mv.title = "changer not reachable"; }
        mv.addEventListener("click", function () { openMoveDialog(lib, { kind: "slot", number: num, barcode: bc }, renderGen); });
        actionCell.appendChild(mv);
      } else {
        actionCell.appendChild(document.createTextNode("—"));
      }
      return mk("tr", { children: [
        mk("td", { class: "mono", text: String(num) }),
        mk("td", { class: "mono", text: full ? (bc || "(no barcode)") : "—" }),
        mk("td", { text: full ? "occupied" : "empty" }),
        actionCell,
      ] });
    });
    var thead = mk("thead", { children: [mk("tr", { children: [
      mk("th", { text: "Slot" }), mk("th", { text: "Barcode" }), mk("th", { text: "Status" }), mk("th", { text: "Actions" }),
    ] })] });
    return mk("table", { class: "slot-table", children: [thead, mk("tbody", { children: rows })] });
  }

  // renderActiveLibrary renders ONE library (drives, MAP area, and the
  // bounded/filtered slot view with summary counts).
  function renderActiveLibrary(lib) {
    var slots = coerceArray(lib.slots);

    var block = mk("div", { class: "rack" });
    var title = mk("div");
    title.appendChild(mk("b", { text: (lib.vendor || "") + " " + (lib.product || "") }));
    title.appendChild(document.createTextNode(" "));
    title.appendChild(mk("span", { class: "muted mono", text: "#" + coerceNumber(lib.id) + " · " + (lib.serial || "") }));
    block.appendChild(title);
    var ln = nodesBlock(lib.nodes, "changer", "lib" + coerceNumber(lib.id));
    if (ln) { ln.classList.add("changer-nodes"); block.appendChild(ln); }
    if (lib.live !== true) {
      // Without the changer, slots show configured initial contents
      // (library_contents) and drives show no load state.
      block.appendChild(mk("p", {
        class: "muted",
        text: "Live changer state unavailable" + (lib.live_error ? ": " + String(lib.live_error) : "") +
          ". Showing configured contents; mounting is disabled.",
      }));
    }

    var driveRow = mk("div", { class: "drive-row" });
    coerceArray(lib.drives).forEach(function (d) { driveRow.appendChild(driveCard(lib, d)); });
    block.appendChild(driveRow);

    block.appendChild(mapArea(lib));

    // US-F6: per-library summary counts + searchable/filterable bounded slots.
    var counts = countSlots(slots);
    var shown = filterSlots(slots, { query: libView.query, state: libView.state });
    var summaryText = "slots: " + counts.total + " total · " + counts.occupied + " occupied · " + counts.empty + " empty";
    if (shown.length !== counts.total) { summaryText += " · showing " + shown.length; }
    block.appendChild(mk("div", { class: "slot-summary muted", text: summaryText }));

    var scroll = mk("div", { class: "slot-scroll" });
    if (!slots.length) {
      scroll.appendChild(mk("p", { class: "muted", text: "No storage slots." }));
    } else if (!shown.length) {
      scroll.appendChild(mk("p", { class: "muted", text: "No slots match the current search/filter." }));
    } else if (libView.table) {
      scroll.appendChild(slotTableView(lib, shown));
    } else {
      scroll.appendChild(slotGridView(shown));
    }
    block.appendChild(scroll);
    return block;
  }

  function renderLibraries(data) {
    lastLibData = data; // cache so toolbar controls can re-render without refetch
    var libs = coerceArray(data && data.libraries);
    $("lib-count").textContent = libs.length ? "(" + libs.length + ")" : "";
    var rack = $("library-rack");
    var toolbar = $("lib-toolbar");
    var nav = $("library-nav");
    var selector = $("library-selector");
    clear(rack);

    if (!libs.length) {
      if (toolbar) { toolbar.hidden = true; }
      rack.appendChild(mk("p", { class: "muted", text: "No libraries configured." }));
      return;
    }
    if (toolbar) { toolbar.hidden = false; }

    // US-F6 multi-library navigation: a selector appears only when >1 library;
    // otherwise a single library renders directly with the selector hidden.
    if (nav) { nav.hidden = libs.length <= 1; }
    if (selector) {
      clear(selector);
      libs.forEach(function (lib) {
        var id = String(coerceNumber(lib.id));
        selector.appendChild(mk("option", {
          text: "#" + id + " — " + (lib.vendor || "") + " " + (lib.product || ""),
          attrs: { value: id },
        }));
      });
    }

    // Resolve the active library from the current selection, defaulting to the
    // first and repairing a stale selection.
    var active = null;
    if (libView.selected != null) {
      libs.forEach(function (lib) { if (String(coerceNumber(lib.id)) === String(libView.selected)) { active = lib; } });
    }
    if (!active) { active = libs[0]; }
    libView.selected = String(coerceNumber(active.id));
    if (selector) { selector.value = libView.selected; }

    rack.appendChild(renderActiveLibrary(active));
  }

  // rerenderLibraries re-renders from cached data when a toolbar control
  // (search / filter / table toggle / library selector) changes.
  function rerenderLibraries() {
    if (lastLibData) { renderLibraries(lastLibData); }
  }

  // diskStatusInfo maps the backend diskDTO.status enum to a TRUTHFUL label and
  // the format eligibility (US-F8). Only no_mvtl (format) and mvtl (reformat)
  // are formattable; in_use and probe_failed are blocked with a reason.
  function diskStatusInfo(d) {
    switch (String(d.status || "")) {
      case "mvtl":
        return { label: "MVTL cartridge", tone: "mvtl", action: "reformat" };
      case "no_mvtl":
        return { label: "No MVTL (unformatted)", tone: "none", action: "format" };
      case "probe_failed":
        return { label: "Probe failed", tone: "err", action: null,
          reason: "probe failed — the device could not be read, so it cannot be formatted" };
      case "in_use":
        return { label: "In use (mounted)", tone: "inuse", action: null,
          reason: "device is in use (mounted) — unmount it before formatting" };
      default:
        // No/unknown status: fall back to the mvtl_formatted flag.
        return d.mvtl_formatted
          ? { label: "MVTL cartridge", tone: "mvtl", action: "reformat" }
          : { label: "No MVTL (unformatted)", tone: "none", action: "format" };
    }
  }

  function renderDisks(data) {
    var renderGen = NodeState.generation;
    var disks = coerceArray(data && data.disks);
    $("disk-count").textContent = disks.length ? "(" + disks.length + ")" : "";
    var dock = $("disk-dock");
    clear(dock);
    if (!disks.length) {
      dock.appendChild(mk("p", { class: "muted", text: "No candidate disks found." }));
      return;
    }
    disks.forEach(function (d) {
      var mvtl = d.mvtl_formatted && d.mvtl ? d.mvtl : null;
      var info = diskStatusInfo(d);
      var row = mk("div", { class: "disk" });

      var head = mk("div", { class: "head" });
      head.appendChild(mk("span", { class: "name", text: d.name || d.path || "?" }));
      // Distinct, truthful status label (US-F8).
      head.appendChild(mk("span", { class: "tag status " + info.tone, text: info.label }));
      head.appendChild(mk("span", { class: "muted", text: fmtBytes(d.size_bytes) }));
      if (mvtl) {
        head.appendChild(mk("span", {
          class: "muted mono",
          text: (mvtl.barcode || "") + " · " + (mvtl.vendor_id || "") + " " + (mvtl.product_id || ""),
        }));
      }

      // Format action: offered only for eligible media (US-F8).
      var fmtBtn;
      if (info.action === "format") {
        fmtBtn = mk("button", { class: "btn sm", text: "Format…", attrs: { type: "button", "aria-label": "Format " + (d.path || d.name || "disk") } });
        fmtBtn.disabled = !mutationsEnabled;
        fmtBtn.addEventListener("click", function () { openFormatDialog(d, renderGen); });
      } else if (info.action === "reformat") {
        fmtBtn = mk("button", { class: "btn sm", text: "Reformat…", attrs: { type: "button", "aria-label": "Reformat (erase) " + (d.path || d.name || "disk") } });
        fmtBtn.disabled = !mutationsEnabled;
        fmtBtn.addEventListener("click", function () { openFormatDialog(d, renderGen); });
      } else {
        fmtBtn = mk("button", { class: "btn sm", text: "Format…", attrs: { type: "button", title: info.reason, "aria-label": info.reason } });
        fmtBtn.disabled = true; // in_use / probe_failed: not formattable
      }
      head.appendChild(fmtBtn);
      row.appendChild(head);

      // Capacity relabel (US-F7): explicit "provisioned <X> / device <Y>", never
      // a misleading "used %". The bar is only shown when a cartridge provisions
      // a known capacity; its ARIA names it as provisioned-vs-device.
      if (mvtl) {
        var prov = coerceNumber(mvtl.total_capacity_bytes);
        var dev = coerceNumber(d.size_bytes);
        row.appendChild(mk("div", {
          class: "cap-line muted mono",
          text: "provisioned " + fmtBytes(prov) + " / device " + fmtBytes(dev),
        }));
        var pct = dev > 0 ? Math.min(100, Math.round((prov / dev) * 100)) : 0;
        var bar = mk("div", {
          class: "bar",
          attrs: {
            role: "progressbar",
            "aria-valuenow": String(prov),
            "aria-valuemin": "0",
            "aria-valuemax": String(dev),
            "aria-label": "cartridge provisioned capacity relative to device size",
          },
        });
        bar.appendChild(mk("span", { attrs: { style: "width:" + pct + "%" } }));
        row.appendChild(bar);
      }
      dock.appendChild(row);
    });
  }

  /* ---- changer / media mutations --------------------------------------- */
  function libPath(libId, suffix) {
    return "/api/v1/libraries/" + encodeURIComponent(coerceNumber(libId)) + suffix;
  }

  // moveText renders a {from, to, barcode} changer move for a toast.
  function moveText(mv) {
    if (!mv) { return ""; }
    return (mv.barcode ? " " + String(mv.barcode) : "") +
      (mv.from ? " from " + locText(mv.from) : "") + (mv.to ? " to " + locText(mv.to) : "");
  }

  function closeDialog(id) {
    var dlg = $(id);
    if (dlg && dlg.open && dlg.close) { dlg.close(); }
  }

  // mutate sends one library/media mutation behind the same generation gate
  // as mount/unmount, then toasts and refreshes every panel. o is
  // {renderGen, path, method (POST), body, btn, what, done(res) -> toast text,
  // status (PanelStatus, default libStatus), errEl (dialog error line: errors
  // stay in the open dialog), dialog (id closed on success)}.
  function mutate(o) {
    var status = o.status || libStatus;
    if (!canSendMutation(o.renderGen, NodeState.generation, mutationsEnabled)) {
      if (o.dialog) { closeDialog(o.dialog); }
      status.error("Node changed — refresh before " + o.what + ".");
      return;
    }
    var gen = NodeState.generation;
    if (o.btn) { o.btn.disabled = true; }
    if (o.errEl) { o.errEl.textContent = ""; }
    api(o.path, { method: o.method || "POST", body: o.body })
      .then(function (res) {
        if (isStale(gen, NodeState.generation)) { return; }
        if (o.dialog) { closeDialog(o.dialog); }
        var msg = o.done ? o.done(res) : "";
        if (msg) { toast(msg); }
        refresh();
      })
      .catch(function (e) {
        if (o.btn) { o.btn.disabled = false; }
        if (isStale(gen, NodeState.generation)) { return; }
        var m = o.what.charAt(0).toUpperCase() + o.what.slice(1) + " failed: " + e.message;
        if (o.errEl) { o.errEl.textContent = m; } else { status.error(m); }
      });
  }

  /* ---- confirm dialog (a <dialog>, never the browser prompt) ------------ */
  var confirmCtx = null; // function run on confirm
  function openConfirm(title, msg, okLabel, onOk) {
    confirmCtx = onOk;
    $("confirm-dialog-title").textContent = title;
    $("confirm-dialog-msg").textContent = msg;
    $("confirm-ok").textContent = okLabel || "OK";
    var dlg = $("confirm-dialog");
    if (dlg.showModal) { dlg.showModal(); }
  }

  /* ---- media panel ----------------------------------------------------- */
  // activeLib returns the cached /libraries entry for the selected library.
  function activeLib() {
    var found = null;
    coerceArray(lastLibData && lastLibData.libraries).forEach(function (lib) {
      if (String(coerceNumber(lib.id)) === String(libView.selected)) { found = lib; }
    });
    return found;
  }

  function mediaButton(text, label, title, disabled, onClick) {
    var b = mk("button", { class: "btn sm", text: text, attrs: { type: "button", "aria-label": label } });
    b.disabled = !!disabled;
    if (title) { b.title = title; }
    b.addEventListener("click", function () { onClick(b); });
    return b;
  }

  // renderMedia draws the cached /media payload of the selected library.
  function renderMedia() {
    var box = $("media-list");
    clear(box);
    var newBtn = $("media-new-btn");
    if (!lastMedia) {
      $("media-count").textContent = "";
      if (newBtn) { newBtn.disabled = true; }
      box.appendChild(mk("p", { class: "muted", text: "No library selected." }));
      return;
    }
    var renderGen = lastMedia.gen;
    var d = lastMedia.data || {};
    var libId = lastMedia.libId;
    var lib = activeLib();
    var drives = lib ? lib.drives : [];
    var live = d.live === true;
    var mapOpen = !!(lib && lib.map_open === true);
    var carts = coerceArray(d.cartridges);
    $("media-count").textContent = "(" + carts.length + ")";
    if (newBtn) { newBtn.disabled = !mutationsEnabled; }

    var shown = filterMedia(carts, mediaView);
    box.appendChild(mk("div", {
      class: "slot-summary muted",
      text: "library #" + libId + " · media in " + String(d.home_dir || "?") +
        (shown.length !== carts.length ? " · showing " + shown.length : ""),
    }));
    if (!live) {
      box.appendChild(mk("p", {
        class: "muted",
        text: "Live changer state unavailable" + (d.live_error ? ": " + String(d.live_error) : "") +
          ". Locations are unknown; editing, import and export are disabled.",
      }));
    }
    if (!carts.length) {
      box.appendChild(mk("p", { class: "muted", text: "No media in this library's home directory." }));
      return;
    }
    if (!shown.length) {
      box.appendChild(mk("p", { class: "muted", text: "No media match the current search/filter." }));
      return;
    }
    var rows = [];
    shown.forEach(function (c) {
      var bc = String(c.barcode || "");
      var loc = c.location || null;
      var kind = loc ? loc.kind : "";
      var cap = coerceNumber(c.capacity_bytes);
      var capCell = mk("td", { class: "cap mono" });
      if (cap > 0) {
        var used = Math.max(0, cap - coerceNumber(c.remaining_bytes));
        capCell.appendChild(mk("div", { text: fmtBytes(used) + " / " + fmtBytes(cap), attrs: { title: "used as of the last unload" } }));
        var bar = mk("div", { class: "bar", attrs: {
          role: "progressbar", "aria-valuenow": String(used), "aria-valuemin": "0", "aria-valuemax": String(cap),
          "aria-label": "capacity used by " + bc,
        } });
        bar.appendChild(mk("span", { attrs: { style: "width:" + Math.min(100, Math.round(used / cap * 100)) + "%" } }));
        capCell.appendChild(bar);
      } else {
        capCell.textContent = "—";
      }

      var acts = mk("td", { class: "acts" });
      var editWhy = !live ? "changer not reachable: cannot confirm the cartridge is not in a drive"
        : kind === "drive" ? "in a drive: unload it first"
          : !c.backend ? "no media on disk"
            : c.media_absent ? "its disk is not attached" : "";
      acts.appendChild(mediaButton("Edit", "Edit " + bc, editWhy || "Change write protect, type, density or size",
        !mutationsEnabled || !!editWhy, function () { openEditDialog(libId, c, renderGen); }));
      if (kind === "shelf") {
        var importWhy = !c.backend ? "no media on disk" : c.media_absent ? "its disk is not attached" : "";
        acts.appendChild(mediaButton("Import", "Import " + bc + " into the library", importWhy || "Put it into the MAP",
          !mutationsEnabled || !!importWhy, function () { openImportDialog(libId, renderGen, bc); }));
      } else if (kind === "slot" || kind === "drive") {
        acts.appendChild(mediaButton("Export", "Export " + bc + " to the MAP",
          mapOpen ? "the MAP is open: close it first" : "Move to the first empty MAP slot",
          !mutationsEnabled || !live || mapOpen, function (btn) {
            mutate({
              renderGen: renderGen, btn: btn, what: "export", status: mediaStatus,
              path: libPath(libId, "/map/export"), body: { barcode: bc },
              done: function (res) { return "Exported" + moveText(res && res.move); },
            });
          }));
      }

      var locBadge = mk("span", { class: "badge loc-" + (kind || "unknown"), text: mediaLocText(loc, drives) });
      var note = c.error ? "⚠ " + String(c.error)
        : c.media_absent ? "Disk not attached: the bay is empty or the disk was removed." : "";
      var tr = mk("tr", { class: note ? "has-note" : "", children: [
        mk("td", { class: "mono", text: bc }),
        mk("td", { text: c.type || "—" }),
        mk("td", { class: "mono", text: c.density || "—", attrs: c.description ? { title: String(c.description) } : null }),
        capCell,
        mk("td", { children: [c.write_protected ? mk("span", { class: "badge wp", text: "WP", attrs: { title: "write protected" } }) : document.createTextNode("—")] }),
        mk("td", { children: [locBadge] }),
        mk("td", { class: "mono", text: String(coerceNumber(c.load_count)) }),
        acts,
      ] });
      rows.push(tr);
      if (note) {
        rows.push(mk("tr", { class: "row-note", children: [mk("td", { text: note, attrs: { colspan: "8" } })] }));
      }
    });
    var thead = mk("thead", { children: [mk("tr", { children: [
      mk("th", { text: "Barcode" }), mk("th", { text: "Type" }), mk("th", { text: "Density" }),
      mk("th", { text: "Used / capacity" }), mk("th", { text: "WP" }), mk("th", { text: "Location" }),
      mk("th", { text: "Loads" }), mk("th", { text: "Actions" }),
    ] })] });
    box.appendChild(mk("div", { class: "media-scroll", children: [
      mk("table", { class: "slot-table media-table", children: [thead, mk("tbody", { children: rows })] }),
    ] }));
  }

  /* ---- create media dialog --------------------------------------------- */
  var createCtx = null; // { libId, renderGen }
  function fillSelect(sel, values, first) {
    clear(sel);
    if (first) { sel.appendChild(mk("option", { text: first.text, attrs: { value: first.value } })); }
    coerceArray(values).forEach(function (v) { sel.appendChild(mk("option", { text: String(v), attrs: { value: String(v) } })); });
  }

  function openCreateDialog() {
    if (!lastMedia || !canSendMutation(lastMedia.gen, NodeState.generation, mutationsEnabled)) {
      mediaStatus.error("Node changed — refresh before creating media.");
      return;
    }
    var d = lastMedia.data || {};
    createCtx = { libId: lastMedia.libId, renderGen: lastMedia.gen };
    $("media-create-target").textContent = "Library #" + lastMedia.libId + " · media in " + String(d.home_dir || "?");
    var typeSel = $("mc-type"), denSel = $("mc-density");
    var keepType = typeSel.value, keepDen = denSel.value;
    fillSelect(typeSel, d.types);
    fillSelect(denSel, d.densities);
    var types = coerceArray(d.types).map(String), dens = coerceArray(d.densities).map(String);
    typeSel.value = types.indexOf(keepType) >= 0 ? keepType : (types.indexOf("data") >= 0 ? "data" : types[0] || "");
    denSel.value = dens.indexOf(keepDen) >= 0 ? keepDen : (dens.indexOf("LTO8") >= 0 ? "LTO8" : dens[0] || "");
    var place = $("mc-place");
    place.disabled = d.live !== true;
    if (d.live !== true) { place.checked = false; }
    $("mc-error").textContent = "";
    updateCreatePreview();
    var dlg = $("media-create-dialog");
    if (dlg.showModal) { dlg.showModal(); }
  }

  // createRequest reads the create form: {barcodes, error, body}.
  function createRequest() {
    var range = $("mc-mode-range").checked;
    var res;
    var body = {};
    if (range) {
      body = {
        prefix: $("mc-prefix").value.trim(), suffix: $("mc-suffix").value.trim(),
        start: Number($("mc-start").value || 0), count: Number($("mc-count").value || 0), width: Number($("mc-width").value || 0),
      };
      res = expandBarcodes({ prefix: body.prefix, suffix: body.suffix, start: $("mc-start").value, count: $("mc-count").value, width: $("mc-width").value });
    } else {
      var bc = $("mc-barcode").value.trim();
      body.barcode = bc;
      res = !bc ? { barcodes: [], error: "enter a barcode" }
        : validBarcode(bc) ? { barcodes: [bc], error: null } : { barcodes: [], error: "barcode must be 1-16 letters or digits" };
    }
    var size = parseSizeMB($("mc-size").value);
    if (!res.error && size.error) { res = { barcodes: [], error: size.error }; }
    if (!res.error && !$("mc-density").value) { res = { barcodes: [], error: "choose a density" }; }
    body.type = $("mc-type").value || "data";
    body.density = $("mc-density").value;
    body.size_mb = size.value || 0;
    body.place = $("mc-place").checked && !$("mc-place").disabled;
    return { barcodes: res.barcodes, error: res.error, body: body };
  }

  function updateCreatePreview() {
    var range = $("mc-mode-range").checked;
    $("mc-range").hidden = !range;
    $("mc-single").hidden = range;
    var req = createRequest();
    var pv = $("mc-preview");
    if (req.error) {
      pv.className = "preview err";
      pv.textContent = req.error;
    } else {
      var b = req.barcodes;
      pv.className = "preview";
      pv.textContent = (b.length === 1 ? "Will create " : "Will create " + b.length + ": ") +
        (b.length <= 4 ? b.join(", ") : b.slice(0, 3).join(", ") + " … " + b[b.length - 1]);
    }
    $("mc-submit").disabled = !!req.error || !mutationsEnabled;
  }

  function doCreate() {
    if (!createCtx) { return; }
    var req = createRequest();
    if (req.error) { updateCreatePreview(); return; }
    mutate({
      renderGen: createCtx.renderGen, btn: $("mc-submit"), what: "create media", status: mediaStatus,
      errEl: $("mc-error"), dialog: "media-create-dialog",
      path: libPath(createCtx.libId, "/media"), body: req.body,
      done: function (res) {
        var created = coerceArray(res && res.created);
        var msg = "Created " + created.length + " cartridge" + (created.length === 1 ? "" : "s");
        var pl = res && res.place;
        if (pl) {
          msg += ", placed " + coerceArray(pl.placed).length;
          if (pl.error) { mediaStatus.warn("Placing stopped: " + String(pl.error)); }
        }
        return msg;
      },
    });
  }

  /* ---- edit media dialog ----------------------------------------------- */
  var editCtx = null; // { libId, cart, renderGen }
  function openEditDialog(libId, cart, renderGen) {
    if (!canSendMutation(renderGen, NodeState.generation, mutationsEnabled)) {
      mediaStatus.error("Node changed — refresh before editing media.");
      return;
    }
    var d = (lastMedia && lastMedia.data) || {};
    editCtx = { libId: libId, cart: cart, renderGen: renderGen };
    $("media-edit-title").textContent = "Edit " + String(cart.barcode || "");
    var cap = coerceNumber(cart.capacity_bytes);
    $("me-current").textContent = "Now: " + (cart.type || "?") + " · " + (cart.density || "?") +
      (cart.description ? " (" + String(cart.description) + ")" : "") +
      (cap > 0 ? " · " + fmtBytes(cap) : "") + (cart.write_protected ? " · write protected" : "");
    $("me-wp").checked = !!cart.write_protected;
    fillSelect($("me-type"), d.types, { value: "", text: "keep current" + (cart.type ? " (" + cart.type + ")" : "") });
    fillSelect($("me-density"), d.densities, { value: "", text: "keep current" + (cart.density ? " (" + cart.density + ")" : "") });
    $("me-size").value = "";
    $("me-error").textContent = "";
    $("me-submit").disabled = !mutationsEnabled;
    var dlg = $("media-edit-dialog");
    if (dlg.showModal) { dlg.showModal(); }
  }

  function doEdit() {
    if (!editCtx) { return; }
    var ctx = editCtx;
    var body;
    try {
      body = buildMediaPatch(ctx.cart, {
        write_protect: $("me-wp").checked, type: $("me-type").value,
        density: $("me-density").value, size_mb: $("me-size").value,
      });
    } catch (e) {
      $("me-error").textContent = e.message;
      return;
    }
    if (!body) { $("me-error").textContent = "Nothing changed."; return; }
    var bc = String(ctx.cart.barcode || "");
    mutate({
      renderGen: ctx.renderGen, btn: $("me-submit"), what: "edit " + bc, status: mediaStatus,
      errEl: $("me-error"), dialog: "media-edit-dialog", method: "PATCH",
      path: libPath(ctx.libId, "/media/" + encodeURIComponent(bc)), body: body,
      done: function () { return "Updated " + bc; },
    });
  }

  /* ---- import dialog --------------------------------------------------- */
  // openImportDialog lists shelf cartridges of libId (or just `only`) from the
  // cached media list; picking one imports it through the MAP.
  function openImportDialog(libId, renderGen, only) {
    if (!canSendMutation(renderGen, NodeState.generation, mutationsEnabled)) {
      libStatus.error("Node changed — refresh before importing.");
      return;
    }
    libId = String(coerceNumber(libId));
    $("import-dialog-target").textContent = only
      ? "Put " + only + " into an empty MAP slot of library #" + libId + ". Pick it below to import it."
      : "Pick a shelf cartridge to put into an empty MAP slot of library #" + libId + ".";
    $("import-store").checked = true;
    $("import-error").textContent = "";
    var list = $("import-list");
    clear(list);
    var carts = lastMedia && lastMedia.libId === libId ? coerceArray(lastMedia.data && lastMedia.data.cartridges) : [];
    var shelf = filterMedia(carts, { where: "shelf" }).filter(function (c) {
      return c.backend && (!only || String(c.barcode) === only);
    });
    if (!lastMedia || lastMedia.libId !== libId) {
      list.appendChild(mk("p", { class: "muted", text: "The media list of this library is not loaded yet; refresh and try again." }));
    } else if (!shelf.length) {
      list.appendChild(mk("p", { class: "muted", text: "No cartridges on the shelf. Create media, or eject cartridges from the MAP." }));
    }
    shelf.forEach(function (c) {
      var bc = String(c.barcode);
      var btn = mk("button", { class: "btn slot-pick", attrs: { type: "button", "aria-label": "Import " + bc }, children: [
        mk("span", { class: "bc mono", text: bc }),
        mk("span", { class: "n mono", text: (c.type || "") + " " + (c.density || "") }),
      ] });
      btn.addEventListener("click", function () {
        var store = $("import-store").checked;
        mutate({
          renderGen: renderGen, btn: btn, what: "import " + bc, status: mediaStatus,
          errEl: $("import-error"), dialog: "import-dialog",
          path: libPath(libId, "/map/import"), body: { barcode: bc, store: store },
          done: function (res) {
            if (res && res.store_error) { mediaStatus.warn("Imported " + bc + " but not stored: " + String(res.store_error)); }
            return res && res.move ? "Imported" + moveText(res.move) : "Imported " + bc + " into the MAP";
          },
        });
      });
      list.appendChild(btn);
    });
    var dlg = $("import-dialog");
    if (dlg.showModal) { dlg.showModal(); }
  }

  /* ---- move dialog ----------------------------------------------------- */
  var moveCtx = null; // { libId, from, barcode, renderGen }
  function openMoveDialog(lib, from, renderGen) {
    if (!canSendMutation(renderGen, NodeState.generation, mutationsEnabled)) {
      libStatus.error("Node changed — refresh before moving.");
      return;
    }
    moveCtx = { libId: coerceNumber(lib.id), from: { kind: from.kind, number: from.number }, barcode: from.barcode, renderGen: renderGen };
    $("move-dialog-source").textContent = "Library #" + moveCtx.libId + ": " + (from.barcode || "(no barcode)") + " in " + locText(from) + ".";
    var sel = $("move-dest");
    clear(sel);
    var dests = emptyLocations(lib);
    dests.forEach(function (loc) {
      sel.appendChild(mk("option", { text: loc.kind === "map" ? "MAP " + loc.number + " (import/export)" : "slot " + loc.number, attrs: { value: loc.kind + ":" + loc.number } }));
    });
    $("move-error").textContent = dests.length ? "" : "No empty storage or MAP slot.";
    $("move-submit").disabled = !dests.length || !mutationsEnabled;
    var dlg = $("move-dialog");
    if (dlg.showModal) { dlg.showModal(); }
  }

  function doMove() {
    if (!moveCtx) { return; }
    var ctx = moveCtx;
    var parts = String($("move-dest").value).split(":");
    var to = { kind: parts[0], number: coerceNumber(parts[1]) };
    if ((to.kind !== "slot" && to.kind !== "map") || to.number <= 0) { return; }
    mutate({
      renderGen: ctx.renderGen, btn: $("move-submit"), what: "move",
      errEl: $("move-error"), dialog: "move-dialog",
      path: libPath(ctx.libId, "/move"), body: { from: ctx.from, to: to },
      done: function () { return "Moved " + (ctx.barcode || "") + " from " + locText(ctx.from) + " to " + locText(to); },
    });
  }

  /* ---- drive statistics ------------------------------------------------ */
  var statsCtx = null; // { libId, driveId, loaded, seq }
  var statsSeq = 0;
  // setStatsDrive records whether the drive holds a cartridge (its volume
  // statistics are stale otherwise) and titles the dialog accordingly.
  function setStatsDrive(lib, d) {
    var st = driveState(lib, d);
    statsCtx.loaded = st.state === "loaded";
    $("stats-dialog-title").textContent = "Drive " + statsCtx.driveId + " statistics" +
      (st.state === "loaded" ? " · " + (st.barcode || "(no barcode)") : st.state === "empty" ? " · empty" : "");
  }
  function openStats(lib, d) {
    statsCtx = { libId: coerceNumber(lib.id), driveId: coerceNumber(d.id), loaded: false };
    setStatsDrive(lib, d);
    var dlg = $("stats-dialog");
    if (dlg.showModal && !dlg.open) { dlg.showModal(); }
    loadStats();
  }

  function loadStats() {
    if (!statsCtx) { return; }
    var ctx = statsCtx;
    var seq = ++statsSeq;
    var gen = NodeState.generation;
    var body = $("stats-body");
    clear(body);
    body.appendChild(mk("p", { class: "muted", text: "Reading log pages…" }));
    // Re-read the drive's load state first: the cartridge may have been
    // unloaded since the dialog opened, and an empty drive's volume page
    // still carries the last cartridge's figures.
    api("/api/v1/libraries")
      .then(function (data) {
        if (isStale(gen, NodeState.generation) || seq !== statsSeq) { return; }
        coerceArray(data && data.libraries).forEach(function (lib) {
          if (coerceNumber(lib.id) !== ctx.libId) { return; }
          coerceArray(lib.drives).forEach(function (d) {
            if (coerceNumber(d.id) === ctx.driveId) { setStatsDrive(lib, d); }
          });
        });
      }, function () { /* keep the state the dialog opened with */ })
      .then(function () {
        if (isStale(gen, NodeState.generation) || seq !== statsSeq) { return null; }
        return api(libPath(ctx.libId, "/drives/" + encodeURIComponent(ctx.driveId) + "/stats"));
      })
      .then(function (s) {
        if (s === null || isStale(gen, NodeState.generation) || seq !== statsSeq) { return; }
        renderStats(s || {}, ctx.loaded);
      })
      .catch(function (e) {
        if (isStale(gen, NodeState.generation) || seq !== statsSeq) { return; }
        clear(body);
        var msg = e.status === 409 ? "The drive is reserved by another initiator: " + e.message
          : e.status === 503 ? "Statistics unavailable: " + e.message : "Reading statistics failed: " + e.message;
        body.appendChild(mk("p", { class: "error full", text: msg }));
      });
  }

  // statNum formats an optional counter; null when absent.
  function statNum(x) { return x == null ? null : coerceNumber(x).toLocaleString(); }
  function statMiB(x) { return x == null ? null : fmtBytes(coerceNumber(x) * 1048576); }
  function statBytes(x) { return x == null ? null : fmtBytes(x); }
  function statMB(x) { return x == null ? null : coerceNumber(x).toLocaleString() + " MB"; }
  function statBool(x) { return x == null ? null : (x ? "yes" : "no"); }

  // statsSection builds one titled key/value table, skipping absent values;
  // null when nothing is present.
  function statsSection(title, rows, extra) {
    var present = rows.filter(function (r) { return r[1] != null; });
    if (!present.length && !extra) { return null; }
    var sec = mk("section");
    sec.appendChild(mk("h4", { text: title }));
    if (present.length) {
      sec.appendChild(mk("table", { children: [mk("tbody", { children: present.map(function (r) { return kvRow(r[0], r[1]); }) })] }));
    }
    if (extra) { sec.appendChild(extra); }
    return sec;
  }

  function renderStats(s, loaded) {
    var body = $("stats-body");
    clear(body);
    var seq = s.sequential || {};
    var sections = [];
    sections.push(statsSection("Data", [
      ["Host written", statBytes(seq.host_bytes_written)],
      ["Medium written", statBytes(seq.medium_bytes_written)],
      ["Write compression", seq.host_bytes_written != null || seq.write_compression_ratio != null ? fmtRatio(seq.write_compression_ratio) : null],
      ["Host read", statBytes(seq.host_bytes_read)],
      ["Medium read", statBytes(seq.medium_bytes_read)],
      ["Read compression", seq.host_bytes_read != null || seq.read_compression_ratio != null ? fmtRatio(seq.read_compression_ratio) : null],
    ]));

    var posBar = null;
    if (seq.capacity_mib != null && seq.position_mib != null && coerceNumber(seq.capacity_mib) > 0) {
      var capM = coerceNumber(seq.capacity_mib), posM = coerceNumber(seq.position_mib);
      posBar = mk("div", { class: "bar", attrs: {
        role: "progressbar", "aria-valuenow": String(posM), "aria-valuemin": "0", "aria-valuemax": String(capM),
        "aria-label": "tape position relative to capacity",
      } });
      posBar.appendChild(mk("span", { attrs: { style: "width:" + Math.min(100, Math.round(posM / capM * 100)) + "%" } }));
    }
    sections.push(statsSection("Position / capacity", [
      ["Position", statMiB(seq.position_mib)],
      ["Capacity", statMiB(seq.capacity_mib)],
      ["To early warning", statMiB(seq.capacity_to_ew_mib)],
      ["Early warning to LEOP", statMiB(seq.ew_to_leop_mib)],
    ], posBar));

    if (loaded) {
      var v = s.volume || {};
      sections.push(statsSection("Volume", [
        ["Barcode", v.barcode || null],
        ["Serial", v.serial_number || null],
        ["Mounts", statNum(v.mounts)],
        ["Datasets written / read", v.datasets_written != null || v.datasets_read != null
          ? (statNum(v.datasets_written) || "—") + " / " + (statNum(v.datasets_read) || "—") : null],
        ["Last mount written", statMB(v.last_mount_mb_written)],
        ["Last mount read", statMB(v.last_mount_mb_read)],
        ["Lifetime written", statMB(v.lifetime_mb_written)],
        ["Lifetime read", statMB(v.lifetime_mb_read)],
        ["Native capacity", statMiB(v.native_capacity)],
        ["Used native capacity", statMiB(v.used_native_capacity)],
        ["Write protected", statBool(v.write_protect)],
        ["WORM", statBool(v.worm)],
      ]));
    } else if (s.volume) {
      sections.push(statsSection("Volume", [], mk("p", { class: "muted", text: "No cartridge loaded: the drive still reports the previous cartridge's volume statistics, so they are not shown." })));
    }

    var we = s.write_errors, re = s.read_errors;
    if (we || re) {
      var counters = [
        ["Corrected without delay", "corrected_without_delay"], ["Corrected with delay", "corrected_with_delay"],
        ["Retries", "retries"], ["Corrected", "corrected"], ["Algorithm invocations", "algorithm_invocations"],
        ["Bytes processed", "bytes_processed"], ["Uncorrected", "uncorrected"],
      ];
      var rows = counters.map(function (c) {
        return mk("tr", { children: [
          mk("th", { text: c[0] }),
          mk("td", { text: (we && statNum(we[c[1]])) || "—" }),
          mk("td", { text: (re && statNum(re[c[1]])) || "—" }),
        ] });
      });
      var errSec = mk("section");
      errSec.appendChild(mk("h4", { text: "Errors" }));
      errSec.appendChild(mk("table", { children: [
        mk("thead", { children: [mk("tr", { children: [mk("th", { text: "" }), mk("th", { text: "Write" }), mk("th", { text: "Read" })] })] }),
        mk("tbody", { children: rows }),
      ] }));
      sections.push(errSec);
    }

    var dev = s.device_statistics || {};
    sections.push(statsSection("Device", [
      ["Volume loads", statNum(dev.volume_loads)],
      ["Cleaning operations", statNum(dev.cleaning_operations)],
      ["Power-on hours", statNum(dev.power_on_hours)],
      ["Medium motion hours", statNum(dev.medium_motion_hours)],
      ["Metres processed", statNum(dev.metres_processed)],
      ["Temperature", s.temperature_c != null ? coerceNumber(s.temperature_c) + " °C" : null],
    ]));

    var pe = s.page_errors && typeof s.page_errors === "object" ? s.page_errors : null;
    if (pe && Object.keys(pe).length) {
      sections.push(statsSection("Unreadable log pages", Object.keys(pe).sort().map(function (k) { return ["page " + k, String(pe[k])]; })));
    }

    sections.forEach(function (sec) { if (sec) { body.appendChild(sec); } });
    if (!body.firstChild) { body.appendChild(mk("p", { class: "muted", text: "The drive reported no statistics." })); }
    body.appendChild(mk("p", {
      class: "muted full footnote",
      text: "device " + String(s.device || "?") + " · log pages " + coerceArray(s.supported_pages).map(String).join(" "),
    }));
  }

  /* ---- mount picker ---------------------------------------------------- */
  var mountCtx = null; // { libId, drive, renderGen }
  function openMountPicker(lib, drive, renderGen) {
    // Hard gate: refuse to open the picker (which would then target the current
    // node) when this control is stale or mutations are disabled.
    if (!canSendMutation(renderGen, NodeState.generation, mutationsEnabled)) {
      libStatus.error("Node changed — refresh before mounting.");
      return;
    }
    mountCtx = { libId: coerceNumber(lib.id), drive: coerceNumber(drive), renderGen: renderGen };
    var dlg = $("mount-dialog");
    $("mount-dialog-target").textContent =
      "Library #" + coerceNumber(lib.id) + " → drive " + coerceNumber(drive) +
      ". Choose an occupied storage or MAP slot to load.";
    var list = $("mount-slot-list");
    clear(list);
    var candidates = mountCandidates(lib); // live changer view only
    if (!candidates.length) {
      list.appendChild(mk("p", { class: "muted", text: "No occupied storage or MAP slots to mount." }));
    } else {
      candidates.forEach(function (c) {
        var btn = mk("button", {
          class: "btn slot-pick",
          attrs: { type: "button" },
          children: [
            mk("span", { class: "n mono", text: locText(c) }),
            mk("span", { class: "bc mono", text: c.barcode || "(no barcode)" }),
          ],
        });
        btn.addEventListener("click", function () { doMount(mountCtx, c, btn, mountCtx.renderGen); });
        list.appendChild(btn);
      });
    }
    if (dlg.showModal) { dlg.showModal(); }
  }

  // doMount moves the cartridge at source ({kind: "slot"|"map", number}) into
  // ctx.drive through the library's changer.
  function doMount(ctx, source, btn, renderGen) {
    if (!ctx) { return; }
    // Hard generation gate: drop the send if the node switched after this
    // control was rendered, so a stale previous-node button can never mount on
    // the newly-selected node.
    if (!canSendMutation(renderGen, NodeState.generation, mutationsEnabled)) {
      libStatus.error("Node changed — refresh before mounting.");
      return;
    }
    var gen = NodeState.generation;
    var libId = ctx.libId, drive = ctx.drive;
    var bodyObj;
    try {
      bodyObj = buildMountBody(drive, source.number, source.kind); // throws unless number > 0
    } catch (e) {
      libStatus.error("Mount aborted: " + e.message);
      return;
    }
    if (btn) { btn.disabled = true; }
    api("/api/v1/libraries/" + encodeURIComponent(libId) + "/mount", { method: "POST", body: bodyObj })
      .then(function () {
        if (isStale(gen, NodeState.generation)) { return; } // dropped: node changed
        var dlg = $("mount-dialog");
        if (dlg.open && dlg.close) { dlg.close(); }
        toast("Drive " + drive + " mounted " + locText(source));
        refresh();
      })
      .catch(function (e) {
        if (btn) { btn.disabled = false; }
        if (isStale(gen, NodeState.generation)) { return; }
        libStatus.error("Mount failed: " + e.message);
      });
  }

  // doUnmount returns the drive's cartridge through the changer; the server
  // picks the destination (its source slot, else the first empty slot).
  function doUnmount(libId, drive, btn, renderGen) {
    if (!canSendMutation(renderGen, NodeState.generation, mutationsEnabled)) {
      libStatus.error("Node changed — refresh before unmounting.");
      return;
    }
    var gen = NodeState.generation;
    btn.disabled = true;
    api("/api/v1/libraries/" + encodeURIComponent(coerceNumber(libId)) + "/unmount", { method: "POST", body: { drive: coerceNumber(drive) } })
      .then(function (res) {
        if (isStale(gen, NodeState.generation)) { return; }
        var mv = res && res.move;
        var dest = mv && mv.to ? " to " + locText(mv.to) : "";
        var bc = mv && mv.barcode ? " " + String(mv.barcode) : "";
        toast("Drive " + coerceNumber(drive) + " unloaded" + bc + dest);
        refresh();
      })
      .catch(function (e) {
        btn.disabled = false;
        if (isStale(gen, NodeState.generation)) { return; }
        libStatus.error("Unmount failed: " + e.message);
      });
  }

  /* ---- format dialog --------------------------------------------------- */
  var formatCtx = null; // { device, serial, force, renderGen }
  function openFormatDialog(disk, renderGen) {
    // Hard gate BEFORE opening the dialog with a device path: a stale control
    // (rendered under a previous node) must never pre-load the dialog with that
    // node's /dev/sdX and then format the newly-selected node's disk.
    if (!canSendMutation(renderGen, NodeState.generation, mutationsEnabled)) {
      diskStatus.error("Node changed — refresh before formatting.");
      return;
    }
    var device = String(disk.path || disk.name || "");
    var serial = String(disk.serial || "");
    var mvtl = disk.mvtl_formatted && disk.mvtl ? disk.mvtl : null;
    formatCtx = { device: device, serial: serial, force: !!(disk.mvtl_formatted), renderGen: renderGen };

    var details = $("format-details");
    clear(details);
    var tbody = mk("tbody", { children: [
      kvRow("Device", device || "—"),
      kvRow("Model", disk.model || "—"),
      kvRow("Serial", serial || "—"),
      kvRow("Capacity", fmtBytes(disk.size_bytes)),
      kvRow("Target node", NodeState.origin ? NodeState.origin : "Local"),
      kvRow("Current format", mvtl
        ? ("MVTL " + (mvtl.barcode || "") + " (" + (mvtl.vendor_id || "") + " " + (mvtl.product_id || "") + ")")
        : "unformatted"),
    ] });
    details.appendChild(mk("table", { children: [tbody] }));

    $("format-danger").textContent = mvtl
      ? "DATA LOSS: this device is already MVTL-formatted. Reformatting (force) permanently erases the existing cartridge, its index table and all backup blocks."
      : "DATA LOSS: formatting permanently erases ALL data on this device. This cannot be undone.";

    var input = $("format-confirm-input");
    input.value = "";
    input.placeholder = device;
    var confirmBtn = $("format-confirm-btn");
    confirmBtn.disabled = true;
    $("format-barcode").value = "";

    var dlg = $("format-dialog");
    if (dlg.showModal) { dlg.showModal(); }
    input.focus();
  }

  // Enable the confirm button only when the typed value matches the device path
  // (or the device serial, when present).
  function onFormatConfirmInput() {
    if (!formatCtx) { return; }
    var typed = $("format-confirm-input").value.trim();
    var ok = typed.length > 0 && (typed === formatCtx.device || (formatCtx.serial && typed === formatCtx.serial));
    $("format-confirm-btn").disabled = !ok;
  }

  function doFormat() {
    if (!formatCtx) { return; }
    var ctx = formatCtx;
    // Hard gate: the device captured at render time (ctx.device under
    // ctx.renderGen) must only be formatted on the node it was rendered for.
    if (!canSendMutation(ctx.renderGen, NodeState.generation, mutationsEnabled)) {
      var fdlg = $("format-dialog");
      if (fdlg && fdlg.close) { fdlg.close(); }
      diskStatus.error("Node changed — refresh before formatting.");
      return;
    }
    var gen = NodeState.generation;
    var confirmBtn = $("format-confirm-btn");
    confirmBtn.disabled = true;
    var barcode = $("format-barcode").value.trim();
    var body = { device: ctx.device, force: ctx.force };
    if (barcode) { body.barcode = barcode; }
    api("/api/v1/storage/disks/format", { method: "POST", body: body })
      .then(function () {
        if (isStale(gen, NodeState.generation)) { return; }
        var dlg = $("format-dialog");
        if (dlg.close) { dlg.close(); }
        toast("Formatted " + ctx.device);
        // Re-read the disk summary so the dock reflects the new MVTL identity.
        refreshDisks();
      })
      .catch(function (e) {
        confirmBtn.disabled = false;
        if (isStale(gen, NodeState.generation)) { return; }
        diskStatus.error("Format failed: " + formatErrorMessage(e));
      });
  }

  // Distinct messaging for the documented format error envelopes.
  function formatErrorMessage(e) {
    if (e.status === 409 && e.code === "already_formatted") {
      return "device is already MVTL-formatted — enable reformat (force) to overwrite (" + e.message + ")";
    }
    if (e.status === 409 && e.code === "device_in_use") {
      return "device is in use and cannot be formatted (" + e.message + ")";
    }
    if (e.status === 503) {
      return "raw-disk formatter is unavailable on this node (" + e.message + ")";
    }
    if (e.status >= 500) {
      return "server error (" + e.status + "): " + e.message;
    }
    return e.message;
  }

  /* ---- configuration view (read-only; AC 17) --------------------------
   * Shows device.conf and library_contents as mhvtl-control reads them, the
   * personality each library gets, and the findings of validating the
   * configuration in force. It has no control that changes anything: editing
   * needs the privileged apply helper, and the banner says how to enable it.  */
  function renderConfigNotice(caps) {
    var box = $("config-notice");
    if (!box) { return; }
    clear(box);
    var n = capabilityNotice(caps);
    box.className = "cfg-notice " + n.tone;
    n.lines.forEach(function (l) { box.appendChild(mk("p", { text: l })); });
    if (n.command) { box.appendChild(mk("code", { class: "cfg-command", text: n.command })); }
    var facts = mk("p", { class: "cfg-facts muted" });
    n.facts.forEach(function (f, i) {
      if (i) { facts.appendChild(document.createTextNode(" · ")); }
      facts.appendChild(document.createTextNode(f[0] + ": " + f[1]));
    });
    box.appendChild(facts);
  }

  function hex4(n) { return "0x" + ("0000" + coerceNumber(n).toString(16)).slice(-4); }

  // windowText describes one element type's window: its first address and
  // how many elements fit before the next type.
  function windowText(start, limit, what) {
    return what + " from " + hex4(start) + ", up to " + coerceNumber(limit);
  }

  function configLibrarySummary(lib) {
    var p = lib.personality || {};
    var st = p.start || {}, lim = p.limits || {};
    var rows = [
      ["Address", scsiAddr(lib).addr],
      ["Vendor / product", String(lib.vendor || "") + " " + String(lib.product || "")],
      ["Serial", String(lib.serial || "—")],
      ["NAA", String(lib.naa || "—")],
      ["Media home", String(lib.home || "—") + (lib.home_dir ? "" : " (MHVTL_HOME_PATH)")],
      ["Personality", String(p.name || "?") + " (" + String(p.init_fn || "?") + ", selected at " + String(p.rule_source || "?") + ")"],
      ["VPD page 83h", p.vpd83 ? "yes" : "no"],
      ["Element windows", [
        windowText(st.drive, lim.drives, "drives"),
        windowText(st.storage, lim.slots, "storage slots"),
        windowText(st.map, lim.maps, "MAP slots"),
        windowText(st.picker, lim.pickers, "pickers"),
      ].join(" · ") + " (" + String(p.window_source || "?") + ")"],
    ];
    return mk("table", { class: "cfg-kv", children: [mk("tbody", { children: rows.map(function (r) { return kvRow(r[0], r[1]); }) })] });
  }

  // configDrivesTable lists drives; actions(d), when given, returns the
  // edit buttons of a row.
  function configDrivesTable(lib, actions) {
    var drives = coerceArray(lib.drives);
    if (!drives.length) { return mk("p", { class: "muted", text: "No drives." }); }
    var cols = ["ID", "Slot", "Address", "Vendor / product", "Rev", "Serial", "NAA", "Compression"].concat(actions ? [""] : []);
    var head = mk("thead", { children: [mk("tr", { children: cols.map(function (h) { return mk("th", { text: h }); }) })] });
    var rows = drives.map(function (d) {
      var comp = d.compression_enabled
        ? String(d.compression_type || "on") + " (factor " + coerceNumber(d.compression_factor) + ")"
        : "off";
      return mk("tr", { children: [
        mk("td", { class: "mono", text: coerceNumber(d.id) }),
        mk("td", { class: "mono", text: coerceNumber(d.slot) }),
        mk("td", { class: "mono", text: scsiAddr(d).addr }),
        mk("td", { text: String(d.vendor || "") + " " + String(d.product || "") }),
        mk("td", { class: "mono", text: String(d.revision || "") }),
        mk("td", { class: "mono", text: String(d.serial || "") }),
        mk("td", { class: "mono", text: String(d.naa || "") }),
        mk("td", { text: comp }),
        actions ? mk("td", { class: "acts", children: actions(d) }) : null,
      ] });
    });
    return mk("table", { class: "slot-table", children: [head, mk("tbody", { children: rows })] });
  }

  // configSlotsTable lists every storage slot vtllibrary makes and every MAP
  // slot: the cartridge library_contents puts there and, with the changer
  // readable, the one there now.
  // actions(kind, sl), when given, returns the edit buttons of a row.
  function configSlotsTable(data, actions) {
    var live = !!(data && data.live);
    var cols = ["Element", "library_contents"].concat(live ? ["Live"] : []).concat(actions ? [""] : []);
    var head = mk("thead", { children: [mk("tr", { children: cols.map(function (h) { return mk("th", { text: h }); }) })] });
    var rows = [];
    function add(kind, sl) {
      var file = sl.listed === false ? "(not listed: empty)" : (String(sl.barcode || "") || "empty");
      var cells = [mk("td", { class: "mono", text: kind + " " + coerceNumber(sl.number) }), mk("td", { class: "mono", text: file })];
      if (live) {
        var lb = sl.live_barcode == null ? "?" : (String(sl.live_barcode) || "empty");
        var differs = sl.live_barcode != null && String(sl.live_barcode) !== String(sl.barcode || "");
        cells.push(mk("td", { class: "mono" + (differs ? " warn" : ""), text: lb }));
      }
      if (actions) { cells.push(mk("td", { class: "acts", children: actions(kind === "MAP" ? "map" : "slot", sl) })); }
      rows.push(mk("tr", { children: cells }));
    }
    coerceArray(data && data.maps).forEach(function (sl) { if (sl) { add("MAP", sl); } });
    coerceArray(data && data.slots).forEach(function (sl) { if (sl) { add("Slot", sl); } });
    if (!rows.length) { return mk("p", { class: "muted", text: "No storage or MAP slots." }); }
    return mk("table", { class: "slot-table", children: [head, mk("tbody", { children: rows })] });
  }

  function personalitiesBlock(pers) {
    var libs = coerceArray(pers && pers.libraries);
    var det = mk("details", { class: "cfg-section" });
    det.appendChild(mk("summary", { text: "Library personalities (" + libs.length + ")" }));
    var head = mk("thead", { children: [mk("tr", { children: ["Personality", "Selected by vendor / product prefix", "VPD 83h", "Drives", "Storage", "MAP"].map(function (h) { return mk("th", { text: h }); }) })] });
    var rows = libs.map(function (p) {
      var rules = coerceArray(p.rules).map(function (r) {
        return (String(r.vendor || "*") + " / " + String(r.product_prefix || "*"));
      }).join("; ");
      var lim = p.limits || {};
      return mk("tr", { children: [
        mk("td", { text: String(p.name || "") + " (" + String(p.init_fn || "") + ")" }),
        mk("td", { class: "mono", text: rules }),
        mk("td", { text: p.vpd83 ? "yes" : "no" }),
        mk("td", { class: "mono", text: coerceNumber(lim.drives) }),
        mk("td", { class: "mono", text: coerceNumber(lim.slots) }),
        mk("td", { class: "mono", text: coerceNumber(lim.maps) }),
      ] });
    });
    det.appendChild(mk("div", { class: "slot-scroll", children: [mk("table", { class: "slot-table", children: [head, mk("tbody", { children: rows })] })] }));
    return det;
  }

  function renderConfig() {
    var view = $("config-view");
    if (!view || !lastCfg) { return; }
    clear(view);
    renderConfigNotice(lastCfg.caps);
    var mode = $("config-mode");
    if (mode) { mode.textContent = cfgEditable() ? "editable" : "read-only"; }
    var libs = coerceArray(lastCfg.config && lastCfg.config.libraries);
    var sel = $("config-library");
    if (sel) {
      clear(sel);
      libs.forEach(function (lib) {
        var id = String(coerceNumber(lib.id));
        sel.appendChild(mk("option", { text: "#" + id + " — " + String(lib.vendor || "") + " " + String(lib.product || ""), attrs: { value: id } }));
      });
    }
    var appSel = $("config-target-app");
    if (appSel) {
      clear(appSel);
      appSel.appendChild(mk("option", { text: "Any application (warnings only)", attrs: { value: "" } }));
      coerceArray(lastCfg.personalities && lastCfg.personalities.target_apps).forEach(function (a) {
        appSel.appendChild(mk("option", { text: String(a.name || a.id), attrs: { value: String(a.id || "") } }));
      });
      appSel.value = cfgView.app;
      if (appSel.value !== cfgView.app) { cfgView.app = ""; appSel.value = ""; }
    }
    if (!libs.length) {
      view.appendChild(mk("p", { class: "muted", text: "No libraries configured (" + String(lastCfg.config && lastCfg.config.config_dir || "") + "/device.conf)." }));
    } else {
      var lib = null;
      libs.forEach(function (l) { if (String(coerceNumber(l.id)) === String(cfgView.selected)) { lib = l; } });
      if (!lib) { lib = libs[0]; }
      cfgView.selected = String(coerceNumber(lib.id));
      if (sel) { sel.value = cfgView.selected; }

      // GET /config carries every library's slots, from the same changer
      // read as its live_content, so a refresh reads each changer once.
      var allSlots = lastCfg.config && lastCfg.config.slots;
      var slots = allSlots && typeof allSlots === "object" && Object.prototype.hasOwnProperty.call(allSlots, cfgView.selected)
        ? allSlots[cfgView.selected] : null;
      var editable = cfgEditable();
      var theLib = lib;
      view.appendChild(configLibrarySummary(lib));
      if (editable) {
        view.appendChild(mk("div", { class: "drive-actions", children: [
          editButton("Edit library…", "Edit library " + cfgView.selected, function () {
            attrDialog("Edit library " + coerceNumber(theLib.id), libraryAttrFields(theLib), libCfgPath(theLib.id));
          }),
          editButton("Delete library…", "Delete library " + cfgView.selected, function () { deleteLibrary(theLib, slots); }),
        ] }));
      }
      view.appendChild(mk("h4", { class: "cfg-h", text: "Drives (device.conf)" }));
      view.appendChild(mk("div", { class: "cfg-scroll", children: [configDrivesTable(lib, editable ? function (d) {
        return [
          editButton("Edit…", "Edit drive " + coerceNumber(d.id), function () {
            attrDialog("Edit drive " + coerceNumber(d.id), driveAttrFields(d), libCfgPath(theLib.id, "/drives/" + encodeURIComponent(coerceNumber(d.id))));
          }),
          editButton("Delete…", "Delete drive " + coerceNumber(d.id), function () { deleteDrive(theLib, d); }),
        ];
      } : null)] }));
      view.appendChild(mk("h4", { class: "cfg-h", text: "Slots (library_contents." + cfgView.selected + ")" }));
      if (editable) {
        view.appendChild(mk("div", { class: "drive-actions", children: [
          editButton("Add slots…", "Add storage slots", function () { slotsAddDialog(theLib.id, "slot"); }),
          editButton("Add MAP slots…", "Add MAP slots", function () { slotsAddDialog(theLib.id, "map"); }),
        ] }));
      }
      if (!slots) {
        view.appendChild(mk("p", { class: "muted", text: "No slot data." }));
      } else {
        if (!slots.live) {
          view.appendChild(mk("p", { class: "muted", text: "Live changer state unavailable" + (slots.live_error ? ": " + String(slots.live_error) : "") + ". Showing library_contents only." }));
        }
        if (slots.warning) {
          view.appendChild(mk("p", { class: "cfg-notice warn", text: "Note: " + String(slots.warning) + "." }));
        }
        if (Array.isArray(slots.drives) && slots.drives.length) {
          view.appendChild(mk("p", { class: "muted", text: "In drives now: " + slots.drives.map(function (d) {
            return "drive slot " + coerceNumber(d && d.number) + " (" + String(d && d.barcode) + ")";
          }).join(", ") + "." }));
        }
        view.appendChild(mk("div", { class: "slot-scroll", children: [configSlotsTable(slots, editable ? function (kind, sl) {
          var name = (kind === "map" ? "MAP " : "slot ") + coerceNumber(sl.number);
          return [
            editButton("Set…", "Set " + name, function () { slotDialog(theLib.id, kind, sl, slots); }),
            editButton("Delete…", "Delete " + name, function () { slotDeleteDialog(theLib.id, kind, sl, slots); }),
          ];
        } : null)] }));
      }
    }
    var standalone = coerceArray(lastCfg.config && lastCfg.config.standalone_drives);
    if (standalone.length) {
      view.appendChild(mk("h4", { class: "cfg-h", text: "Drives without a library" }));
      view.appendChild(mk("div", { class: "cfg-scroll", children: [configDrivesTable({ drives: standalone })] }));
    }
    view.appendChild(personalitiesBlock(lastCfg.personalities));
  }

  /* ---- configuration editing (plan 4.2) --------------------------------
   * Each edit goes to a write route, which stages it on the daemon; the
   * stage panel shows the files it would write as a diff, a dry run says
   * what the apply does (units, libraries offline, #43 warnings, findings),
   * and the apply runs only after that. Editing is offered only while
   * capabilities.writes is "ok".                                         */
  function cfgEditable() {
    return !!lastCfg && writesEditable(lastCfg.caps) && mutationsEnabled;
  }

  // stageId is the stage edits add to, if it belongs to this node.
  function stageId() {
    return cfgStage && cfgStage.gen === NodeState.generation ? cfgStage.id : "";
  }

  function cfgPath(suffix) { return "/api/v1/config" + suffix; }

  function libCfgPath(libId, suffix) {
    return cfgPath("/libraries/" + encodeURIComponent(coerceNumber(libId)) + (suffix || ""));
  }

  // formDialog opens a dialog built for one edit. o is {title, intro,
  // fields: [{name, label, value, type ("text" | "number" | "select"),
  // options: [[value, label]], placeholder}], submit, onSubmit(values, done)},
  // where done(err) closes it on success or shows err. The dialog is removed
  // when it closes.
  function formDialog(o) {
    var dlg = mk("dialog", { attrs: { "aria-label": o.title } });
    var form = mk("form", { attrs: { method: "dialog" } });
    form.appendChild(mk("h3", { text: o.title, attrs: { style: "margin:0 0 6px" } }));
    if (o.intro) { form.appendChild(mk("p", { class: "muted", text: o.intro, attrs: { style: "margin:0 0 12px" } })); }
    var grid = mk("div", { class: "form-grid" });
    var inputs = {};
    coerceArray(o.fields).forEach(function (f) {
      var input;
      if (f.type === "select") {
        input = mk("select");
        coerceArray(f.options).forEach(function (opt) {
          input.appendChild(mk("option", { text: opt[1], attrs: { value: opt[0] } }));
        });
        input.value = String(f.value == null ? "" : f.value);
      } else {
        input = mk("input", { attrs: { type: f.type === "number" ? "number" : "text", autocomplete: "off", spellcheck: "false" } });
        if (f.placeholder) { input.placeholder = f.placeholder; }
        input.value = String(f.value == null ? "" : f.value);
      }
      inputs[f.name] = input;
      grid.appendChild(mk("label", { children: [document.createTextNode(f.label), input] }));
    });
    var err = mk("p", { class: "dialog-error", attrs: { role: "alert" } });
    grid.appendChild(err);
    form.appendChild(grid);
    var ok = mk("button", { class: "btn primary", text: o.submit || "Stage", attrs: { type: "button" } });
    var cancel = mk("button", { class: "btn", text: "Cancel", attrs: { value: "cancel" } });
    form.appendChild(mk("div", { class: "dialog-buttons", children: [cancel, ok] }));
    dlg.appendChild(form);
    dlg.addEventListener("close", function () { if (dlg.parentNode) { dlg.parentNode.removeChild(dlg); } });
    ok.addEventListener("click", function () {
      var values = {};
      Object.keys(inputs).forEach(function (k) { values[k] = inputs[k].value; });
      err.textContent = "";
      ok.disabled = true;
      o.onSubmit(values, function (e) {
        ok.disabled = false;
        if (e) { err.textContent = e.message || String(e); } else if (dlg.open) { dlg.close(); }
      });
    });
    document.body.appendChild(dlg);
    if (dlg.showModal) { dlg.showModal(); }
    return dlg;
  }

  // cfgFailed handles a failed staging or apply request (applyErrorAction)
  // and returns the message to show where it was made.
  function cfgFailed(e) {
    switch (applyErrorAction(e)) {
      case "readonly":
        // The gate closed: the banner, re-read, says why.
        refreshConfig(NodeState.generation, currentAbort ? currentAbort.signal : undefined);
        return "Read-only now: " + e.message;
      case "applied":
        cfgStage = null;
        renderStage();
        refresh();
        return "These changes were applied; make new ones on the configuration as it is now.";
      case "reload":
        if (e.code === "stage_not_found") {
          cfgStage = null;
          renderStage();
          return "The pending changes expired or the daemon restarted; make them again.";
        }
        return e.message;
      default:
        return e.message;
    }
  }

  // stageEdit sends one edit. On success the stage is the answer and its
  // dry run, if any, no longer stands.
  function stageEdit(method, path, body, done) {
    var gen = NodeState.generation;
    if (!cfgEditable()) { done(new Error("Editing is not available now.")); return; }
    api(path, { method: method, body: body }).then(function (res) {
      if (isStale(gen, NodeState.generation)) { return; }
      cfgStage = { id: String(res && res.stage || ""), data: res || {}, gen: gen, plan: null, result: null };
      done(null);
      renderStage();
    }).catch(function (e) {
      if (isStale(gen, NodeState.generation)) { return; }
      done(new Error(cfgFailed(e)));
    });
  }

  function attrDialog(title, fields, path) {
    formDialog({
      title: title,
      intro: "An emptied field is removed from device.conf.",
      fields: fields.map(function (f) { return { name: f.key, label: f.key, value: f.value }; }),
      onSubmit: function (values, done) {
        var attrs = buildAttrPatch(fields, values);
        if (!attrs) { done(new Error("Nothing changed.")); return; }
        var body = { attrs: attrs };
        if (stageId()) { body.stage = stageId(); }
        stageEdit("PATCH", path, body, done);
      },
    });
  }

  // cartridgesOf lists the barcodes a library holds in its slots and MAP,
  // live when the changer was read.
  function cartridgesOf(slots) {
    var out = [];
    [slots && slots.slots, slots && slots.maps].forEach(function (arr) {
      coerceArray(arr).forEach(function (sl) {
        var bc = slotBarcode(slots, sl);
        if (bc) { out.push(bc); }
      });
    });
    coerceArray(slots && slots.drives).forEach(function (d) { if (d && d.barcode) { out.push(String(d.barcode)); } });
    return out;
  }

  // slotBarcode is what a slot holds: the live barcode when the changer
  // was read (what the stage edits), else library_contents'.
  function slotBarcode(slots, sl) {
    if (!sl) { return ""; }
    if (slots && slots.live && sl.live_barcode != null) { return String(sl.live_barcode); }
    return String(sl.barcode || "");
  }

  function deleteLibrary(lib, slots) {
    var carts = cartridgesOf(slots);
    var msg = "Library " + coerceNumber(lib.id) + ", its " + coerceArray(lib.drives).length + " drives and library_contents." + coerceNumber(lib.id) + " are removed when the change is applied.";
    if (carts.length) { msg += " Its " + carts.length + " cartridges stay in their home, out of any library (shelf)."; }
    openConfirm("Delete library " + coerceNumber(lib.id), msg, "Stage delete", function () {
      var body = { dispositions: carts.map(function (bc) { return { barcode: bc, to: "shelf" }; }) };
      if (stageId()) { body.stage = stageId(); }
      stageEdit("DELETE", libCfgPath(lib.id), body, function (e) { if (e) { configStatus.error(e.message); } });
    });
  }

  function deleteDrive(lib, d) {
    openConfirm("Delete drive " + coerceNumber(d.id), "Drive " + coerceNumber(d.id) + " (drive slot " + coerceNumber(d.slot) + ") is removed when the change is applied. A loaded drive needs a forced apply, which puts its cartridge back first.", "Stage delete", function () {
      var body = stageId() ? { stage: stageId() } : {};
      stageEdit("DELETE", libCfgPath(lib.id, "/drives/" + encodeURIComponent(coerceNumber(d.id))), body, function (e) { if (e) { configStatus.error(e.message); } });
    });
  }

  function slotDialog(libId, kind, sl, slots) {
    var name = (kind === "map" ? "MAP " : "Slot ") + coerceNumber(sl.number);
    formDialog({
      title: "Set " + name,
      intro: "The cartridge library_contents puts here. Empty it with a blank barcode.",
      fields: [{ name: "barcode", label: "Barcode", value: slotBarcode(slots, sl) }],
      onSubmit: function (v, done) {
        var body;
        try { body = buildSlotPatch(stageId(), kind, v.barcode); } catch (e) { done(e); return; }
        stageEdit("PATCH", libCfgPath(libId, "/slots/" + encodeURIComponent(coerceNumber(sl.number))), body, done);
      },
    });
  }

  function slotDeleteDialog(libId, kind, sl, slots) {
    var name = (kind === "map" ? "MAP " : "Slot ") + coerceNumber(sl.number);
    var bc = slotBarcode(slots, sl);
    var fields = [];
    if (bc) {
      fields.push({ name: "to", label: "Where " + bc + " goes", type: "select", value: "shelf",
        options: [["shelf", "Out of the library (stays in its home)"], ["slot", "To another storage slot"], ["export", "To a MAP slot"]] });
      fields.push({ name: "slot", label: "Storage slot (when moving to one)", type: "number", value: "" });
    }
    formDialog({
      title: "Delete " + name,
      intro: bc ? name + " holds " + bc + "." : name + " is empty.",
      fields: fields,
      submit: "Stage delete",
      onSubmit: function (v, done) {
        var body;
        try { body = buildSlotDelete(stageId(), kind, bc, bc ? { to: v.to, slot: v.slot } : null); } catch (e) { done(e); return; }
        stageEdit("DELETE", libCfgPath(libId, "/slots/" + encodeURIComponent(coerceNumber(sl.number))), body, done);
      },
    });
  }

  function slotsAddDialog(libId, kind) {
    formDialog({
      title: kind === "map" ? "Add MAP slots" : "Add storage slots",
      intro: "Empty slots, numbered after the last. The personality's window limits apply.",
      fields: [{ name: "count", label: "How many", type: "number", value: "1" }],
      onSubmit: function (v, done) {
        var body;
        try { body = buildSlotsAdd(stageId(), kind, v.count); } catch (e) { done(e); return; }
        stageEdit("POST", libCfgPath(libId, "/slots"), body, done);
      },
    });
  }

  function editButton(text, label, onClick) {
    var b = mk("button", { class: "btn sm", text: text, attrs: { type: "button", "aria-label": label } });
    b.addEventListener("click", onClick);
    return b;
  }

  /* ---- stage panel ----------------------------------------------------- */
  var stageStatus = PanelStatus("stage-status");
  var forceTimer = null;

  function diffBlock(row) {
    var pre = mk("pre", { class: "diff" });
    if (!row.hunks.length) { pre.appendChild(mk("span", { class: "diff-ctx", text: "(no line changes)\n" })); }
    row.hunks.forEach(function (h) {
      pre.appendChild(mk("span", { class: "diff-hunk", text: "@@ -" + h.oldStart + " +" + h.newStart + " @@\n" }));
      h.lines.forEach(function (l) {
        var cls = l.op === "+" ? "diff-add" : (l.op === "-" ? "diff-del" : "diff-ctx");
        pre.appendChild(mk("span", { class: cls, text: (l.op === "=" ? " " : l.op) + l.text + "\n" }));
      });
    });
    return pre;
  }

  function listBlock(title, items, cls) {
    if (!items.length) { return null; }
    return mk("div", { class: "stage-block " + (cls || ""), children: [
      mk("h4", { class: "cfg-h", text: title }),
      mk("ul", { children: items.map(function (t) { return mk("li", { class: cls === "units" ? "mono" : "", text: t }); }) }),
    ] });
  }

  function renderPlan(box, plan) {
    if (!plan) { return; }
    if (plan.error) { box.appendChild(mk("p", { class: "cfg-notice " + (plan.force ? "warn" : "error"), text: plan.error })); }
    [
      listBlock("Read before applying", plan.warnings, "warnings"),
      listBlock("Offline to hosts", plan.offline, "offline"),
      listBlock("Units stopped and started", plan.units, "units"),
      listBlock("Applied without a restart", plan.notes, ""),
    ].forEach(function (b) { if (b) { box.appendChild(b); } });
    if (plan.findings.length) {
      var head = mk("thead", { children: [mk("tr", { children: ["", "Library", "Code", "Finding", "Elements"].map(function (h) { return mk("th", { text: h }); }) })] });
      box.appendChild(mk("h4", { class: "cfg-h", text: "Findings" }));
      box.appendChild(mk("div", { class: "media-scroll", children: [mk("table", { class: "slot-table findings-table", children: [head, mk("tbody", { children: plan.findings.map(function (r) {
        return mk("tr", { class: "finding " + r.severity, children: [
          mk("td", { children: [mk("span", { class: "badge sev-" + r.severity, text: r.severity === "error" ? "blocking" : "warning" })] }),
          mk("td", { class: "mono", text: r.library ? String(r.library) : "—" }),
          mk("td", { class: "mono", text: r.code }),
          mk("td", { text: r.message }),
          mk("td", { class: "mono", text: r.where }),
        ] });
      }) })] })] }));
    }
    if (plan.canApply && !plan.units.length) {
      box.appendChild(mk("p", { class: "muted", text: "No unit restarts." }));
    }
  }

  // renderResult shows what a failed apply reported: the unit that failed
  // and its journal, and the cartridges a rollback left aside.
  function renderResult(box, res) {
    if (!res) { return; }
    var lines = [];
    if (res.failed_unit) { lines.push("Failed unit: " + String(res.failed_unit)); }
    coerceArray(res.orphaned_media).forEach(function (b) { lines.push("Created, kept in its home, in no slot: " + String(b)); });
    coerceArray(res.unplaced).forEach(function (b) { lines.push("No slot was free, kept on the shelf: " + String(b)); });
    coerceArray(res.forced_unloads).forEach(function (u) {
      lines.push("Unloaded from library " + coerceNumber(u.library) + " drive " + coerceNumber(u.drive) + (u.barcode ? ": " + String(u.barcode) : ""));
    });
    var b = listBlock("Last apply", lines, "result");
    if (b) { box.appendChild(b); }
    if (res.journal_tail) { box.appendChild(mk("pre", { class: "diff", text: String(res.journal_tail) })); }
  }

  function renderStage() {
    var panel = $("stage-panel"), box = $("stage-view");
    if (!panel || !box) { return; }
    if (cfgStage && cfgStage.gen !== NodeState.generation) { cfgStage = null; }
    if (!cfgStage) {
      panel.hidden = true;
      clear(box);
      return;
    }
    panel.hidden = false;
    clear(box);
    var files = stageFileRows(cfgStage.data, lastCfg && lastCfg.config && lastCfg.config.files);
    $("stage-count").textContent = "(" + files.length + " file" + (files.length === 1 ? "" : "s") + ")";
    if (!files.length) { box.appendChild(mk("p", { class: "muted", text: "The edits so far change no file." })); }
    files.forEach(function (row) {
      box.appendChild(mk("h4", { class: "cfg-h", children: [
        mk("span", { class: "mono", text: row.name }), document.createTextNode(" "),
        mk("span", { class: "badge", text: row.kind === "create" ? "new" : (row.kind === "delete" ? "deleted" : "changed") }),
      ] }));
      if (row.kind !== "delete") { box.appendChild(diffBlock(row)); }
    });
    var disp = coerceArray(cfgStage.data.dispositions).map(function (d) {
      return String(d.barcode) + " (library " + coerceNumber(d.library) + "): " +
        (d.to === "slot" ? "to slot " + coerceNumber(d.slot) : d.to === "export" ? "to a MAP slot" : "out of the library");
    });
    var db = listBlock("Cartridges leaving their element", disp, "");
    if (db) { box.appendChild(db); }
    box.appendChild(mk("p", { class: "muted", text: "library_contents is written with the live placement, so the diff includes cartridges moved since the file was written." }));

    var editable = cfgEditable();
    var check = editButton("Check", "Check the pending changes with the apply helper", function () { checkStage(); });
    var plan = cfgStage.plan;
    var apply = editButton(plan && plan.force ? "Apply with unload…" : "Apply…", "Apply the pending changes", function () { applyStage(); });
    apply.className = "btn sm primary";
    apply.disabled = !editable || !plan || !(plan.canApply || plan.force);
    check.disabled = !editable || !files.length;
    var discard = editButton("Discard", "Discard the pending changes", function () { discardStage(); });
    box.appendChild(mk("div", { class: "drive-actions", children: [check, apply, discard] }));
    if (!editable) { box.appendChild(mk("p", { class: "muted", text: "Read-only now: see the banner above." })); }
    renderPlan(box, plan);
    renderResult(box, cfgStage.result);
  }

  // checkStage asks the helper for a plan of the stage (a dry run).
  function checkStage(then) {
    if (!cfgStage) { return; }
    var gen = NodeState.generation, st = cfgStage;
    stageStatus.loading();
    api(cfgPath("/apply"), { method: "POST", body: { stage: st.id, dry_run: true } }).then(function (res) {
      if (isStale(gen, NodeState.generation) || cfgStage !== st) { return; }
      st.plan = planSummary(res);
      st.result = null;
      stageStatus.ok();
      renderStage();
      if (then) { then(); }
    }).catch(function (e) {
      if (isStale(gen, NodeState.generation) || cfgStage !== st) { return; }
      st.plan = e.body && e.body.result ? planSummary(e.body) : null;
      if (applyErrorAction(e) !== "force") {
        stageFailed(e);
        renderStage();
        return;
      }
      // A drive_loaded plan is a plan: the force dialog offers its apply.
      stageStatus.ok();
      renderStage();
      if (then) { then(); }
    });
  }

  // stageFailed reports a failed check or apply in the stage panel.
  function stageFailed(e) {
    var action = applyErrorAction(e);
    var msg = cfgFailed(e);
    if (action === "reload" && cfgStage) {
      stageStatus.error(msg);
      var region = $("stage-status");
      var reload = editButton("Reload", "Reload the configuration and drop the pending changes", function () {
        cfgStage = null;
        renderStage();
        refresh();
      });
      if (region) { region.appendChild(reload); }
      return;
    }
    stageStatus.error(msg);
  }

  function applyStage() {
    var plan = cfgStage && cfgStage.plan;
    if (!plan) { return; }
    if (plan.force) { openForceDialog(plan.force); return; }
    var parts = [];
    parts.push(plan.units.length ? plan.units.length + (plan.units.length === 1 ? " unit stops and starts." : " units stop and start.") : "No unit restarts.");
    if (plan.offline.length) { parts.push(plan.offline.join(" ")); }
    if (plan.warnings.length) { parts.push("Read the " + (plan.warnings.length === 1 ? "warning" : plan.warnings.length + " warnings") + " above first."); }
    openConfirm("Apply the pending changes", parts.join(" "), "Apply", function () { doApply(""); });
  }

  // openForceDialog offers the forced apply of a drive_loaded plan: the
  // drives it unloads and how long its token lasts.
  function openForceDialog(force) {
    var dlg = mk("dialog", { class: "wide", attrs: { "aria-label": "Apply with unload" } });
    var form = mk("form", { attrs: { method: "dialog" } });
    form.appendChild(mk("h3", { text: "Apply with unload", attrs: { style: "margin:0 0 6px" } }));
    form.appendChild(mk("p", { text: force.message }));
    form.appendChild(mk("p", { class: "muted", text: "The apply puts each loaded cartridge back in its slot, then stops and restarts these drives:" }));
    form.appendChild(mk("ul", { children: force.drives.map(function (u) { return mk("li", { class: "mono", text: u }); }) }));
    var left = mk("p", { class: "mono" });
    form.appendChild(left);
    var ok = mk("button", { class: "btn primary", text: "Unload and apply", attrs: { type: "button" } });
    form.appendChild(mk("div", { class: "dialog-buttons", children: [mk("button", { class: "btn", text: "Cancel", attrs: { value: "cancel" } }), ok] }));
    dlg.appendChild(form);
    function tick() {
      var sec = forceRemaining(force.expires, Date.now());
      left.textContent = sec ? "The confirmation expires in " + fmtCountdown(sec) + "." : "The confirmation expired: check again.";
      ok.disabled = !sec;
    }
    tick();
    if (forceTimer) { clearInterval(forceTimer); }
    forceTimer = setInterval(tick, 1000);
    dlg.addEventListener("close", function () {
      clearInterval(forceTimer);
      forceTimer = null;
      if (dlg.parentNode) { dlg.parentNode.removeChild(dlg); }
    });
    ok.addEventListener("click", function () {
      dlg.close();
      doApply(force.token);
    });
    document.body.appendChild(dlg);
    if (dlg.showModal) { dlg.showModal(); }
  }

  // discardStage drops the stage on the daemon (DELETE
  // /config/stages/{id}) and here. One already gone is dropped here too;
  // one an apply uses is kept.
  function discardStage() {
    if (!cfgStage) { return; }
    var gen = NodeState.generation, st = cfgStage;
    api(cfgPath("/stages/" + encodeURIComponent(st.id)), { method: "DELETE" }).then(function () {
      if (isStale(gen, NodeState.generation) || cfgStage !== st) { return; }
      cfgStage = null;
      renderStage();
    }).catch(function (e) {
      if (isStale(gen, NodeState.generation) || cfgStage !== st) { return; }
      var action = applyErrorAction(e);
      if (action === "reload") {
        cfgStage = null;
        renderStage();
      } else if (action === "applied") {
        learnOutcome(st, gen);
      } else {
        stageStatus.error(cfgFailed(e));
      }
    });
  }

  // applied ends the stage after an apply that applied (res is its answer,
  // or the tombstone's last_apply).
  function applied(res) {
    cfgStage = null;
    renderStage();
    var n = coerceArray(res && res.plan && res.plan.units).length;
    toast("Applied" + (n ? ": " + n + (n === 1 ? " unit" : " units") + " restarted" : ""));
    var warn = coerceArray(res && res.warnings).concat(coerceArray(res && res.placement_drift)).map(String);
    if (warn.length) { configStatus.warn("Applied, with warnings: " + warn.join("; ")); }
    refresh();
  }

  // learnOutcome asks the daemon what became of an apply whose answer did
  // not arrive, or of a stage another request applied: the stage's state
  // and last apply (GET /config/stages/{id}).
  function learnOutcome(st, gen) {
    stageStatus.loading();
    api(cfgPath("/stages/" + encodeURIComponent(st.id))).then(function (dto) {
      if (isStale(gen, NodeState.generation) || cfgStage !== st) { return; }
      var out = stageOutcome(dto);
      if (out.state === "applied") {
        applied(dto.last_apply);
        return;
      }
      st.plan = null;
      st.result = dto && dto.last_apply ? dto.last_apply : null;
      renderStage();
      if (out.state === "busy") {
        stageStatus.error("The apply is still running.");
        var region = $("stage-status");
        if (region) { region.appendChild(editButton("Check again", "Ask whether the apply finished", function () { learnOutcome(st, gen); })); }
      } else if (out.result) {
        stageStatus.error("The last apply ended " + out.result + (out.message ? ": " + out.message : "") + ". Check again before applying.");
      } else {
        stageStatus.error("The apply did not run. Check again before applying.");
      }
    }).catch(function (e) {
      if (isStale(gen, NodeState.generation) || cfgStage !== st) { return; }
      stageFailed(e);
    });
  }

  // doApply applies the stage, expecting the units of its dry run (the
  // daemon sends them). plan_required means the stage changed since: it is
  // checked again for the user to confirm afresh. With no answer, the
  // stage says how the apply ended.
  function doApply(token) {
    if (!cfgStage) { return; }
    var gen = NodeState.generation, st = cfgStage;
    var body = { stage: st.id };
    if (token) { body.force_token = token; }
    stageStatus.loading();
    api(cfgPath("/apply"), { method: "POST", body: body }).then(function (res) {
      if (isStale(gen, NodeState.generation)) { return; }
      applied(res);
    }).catch(function (e) {
      if (isStale(gen, NodeState.generation) || cfgStage !== st) { return; }
      var action = applyErrorAction(e);
      if (action === "lost" || action === "applied") {
        learnOutcome(st, gen);
        return;
      }
      st.plan = null;
      st.result = e.body && e.body.result ? e.body : null;
      if (action === "replan") {
        var why = e.message;
        checkStage(function () { stageStatus.error(why.replace(/\.?$/, ".") + " Checked again: review the plan and apply."); });
        return;
      }
      stageFailed(e);
      renderStage();
    });
  }

  function renderFindings(problems, result) {
    var box = $("config-findings");
    if (!box) { return; }
    clear(box);
    var rows = findingRows(result);
    var errs = rows.filter(function (r) { return r.severity === "error"; }).length;
    var count = $("findings-count");
    if (count) { count.textContent = "(" + errs + " blocking · " + (rows.length - errs) + " warnings)"; }

    problems = coerceArray(problems);
    if (problems.length) {
      box.appendChild(mk("h4", { class: "cfg-h", text: "Lines mhVTL does not read as written" }));
      box.appendChild(mk("ul", { class: "cfg-problems", children: problems.map(function (p) { return mk("li", { class: "mono", text: problemText(p) }); }) }));
    }
    var liveErrs = result && result.live_errors && typeof result.live_errors === "object" ? result.live_errors : {};
    Object.keys(liveErrs).forEach(function (id) {
      box.appendChild(mk("p", { class: "muted", text: "Library " + coerceNumber(id) + ": live state not read (" + String(liveErrs[id]) + "); its library_contents stands in for it." }));
    });
    if (!rows.length) {
      box.appendChild(mk("p", { class: "ok", text: "No findings: the configuration in force passes validation." }));
      return;
    }
    var head = mk("thead", { children: [mk("tr", { children: ["", "Library", "Code", "Finding", "Elements"].map(function (h) { return mk("th", { text: h }); }) })] });
    var trs = rows.map(function (r) {
      var sev = r.severity === "error" ? "blocking" : (r.tag === "pre-existing" ? "pre-existing" : "warning");
      return mk("tr", { class: "finding " + r.severity, children: [
        mk("td", { children: [mk("span", { class: "badge sev-" + r.severity, text: sev })] }),
        mk("td", { class: "mono", text: r.library ? String(r.library) : "—" }),
        mk("td", { class: "mono", text: r.code }),
        mk("td", { text: r.message }),
        mk("td", { class: "mono", text: r.where }),
      ] });
    });
    box.appendChild(mk("div", { class: "media-scroll", children: [mk("table", { class: "slot-table findings-table", children: [head, mk("tbody", { children: trs })] })] }));
  }

  // refreshConfigLibrary validates the configuration in force for the
  // application the selected library is checked for. Validation proposes no
  // file and never writes; it reuses the changer reads GET /config made.
  function refreshConfigLibrary(gen, signal) {
    if (gen === undefined) { gen = NodeState.generation; }
    if (!lastCfg || isStale(gen, NodeState.generation)) { return Promise.resolve(); }
    var libId = cfgView.selected;
    var problems = lastCfg.config && lastCfg.config.problems;
    findingsStatus.loading();
    busy("findings-panel", true);
    return api("/api/v1/config/validate", { method: "POST", body: buildValidateBody(libId, cfgView.app), signal: signal }).then(function (res) {
      busy("findings-panel", false);
      if (isStale(gen, NodeState.generation)) { return; }
      renderFindings(problems, res);
      setPanelGrey("config-findings", false);
      findingsStatus.ok();
    }).catch(function (e) {
      busy("findings-panel", false);
      if (e.name === "AbortError" || isStale(gen, NodeState.generation)) { return; }
      findingsStatus.error("validate: " + e.message);
    });
  }

  function refreshConfig(gen, signal) {
    configStatus.loading();
    setPanelGrey("config-view", true);
    setPanelGrey("config-findings", true);
    busy("config-panel", true);
    return Promise.all([
      api("/api/v1/config/capabilities", { signal: signal }),
      api("/api/v1/config", { signal: signal }),
      api("/api/v1/config/personalities", { signal: signal }),
    ]).then(function (r) {
      busy("config-panel", false);
      if (isStale(gen, NodeState.generation)) { return; }
      lastCfg = { caps: r[0] || {}, config: r[1] || {}, personalities: r[2] || {} };
      renderConfig();
      renderStage();
      setPanelGrey("config-view", false);
      configStatus.ok();
      return refreshConfigLibrary(gen, signal);
    }).catch(function (e) {
      busy("config-panel", false);
      if (e.name === "AbortError" || isStale(gen, NodeState.generation)) { return; }
      configStatus.error("configuration: " + e.message);
    });
  }

  /* ---- refresh cycle (generation-guarded, abortable) ------------------- */
  function refreshSystem(gen, signal) {
    sysStatus.loading();
    setPanelGrey("system-status", true); // loading/greyed until THIS panel's data lands
    busy("system-panel", true);
    return api("/api/v1/system", { signal: signal }).then(function (s) {
      busy("system-panel", false);
      if (isStale(gen, NodeState.generation)) { return; }
      renderSystem(s);
      setPanelGrey("system-status", false); // new-node content rendered: interactive again
      sysStatus.ok();
    }).catch(function (e) {
      busy("system-panel", false);
      if (e.name === "AbortError" || isStale(gen, NodeState.generation)) { return; }
      sysStatus.error("system: " + e.message);
    });
  }
  function refreshLibraries(gen, signal) {
    libStatus.loading();
    setPanelGrey("library-rack", true); // loading/greyed until THIS panel's data lands
    busy("library-panel", true);
    return api("/api/v1/libraries", { signal: signal }).then(function (d) {
      busy("library-panel", false);
      if (isStale(gen, NodeState.generation)) { return; }
      renderLibraries(d);
      setPanelGrey("library-rack", false); // new-node content rendered: interactive again
      libStatus.ok();
    }).catch(function (e) {
      busy("library-panel", false);
      if (e.name === "AbortError" || isStale(gen, NodeState.generation)) { return; }
      libStatus.error("libraries: " + e.message);
    });
  }
  function refreshDisks(gen, signal) {
    if (gen === undefined) { gen = NodeState.generation; }
    diskStatus.loading();
    setPanelGrey("disk-dock", true); // loading/greyed until THIS panel's data lands
    busy("disk-panel", true);
    return api("/api/v1/storage/disks", { signal: signal }).then(function (d) {
      busy("disk-panel", false);
      if (isStale(gen, NodeState.generation)) { return; }
      renderDisks(d);
      setPanelGrey("disk-dock", false); // new-node content rendered: interactive again
      diskStatus.ok();
    }).catch(function (e) {
      busy("disk-panel", false);
      if (e.name === "AbortError" || isStale(gen, NodeState.generation)) { return; }
      diskStatus.error("disks: " + e.message);
    });
  }

  // refreshMedia reads the media of the library selected in the rack. A
  // response for a library that is no longer selected is dropped.
  function refreshMedia(gen, signal) {
    if (gen === undefined) { gen = NodeState.generation; }
    if (isStale(gen, NodeState.generation) || (signal && signal.aborted)) { return Promise.resolve(); }
    var libId = libView.selected == null ? null : String(libView.selected);
    if (libId === null) {
      lastMedia = null;
      renderMedia();
      return Promise.resolve();
    }
    mediaStatus.loading();
    setPanelGrey("media-list", true);
    busy("media-panel", true);
    return api(libPath(libId, "/media"), { signal: signal }).then(function (d) {
      busy("media-panel", false);
      if (isStale(gen, NodeState.generation) || String(libView.selected) !== libId) { return; }
      lastMedia = { libId: libId, gen: gen, data: d || {} };
      renderMedia();
      setPanelGrey("media-list", false);
      mediaStatus.ok();
    }).catch(function (e) {
      busy("media-panel", false);
      if (e.name === "AbortError" || isStale(gen, NodeState.generation)) { return; }
      mediaStatus.error("media: " + e.message);
    });
  }

  function refresh() {
    var gen = NodeState.generation;
    if (currentAbort) { currentAbort.abort(); }
    currentAbort = new AbortController();
    var signal = currentAbort.signal;
    // Concurrent, independently-reported refreshes. Media follows the
    // libraries because it is read for the library they resolve as selected.
    refreshSystem(gen, signal);
    refreshLibraries(gen, signal).then(function () { return refreshMedia(gen, signal); });
    refreshDisks(gen, signal);
    refreshConfig(gen, signal);
  }

  /* ---- node switching + health probe ----------------------------------- */
  // setPanelGrey toggles the greyed (pointer-events:none, dimmed) state on ONE
  // panel body. Fix A: a panel stays greyed — its mutating controls uninteractive
  // — until ITS post-switch data actually re-renders, closing the window in which
  // the previous node's buttons were live while the new node's data was still in
  // flight.
  function setPanelGrey(domId, on) {
    var p = $(domId);
    if (p) { p.classList.toggle("greyed", !!on); }
  }
  function greyPanels() {
    sysStatus.grey(); libStatus.grey(); diskStatus.grey(); mediaStatus.grey();
    configStatus.grey(); findingsStatus.grey();
    setPanelGrey("system-status", true);
    setPanelGrey("library-rack", true);
    setPanelGrey("disk-dock", true);
    setPanelGrey("media-list", true);
    setPanelGrey("config-view", true);
    setPanelGrey("config-findings", true);
    ["media-create-dialog", "media-edit-dialog", "import-dialog", "move-dialog", "confirm-dialog", "stats-dialog"].forEach(closeDialog);
    // The pending changes were staged on the previous node; their dialogs
    // (built per edit) go with them.
    stageStatus.grey();
    cfgStage = null;
    renderStage();
    Array.prototype.forEach.call(document.querySelectorAll("dialog[open]"), function (d) { if (d.close) { d.close(); } });
  }

  // Probe a candidate node WITHOUT mutating NodeState. Resolves true/false.
  function probe(origin, token, signal) {
    var headers = {};
    if (token) { headers["Authorization"] = "Bearer " + token; }
    var base = origin ? origin.replace(/\/+$/, "") : "";
    function hit(path) {
      return fetch(base + path, { headers: headers, signal: signal }).then(function (r) { return r.ok; });
    }
    return hit("/healthz").then(function (ok) {
      return ok ? true : hit("/api/v1/system");
    }).catch(function () { return false; });
  }

  // Apply a new target: bump generation, abort in-flight, grey + disable, then
  // verify the new node before re-enabling mutations and refreshing.
  function switchTarget(origin, token, remember) {
    NodeState.generation++;
    var gen = NodeState.generation;
    if (currentAbort) { currentAbort.abort(); }
    currentAbort = new AbortController();

    NodeState.origin = origin;
    NodeState.token = token;
    NodeState.remember = remember;
    NodeState.health = "unknown";
    mutationsEnabled = false;

    greyPanels();
    updateBadge();

    probe(origin, token, currentAbort.signal).then(function (ok) {
      if (isStale(gen, NodeState.generation)) { return; } // another switch superseded us
      if (ok) {
        NodeState.health = "ok";
        mutationsEnabled = true;
        updateBadge();
        // Fix A: do NOT ungrey here. refresh() keeps each panel greyed (its stale
        // controls uninteractive) and ungreys it only when ITS new-node data
        // actually re-renders — closing the cross-node mutation window.
        refresh();
      } else {
        NodeState.health = "err";
        mutationsEnabled = false;
        updateBadge();
        // Panels stay greyed (controls uninteractive) while the node is down.
        var m = "node unreachable — controls disabled until it responds";
        sysStatus.error(m); libStatus.error(m); diskStatus.error(m); mediaStatus.error(m);
        configStatus.error(m); findingsStatus.error(m);
      }
    });
  }

  /* ---- persistence of target ------------------------------------------- */
  function persistTarget(origin, token, remember) {
    // origin + remember flag persist (not sensitive); token only when remember.
    try {
      lsSet(LS_ORIGIN, origin);
      lsSet(LS_REMEMBER, remember ? "1" : "0");
      if (remember) {
        lsSet(LS_TOKEN, token);
      } else {
        lsDel(LS_TOKEN);
      }
      return { ok: true };
    } catch (e) {
      return { ok: false, error: e };
    }
  }

  function forgetTarget() {
    lsDel(LS_ORIGIN); lsDel(LS_TOKEN); lsDel(LS_REMEMBER);
    NodeState.token = "";
    NodeState.remember = false;
    $("target-token").value = "";
    $("target-remember").checked = false;
    var res = $("target-test-result");
    if (res) { res.textContent = "Saved token forgotten."; res.className = "test-result ok"; }
  }

  function restoreTarget() {
    var origin = lsGet(LS_ORIGIN) || "";
    var remember = lsGet(LS_REMEMBER) === "1";
    NodeState.origin = origin;
    NodeState.remember = remember;
    NodeState.token = remember ? (lsGet(LS_TOKEN) || "") : "";
  }

  /* ---- wiring ---------------------------------------------------------- */
  document.addEventListener("DOMContentLoaded", function () {
    applyTheme(lsGet(LS_THEME) || "modern");
    $("theme-select").addEventListener("change", function (e) { applyTheme(e.target.value); });
    $("refresh-btn").addEventListener("click", refresh);

    // US-F6 library toolbar: search / state filter / table toggle / selector.
    // Each updates libView and re-renders from cached data (no refetch).
    var slotSearch = $("slot-search");
    if (slotSearch) { slotSearch.addEventListener("input", function (e) { libView.query = e.target.value; rerenderLibraries(); }); }
    var stateFilter = $("slot-state-filter");
    if (stateFilter) { stateFilter.addEventListener("change", function (e) { libView.state = e.target.value; rerenderLibraries(); }); }
    var viewToggle = $("view-mode-toggle");
    if (viewToggle) { viewToggle.addEventListener("change", function (e) { libView.table = !!e.target.checked; rerenderLibraries(); }); }
    var librarySelector = $("library-selector");
    if (librarySelector) {
      librarySelector.addEventListener("change", function (e) {
        libView.selected = e.target.value;
        rerenderLibraries();
        refreshMedia(NodeState.generation, currentAbort ? currentAbort.signal : undefined);
      });
    }

    // Media panel toolbar: search / location filter re-render from cache.
    $("media-search").addEventListener("input", function (e) { mediaView.query = e.target.value; renderMedia(); });
    $("media-where").addEventListener("change", function (e) { mediaView.where = e.target.value; renderMedia(); });
    $("media-new-btn").addEventListener("click", openCreateDialog);

    // Configuration view: the library shown and the application it is
    // checked for. Both only read: slots and a validation of the files as
    // they are.
    $("config-library").addEventListener("change", function (e) {
      cfgView.selected = e.target.value;
      renderConfig();
      refreshConfigLibrary(NodeState.generation, currentAbort ? currentAbort.signal : undefined);
    });
    $("config-target-app").addEventListener("change", function (e) {
      cfgView.app = e.target.value;
      refreshConfigLibrary(NodeState.generation, currentAbort ? currentAbort.signal : undefined);
    });

    // Enter in a text field runs the dialog's primary action instead of the
    // form's default (first) button, which is Cancel.
    [["media-create-form", "mc-submit"], ["media-edit-form", "me-submit"], ["move-form", "move-submit"]].forEach(function (p) {
      $(p[0]).addEventListener("keydown", function (e) {
        if (e.key === "Enter" && e.target && e.target.tagName === "INPUT") {
          e.preventDefault();
          if (!$(p[1]).disabled) { $(p[1]).click(); }
        }
      });
    });
    $("media-create-form").addEventListener("input", updateCreatePreview);
    $("media-create-form").addEventListener("change", updateCreatePreview);
    $("mc-submit").addEventListener("click", doCreate);
    $("me-submit").addEventListener("click", doEdit);
    $("move-submit").addEventListener("click", doMove);
    $("stats-refresh").addEventListener("click", loadStats);
    $("confirm-form").addEventListener("submit", function (e) {
      var fn = confirmCtx;
      confirmCtx = null;
      if (fn && e.submitter && e.submitter.value === "confirm") { fn(); }
    });

    restoreTarget();
    updateBadge();

    // Target dialog.
    var dlg = $("target-dialog");
    $("target-btn").addEventListener("click", function () {
      $("target-url").value = NodeState.origin;
      $("target-token").value = NodeState.token;
      $("target-remember").checked = NodeState.remember;
      var res = $("target-test-result");
      if (res) { res.textContent = ""; res.className = "test-result"; }
      if (dlg.showModal) { dlg.showModal(); }
    });

    $("target-test").addEventListener("click", function () {
      var res = $("target-test-result");
      res.textContent = "Testing…"; res.className = "test-result";
      var origin = $("target-url").value.trim();
      var token = $("target-token").value;
      probe(origin, token).then(function (ok) {
        res.textContent = ok ? "Reachable ✓" : "Unreachable ✕";
        res.className = "test-result " + (ok ? "ok" : "err");
      });
    });

    $("target-forget").addEventListener("click", forgetTarget);

    $("target-form").addEventListener("submit", function (e) {
      if (!(e.submitter && e.submitter.value === "save")) { return; }
      var origin = $("target-url").value.trim();
      var token = $("target-token").value;
      var remember = $("target-remember").checked;
      var p = persistTarget(origin, token, remember);
      if (!p.ok) {
        // Surface the failure; do NOT imply the setting was saved.
        sysStatus.error("Could not save target to this device: " + (p.error && p.error.message || p.error));
      }
      switchTarget(origin, token, remember);
    });

    // Mount + format dialog controls.
    $("format-confirm-input").addEventListener("input", onFormatConfirmInput);
    $("format-form").addEventListener("submit", function (e) {
      if (e.submitter && e.submitter.value === "confirm") { doFormat(); }
    });

    // Initial load: same-origin node, verified via health probe.
    switchTarget(NodeState.origin, NodeState.token, NodeState.remember);
  });
})();
