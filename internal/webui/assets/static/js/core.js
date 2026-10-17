/*
 * mhvtl-control dashboard — pure, framework-free helpers.
 *
 * Security-critical: these are the ONLY sanctioned encoders for API-derived
 * data. app.js must route every interpolated value through esc()/attr() when a
 * template string is unavoidable, and prefer textContent/setAttribute otherwise.
 *
 * This module is dependency-free and side-effect-free so Node's built-in test
 * runner can require() it directly (see internal/webui/jstest/core.test.cjs).
 * The UMD tail exposes the same API as CommonJS exports AND as a browser global
 * (window.VTL), so the embedded page needs no bundler/build step.
 */
(function () {
  "use strict";

  // HTML-escape ALL FIVE significant characters. The previous esc() omitted the
  // single quote, which let `onerror='...'` and `');alert(1)//` style payloads
  // survive inside single-quoted attributes. Order matters: & first.
  var HTML_ESCAPE = {
    "&": "&amp;",
    "<": "&lt;",
    ">": "&gt;",
    '"': "&quot;",
    "'": "&#39;",
  };

  function esc(s) {
    return String(s == null ? "" : s).replace(/[&<>"']/g, function (c) {
      return HTML_ESCAPE[c];
    });
  }

  // Safe attribute-value encoding. Escapes the same five characters as esc() so
  // a value is safe inside either single- or double-quoted attributes.
  function attr(s) {
    return String(s == null ? "" : s).replace(/[&<>"']/g, function (c) {
      return HTML_ESCAPE[c];
    });
  }

  // coerceNumber validates untrusted numeric API data. Returns a finite number
  // or 0; never NaN/Infinity.
  function coerceNumber(x) {
    var n = Number(x);
    return Number.isFinite(n) ? n : 0;
  }

  // coerceArray validates untrusted array API data. Returns x if it is an array,
  // otherwise an empty array, so callers can always .forEach safely.
  function coerceArray(x) {
    return Array.isArray(x) ? x : [];
  }

  // buildMountBody returns the POST body for a MOUNT: the cartridge in storage
  // slot n ({drive, slot}) or, with kind "map", in MAP slot n ({drive, map}).
  // n MUST be a positive integer: the API refuses a mount that names no
  // source, and a body is never produced for one. Throws RangeError otherwise.
  function buildMountBody(drive, n, kind) {
    var d = Number(drive);
    var num = Number(n);
    var k = kind == null ? "slot" : kind;
    if (k !== "slot" && k !== "map") {
      throw new RangeError("mount source must be \"slot\" or \"map\"; got " + String(kind));
    }
    if (!Number.isInteger(num) || num <= 0) {
      throw new RangeError("mount requires a positive integer " + k + "; got " + String(n));
    }
    if (!Number.isInteger(d) || d < 0) {
      throw new RangeError("mount requires a valid drive id; got " + String(drive));
    }
    var body = { drive: d };
    body[k] = num;
    return body;
  }

  // driveState derives a drive's load state from a /libraries entry. Only a
  // live library (changer reachable) reports load state; otherwise the state
  // is "unknown" and nothing is claimed. drive.slot is the drive's POSITION in
  // the library, never a storage slot, and is not used here.
  // Returns {state: "loaded"|"empty"|"unknown", barcode, source} where source
  // is {kind: "slot"|"map", number} or null.
  function driveState(lib, d) {
    if (!lib || lib.live !== true || !d || typeof d.loaded !== "boolean") {
      return { state: "unknown", barcode: "", source: null };
    }
    if (!d.loaded) {
      return { state: "empty", barcode: "", source: null };
    }
    var source = null;
    if (coerceNumber(d.source_slot) > 0) {
      source = { kind: "slot", number: coerceNumber(d.source_slot) };
    } else if (coerceNumber(d.source_map) > 0) {
      source = { kind: "map", number: coerceNumber(d.source_map) };
    }
    return { state: "loaded", barcode: d.barcode == null ? "" : String(d.barcode), source: source };
  }

  // firstEmptyDrive returns the id of the first drive the changer reports
  // empty, or null when none is (or the library is not live).
  function firstEmptyDrive(lib) {
    var found = null;
    coerceArray(lib && lib.drives).forEach(function (d) {
      if (found === null && driveState(lib, d).state === "empty") { found = coerceNumber(d.id); }
    });
    return found;
  }

  // mountCandidates lists what can be mounted from a live library: occupied
  // storage slots, then occupied MAP slots, as {kind, number, barcode}. A
  // library without live state yields none, so the UI never offers a mount
  // based on configured (possibly stale) contents.
  function mountCandidates(lib) {
    if (!lib || lib.live !== true) { return []; }
    var out = [];
    [["slot", lib.slots], ["map", lib.maps]].forEach(function (pair) {
      coerceArray(pair[1]).forEach(function (sl) {
        if (slotIsOccupied(sl) && coerceNumber(sl.number) > 0) {
          out.push({ kind: pair[0], number: coerceNumber(sl.number), barcode: sl.barcode == null ? "" : String(sl.barcode) });
        }
      });
    });
    return out;
  }

  // emptyLocations lists where a cartridge can be moved in a live library:
  // empty storage slots, then empty MAP slots, as {kind, number}. Like
  // mountCandidates it offers nothing without live state.
  function emptyLocations(lib) {
    if (!lib || lib.live !== true) { return []; }
    var out = [];
    [["slot", lib.slots], ["map", lib.maps]].forEach(function (pair) {
      coerceArray(pair[1]).forEach(function (sl) {
        if (sl && !slotIsOccupied(sl) && coerceNumber(sl.number) > 0) {
          out.push({ kind: pair[0], number: coerceNumber(sl.number) });
        }
      });
    });
    return out;
  }

  // isStale reports whether a response captured at requestGen should be
  // discarded because the active node generation has since advanced.
  function isStale(requestGen, currentGen) {
    return requestGen !== currentGen;
  }

  // canSendMutation is the hard generation gate guarding every mutating request
  // (mount / unmount / MAP load / format). It returns true ONLY when mutations
  // are enabled AND the generation a control was RENDERED under still matches the
  // active node generation. A stale control (rendered under a previous node whose
  // DOM has not yet been replaced) therefore can never send that node's
  // identifiers to a newly-selected node — the destructive cross-node window.
  function canSendMutation(renderGen, currentGen, mutationsEnabled) {
    return mutationsEnabled === true && renderGen === currentGen;
  }

  function fmtBytes(n) {
    n = coerceNumber(n);
    var u = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"], i = 0;
    while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
    return n.toFixed(i ? 1 : 0) + " " + u[i];
  }

  // slotIsOccupied is the single source of truth for "a slot holds a cartridge":
  // the live `full` flag when the changer reported one (a full slot may carry
  // no readable barcode), else a non-empty configured barcode. Used by
  // filterSlots, countSlots and mountCandidates so they never disagree.
  function slotIsOccupied(sl) {
    if (!sl) { return false; }
    if (typeof sl.full === "boolean") { return sl.full; }
    return sl.barcode != null && String(sl.barcode).length > 0;
  }

  // filterSlots narrows an (untrusted) slot array by a barcode substring query
  // (case-insensitive) and an occupancy state ("all" | "occupied" | "empty").
  // Pure: returns a new array and never mutates its input.
  function filterSlots(slots, opts) {
    opts = opts || {};
    var query = String(opts.query == null ? "" : opts.query).trim().toLowerCase();
    var state = opts.state === "occupied" || opts.state === "empty" ? opts.state : "all";
    return coerceArray(slots).filter(function (sl) {
      var occ = slotIsOccupied(sl);
      if (state === "occupied" && !occ) { return false; }
      if (state === "empty" && occ) { return false; }
      if (query) {
        var bc = sl && sl.barcode != null ? String(sl.barcode).toLowerCase() : "";
        if (bc.indexOf(query) < 0) { return false; }
      }
      return true;
    });
  }

  // countSlots returns {total, occupied, empty} for an (untrusted) slot array.
  function countSlots(slots) {
    var arr = coerceArray(slots);
    var occupied = 0;
    arr.forEach(function (sl) { if (slotIsOccupied(sl)) { occupied++; } });
    return { total: arr.length, occupied: occupied, empty: arr.length - occupied };
  }

  // nodeRows lists a device's nodes (API "nodes": sg, st, nst, sch, by_id,
  // by_path) as {label, path} in the order a backup application wants them:
  // the non-rewinding tape node first, the changer node, then sg. Links come
  // back separately. Non-string or empty values are dropped.
  function nodeRows(nodes) {
    var n = (nodes && typeof nodes === "object") ? nodes : {};
    var str = function (v) { return typeof v === "string" && v !== ""; };
    var rows = [];
    ["nst", "st", "sch", "sg"].forEach(function (k) {
      if (str(n[k])) { rows.push({ label: k, path: n[k] }); }
    });
    var links = [];
    [["by_id", "by-id"], ["by_path", "by-path"]].forEach(function (kl) {
      coerceArray(n[kl[0]]).forEach(function (p) {
        if (str(p)) { links.push({ label: kl[1], path: p }); }
      });
    });
    return { rows: rows, links: links };
  }

  // fmtRatio renders a compression ratio as "x.xx:1"; absent or non-positive
  // ratios (no data moved yet) render as "—".
  function fmtRatio(x) {
    if (x == null) { return "—"; }
    var n = Number(x);
    if (!Number.isFinite(n) || n <= 0) { return "—"; }
    return n.toFixed(2) + ":1";
  }

  // validBarcode mirrors the server rule: 1-16 ASCII letters and digits.
  function validBarcode(s) {
    return typeof s === "string" && /^[A-Za-z0-9]{1,16}$/.test(s);
  }

  // intField reads a form value as an integer: blank -> 0, otherwise it must
  // be an integer. Returns NaN when it is not.
  function intField(v) {
    if (v == null || String(v).trim() === "") { return 0; }
    var n = Number(String(v).trim());
    return Number.isInteger(n) ? n : NaN;
  }

  // expandBarcodes mirrors the server's range expansion for POST /media:
  // prefix + start..start+count-1 zero-padded to width + suffix, where width is
  // max(width, digits of the last number). Returns {barcodes, error}; on error
  // barcodes is empty and error carries the server's wording.
  function expandBarcodes(o) {
    o = o || {};
    var prefix = o.prefix == null ? "" : String(o.prefix);
    var suffix = o.suffix == null ? "" : String(o.suffix);
    var start = intField(o.start), count = intField(o.count), width = intField(o.width);
    function fail(msg) { return { barcodes: [], error: msg }; }
    if (Number.isNaN(start) || Number.isNaN(count) || Number.isNaN(width)) {
      return fail("start, count and width must be whole numbers");
    }
    if (count < 1 || count > 500) { return fail("count must be 1-500"); }
    if (start < 0) { return fail("start must not be negative"); }
    if (width < 0 || width > 16) { return fail("width must be 0-16"); }
    var w = Math.max(width, String(start + count - 1).length);
    var out = [];
    for (var i = 0; i < count; i++) {
      var num = String(start + i);
      while (num.length < w) { num = "0" + num; }
      var bc = prefix + num + suffix;
      if (!validBarcode(bc)) {
        return fail("barcode \"" + bc + "\" is not 1-16 letters or digits");
      }
      out.push(bc);
    }
    return { barcodes: out, error: null };
  }

  // parseSizeMB reads a "size MB" form field: blank -> value null (keep /
  // unset), otherwise a non-negative integer (0 = the density's native
  // capacity). Returns {value, error}.
  function parseSizeMB(s) {
    var t = s == null ? "" : String(s).trim();
    if (t === "") { return { value: null, error: null }; }
    if (!/^[0-9]+$/.test(t)) { return { value: null, error: "size must be a whole number of MB (0 = native)" }; }
    return { value: Number(t), error: null };
  }

  // buildMediaPatch returns the PATCH body for a cartridge edit holding only
  // the fields that change, or null when nothing does. form is
  // {write_protect: bool, type: "" (keep) | name, density: "" (keep) | mktape
  // name, size_mb: "" (keep) | digits}. The cartridge reports its MAM density
  // ("U-832"), never the mktape name, so any chosen density is sent. Throws
  // RangeError on an invalid size.
  function buildMediaPatch(original, form) {
    original = original || {};
    form = form || {};
    var body = {};
    if (typeof form.write_protect === "boolean" && form.write_protect !== !!original.write_protected) {
      body.write_protect = form.write_protect;
    }
    if (form.type && form.type !== original.type) { body.type = String(form.type); }
    if (form.density) { body.density = String(form.density); }
    var size = parseSizeMB(form.size_mb);
    if (size.error) { throw new RangeError(size.error); }
    if (size.value !== null) { body.size_mb = size.value; }
    return Object.keys(body).length ? body : null;
  }

  // mediaLocText renders a cartridge location from GET /media. Drive locations
  // carry the drive POSITION; drives (the library's drives[]) maps it to the
  // drive id when known.
  function mediaLocText(loc, drives) {
    if (!loc || !loc.kind) { return "—"; }
    var n = coerceNumber(loc.number);
    switch (loc.kind) {
      case "slot": return "slot " + n;
      case "map": return "MAP " + n;
      case "shelf": return "shelf";
      case "drive":
        var id = null;
        coerceArray(drives).forEach(function (d) {
          if (id === null && d && coerceNumber(d.slot) === n) { id = coerceNumber(d.id); }
        });
        return id === null ? "drive position " + n : "drive " + id;
      default: return String(loc.kind);
    }
  }

  // filterMedia narrows cartridges by a barcode substring (case-insensitive)
  // and where: "all" | "library" (slot, MAP or drive) | "shelf".
  function filterMedia(carts, opts) {
    opts = opts || {};
    var query = String(opts.query == null ? "" : opts.query).trim().toLowerCase();
    var where = opts.where === "library" || opts.where === "shelf" ? opts.where : "all";
    return coerceArray(carts).filter(function (c) {
      var kind = c && c.location ? c.location.kind : "";
      if (where === "shelf" && kind !== "shelf") { return false; }
      if (where === "library" && kind !== "slot" && kind !== "map" && kind !== "drive") { return false; }
      if (query && String(c && c.barcode || "").toLowerCase().indexOf(query) < 0) { return false; }
      return true;
    });
  }

  /* ---- configuration view (read-only; plan 2.4, AC 17) ---------------- */

  // HELPER_ENABLE is how the apply helper is enabled once it is installed
  // (plan 3.1: the socket unit, never enabled by packaging).
  var HELPER_ENABLE = "systemctl enable --now mhvtl-control-apply.socket";

  // HELPER_STATE is the apply helper's state directory (plan 3.1:
  // StateDirectory=mhvtl-control-apply).
  var HELPER_STATE = "/var/lib/mhvtl-control-apply";

  // capabilityNotice turns GET /config/capabilities into the configuration
  // view's banner: whether the configuration can be edited here, and if not,
  // why and what enables editing. command is a shell command to show, or
  // null.
  function capabilityNotice(caps) {
    caps = caps || {};
    var lines = [];
    var command = null;
    var tone = "info";
    switch (String(caps.helper || "")) {
      case "present":
        lines.push(caps.writes === "ok"
          ? "The apply helper is enabled: changes are staged here, checked by the helper, then applied."
          : "Read-only. The apply helper is enabled, but this daemon refuses changes:");
        break;
      case "legacy_env_blocks":
        tone = "warn";
        lines.push("Read-only. The apply helper refuses to run while /etc/mhvtl/mhvtl-control.env exists.");
        lines.push("Move it to /etc/mhvtl-control/mhvtl-control.env and restart mhvtl-control.");
        break;
      case "in_progress":
        lines.push("An apply is running now; the configuration shown may change when it ends.");
        break;
      case "incomplete_apply":
        tone = "warn";
        lines.push("Read-only. An earlier apply did not finish; the apply helper recovers it before any new change.");
        break;
      case "intent_unreadable":
        tone = "error";
        lines.push("An earlier apply died and left " + HELPER_STATE + "/intent.json, which the apply helper cannot read. It refuses every change until it is recovered by hand.");
        lines.push("Restore the files from the copies under " + HELPER_STATE + "/rollback, then remove " + HELPER_STATE + "/intent.json.");
        break;
      case "unreachable":
        tone = "warn";
        lines.push("Read-only. The apply helper's socket exists, but the helper did not answer: " + String(caps.helper_error || "no reason given") + ".");
        break;
      default:
        lines.push("Read-only. Editing the configuration needs the privileged apply helper, mhvtl-control-apply, which is not enabled on this host.");
        lines.push("Once it is installed, enable it with:");
        command = HELPER_ENABLE;
    }
    if (caps.writes === "tls_required") {
      if (tone === "info") tone = "warn";
      lines.push("This daemon listens on a non-loopback address without TLS, so it would refuse changes: start it with --tls-cert and --tls-key, or listen on loopback.");
    } else if (caps.writes === "token_required") {
      if (tone === "info") tone = "warn";
      lines.push("Changes need an API token, which this daemon does not have: set MHVTL_CONTROL_API_TOKEN in /etc/mhvtl-control/mhvtl-control.env and restart mhvtl-control.");
    }
    var facts = [
      ["mhVTL systemd generator", caps.generator === true ? "installed" : "not installed"],
      ["make_vtl_media per-library home", caps.per_library_home_media === true ? "supported" : "not supported (mhVTL before markh794/mhvtl#276)"],
    ];
    return { tone: tone, lines: lines, command: command, facts: facts };
  }

  var ELEMENT_NAMES = { library: "library", drive: "drive", slot: "slot", map: "MAP", picker: "picker", drive_line: "Drive line" };

  // elementText names one element a finding concerns, e.g. "MAP 2".
  function elementText(el) {
    if (!el) { return ""; }
    var name = Object.prototype.hasOwnProperty.call(ELEMENT_NAMES, el.kind) ? ELEMENT_NAMES[el.kind] : String(el.kind || "element");
    return name + " " + coerceNumber(el.number);
  }

  // findingRows flattens a POST /config/validate result into display rows,
  // errors first. The library element itself is left out of "where": the
  // row names the library already.
  function findingRows(res) {
    var rows = [];
    [["error", res && res.errors], ["warning", res && res.warnings]].forEach(function (p) {
      coerceArray(p[1]).forEach(function (f) {
        if (!f) { return; }
        var where = coerceArray(f.elements)
          .filter(function (el) { return el && el.kind !== "library"; })
          .map(elementText);
        rows.push({
          severity: p[0],
          code: String(f.code || ""),
          library: coerceNumber(f.library),
          message: String(f.message || ""),
          tag: String(f.tag || ""),
          where: where.join(", "),
        });
      });
    });
    return rows;
  }

  // problemText renders a Problem of GET /config: "file:line: message".
  function problemText(p) {
    p = p || {};
    return String(p.file || "?") + ":" + coerceNumber(p.line) + ": " + String(p.message || "");
  }

  // buildValidateBody is the POST /config/validate body that checks the
  // configuration in force, with library libId set up for target
  // application app ("" for none). It proposes no file: validation then
  // reads the files as they are and the live placement.
  function buildValidateBody(libId, app) {
    var body = {};
    var id = Number(libId);
    // Library IDs are 1..1023 (validate.MaxID): 0 is the System V message
    // type no daemon can receive on, so it is never a library.
    if (app && Number.isInteger(id) && id > 0) {
      body.target_apps = {};
      body.target_apps[String(id)] = String(app);
    }
    return body;
  }

  /* ---- configuration editing (plan 4.2) ------------------------------- */

  // writesEditable is whether the dashboard offers edits: only when the
  // daemon's gate is open (capabilities.writes "ok"). helper_absent,
  // tls_required, token_required and helper_blocked all leave the view
  // read-only, with the banner saying why.
  function writesEditable(caps) {
    return !!caps && caps.writes === "ok";
  }

  // stageEditable is whether the configuration takes edits: editable (the
  // gate is open) and no apply of the stage st in flight, whose outcome is
  // not known yet.
  function stageEditable(editable, st) {
    return !!editable && !(st && st.applying);
  }

  // splitLines splits a file into lines. A final newline ends the last line
  // rather than starting an empty one; a file without one keeps its last
  // line distinct ("a" and "a\n" differ, as they do to mhVTL's readline).
  function splitLines(text) {
    var t = text == null ? "" : String(text);
    if (t === "") { return []; }
    var lines = t.split("\n");
    if (lines[lines.length - 1] === "") {
      lines.pop();
      return lines;
    }
    lines[lines.length - 1] += "\u0000"; // no final newline: never equal to a terminated line
    return lines;
  }

  function unmark(line) { return line.replace(/\u0000$/, ""); }

  // MAX_DIFF_CELLS bounds the LCS table; larger files diff as a whole
  // replacement. mhVTL's files are a few hundred lines.
  var MAX_DIFF_CELLS = 4000000;

  // lineDiff is the line diff of a and b, as {op: "=" | "-" | "+", text},
  // by longest common subsequence: deletions before insertions in a change.
  function lineDiff(a, b) {
    var x = splitLines(a), y = splitLines(b);
    var n = x.length, m = y.length, out = [], i, j;
    if (n * m > MAX_DIFF_CELLS) {
      x.forEach(function (l) { out.push({ op: "-", text: unmark(l) }); });
      y.forEach(function (l) { out.push({ op: "+", text: unmark(l) }); });
      return out;
    }
    // lcs[i][j] is the LCS length of x[i:] and y[j:].
    var lcs = [];
    for (i = 0; i <= n; i++) { lcs.push(new Array(m + 1).fill(0)); }
    for (i = n - 1; i >= 0; i--) {
      for (j = m - 1; j >= 0; j--) {
        lcs[i][j] = x[i] === y[j] ? lcs[i + 1][j + 1] + 1 : Math.max(lcs[i + 1][j], lcs[i][j + 1]);
      }
    }
    i = 0; j = 0;
    while (i < n || j < m) {
      if (i < n && j < m && x[i] === y[j]) {
        out.push({ op: "=", text: unmark(x[i]) }); i++; j++;
      } else if (j >= m || (i < n && lcs[i + 1][j] >= lcs[i][j + 1])) {
        out.push({ op: "-", text: unmark(x[i]) }); i++;
      } else {
        out.push({ op: "+", text: unmark(y[j]) }); j++;
      }
    }
    return out;
  }

  // diffHunks groups a lineDiff into hunks with ctx unchanged lines around
  // each change, as {oldStart, newStart (1-based), lines}. Changes closer
  // than 2*ctx lines share a hunk. No change gives no hunk.
  function diffHunks(ops, ctx) {
    ops = coerceArray(ops);
    ctx = Math.max(0, coerceNumber(ctx));
    var changed = [];
    ops.forEach(function (o, k) { if (o.op !== "=") { changed.push(k); } });
    if (!changed.length) { return []; }
    var ranges = [];
    changed.forEach(function (k) {
      var from = Math.max(0, k - ctx), to = Math.min(ops.length - 1, k + ctx);
      var last = ranges[ranges.length - 1];
      if (last && from <= last[1] + 1) { last[1] = Math.max(last[1], to); } else { ranges.push([from, to]); }
    });
    // Line numbers at each op: old lines are "=" and "-", new "=" and "+".
    var oldNo = [], newNo = [], o = 1, nn = 1;
    ops.forEach(function (op) {
      oldNo.push(o); newNo.push(nn);
      if (op.op !== "+") { o++; }
      if (op.op !== "-") { nn++; }
    });
    return ranges.map(function (r) {
      return { oldStart: oldNo[r[0]], newStart: newNo[r[0]], lines: ops.slice(r[0], r[1] + 1) };
    });
  }

  // stageFileRows lists the files a stage changes, by name, each with its
  // kind (modify, create, delete) and its diff against the file on disk
  // (GET /config files).
  function stageFileRows(stage, files) {
    var staged = stage && stage.files && typeof stage.files === "object" ? stage.files : {};
    var disk = Object.create(null);
    coerceArray(files).forEach(function (f) {
      if (f && typeof f.name === "string") { disk[f.name] = String(f.content == null ? "" : f.content); }
    });
    var has = function (o, k) { return Object.prototype.hasOwnProperty.call(o, k); };
    return Object.keys(staged).sort().map(function (name) {
      var next = staged[name];
      var old = has(disk, name) ? disk[name] : null;
      var kind = next === null ? "delete" : (old === null ? "create" : "modify");
      return { name: name, kind: kind, hunks: diffHunks(lineDiff(old || "", next === null ? "" : String(next)), 3) };
    });
  }

  // planSummary reads a POST /config/apply dry run (the 200 answer, or the
  // body of a refusal) for display: the units, a note per library offline to
  // hosts during the apply, the warnings to read before confirming
  // (mhvtl-control#43), the findings, and for a drive_loaded refusal the
  // force offer, with the vtltape@ units of the drives it unloads.
  function planSummary(res) {
    res = res || {};
    var plan = res.plan || {};
    var units = coerceArray(plan.units).map(String);
    var code = res.error && res.error.code ? String(res.error.code) : "";
    var force = null;
    if (code === "drive_loaded" && res.force_token) {
      force = {
        token: String(res.force_token),
        expires: String(res.force_expires || ""),
        drives: units.filter(function (u) { return u.indexOf("vtltape@") === 0; }),
        message: String(res.error.message || ""),
      };
    }
    return {
      canApply: res.result === "validated",
      units: units,
      notes: coerceArray(plan.notes).map(String),
      offline: coerceArray(plan.offline_libraries).map(function (id) {
        return "Library " + coerceNumber(id) + " is offline to hosts during the apply.";
      }),
      warnings: coerceArray(res.warnings).map(String),
      findings: findingRows(res.findings),
      force: force,
      error: res.error && res.error.message ? String(res.error.message) : "",
    };
  }

  // forceRemaining is how many whole seconds a force token has left at
  // nowMs; 0 when it expired or the time cannot be read.
  function forceRemaining(expires, nowMs) {
    var t = Date.parse(String(expires || ""));
    if (!Number.isFinite(t)) { return 0; }
    return Math.max(0, Math.floor((t - coerceNumber(nowMs)) / 1000));
  }

  function fmtCountdown(sec) {
    sec = Math.max(0, Math.floor(coerceNumber(sec)));
    if (!sec) { return "expired"; }
    var s = sec % 60;
    return Math.floor(sec / 60) + ":" + (s < 10 ? "0" : "") + s;
  }

  // READONLY_CODES are the answers that mean the daemon or the helper will
  // not take changes now: the view turns read-only and the banner says why.
  var READONLY_CODES = { helper_absent: 1, helper_unreachable: 1, tls_required: 1, token_required: 1, helper_blocked: 1, intent_unreadable: 1, legacy_env: 1 };

  // applyErrorAction says what the UI does with a failed staging or apply
  // request: reload (the files changed under the stage, or it is gone),
  // replan (the stage needs a dry run first, the helper recovered an
  // apply, or an apply found a drive loaded), readonly, force (a dry run
  // refused for drive_loaded, which carries the token), applied (the stage
  // was applied, maybe by a request whose answer was lost), lost (no
  // answer: GET /config/stages/{id} says what happened) or show the
  // message. applying is true for an apply that is not a dry run: the
  // helper may have run it without answering (helper_unreachable).
  function applyErrorAction(err, applying) {
    if (!err) { return "show"; }
    var code = err.code ? String(err.code) : "";
    if (!err.status) { return err.name === "AbortError" ? "show" : "lost"; }
    if (applying && code === "helper_unreachable") { return "lost"; }
    if (code === "conflict" || code === "stage_not_found") { return "reload"; }
    if (code === "plan_required" || code === "recovered") { return "replan"; }
    if (code === "stage_applied") { return "applied"; }
    if (Object.prototype.hasOwnProperty.call(READONLY_CODES, code)) { return "readonly"; }
    if (code === "drive_loaded") { return err.body && err.body.force_token ? "force" : "replan"; }
    return "show";
  }

  var STAGE_STATES = { open: 1, busy: 1, applied: 1 };

  // stageOutcome reads GET /config/stages/{id}: the stage's state (open,
  // busy while an apply runs, applied) and its last apply's result.
  function stageOutcome(dto) {
    var st = dto && Object.prototype.hasOwnProperty.call(STAGE_STATES, dto.state) ? dto.state : "unknown";
    var last = (dto && dto.last_apply) || {};
    return {
      state: st,
      result: last.result ? String(last.result) : "",
      message: last.error && last.error.message ? String(last.error.message) : "",
      units: coerceArray(last.plan && last.plan.units).map(String),
    };
  }

  // outcomeUnread is whether GET /config/stages/{id}, read to learn a lost
  // apply's outcome, failed without saying anything of the stage: no answer
  // or a 5xx. The outcome is still unknown, so edits stay held.
  function outcomeUnread(err) {
    return !!err && (!err.status || err.status >= 500);
  }

  // libraryAttrFields and driveAttrFields are the device.conf keys the edit
  // dialogs offer, with the values GET /config reports.
  function libraryAttrFields(lib) {
    lib = lib || {};
    return [
      { key: "Vendor identification", value: String(lib.vendor || "") },
      { key: "Product identification", value: String(lib.product || "") },
      { key: "Unit serial number", value: String(lib.serial || "") },
      { key: "NAA", value: String(lib.naa || "") },
      { key: "Home directory", value: String(lib.home_dir || "") },
    ];
  }

  function driveAttrFields(d) {
    d = d || {};
    return [
      { key: "Vendor identification", value: String(d.vendor || "") },
      { key: "Product identification", value: String(d.product || "") },
      { key: "Product revision level", value: String(d.revision || "") },
      { key: "Unit serial number", value: String(d.serial || "") },
      { key: "NAA", value: String(d.naa || "") },
    ];
  }

  // buildAttrPatch is the attrs of a PATCH: each offered field whose trimmed
  // value changed, null for one emptied (the key is deleted). null when
  // nothing changes.
  function buildAttrPatch(fields, form) {
    form = form || {};
    var out = {}, any = false;
    coerceArray(fields).forEach(function (f) {
      if (!f || !Object.prototype.hasOwnProperty.call(form, f.key)) { return; }
      var v = String(form[f.key] == null ? "" : form[f.key]).trim();
      if (v === String(f.value || "")) { return; }
      out[f.key] = v === "" ? null : v;
      any = true;
    });
    return any ? out : null;
  }

  function withStage(stage, body) {
    if (stage) { body.stage = String(stage); }
    return body;
  }

  // buildSlotPatch pre-fills (or with "" empties) a Slot or MAP slot. A
  // barcode library_contents could not hold whole is refused.
  function buildSlotPatch(stage, kind, barcode) {
    var bc = String(barcode == null ? "" : barcode).trim();
    if (bc !== "" && !/^[\x21-\x7e]{1,32}$/.test(bc)) {
      throw new RangeError("a barcode is one word of printable characters");
    }
    return withStage(stage, { kind: kind === "map" ? "map" : "slot", barcode: bc });
  }

  // buildSlotDelete removes a Slot or MAP slot. One that holds a cartridge
  // needs a disposition: to a storage slot, the MAP (export) or the shelf.
  function buildSlotDelete(stage, kind, barcode, disposition) {
    var body = withStage(stage, { kind: kind === "map" ? "map" : "slot" });
    if (!barcode) { return body; }
    if (!disposition || !disposition.to) {
      throw new RangeError("say where the cartridge " + String(barcode) + " goes");
    }
    var d = { to: String(disposition.to) };
    if (d.to === "slot") {
      var n = intField(disposition.slot);
      if (!(n > 0)) { throw new RangeError("the slot must be a positive number"); }
      d.slot = n;
    }
    body.disposition = d;
    return body;
  }

  // buildSlotsAdd adds count Slot or MAP lines after the last.
  function buildSlotsAdd(stage, kind, count) {
    var n = intField(count);
    if (!(n > 0 && n <= 4096)) { throw new RangeError("count must be 1-4096"); }
    return withStage(stage, { kind: kind === "map" ? "map" : "slot", count: n });
  }

  /* ---- create wizard and system settings (plan 4.2) ------------------- */

  // MAX_ID is the highest device.conf ID (validate.MaxID); MAX_NEW the most
  // Slot or MAP lines one create request adds (maxNewElements in
  // internal/api); MAX_SERIAL mhvtl-control's serial policy
  // (validate.MaxSerialLen).
  var MAX_ID = 1023, MAX_NEW = 4096, MAX_SERIAL = 10;

  function hasPrefixFold(s, prefix) {
    s = String(s || ""); prefix = String(prefix || "");
    return s.length >= prefix.length && s.slice(0, prefix.length).toLowerCase() === prefix.toLowerCase();
  }

  // allRules flattens the personalities' rules with their personality, in
  // vtllibrary's evaluation order.
  function allRules(pers) {
    var out = [];
    coerceArray(pers && pers.libraries).forEach(function (p) {
      coerceArray(p && p.rules).forEach(function (r) { out.push({ rule: r, p: p }); });
    });
    return out.sort(function (a, b) { return coerceNumber(a.rule.order) - coerceNumber(b.rule.order); });
  }

  // resolvePersonality is the personality vtllibrary gives a library of
  // this vendor and product: the first rule whose vendor and product
  // prefixes both match, case-insensitively (personality.Lookup).
  function resolvePersonality(pers, vendor, product) {
    var m = resolveRule(pers, vendor, product);
    return m ? m.p : null;
  }

  // resolveRule is that first matching rule, with its personality.
  function resolveRule(pers, vendor, product) {
    var rules = allRules(pers);
    for (var i = 0; i < rules.length; i++) {
      var r = rules[i].rule;
      if (hasPrefixFold(vendor, r.vendor) && hasPrefixFold(product, r.product_prefix)) { return rules[i]; }
    }
    return null;
  }

  // presetWarning is the finding validation will give a library of this
  // vendor and product set up for target app (validate.Presets): for
  // Proxmox Backup Server, a personality without VPD page 83h. "" when it
  // suits.
  function presetWarning(pers, app, vendor, product, libID) {
    if (app !== "pbs") { return ""; }
    var m = resolveRule(pers, vendor, product);
    if (!m || m.p.vpd83) { return ""; }
    var name = "Proxmox Backup Server";
    coerceArray(pers && pers.target_apps).forEach(function (a) { if (a && a.id === app && a.name) { name = String(a.name); } });
    return "library " + coerceNumber(libID) + " is set up for " + name + ", but it finds changers by udev's ID_SCSI_SERIAL, which comes from VPD page 83h, " +
      "and the " + String(m.p.name || "") + " personality (" + String(m.p.init_fn || "") + ", selected at " + String(m.rule.source || "") + ") has no such page; " +
      "use a model that has it, such as IBM 03584 or Quantum Scalar";
  }

  // wizardDefaultHome is the home a blank "Home directory" gets: the
  // stage's mhvtl.conf when it has one, else the file on disk, as the
  // helper reads them (default_home_effective); default_home from a daemon
  // that does not report it. "" when mhvtl.conf cannot be read.
  function wizardDefaultHome(config, stageData) {
    if (stageData && stageData.default_home_effective) { return String(stageData.default_home_effective); }
    if (config && config.system && config.system.file_error) { return ""; }
    if (config && config.default_home_effective) { return String(config.default_home_effective); }
    return config && config.default_home ? String(config.default_home) : "";
  }

  // modelPresets are the models the wizard offers to fill in: each rule
  // that names both a vendor and a product, in rule order.
  function modelPresets(pers) {
    return allRules(pers).filter(function (x) { return x.rule.vendor && x.rule.product_prefix; }).map(function (x) {
      return { vendor: String(x.rule.vendor).toUpperCase(), product: String(x.rule.product_prefix), name: String(x.p.name || "") };
    });
  }

  // createLimits are the most storage slots, MAP slots and drives a new
  // library of personality p can have: its element windows
  // (personality.Model.Limits), the slots one create request adds, and
  // for drives also the IDs left after libID.
  function createLimits(p, libID) {
    var l = (p && p.limits) || {};
    var drives = Math.max(0, coerceNumber(l.drives));
    if (libID) { drives = Math.min(drives, Math.max(0, MAX_ID - coerceNumber(libID))); }
    return {
      slots: Math.min(Math.max(0, coerceNumber(l.slots)), MAX_NEW),
      maps: Math.min(Math.max(0, coerceNumber(l.maps)), MAX_NEW),
      drives: drives,
    };
  }

  // nextLibraryID is the first multiple of ten whose ID and the drives'
  // after it are free, as mhVTL's own device.conf numbers them; else the
  // first free run of that length; null when there is none.
  function nextLibraryID(used, drives) {
    var taken = {};
    coerceArray(used).forEach(function (id) { taken[coerceNumber(id)] = true; });
    drives = Math.max(0, coerceNumber(drives));
    function free(id) {
      if (id < 1 || id + drives > MAX_ID) { return false; }
      for (var i = id; i <= id + drives; i++) { if (taken[i]) { return false; } }
      return true;
    }
    for (var t = 10; t <= MAX_ID; t += 10) { if (free(t)) { return t; } }
    for (var id = 1; id <= MAX_ID; id++) { if (free(id)) { return id; } }
    return null;
  }

  // freeTargets are count targets on channel whose LUN 0 no device uses,
  // lowest first.
  function freeTargets(used, count, channel) {
    var taken = {};
    coerceArray(used).forEach(function (a) {
      if (a && coerceNumber(a.channel) === coerceNumber(channel) && coerceNumber(a.lun) === 0) { taken[coerceNumber(a.target)] = true; }
    });
    var out = [];
    for (var t = 0; out.length < count; t++) { if (!taken[t]) { out.push(t); } }
    return out;
  }

  // devices lists every library and drive of GET /config.
  // stagedDevices are the devices of the stage's device.conf (stageData is
  // its GET /config/stages answer): each Library or Drive header's ID and
  // address, with the stanza's serial and NAA. Only a staged device.conf is
  // read; one the stage leaves alone is GET /config's.
  function stagedDevices(stageData) {
    var conf = stageData && stageData.files && stageData.files["device.conf"];
    var out = [], cur = null;
    if (typeof conf !== "string") { return out; }
    conf.split("\n").forEach(function (line) {
      var h = /^(?:Library|Drive):\s*(\d+)\s+CHANNEL:\s*(\d+)\s+TARGET:\s*(\d+)\s+LUN:\s*(\d+)/.exec(line);
      if (h) {
        cur = { id: Number(h[1]), channel: Number(h[2]), target: Number(h[3]), lun: Number(h[4]), serial: "", naa: "" };
        out.push(cur);
        return;
      }
      var a = /^\s+(Unit serial number|NAA):\s*(.*?)\s*$/.exec(line);
      if (cur && a) { cur[a[1] === "NAA" ? "naa" : "serial"] = a[2]; }
    });
    return out;
  }

  // devices are config's devices, then those the stage's device.conf has:
  // what the wizard must not reuse.
  function devices(config, stageData) {
    var out = [];
    coerceArray(config && config.libraries).forEach(function (l) {
      if (!l) { return; }
      out.push(l);
      coerceArray(l.drives).forEach(function (d) { if (d) { out.push(d); } });
    });
    coerceArray(config && config.standalone_drives).forEach(function (d) { if (d) { out.push(d); } });
    var seen = {};
    out.forEach(function (d) { seen[coerceNumber(d.id)] = true; });
    stagedDevices(stageData).forEach(function (d) { if (!seen[d.id]) { out.push(d); } });
    return out;
  }

  function usedIDs(config, stageData) {
    return devices(config, stageData).map(function (d) { return coerceNumber(d.id); });
  }

  function usedAddresses(config, stageData) {
    return devices(config, stageData).map(function (d) {
      return { channel: coerceNumber(d.channel), target: coerceNumber(d.target), lun: coerceNumber(d.lun) };
    });
  }

  function usedSerials(config, stageData) {
    return devices(config, stageData).map(function (d) { return String(d.serial || ""); }).filter(function (s) { return s !== ""; });
  }

  function usedNAAs(config, stageData) {
    return devices(config, stageData).map(function (d) { return String(d.naa || ""); }).filter(function (s) { return s !== ""; });
  }

  // wizardDefaults are the wizard's starting values: a free library ID,
  // and 20 storage slots, 4 MAP slots and 2 drives, within personality
  // p's limits.
  function wizardDefaults(config, p, stageData) {
    var lim = createLimits(p);
    var drives = Math.min(2, lim.drives);
    var id = nextLibraryID(usedIDs(config, stageData), drives);
    return { id: id, slots: Math.min(20, lim.slots), maps: Math.min(4, lim.maps), drives: Math.min(drives, createLimits(p, id).drives) };
  }

  function pad(n, width) {
    var s = String(n);
    while (s.length < width) { s = "0" + s; }
    return s;
  }

  // librarySerial and driveSerial are serials unique by ID and drive slot,
  // within the 10-character policy for IDs up to 1023 and 999 drives.
  function librarySerial(id) { return "MHV" + pad(coerceNumber(id), 4); }

  function driveSerial(libID, slot, count) {
    return librarySerial(libID) + pad(coerceNumber(slot), Math.max(2, String(coerceNumber(count)).length));
  }

  function dec2(n) { return pad(coerceNumber(n), 2); }

  // naaFor is the NAA mhVTL's generate_device_conf writes: the library ID,
  // then the device's channel, target and LUN, which no two devices share,
  // each printed "%02d", in decimal digits
  // (https://github.com/markh794/mhvtl/blob/852b78a7ae3add53ffdc418add5f14999762d788/etc/generate_device_conf.in#L86).
  function naaFor(libID, channel, target, lun) {
    return [dec2(libID), "22", "33", "44", "ab", dec2(channel), dec2(target), dec2(lun)].join(":");
  }

  // driveVendorFor is the vendor a drive product is sold under, for its
  // Vendor identification.
  function driveVendorFor(product) {
    var p = String(product || "");
    if (/^(ULT3580|ULTRIUM|0359)/.test(p)) { return "IBM"; }
    if (/^Ultrium /.test(p)) { return "HP"; }
    if (/^T(10000|9[89]40)/.test(p)) { return "STK"; }
    if (/^S?DLT/.test(p)) { return "QUANTUM"; }
    if (/^SDX-/.test(p)) { return "SONY"; }
    return "";
  }

  // wizardErrors checks the wizard's values w (id, vendor, product,
  // serial, home, slots, maps, drives, driveProduct, targets) against the
  // limits and what device.conf and the stage already use (taken: {ids,
  // serials, naas}).
  function wizardErrors(w, lim, taken) {
    var errs = [];
    var ids = {}, serials = {}, naas = {};
    coerceArray(taken && taken.ids).forEach(function (i) { ids[coerceNumber(i)] = true; });
    coerceArray(taken && taken.serials).forEach(function (s) { serials[String(s)] = true; });
    coerceArray(taken && taken.naas).forEach(function (s) { naas[String(s).toLowerCase()] = true; });
    var id = coerceNumber(w.id);
    if (!(Number.isInteger(id) && id >= 1 && id <= MAX_ID)) { errs.push("The library ID must be 1 to " + MAX_ID + "."); }
    if (!String(w.vendor || "").trim()) { errs.push("Vendor identification is required."); }
    if (!String(w.product || "").trim()) { errs.push("Product identification is required."); }
    var slots = coerceNumber(w.slots), maps = coerceNumber(w.maps), drives = coerceNumber(w.drives);
    var lim1 = Math.min(1, lim.slots);
    if (!(Number.isInteger(slots) && slots >= lim1 && slots <= lim.slots)) { errs.push("Storage slots must be " + lim1 + " to " + lim.slots + "."); }
    if (!(Number.isInteger(maps) && maps >= 0 && maps <= lim.maps)) { errs.push("MAP slots must be 0 to " + lim.maps + "."); }
    var drivesOK = Number.isInteger(drives) && drives >= 0 && drives <= lim.drives;
    if (!drivesOK) { errs.push("Drives must be 0 to " + lim.drives + "."); }
    var serial = String(w.serial || "").trim();
    if (!serial) {
      errs.push("The library serial is required.");
    } else if (serial.length > MAX_SERIAL) {
      errs.push("The library serial is longer than " + MAX_SERIAL + " characters.");
    }
    var home = String(w.home || "").trim();
    if (home && home.charAt(0) !== "/") { errs.push("The home directory must be an absolute path."); }
    if (errs.length) { return errs; }
    if (ids[id]) { errs.push("ID " + id + ", for the library, is taken."); }
    if (serials[serial]) { errs.push("Serial " + serial + ", for the library, is taken."); }
    if (drives > 0 && !w.driveProduct) { errs.push("Choose a drive model."); }
    if (drives > 0 && w.driveProduct && !String(w.driveVendor || driveVendorFor(w.driveProduct)).trim()) {
      errs.push("Drive vendor identification is required.");
    }
    var targets = coerceArray(w.targets);
    var libNAA = targets.length ? naaFor(id, 0, targets[0], 0) : "";
    if (libNAA && naas[libNAA]) { errs.push("NAA " + libNAA + ", for the library, is taken."); }
    for (var i = 1; i <= drives; i++) {
      if (ids[id + i]) { errs.push("ID " + (id + i) + ", for drive " + i + ", is taken."); }
      var ds = driveSerial(id, i, drives);
      if (serials[ds]) { errs.push("Serial " + ds + ", for drive " + i + ", is taken."); }
      var dn = i < targets.length ? naaFor(id, 0, targets[i], 0) : "";
      if (dn && naas[dn]) { errs.push("NAA " + dn + ", for drive " + i + ", is taken."); }
    }
    if (targets.length < 1 + drives) { errs.push("Not enough free SCSI targets on channel 0."); }
    return errs;
  }

  // wizardRequests are the write requests that stage the wizard's library
  // (w as wizardErrors takes it, without errors): the library, on the
  // first target, then each drive in slot order on the next ones, all on
  // channel 0 LUN 0. The caller adds the stage.
  function wizardRequests(w) {
    var id = coerceNumber(w.id), drives = coerceNumber(w.drives), targets = coerceArray(w.targets);
    var attrs = [
      { key: "Vendor identification", value: String(w.vendor).trim() },
      { key: "Product identification", value: String(w.product).trim() },
      { key: "Unit serial number", value: String(w.serial).trim() },
      { key: "NAA", value: naaFor(id, 0, targets[0], 0) },
    ];
    if (String(w.home || "").trim()) { attrs.push({ key: "Home directory", value: String(w.home).trim() }); }
    var reqs = [{ method: "POST", path: "/api/v1/config/libraries",
      body: { id: id, channel: 0, target: targets[0], lun: 0, slots: coerceNumber(w.slots), maps: coerceNumber(w.maps), attrs: attrs } }];
    for (var i = 1; i <= drives; i++) {
      reqs.push({ method: "POST", path: "/api/v1/config/libraries/" + id + "/drives", body: {
        id: id + i, channel: 0, target: targets[i], lun: 0, slot: i, attrs: [
          { key: "Vendor identification", value: String(w.driveVendor || driveVendorFor(w.driveProduct)) },
          { key: "Product identification", value: String(w.driveProduct) },
          { key: "Unit serial number", value: driveSerial(id, i, drives) },
          { key: "NAA", value: naaFor(id, 0, targets[i], 0) },
        ] } });
    }
    return reqs;
  }

  // SYSTEM_KEYS are the mhvtl.conf keys PATCH /config/system sets, with
  // the values mhVTL's etc/mhvtl.conf.in ships and what each takes
  // (apply's editableKeys).
  var SYSTEM_KEYS = [
    { key: "CAPACITY", shipped: "500", valid: function (v) { return /^[1-9][0-9]*$/.test(v) && Number(v) <= 4294967296; }, says: "a size in MiB, 1 to 4294967296" },
    { key: "VERBOSE", shipped: "1", valid: function (v) { return /^[0-3]$/.test(v); }, says: "0 to 3" },
    { key: "VTL_DEBUG", shipped: "0", valid: function (v) { return /^[01]$/.test(v); }, says: "0 or 1" },
  ];

  // systemFields are the settings form's fields from GET /config's system:
  // the file's value, or "" with the shipped one to show when it has none.
  function systemFields(system) {
    var vals = (system && system.values) || {};
    return SYSTEM_KEYS.map(function (k) {
      var set = Object.prototype.hasOwnProperty.call(vals, k.key);
      return { key: k.key, value: set ? String(vals[k.key]) : "", set: set, shipped: k.shipped };
    });
  }

  // buildSystemPatch is PATCH /config/system's values: each field whose
  // trimmed value changed, checked as the helper checks it. null when
  // nothing changes. A value cannot be removed.
  function buildSystemPatch(fields, form) {
    form = form || {};
    var out = {}, any = false;
    coerceArray(fields).forEach(function (f) {
      if (!f || !Object.prototype.hasOwnProperty.call(form, f.key)) { return; }
      var v = String(form[f.key] == null ? "" : form[f.key]).trim();
      if (v === f.value) { return; }
      var k = SYSTEM_KEYS.filter(function (x) { return x.key === f.key; })[0];
      if (v === "") { throw new RangeError(f.key + " cannot be removed here; give it a value"); }
      if (!k.valid(v)) { throw new RangeError(f.key + " must be " + k.says); }
      out[f.key] = v;
      any = true;
    });
    return any ? out : null;
  }

  var API = {
    writesEditable: writesEditable,
    lineDiff: lineDiff,
    diffHunks: diffHunks,
    stageFileRows: stageFileRows,
    planSummary: planSummary,
    forceRemaining: forceRemaining,
    fmtCountdown: fmtCountdown,
    applyErrorAction: applyErrorAction,
    stageEditable: stageEditable,
    stagedDevices: stagedDevices,
    usedNAAs: usedNAAs,
    stageOutcome: stageOutcome,
    outcomeUnread: outcomeUnread,
    intField: intField,
    resolvePersonality: resolvePersonality,
    presetWarning: presetWarning,
    wizardDefaultHome: wizardDefaultHome,
    modelPresets: modelPresets,
    createLimits: createLimits,
    nextLibraryID: nextLibraryID,
    freeTargets: freeTargets,
    usedIDs: usedIDs,
    usedAddresses: usedAddresses,
    usedSerials: usedSerials,
    wizardDefaults: wizardDefaults,
    librarySerial: librarySerial,
    driveSerial: driveSerial,
    naaFor: naaFor,
    driveVendorFor: driveVendorFor,
    wizardErrors: wizardErrors,
    wizardRequests: wizardRequests,
    systemFields: systemFields,
    buildSystemPatch: buildSystemPatch,
    libraryAttrFields: libraryAttrFields,
    driveAttrFields: driveAttrFields,
    buildAttrPatch: buildAttrPatch,
    buildSlotPatch: buildSlotPatch,
    buildSlotDelete: buildSlotDelete,
    buildSlotsAdd: buildSlotsAdd,
    esc: esc,
    fmtRatio: fmtRatio,
    validBarcode: validBarcode,
    expandBarcodes: expandBarcodes,
    parseSizeMB: parseSizeMB,
    buildMediaPatch: buildMediaPatch,
    mediaLocText: mediaLocText,
    filterMedia: filterMedia,
    attr: attr,
    buildMountBody: buildMountBody,
    driveState: driveState,
    firstEmptyDrive: firstEmptyDrive,
    mountCandidates: mountCandidates,
    emptyLocations: emptyLocations,
    isStale: isStale,
    canSendMutation: canSendMutation,
    coerceArray: coerceArray,
    coerceNumber: coerceNumber,
    fmtBytes: fmtBytes,
    slotIsOccupied: slotIsOccupied,
    filterSlots: filterSlots,
    countSlots: countSlots,
    nodeRows: nodeRows,
    capabilityNotice: capabilityNotice,
    elementText: elementText,
    findingRows: findingRows,
    problemText: problemText,
    buildValidateBody: buildValidateBody,
  };

  /* ---- UMD tail ------------------------------------------------------- */
  if (typeof module !== "undefined" && module.exports) {
    module.exports = API;
  }
  // Browser global. Under strict-mode CommonJS `this` is undefined, so fall
  // back to globalThis; the browser still gets window.VTL.
  var glob = (typeof window !== "undefined") ? window
    : (typeof globalThis !== "undefined") ? globalThis
    : this;
  if (glob) { glob.VTL = API; }
})();
