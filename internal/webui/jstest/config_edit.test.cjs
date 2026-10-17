// Tests of the configuration editing helpers in core.js (plan 4.2): the
// per-file diff, the plan summary, the force countdown, the apply error
// actions and the edit bodies.
//
// Run: node --test internal/webui/jstest
const test = require("node:test");
const assert = require("node:assert");
const core = require("../assets/static/js/core.js");

test("lineDiff: an edit, an insert and a delete, line by line", () => {
  const ops = core.lineDiff("a\nb\nc\nd\n", "a\nB\nc\nd\ne\n");
  assert.deepStrictEqual(ops, [
    { op: "=", text: "a" },
    { op: "-", text: "b" },
    { op: "+", text: "B" },
    { op: "=", text: "c" },
    { op: "=", text: "d" },
    { op: "+", text: "e" },
  ]);
  assert.deepStrictEqual(core.lineDiff("", "x\n"), [{ op: "+", text: "x" }]);
  assert.deepStrictEqual(core.lineDiff("x\n", ""), [{ op: "-", text: "x" }]);
  assert.deepStrictEqual(core.lineDiff("same\n", "same\n"), [{ op: "=", text: "same" }]);
  // A missing final newline is a change mhVTL's readline sees.
  assert.deepStrictEqual(core.lineDiff("a", "a\n").map((o) => o.op), ["-", "+"]);
});

test("diffHunks keeps 3 lines of context and numbers each hunk", () => {
  const old = Array.from({ length: 20 }, (_, i) => "l" + (i + 1)).join("\n") + "\n";
  const nu = old.replace("l10\n", "L10\n");
  const hunks = core.diffHunks(core.lineDiff(old, nu), 3);
  assert.strictEqual(hunks.length, 1);
  assert.strictEqual(hunks[0].oldStart, 7);
  assert.strictEqual(hunks[0].newStart, 7);
  assert.deepStrictEqual(hunks[0].lines.map((l) => l.op + l.text), ["=l7", "=l8", "=l9", "-l10", "+L10", "=l11", "=l12", "=l13"]);
  // Two changes far apart are two hunks; close ones merge.
  assert.strictEqual(core.diffHunks(core.lineDiff(old, nu.replace("l1\n", "L1\n").replace("l20\n", "L20\n")), 3).length, 3);
  assert.strictEqual(core.diffHunks(core.lineDiff(old, nu.replace("l13\n", "L13\n")), 3).length, 1);
  assert.deepStrictEqual(core.diffHunks(core.lineDiff(old, old), 3), []);
});

test("stageFileRows: modified, new and deleted files against the files on disk", () => {
  const files = [
    { name: "device.conf", content: "Library: 10\n Backoff: 400\n" },
    { name: "library_contents.30", content: "VERSION: 2\n" },
  ];
  const rows = core.stageFileRows({
    files: { "device.conf": "Library: 10\n Backoff: 500\n", "library_contents.50": "Slot 1:\n", "library_contents.30": null },
  }, files);
  assert.deepStrictEqual(rows.map((r) => [r.name, r.kind]), [
    ["device.conf", "modify"], ["library_contents.30", "delete"], ["library_contents.50", "create"],
  ]);
  assert.deepStrictEqual(rows[0].hunks[0].lines.map((l) => l.op + l.text), ["=Library: 10", "- Backoff: 400", "+ Backoff: 500"]);
  assert.deepStrictEqual(rows[1].hunks[0].lines.map((l) => l.op + l.text), ["-VERSION: 2"]);
  assert.deepStrictEqual(rows[2].hunks[0].lines.map((l) => l.op + l.text), ["+Slot 1:"]);
  assert.deepStrictEqual(core.stageFileRows(null, files), []);
  assert.deepStrictEqual(core.stageFileRows(JSON.parse('{"files": {"__proto__": "x"}}'), null).map((r) => r.kind), ["create"]);
});

test("planSummary: units, libraries offline, #43 warnings and a force offer", () => {
  const ok = core.planSummary({
    result: "validated", warnings: ["slot 25: NEW025L8 has no cartridge"],
    plan: { units: ["vtllibrary@10.service", "vtltape@11.service"], notes: ["n1"], offline_libraries: [10] },
    findings: { errors: [], warnings: [{ code: "w", library: 10, message: "m" }] },
  });
  assert.strictEqual(ok.canApply, true);
  assert.strictEqual(ok.force, null);
  assert.deepStrictEqual(ok.units, ["vtllibrary@10.service", "vtltape@11.service"]);
  assert.deepStrictEqual(ok.offline, ["Library 10 is offline to hosts during the apply."]);
  assert.deepStrictEqual(ok.warnings, ["slot 25: NEW025L8 has no cartridge"]);
  assert.deepStrictEqual(ok.notes, ["n1"]);
  assert.strictEqual(ok.findings.length, 1);

  const forced = core.planSummary({
    result: "refused", error: { code: "drive_loaded", message: "loaded drives the apply would stop: library 10 drive 1" },
    force_token: "tok", force_expires: "2026-10-17T12:05:00Z",
    plan: { units: ["vtllibrary@10.service", "vtltape@11.service", "vtltape@12.service"], offline_libraries: [10] },
  });
  assert.strictEqual(forced.canApply, false);
  assert.deepStrictEqual(forced.force, {
    token: "tok", expires: "2026-10-17T12:05:00Z",
    drives: ["vtltape@11.service", "vtltape@12.service"],
    message: "loaded drives the apply would stop: library 10 drive 1",
  });

  const refused = core.planSummary({ result: "refused", error: { code: "invalid", message: "x" }, findings: { errors: [{ code: "e" }] } });
  assert.strictEqual(refused.canApply, false);
  assert.strictEqual(refused.force, null);
  assert.strictEqual(refused.error, "x");
  assert.strictEqual(core.planSummary(undefined).canApply, false);
});

