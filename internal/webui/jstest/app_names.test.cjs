// app.js is one closure: two function declarations with the same name don't
// fail, the later one silently replaces the earlier everywhere. That turned
// the media list into renderMedia -> drawMedia -> drawPanel -> renderMedia
// and a "Maximum call stack size exceeded" error. Keep the names unique.
//
// Run: node --test internal/webui/jstest
const test = require("node:test");
const assert = require("node:assert");
const fs = require("node:fs");
const path = require("node:path");

test("app.js declares each closure-level function once", () => {
  const src = fs.readFileSync(path.join(__dirname, "../assets/static/js/app.js"), "utf8");
  const seen = new Map();
  for (const m of src.matchAll(/^  function ([A-Za-z_$][\w$]*)\s*\(/gm)) {
    seen.set(m[1], (seen.get(m[1]) || 0) + 1);
  }
  assert.ok(seen.size > 50, "found only " + seen.size + " declarations; the pattern no longer matches app.js");
  const dups = [...seen].filter(([, n]) => n > 1).map(([name]) => name);
  assert.deepStrictEqual(dups, []);
});
