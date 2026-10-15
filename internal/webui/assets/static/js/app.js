/*
 * mhvtld dashboard client (framework-free, no build step).
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

  /* ---- library view state (US-F6: search / filter / table / multi-lib) --
   * These persist across refreshes so a running search/filter is not reset by
   * background polling. lastLibData caches the last successful /libraries
   * payload so the toolbar can re-render without a refetch.                  */
  var libView = { query: "", state: "all", table: false, selected: null };
  var lastLibData = null;

  /* ---- localStorage (errors surfaced, never silently implying success) -- */
  var LS_THEME = "mhvtld.theme";
  var LS_ORIGIN = "mhvtld.origin";
  var LS_TOKEN = "mhvtld.token";
  var LS_REMEMBER = "mhvtld.remember";

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
      },
      grey: function () {
        if (!region) { return; }
        clear(region);
        region.appendChild(mk("span", { class: "status-line loading", text: "Switching node…" }));
      },
      error: function (msg) {
        if (!region) { return; }
        clear(region);
        var box = mk("div", { class: "panel-error" });
        box.appendChild(mk("span", { class: "msg", text: String(msg) }));
        var x = mk("button", { class: "dismiss", text: "✕", attrs: { type: "button", "aria-label": "Dismiss" } });
        x.addEventListener("click", function () { clear(region); });
        box.appendChild(x);
        region.appendChild(box);
      },
    };
  }
  var sysStatus = PanelStatus("system-status-region");
  var libStatus = PanelStatus("lib-status");
  var diskStatus = PanelStatus("disk-status");

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

    var actions = mk("div", { class: "drive-actions" });
    if (st.state === "loaded") {
      var unmountBtn = mk("button", { class: "btn sm", text: "Unmount", attrs: { type: "button", "aria-label": "Unmount drive " + driveId } });
      unmountBtn.disabled = !mutationsEnabled;
      unmountBtn.addEventListener("click", function () { doUnmount(lib.id, driveId, unmountBtn, renderGen); });
      actions.appendChild(unmountBtn);
    } else {
      var mountBtn = mk("button", { class: "btn sm", text: "Mount", attrs: { type: "button", "aria-label": "Mount a cartridge into drive " + driveId } });
      mountBtn.disabled = !mutationsEnabled || st.state !== "empty";
      if (st.state !== "empty") { mountBtn.title = "drive state unknown: changer not reachable"; }
      mountBtn.addEventListener("click", function () { openMountPicker(lib, driveId, renderGen); });
      actions.appendChild(mountBtn);
    }
    card.appendChild(actions);
    return card;
  }

  // mapArea renders a library's Import/Export (MAP) slots as a DISTINCT region
  // (US-F7), visually separate from storage slots. "Load from MAP" mounts that
  // MAP slot ({drive, map}) into the first drive the changer reports empty.
  // Export/Eject has no REST endpoint in this build, so its control is rendered
  // DISABLED with an explicit note.
  function mapArea(lib) {
    var renderGen = NodeState.generation;
    var maps = coerceArray(lib.maps);
    var box = mk("div", { class: "map-area" });
    box.appendChild(mk("div", { class: "map-head", text: "Import / Export (MAP)" }));
    if (!maps.length) {
      box.appendChild(mk("p", { class: "muted", text: "No import/export (MAP) slots." }));
      return box;
    }
    var emptyDrive = firstEmptyDrive(lib);
    var list = mk("div", { class: "map-list" });
    maps.forEach(function (m) {
      var occ = slotIsOccupied(m);
      var num = coerceNumber(m.number);
      var cell = mk("div", { class: "map-slot" + (occ ? " full" : "") });
      cell.appendChild(mk("span", { class: "n mono", text: "MAP " + num }));
      cell.appendChild(mk("span", { class: "bc mono", text: occ ? String(m.barcode || "(no barcode)") : "—" }));

      var acts = mk("div", { class: "map-actions" });
      var imp = mk("button", { class: "btn sm", text: "Load from MAP", attrs: { type: "button", "aria-label": "Import (load) the cartridge in MAP " + num + " into a drive" } });
      imp.disabled = !mutationsEnabled || lib.live !== true || !occ || emptyDrive === null;
      imp.title = lib.live !== true ? "changer not reachable"
        : !occ ? "MAP slot is empty"
          : (emptyDrive === null ? "no empty drive available to load into"
            : "Load the cartridge in this MAP slot into drive " + emptyDrive);
      imp.addEventListener("click", function () {
        doMount({ libId: coerceNumber(lib.id), drive: emptyDrive }, { kind: "map", number: num }, imp, renderGen);
      });
      acts.appendChild(imp);

      var exp = mk("button", {
        class: "btn sm ghost", text: "Export / Eject",
        attrs: { type: "button", title: "export/eject not available in this build", "aria-label": "Export or eject MAP " + num + " (not available in this build)" },
      });
      exp.disabled = true; // No REST endpoint exists; do NOT fake-wire it.
      acts.appendChild(exp);
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

  // slotTableView renders the compact table (slot, barcode, status, action)
  // alternative for an already-filtered slot set. The action mounts an occupied
  // slot into the first empty drive (disabled when none is available or the
  // library has no live state).
  function slotTableView(lib, slots) {
    var renderGen = NodeState.generation;
    var emptyDrive = firstEmptyDrive(lib);
    var rows = slots.map(function (sl) {
      var full = slotIsOccupied(sl);
      var num = coerceNumber(sl.number);
      var actionCell = mk("td");
      if (full) {
        var b = mk("button", { class: "btn sm", text: "Mount", attrs: { type: "button", "aria-label": "Mount slot " + num + " into a drive" } });
        b.disabled = !mutationsEnabled || emptyDrive === null;
        b.title = lib.live !== true ? "changer not reachable"
          : (emptyDrive === null ? "no empty drive available" : "Mount this slot into drive " + emptyDrive);
        b.addEventListener("click", function () { doMount({ libId: coerceNumber(lib.id), drive: emptyDrive }, { kind: "slot", number: num }, b, renderGen); });
        actionCell.appendChild(b);
      } else {
        actionCell.appendChild(document.createTextNode("—"));
      }
      return mk("tr", { children: [
        mk("td", { class: "mono", text: String(num) }),
        mk("td", { class: "mono", text: full ? String(sl.barcode || "(no barcode)") : "—" }),
        mk("td", { text: full ? "occupied" : "empty" }),
        actionCell,
      ] });
    });
    var thead = mk("thead", { children: [mk("tr", { children: [
      mk("th", { text: "Slot" }), mk("th", { text: "Barcode" }), mk("th", { text: "Status" }), mk("th", { text: "Action" }),
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

  function refresh() {
    var gen = NodeState.generation;
    if (currentAbort) { currentAbort.abort(); }
    currentAbort = new AbortController();
    var signal = currentAbort.signal;
    // Three concurrent, independently-reported refreshes.
    refreshSystem(gen, signal);
    refreshLibraries(gen, signal);
    refreshDisks(gen, signal);
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
    sysStatus.grey(); libStatus.grey(); diskStatus.grey();
    setPanelGrey("system-status", true);
    setPanelGrey("library-rack", true);
    setPanelGrey("disk-dock", true);
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
        sysStatus.error(m); libStatus.error(m); diskStatus.error(m);
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
    if (librarySelector) { librarySelector.addEventListener("change", function (e) { libView.selected = e.target.value; rerenderLibraries(); }); }

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
