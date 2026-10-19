// Node built-in test runner for the pure dashboard helpers in
// assets/static/js/core.js. This directory lives OUTSIDE assets/ on purpose so
// it is never embedded into the daemon binary (see internal/webui/embed.go,
// which does `//go:embed all:assets`).
//
// Run: node --test internal/webui/jstest
const test = require("node:test");
const assert = require("node:assert");
const core = require("../assets/static/js/core.js");

test("esc maps all five significant characters", () => {
  assert.strictEqual(core.esc("&"), "&amp;");
  assert.strictEqual(core.esc("<"), "&lt;");
  assert.strictEqual(core.esc(">"), "&gt;");
  assert.strictEqual(core.esc('"'), "&quot;");
  assert.strictEqual(core.esc("'"), "&#39;");
});

test("esc neutralizes an img/onerror XSS payload", () => {
  const out = core.esc('<img src=x onerror=alert(1)>');
  assert.ok(!out.includes("<"), "must not contain a raw <");
  assert.ok(!out.includes(">"), "must not contain a raw >");
  assert.strictEqual(out, "&lt;img src=x onerror=alert(1)&gt;");
});

test("esc neutralizes a single-quote breakout payload", () => {
  const out = core.esc("');alert(1)//");
  assert.ok(!out.includes("'"), "must not contain a raw single quote");
  assert.strictEqual(out, "&#39;);alert(1)//");
});

test("esc coerces null/undefined to empty string", () => {
  assert.strictEqual(core.esc(null), "");
  assert.strictEqual(core.esc(undefined), "");
});

test("attr escapes the five attribute-significant characters", () => {
  assert.strictEqual(core.attr('a"b\'c<d>e&f'), "a&quot;b&#39;c&lt;d&gt;e&amp;f");
});

test("buildMountBody rejects slot 0", () => {
  assert.throws(() => core.buildMountBody(1, 0), RangeError);
});

test("buildMountBody rejects undefined slot", () => {
  assert.throws(() => core.buildMountBody(1, undefined), RangeError);
});

test("buildMountBody rejects a non-integer slot", () => {
  assert.throws(() => core.buildMountBody(1, 2.5), RangeError);
  assert.throws(() => core.buildMountBody(1, "abc"), RangeError);
});

test("buildMountBody accepts a positive slot and returns {drive,slot}", () => {
  assert.deepStrictEqual(core.buildMountBody(2, 7), { drive: 2, slot: 7 });
  assert.deepStrictEqual(core.buildMountBody(2, 7, "slot"), { drive: 2, slot: 7 });
  // string-coercible inputs are accepted and returned as numbers
  assert.deepStrictEqual(core.buildMountBody("0", "3"), { drive: 0, slot: 3 });
});

test("buildMountBody with kind map returns {drive,map}", () => {
  assert.deepStrictEqual(core.buildMountBody(11, 2, "map"), { drive: 11, map: 2 });
  assert.throws(() => core.buildMountBody(11, 0, "map"), RangeError);
});

test("buildMountBody rejects an unknown source kind", () => {
  assert.throws(() => core.buildMountBody(11, 2, "drive"), RangeError);
});

test("isStale is true when generations differ, false when equal", () => {
  assert.strictEqual(core.isStale(1, 2), true);
  assert.strictEqual(core.isStale(5, 5), false);
  assert.strictEqual(core.isStale(0, 0), false);
});

test("canSendMutation is true only when enabled AND render gen matches current", () => {
  // current gen + enabled -> true
  assert.strictEqual(core.canSendMutation(3, 3, true), true);
  assert.strictEqual(core.canSendMutation(0, 0, true), true);
});

test("canSendMutation rejects a stale render generation", () => {
  // control rendered under a previous node (renderGen != currentGen) -> false,
  // even with mutations enabled. This is the cross-node destructive-op guard.
  assert.strictEqual(core.canSendMutation(2, 3, true), false);
  assert.strictEqual(core.canSendMutation(3, 2, true), false);
});

test("canSendMutation rejects when mutations are disabled", () => {
  // current gen but mutationsEnabled false -> false
  assert.strictEqual(core.canSendMutation(3, 3, false), false);
  // only the strict boolean true enables; truthy-but-not-true must not pass
  assert.strictEqual(core.canSendMutation(3, 3, 1), false);
  assert.strictEqual(core.canSendMutation(3, 3, "yes"), false);
  assert.strictEqual(core.canSendMutation(3, 3, undefined), false);
});

