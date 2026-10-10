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
  // string-coercible inputs are accepted and returned as numbers
  assert.deepStrictEqual(core.buildMountBody("0", "3"), { drive: 0, slot: 3 });
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