test("forceRemaining and fmtCountdown", () => {
  const now = Date.parse("2026-10-17T12:00:00Z");
  assert.strictEqual(core.forceRemaining("2026-10-17T12:05:00Z", now), 300);
  assert.strictEqual(core.forceRemaining("2026-10-17T11:59:00Z", now), 0);
  assert.strictEqual(core.forceRemaining("garbage", now), 0);
  assert.strictEqual(core.fmtCountdown(300), "5:00");
  assert.strictEqual(core.fmtCountdown(61), "1:01");
  assert.strictEqual(core.fmtCountdown(0), "expired");
});

test("applyErrorAction maps an apply or staging error to what the UI does", () => {
  const act = (status, code) => core.applyErrorAction({ status: status, code: code });
  assert.strictEqual(act(409, "conflict"), "reload");
  assert.strictEqual(act(404, "stage_not_found"), "reload");
  assert.strictEqual(act(409, "plan_required"), "replan");
  assert.strictEqual(act(409, "recovered"), "replan");
  assert.strictEqual(act(409, "helper_absent"), "readonly");
  assert.strictEqual(act(403, "token_required"), "readonly");
  assert.strictEqual(act(403, "tls_required"), "readonly");
  assert.strictEqual(act(409, "intent_unreadable"), "readonly");
  assert.strictEqual(act(422, "legacy_env"), "readonly");
  assert.strictEqual(act(422, "drive_loaded"), "force");
  assert.strictEqual(act(422, "invalid"), "show");
  assert.strictEqual(act(500, "rolled_back"), "show");
  assert.strictEqual(core.applyErrorAction(null), "show");
});

test("writesEditable and the notice when edits are open", () => {
  assert.strictEqual(core.writesEditable({ helper: "present", writes: "ok" }), true);
  ["helper_absent", "tls_required", "token_required", "helper_blocked", undefined].forEach((w) => {
    assert.strictEqual(core.writesEditable({ helper: "present", writes: w }), false, String(w));
  });
  const open = core.capabilityNotice({ helper: "present", writes: "ok" });
  assert.ok(!open.lines[0].includes("read-only") && open.lines[0].includes("staged"), open.lines[0]);
  const closed = core.capabilityNotice({ helper: "present", writes: "token_required" });
  assert.ok(closed.lines[0].startsWith("Read-only."), closed.lines[0]);
});

test("attrFields and buildAttrPatch", () => {
  const lib = { vendor: "STK", product: "L700", serial: "XYZZY_A", naa: "10:22", home_dir: "" };
  assert.deepStrictEqual(core.libraryAttrFields(lib), [
    { key: "Vendor identification", value: "STK" },
    { key: "Product identification", value: "L700" },
    { key: "Unit serial number", value: "XYZZY_A" },
    { key: "NAA", value: "10:22" },
    { key: "Home directory", value: "" },
  ]);
  assert.deepStrictEqual(core.driveAttrFields({ vendor: "IBM", product: "ULT3580-TD8", revision: "HB81", serial: "S1", naa: "" }).map((f) => f.key), [
    "Vendor identification", "Product identification", "Product revision level", "Unit serial number", "NAA",
  ]);
  const fields = core.libraryAttrFields(lib);
  assert.deepStrictEqual(core.buildAttrPatch(fields, { "Product identification": "L700 ", "NAA": "", "Home directory": "/srv/tapes" }), {
    "NAA": null, "Home directory": "/srv/tapes",
  });
  assert.strictEqual(core.buildAttrPatch(fields, { "Vendor identification": "STK" }), null);
  // A field not offered is never sent.
  assert.strictEqual(core.buildAttrPatch(fields, { "Library ID": "30 Slot: 01" }), null);
});

test("slot edit bodies", () => {
  assert.deepStrictEqual(core.buildSlotPatch("stg", "slot", " E01001L8 "), { stage: "stg", kind: "slot", barcode: "E01001L8" });
  assert.deepStrictEqual(core.buildSlotPatch("", "map", ""), { kind: "map", barcode: "" });
  assert.throws(() => core.buildSlotPatch("", "slot", "two words"), RangeError);
  assert.deepStrictEqual(core.buildSlotDelete("stg", "slot", "E01001L8", { to: "slot", slot: "7" }), { stage: "stg", kind: "slot", disposition: { to: "slot", slot: 7 } });
  assert.deepStrictEqual(core.buildSlotDelete("", "slot", "", null), { kind: "slot" });
  assert.deepStrictEqual(core.buildSlotDelete("", "map", "X", { to: "shelf" }), { kind: "map", disposition: { to: "shelf" } });
  assert.throws(() => core.buildSlotDelete("", "slot", "X", { to: "slot", slot: "0" }), RangeError);
  assert.throws(() => core.buildSlotDelete("", "slot", "X", null), RangeError);
  assert.deepStrictEqual(core.buildSlotsAdd("", "map", "3"), { kind: "map", count: 3 });
  assert.throws(() => core.buildSlotsAdd("", "slot", "0"), RangeError);
});