test("coerceArray returns arrays unchanged and non-arrays as []", () => {
  const a = [1, 2, 3];
  assert.strictEqual(core.coerceArray(a), a);
  assert.deepStrictEqual(core.coerceArray(null), []);
  assert.deepStrictEqual(core.coerceArray(undefined), []);
  assert.deepStrictEqual(core.coerceArray("nope"), []);
  assert.deepStrictEqual(core.coerceArray({ length: 2 }), []);
});

test("coerceNumber returns finite numbers and 0 otherwise", () => {
  assert.strictEqual(core.coerceNumber(42), 42);
  assert.strictEqual(core.coerceNumber("17"), 17);
  assert.strictEqual(core.coerceNumber(NaN), 0);
  assert.strictEqual(core.coerceNumber(Infinity), 0);
  assert.strictEqual(core.coerceNumber("nope"), 0);
  assert.strictEqual(core.coerceNumber(null), 0);
  assert.strictEqual(core.coerceNumber(undefined), 0);
});

test("fmtBytes formats byte counts with binary units", () => {
  assert.strictEqual(core.fmtBytes(0), "0 B");
  assert.strictEqual(core.fmtBytes(1024), "1.0 KiB");
  assert.strictEqual(core.fmtBytes(1536), "1.5 KiB");
  assert.strictEqual(core.fmtBytes("bad"), "0 B");
});

/* ---- slot occupancy / filtering / counts (US-F6) --------------------- */
const SLOTS = [
  { number: 1, barcode: "VOL001L5" },
  { number: 2, barcode: "" },
  { number: 3, barcode: "ABC123L6" },
  { number: 4 }, // no barcode key at all
  { number: 5, barcode: "VOL002L5" },
];

test("slotIsOccupied is true only for a non-empty barcode", () => {
  assert.strictEqual(core.slotIsOccupied({ barcode: "VOL001L5" }), true);
  assert.strictEqual(core.slotIsOccupied({ barcode: "" }), false);
  assert.strictEqual(core.slotIsOccupied({ number: 4 }), false);
  assert.strictEqual(core.slotIsOccupied(null), false);
  assert.strictEqual(core.slotIsOccupied({ barcode: null }), false);
});

test("slotIsOccupied prefers the live full flag over the barcode", () => {
  // A full slot whose cartridge has no readable barcode is still occupied.
  assert.strictEqual(core.slotIsOccupied({ barcode: "", full: true }), true);
  assert.strictEqual(core.slotIsOccupied({ barcode: "STALE", full: false }), false);
  // Only a real boolean counts as the live flag.
  assert.strictEqual(core.slotIsOccupied({ barcode: "", full: "yes" }), false);
});

test("filterSlots barcode query is a case-insensitive substring match", () => {
  const out = core.filterSlots(SLOTS, { query: "vol" });
  assert.deepStrictEqual(out.map((s) => s.number), [1, 5]);
  // substring anywhere, mixed case
  assert.deepStrictEqual(core.filterSlots(SLOTS, { query: "l6" }).map((s) => s.number), [3]);
  // empty query returns everything
  assert.strictEqual(core.filterSlots(SLOTS, { query: "" }).length, SLOTS.length);
});

test("filterSlots state narrows to occupied vs empty", () => {
  assert.deepStrictEqual(core.filterSlots(SLOTS, { state: "occupied" }).map((s) => s.number), [1, 3, 5]);
  assert.deepStrictEqual(core.filterSlots(SLOTS, { state: "empty" }).map((s) => s.number), [2, 4]);
  assert.strictEqual(core.filterSlots(SLOTS, { state: "all" }).length, SLOTS.length);
  // unknown state falls back to "all"
  assert.strictEqual(core.filterSlots(SLOTS, { state: "nope" }).length, SLOTS.length);
});

test("filterSlots combines query and state", () => {
  assert.deepStrictEqual(
    core.filterSlots(SLOTS, { query: "vol", state: "occupied" }).map((s) => s.number),
    [1, 5]
  );
  assert.deepStrictEqual(core.filterSlots(SLOTS, { query: "vol", state: "empty" }), []);
});

