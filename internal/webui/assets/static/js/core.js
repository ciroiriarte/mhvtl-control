/*
 * mhvtld dashboard — pure, framework-free helpers.
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

  var API = {
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
