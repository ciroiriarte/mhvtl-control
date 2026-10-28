// Tests of the view-state helpers in core.js (#77): the payload signature
// that lets a refresh skip an unchanged rebuild, and the capture/restore of
// scroll offsets, open <details> and focus across a rebuild. The elements
// are plain objects with the few members the helpers use.
//
// Run: node --test internal/webui/jstest
const test = require("node:test");
const assert = require("node:assert");
const core = require("../assets/static/js/core.js");

// el builds a fake element; children are searched by querySelectorAll.
function el(tag, key, props) {
  const n = Object.assign({ tagName: tag, scrollTop: 0, scrollLeft: 0, open: false, children: [], focusCalls: [] }, props || {});
  n.getAttribute = (a) => (a === "data-view-key" ? (key == null ? null : key) : null);
  n.focus = (opts) => { n.focusCalls.push(opts); };
  return n;
}
function root(children) {
  const r = el("DIV", null, { children: children });
  const all = () => {
    const out = [];
    const walk = (n) => n.children.forEach((c) => { out.push(c); walk(c); });
    walk(r);
    return out;
  };
  r.querySelectorAll = (sel) => {
    assert.strictEqual(sel, "[data-view-key]");
    return all().filter((n) => n.getAttribute("data-view-key") != null);
  };
  r.contains = (n) => n === r || all().indexOf(n) >= 0;
  return r;
}

test("viewSignature ignores key order and sees every change", () => {
  const a = { libraries: [{ id: 10, slots: [{ number: 1, barcode: "E01001L8" }], live: true }] };
  const b = { libraries: [{ live: true, slots: [{ barcode: "E01001L8", number: 1 }], id: 10 }] };
  assert.strictEqual(core.viewSignature(a, [1, true]), core.viewSignature(b, [1, true]));
  const c = { libraries: [{ id: 10, slots: [{ number: 1, barcode: "" }], live: true }] };
  assert.notStrictEqual(core.viewSignature(a, [1, true]), core.viewSignature(c, [1, true]));
  // The extras (generation, mutations enabled, ...) count too.
  assert.notStrictEqual(core.viewSignature(a, [1, true]), core.viewSignature(a, [2, true]));
  assert.notStrictEqual(core.viewSignature(a, [1, true]), core.viewSignature(a, [1, false]));
  // Arrays keep their order: a reordered slot list is a different view.
  assert.notStrictEqual(core.viewSignature([1, 2]), core.viewSignature([2, 1]));
  // An undefined field is the same as a missing one, as in JSON.
  assert.strictEqual(core.viewSignature({ a: 1, b: undefined }), core.viewSignature({ a: 1 }));
});

test("captureView records scroll, <details> and the focused key inside root", () => {
  const scroll = el("DIV", "lib10/slots", { scrollTop: 240, scrollLeft: 8 });
  const still = el("DIV", "media");
  const det = el("DETAILS", "cfg/personalities", { open: true });
  const shut = el("DETAILS", "lib10/drive1/links");
  const btn = el("BUTTON", "lib10/drive1/load");
  const plain = el("SPAN", null, { scrollTop: 99 });
  const r = root([scroll, still, det, shut, el("DIV", "card", { children: [btn, plain] })]);

  const saved = core.captureView(r, btn);
  assert.deepStrictEqual(saved.scroll, { "lib10/slots": { top: 240, left: 8 } });
  assert.deepStrictEqual(saved.open, { "cfg/personalities": true, "lib10/drive1/links": false });
  assert.strictEqual(saved.focus, "lib10/drive1/load");
});

test("captureView ignores focus outside root and an unkeyed focused element", () => {
  const r = root([el("BUTTON", "a")]);
  assert.strictEqual(core.captureView(r, el("INPUT", "slot-search")).focus, null);
  const unkeyed = el("BUTTON", null);
  const r2 = root([unkeyed]);
  assert.strictEqual(core.captureView(r2, unkeyed).focus, null);
  assert.strictEqual(core.captureView(r2, null).focus, null);
  assert.strictEqual(core.captureView(r2, r2).focus, null);
});

test("restoreView puts state back on the rebuilt elements with the same keys", () => {
  const saved = {
    scroll: { "lib10/slots": { top: 240, left: 8 } },
    open: { "cfg/personalities": true, "lib10/drive1/links": false },
    focus: "lib10/drive1/load",
  };
  // The rebuilt panel: new objects, same keys; the drive's Mount became
  // Unmount under the same key.
  const scroll = el("DIV", "lib10/slots");
  const det = el("DETAILS", "cfg/personalities");
  const shut = el("DETAILS", "lib10/drive1/links", { open: true });
  const unmount = el("BUTTON", "lib10/drive1/load");
  const other = el("BUTTON", "lib10/drive1/stats");
  core.restoreView(root([scroll, det, shut, unmount, other]), saved);

  assert.strictEqual(scroll.scrollTop, 240);
  assert.strictEqual(scroll.scrollLeft, 8);
  assert.strictEqual(det.open, true);
  assert.strictEqual(shut.open, false);
  assert.deepStrictEqual(unmount.focusCalls, [{ preventScroll: true }]);
  assert.deepStrictEqual(other.focusCalls, []);
});

test("restoreView skips keys that are gone and leaves unknown elements alone", () => {
  const saved = { scroll: { gone: { top: 5, left: 0 } }, open: { "gone/details": true }, focus: "gone/button" };
  const fresh = el("DETAILS", "new/details", { open: true });
  const btn = el("BUTTON", "new/button");
  assert.doesNotThrow(() => core.restoreView(root([fresh, btn]), saved));
  assert.strictEqual(fresh.open, true); // not in saved: untouched
  assert.deepStrictEqual(btn.focusCalls, []);
  assert.doesNotThrow(() => core.restoreView(root([]), null));
});

test("restoreView does not move focus that was outside root", () => {
  const btn = el("BUTTON", "lib10/drive1/load");
  const outside = el("INPUT", "slot-search");
  const saved = core.captureView(root([el("BUTTON", "lib10/drive1/load")]), outside);
  core.restoreView(root([btn]), saved);
  assert.deepStrictEqual(btn.focusCalls, []);
});

test("restoreView focuses only the first element of a duplicated key", () => {
  const a = el("BUTTON", "dup"), b = el("BUTTON", "dup");
  core.restoreView(root([a, b]), { scroll: {}, open: {}, focus: "dup" });
  assert.strictEqual(a.focusCalls.length, 1);
  assert.strictEqual(b.focusCalls.length, 0);
});