test("filterSlots tolerates non-array input and never mutates", () => {
  assert.deepStrictEqual(core.filterSlots(null, { query: "x" }), []);
  assert.deepStrictEqual(core.filterSlots("nope", {}), []);
  const copy = SLOTS.slice();
  core.filterSlots(SLOTS, { query: "vol", state: "occupied" });
  assert.deepStrictEqual(SLOTS, copy); // input untouched
});

test("countSlots returns total/occupied/empty", () => {
  assert.deepStrictEqual(core.countSlots(SLOTS), { total: 5, occupied: 3, empty: 2 });
  assert.deepStrictEqual(core.countSlots([]), { total: 0, occupied: 0, empty: 0 });
  assert.deepStrictEqual(core.countSlots(null), { total: 0, occupied: 0, empty: 0 });
});

/* ---- live changer state ---------------------------------------------- */
const LIVE = {
  id: 10,
  live: true,
  drives: [
    { id: 11, slot: 1, loaded: true, barcode: "E01005L8", source_slot: 5 },
    { id: 12, slot: 2, loaded: false },
    { id: 13, slot: 3, loaded: true, barcode: "IMP001L8", source_map: 2 },
    { id: 14, slot: 4, loaded: true, barcode: "" },
  ],
  slots: [
    { number: 1, barcode: "E01001L8", full: true },
    { number: 2, barcode: "", full: false },
    { number: 3, barcode: "", full: true },
  ],
  maps: [
    { number: 1, barcode: "", full: false },
    { number: 2, barcode: "IMP002L8", full: true },
  ],
};
// Same topology from device.conf only: drive.slot is a POSITION, and the
// configured slot barcodes must never be offered for mounting.
const CONFIG_ONLY = {
  id: 10,
  live: false,
  live_error: "changer unavailable",
  drives: [{ id: 11, slot: 1 }, { id: 12, slot: 2 }],
  slots: [{ number: 1, barcode: "E01001L8" }],
  maps: [],
};

test("driveState reads live load state and source", () => {
  assert.deepStrictEqual(core.driveState(LIVE, LIVE.drives[0]),
    { state: "loaded", barcode: "E01005L8", source: { kind: "slot", number: 5 } });
  assert.deepStrictEqual(core.driveState(LIVE, LIVE.drives[1]), { state: "empty", barcode: "", source: null });
  assert.deepStrictEqual(core.driveState(LIVE, LIVE.drives[2]).source, { kind: "map", number: 2 });
  assert.deepStrictEqual(core.driveState(LIVE, LIVE.drives[3]), { state: "loaded", barcode: "", source: null });
});

test("driveState never derives a load from drive.slot", () => {
  for (const d of CONFIG_ONLY.drives) {
    assert.strictEqual(core.driveState(CONFIG_ONLY, d).state, "unknown");
  }
  // live:true but no boolean loaded field is still unknown
  assert.strictEqual(core.driveState({ live: true }, { id: 1, slot: 3 }).state, "unknown");
  assert.strictEqual(core.driveState(null, null).state, "unknown");
});

test("firstEmptyDrive picks the first live-empty drive, null otherwise", () => {
  assert.strictEqual(core.firstEmptyDrive(LIVE), 12);
  assert.strictEqual(core.firstEmptyDrive(CONFIG_ONLY), null);
  assert.strictEqual(core.firstEmptyDrive({ live: true, drives: [{ id: 1, loaded: true }] }), null);
  assert.strictEqual(core.firstEmptyDrive(null), null);
});

test("mountCandidates lists occupied live storage then MAP slots", () => {
  assert.deepStrictEqual(core.mountCandidates(LIVE), [
    { kind: "slot", number: 1, barcode: "E01001L8" },
    { kind: "slot", number: 3, barcode: "" },
    { kind: "map", number: 2, barcode: "IMP002L8" },
  ]);
});

test("mountCandidates offers nothing without live state", () => {
  assert.deepStrictEqual(core.mountCandidates(CONFIG_ONLY), []);
  assert.deepStrictEqual(core.mountCandidates(null), []);
  assert.deepStrictEqual(core.mountCandidates({ live: true, slots: "nope", maps: null }), []);
});

test("fmtRatio renders x.xx:1 and a dash for absent or non-positive ratios", () => {
  assert.strictEqual(core.fmtRatio(1.98), "1.98:1");
  assert.strictEqual(core.fmtRatio(2), "2.00:1");
  assert.strictEqual(core.fmtRatio(null), "—");
  assert.strictEqual(core.fmtRatio(undefined), "—");
  assert.strictEqual(core.fmtRatio(0), "—");
  assert.strictEqual(core.fmtRatio(-1), "—");
  assert.strictEqual(core.fmtRatio("abc"), "—");
});

test("validBarcode accepts 1-16 alphanumerics only", () => {
  assert.strictEqual(core.validBarcode("ZT001L8"), true);
  assert.strictEqual(core.validBarcode("A".repeat(16)), true);
  assert.strictEqual(core.validBarcode("A".repeat(17)), false);
  assert.strictEqual(core.validBarcode(""), false);
  assert.strictEqual(core.validBarcode("ZT 01"), false);
  assert.strictEqual(core.validBarcode("ZT-01"), false);
  assert.strictEqual(core.validBarcode(null), false);
});

test("expandBarcodes pads to the given width", () => {
  const r = core.expandBarcodes({ prefix: "ZT", start: 1, count: 3, width: 3, suffix: "L8" });
  assert.strictEqual(r.error, null);
  assert.deepStrictEqual(r.barcodes, ["ZT001L8", "ZT002L8", "ZT003L8"]);
});

test("expandBarcodes defaults the width to the digits of the last number", () => {
  assert.deepStrictEqual(core.expandBarcodes({ prefix: "A", start: 7, count: 2, width: 0 }).barcodes, ["A7", "A8"]);
  // 9 -> 10 grows the width: every barcode gets two digits, as on the server.
  assert.deepStrictEqual(core.expandBarcodes({ prefix: "A", start: 9, count: 2 }).barcodes, ["A09", "A10"]);
  // an explicit width smaller than the last number's digits is widened
  assert.deepStrictEqual(core.expandBarcodes({ prefix: "B", start: 99, count: 2, width: 1 }).barcodes, ["B099", "B100"]);
  // form strings are accepted
  assert.deepStrictEqual(core.expandBarcodes({ prefix: "C", start: "0", count: "2", width: "", suffix: "" }).barcodes, ["C0", "C1"]);
});

test("expandBarcodes rejects invalid characters and over-long barcodes", () => {
  const bad = core.expandBarcodes({ prefix: "ZT-", start: 1, count: 1 });
  assert.match(bad.error, /not 1-16 letters or digits/);
  assert.deepStrictEqual(bad.barcodes, []);
  assert.match(core.expandBarcodes({ prefix: "ABCDEFGHIJKLMN", start: 1, count: 1, width: 3 }).error, /not 1-16/);
  assert.strictEqual(core.expandBarcodes({ prefix: "ABCDEFGHIJKLMN", start: 1, count: 1, width: 2 }).error, null);
});

test("expandBarcodes enforces count, start and width bounds", () => {
  assert.match(core.expandBarcodes({ prefix: "A", start: 1, count: 0 }).error, /count must be 1-500/);
  assert.match(core.expandBarcodes({ prefix: "A", start: 1, count: 501 }).error, /count must be 1-500/);
  assert.strictEqual(core.expandBarcodes({ prefix: "A", start: 1, count: 500 }).barcodes.length, 500);
  assert.match(core.expandBarcodes({ prefix: "A", start: -1, count: 1 }).error, /start must not be negative/);
  assert.match(core.expandBarcodes({ prefix: "A", start: 1, count: 1, width: 17 }).error, /width must be 0-16/);
  assert.match(core.expandBarcodes({ prefix: "A", start: 1.5, count: 1 }).error, /whole numbers/);
  assert.match(core.expandBarcodes({}).error, /count must be 1-500/);
});

test("parseSizeMB: blank keeps, digits parse, anything else errors", () => {
  assert.deepStrictEqual(core.parseSizeMB(""), { value: null, error: null });
  assert.deepStrictEqual(core.parseSizeMB("  "), { value: null, error: null });
  assert.deepStrictEqual(core.parseSizeMB("0"), { value: 0, error: null });
  assert.deepStrictEqual(core.parseSizeMB("2048"), { value: 2048, error: null });
  assert.ok(core.parseSizeMB("-1").error);
  assert.ok(core.parseSizeMB("1.5").error);
  assert.ok(core.parseSizeMB("abc").error);
});

test("buildMediaPatch sends only changed fields", () => {
  const orig = { barcode: "ZT001L8", write_protected: false, type: "data", density: "U-832" };
  assert.strictEqual(core.buildMediaPatch(orig, { write_protect: false, type: "", density: "", size_mb: "" }), null);
  // same type re-selected is not a change
  assert.strictEqual(core.buildMediaPatch(orig, { write_protect: false, type: "data", density: "", size_mb: "" }), null);
  assert.deepStrictEqual(core.buildMediaPatch(orig, { write_protect: true, type: "", density: "", size_mb: "" }), { write_protect: true });
  assert.deepStrictEqual(core.buildMediaPatch({ write_protected: true }, { write_protect: false }), { write_protect: false });
  assert.deepStrictEqual(core.buildMediaPatch(orig, { write_protect: false, type: "WORM", density: "LTO8", size_mb: "0" }),
    { type: "WORM", density: "LTO8", size_mb: 0 });
  assert.deepStrictEqual(core.buildMediaPatch(orig, { size_mb: "1024" }), { size_mb: 1024 });
});

test("buildMediaPatch throws on an invalid size", () => {
  assert.throws(() => core.buildMediaPatch({}, { size_mb: "-5" }), RangeError);
});

test("mediaLocText names slots, MAP, shelf and drives by id", () => {
  const drives = [{ id: 11, slot: 1 }, { id: 12, slot: 2 }];
  assert.strictEqual(core.mediaLocText({ kind: "slot", number: 3 }, drives), "slot 3");
  assert.strictEqual(core.mediaLocText({ kind: "map", number: 2 }, drives), "MAP 2");
  assert.strictEqual(core.mediaLocText({ kind: "shelf" }, drives), "shelf");
  assert.strictEqual(core.mediaLocText({ kind: "drive", number: 2 }, drives), "drive 12");
  assert.strictEqual(core.mediaLocText({ kind: "drive", number: 9 }, drives), "drive position 9");
  assert.strictEqual(core.mediaLocText(undefined, drives), "—");
});

test("filterMedia filters by barcode and location", () => {
  const carts = [
    { barcode: "ZT001L8", location: { kind: "slot", number: 1 } },
    { barcode: "ZT002L8", location: { kind: "shelf" } },
    { barcode: "E01001L8", location: { kind: "drive", number: 1 } },
    { barcode: "X1" },
  ];
  assert.strictEqual(core.filterMedia(carts, {}).length, 4);
  assert.deepStrictEqual(core.filterMedia(carts, { where: "shelf" }).map((c) => c.barcode), ["ZT002L8"]);
  assert.deepStrictEqual(core.filterMedia(carts, { where: "library" }).map((c) => c.barcode), ["ZT001L8", "E01001L8"]);
  assert.deepStrictEqual(core.filterMedia(carts, { query: "zt" }).map((c) => c.barcode), ["ZT001L8", "ZT002L8"]);
  assert.deepStrictEqual(core.filterMedia("nope", {}), []);
});

test("emptyLocations lists empty live storage then MAP slots", () => {
  const lib = {
    live: true,
    slots: [{ number: 1, barcode: "A", full: true }, { number: 2, barcode: "", full: false }],
    maps: [{ number: 1, barcode: "", full: false }, { number: 2, barcode: "B", full: true }],
  };
  assert.deepStrictEqual(core.emptyLocations(lib), [{ kind: "slot", number: 2 }, { kind: "map", number: 1 }]);
  assert.deepStrictEqual(core.emptyLocations({ live: false, slots: lib.slots }), []);
  assert.deepStrictEqual(core.emptyLocations(null), []);
});

test("nodeRows orders device nodes and separates the udev links", () => {
  const drive = {
    sg: "/dev/sg8", st: "/dev/st2", nst: "/dev/nst2",
    by_id: ["/dev/tape/by-id/scsi-00XYZZY_A1-nst", 7, ""],
    by_path: ["/dev/tape/by-path/pci-0-nst"],
  };
  assert.deepStrictEqual(core.nodeRows(drive), {
    rows: [
      { label: "nst", path: "/dev/nst2" },
      { label: "st", path: "/dev/st2" },
      { label: "sg", path: "/dev/sg8" },
    ],
    links: [
      { label: "by-id", path: "/dev/tape/by-id/scsi-00XYZZY_A1-nst" },
      { label: "by-path", path: "/dev/tape/by-path/pci-0-nst" },
    ],
  });
  assert.deepStrictEqual(core.nodeRows({ sg: "/dev/sg10", sch: "/dev/sch0" }).rows.map((r) => r.label), ["sch", "sg"]);
  assert.deepStrictEqual(core.nodeRows(undefined), { rows: [], links: [] });
  assert.deepStrictEqual(core.nodeRows({ sg: 3, by_id: "x" }), { rows: [], links: [] });
});

test("capabilityNotice: helper absent says how to enable it and offers no write", () => {
  const n = core.capabilityNotice({ helper: "absent", writes: "helper_absent", generator: true, per_library_home_media: false });
  assert.strictEqual(n.tone, "info");
  assert.strictEqual(n.command, "systemctl enable --now mhvtl-control-apply.socket");
  assert.ok(n.lines[0].startsWith("Read-only."));
  assert.deepStrictEqual(n.facts, [
    ["mhVTL systemd generator", "installed"],
    ["make_vtl_media per-library home", "not supported (mhVTL before markh794/mhvtl#276)"],
  ]);
});

test("capabilityNotice: legacy env file, TLS and an unknown payload", () => {
  const legacy = core.capabilityNotice({ helper: "legacy_env_blocks", writes: "ok" });
  assert.strictEqual(legacy.tone, "warn");
  assert.strictEqual(legacy.command, null);
  assert.ok(legacy.lines.join(" ").includes("/etc/mhvtl-control/mhvtl-control.env"));
  const tls = core.capabilityNotice({ helper: "present", writes: "tls_required" });
  assert.strictEqual(tls.tone, "warn");
  assert.ok(tls.lines.some((l) => l.includes("--tls-cert")));
  const token = core.capabilityNotice({ helper: "present", writes: "token_required" });
  assert.strictEqual(token.tone, "warn");
  assert.ok(token.lines.some((l) => l.includes("MHVTL_CONTROL_API_TOKEN") && l.includes("/etc/mhvtl-control/mhvtl-control.env")), token.lines.join(" "));
  assert.strictEqual(core.capabilityNotice({ helper: "intent_unreadable", writes: "token_required" }).tone, "error");
  // Anything unexpected reads as "no helper": read-only, with the enable hint.
  const junk = core.capabilityNotice({ helper: "<img src=x>" });
  assert.strictEqual(junk.command, "systemctl enable --now mhvtl-control-apply.socket");
  assert.strictEqual(core.capabilityNotice(null).facts[0][1], "not installed");
});

test("capabilityNotice: the helper's hello states", () => {
  const busy = core.capabilityNotice({ helper: "in_progress", writes: "ok" });
  assert.strictEqual(busy.tone, "info");
  assert.strictEqual(busy.command, null);
  assert.ok(busy.lines[0].includes("An apply is running"));
  const incomplete = core.capabilityNotice({ helper: "incomplete_apply", writes: "ok" });
  assert.strictEqual(incomplete.tone, "warn");
  assert.ok(incomplete.lines[0].includes("did not finish"));
  const unreadable = core.capabilityNotice({ helper: "intent_unreadable", writes: "ok" });
  assert.strictEqual(unreadable.tone, "error");
  assert.strictEqual(unreadable.command, null);
  const text = unreadable.lines.join(" ");
  assert.ok(text.includes("/var/lib/mhvtl-control-apply/rollback"), text);
  assert.ok(text.includes("/var/lib/mhvtl-control-apply/intent.json"), text);
  // tls_required raises an info notice to warn, and lowers no other tone.
  assert.strictEqual(core.capabilityNotice({ helper: "intent_unreadable", writes: "tls_required" }).tone, "error");
  assert.strictEqual(core.capabilityNotice({ helper: "in_progress", writes: "tls_required" }).tone, "warn");
  const down = core.capabilityNotice({ helper: "unreachable", writes: "ok", helper_error: "helper response: <b>x</b>" });
  assert.strictEqual(down.tone, "warn");
  assert.ok(down.lines.join(" ").includes("helper response: <b>x</b>"));
});

test("findingRows flattens errors then warnings with their elements", () => {
  const rows = core.findingRows({
    errors: [{ code: "preset.unmet", library: 10, message: "m1", elements: [{ kind: "library", library: 10, number: 10 }] }],
    warnings: [
      { code: "removal.move_back", library: 10, message: "m2", tag: "pre-existing",
        elements: [{ kind: "library", number: 10 }, { kind: "map", library: 10, number: 2 }, { kind: "drive", number: 11 }, { kind: "drive_line", number: 1 }] },
      null,
    ],
  });
  assert.deepStrictEqual(rows, [
    { severity: "error", code: "preset.unmet", library: 10, message: "m1", tag: "", where: "" },
    { severity: "warning", code: "removal.move_back", library: 10, message: "m2", tag: "pre-existing", where: "MAP 2, drive 11, Drive line 1" },
  ]);
  assert.deepStrictEqual(core.findingRows(undefined), []);
  assert.deepStrictEqual(core.findingRows({ errors: "x" }), []);
  assert.strictEqual(core.elementText({ kind: "__proto__", number: "x" }), "__proto__ 0");
});

test("problemText and buildValidateBody", () => {
  assert.strictEqual(core.problemText({ file: "device.conf", line: 12, message: "bad" }), "device.conf:12: bad");
  assert.strictEqual(core.problemText(null), "?:0: ");
  assert.deepStrictEqual(core.buildValidateBody(10, "pbs"), { target_apps: { "10": "pbs" } });
  assert.deepStrictEqual(core.buildValidateBody("10", ""), {});
  assert.deepStrictEqual(core.buildValidateBody(null, "pbs"), {});
  assert.deepStrictEqual(core.buildValidateBody(1.5, "pbs"), {});
});

test("kernelChNote warns only when the kernel is not known to have the ch fix", () => {
  assert.strictEqual(core.kernelChNote(null), "");
  assert.strictEqual(core.kernelChNote({ kernel_fixed: false, changers: [], mhvtl_module_refcount: 4, mhvtl_devices: 4, leaked: false }), "");
  assert.strictEqual(core.kernelChNote({ kernel_fixed: true, changers: [{ library: 10, hctl: "3:0:0:0" }], leaked: false }), "");

  assert.strictEqual(core.kernelChNote({ kernel_fixed: false, ch_loaded: true, changers: [], leaked: false }), "", "TCMU: no mhvtl.ko");

  const bound = core.kernelChNote({ kernel_fixed: false, mhvtl_module_refcount: 39, changers: [{ library: 10, hctl: "3:0:0:0" }, { library: 30, hctl: "3:0:8:0" }], leaked: false });
  assert.match(bound, /^⚠ The kernel ch driver is bound to library 10's changer, library 30's changer,/);
  assert.match(bound, /Blacklist ch/);
  assert.match(core.kernelChNote({ kernel_fixed: false, mhvtl_module_refcount: 9, changers: [{ hctl: "3:0:16:0" }] }),
    /bound to the changer at 3:0:16:0,/);

  const loaded = core.kernelChNote({ kernel_fixed: false, ch_loaded: true, mhvtl_module_refcount: 0, changers: [], leaked: false });
  assert.match(loaded, /^⚠ The kernel ch driver is loaded and binds to every changer mhVTL adds,/);

  const leaked = core.kernelChNote({ kernel_fixed: false, changers: [], mhvtl_module_refcount: 142, mhvtl_devices: 0, leaked: true });
  assert.match(leaked, /^⚠ mhvtl.ko holds 142 references with no mhVTL device left, which is what/);
  assert.doesNotMatch(leaked, /bound/);
});

test("mediaLocText and filterMedia know the picker", () => {
  assert.strictEqual(core.mediaLocText({ kind: "picker", number: 1 }, []), "picker 1");
  const carts = [{ barcode: "P1", location: { kind: "picker", number: 1 } }, { barcode: "S1", location: { kind: "shelf" } }];
  assert.deepStrictEqual(core.filterMedia(carts, { where: "library" }).map((c) => c.barcode), ["P1"]);
});
