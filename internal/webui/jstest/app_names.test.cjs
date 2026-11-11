// app.js and core.js are each one closure: two function declarations with
// the same name don't fail, the later one silently replaces the earlier
// everywhere, and so does a closure-level `var x =` next to `function x`.
// That turned the media list into renderMedia -> drawMedia -> drawPanel ->
// renderMedia and a "Maximum call stack size exceeded" error. Keep the
// names unique. (Duplicate let/const fail at load and need no check.)
//
// Run: node --test internal/webui/jstest
const test = require("node:test");
const assert = require("node:assert");
const fs = require("node:fs");
const path = require("node:path");

// closureNames counts the function and var names declared at the closure's
// top level (two-space indent) in src.
function closureNames(src) {
  const seen = new Map();
  for (const m of src.matchAll(/^  (?:function ([A-Za-z_$][\w$]*)\s*\(|var ([A-Za-z_$][\w$]*)\s*=)/gm)) {
    const name = m[1] || m[2];
    seen.set(name, (seen.get(name) || 0) + 1);
  }
  return seen;
}

for (const file of ["app.js", "core.js"]) {
  test(file + " declares each closure-level name once", () => {
    const seen = closureNames(fs.readFileSync(path.join(__dirname, "../assets/static/js", file), "utf8"));
    assert.ok(seen.size > 50, "found only " + seen.size + " declarations; the pattern no longer matches " + file);
    const dups = [...seen].filter(([, n]) => n > 1).map(([name]) => name);
    assert.deepStrictEqual(dups, []);
  });
}

test("closureNames counts a var next to a function of the same name", () => {
  const seen = closureNames("(function () {\n  function f() {}\n  var f = function () {};\n  var g = 1;\n})();\n");
  assert.strictEqual(seen.get("f"), 2);
  assert.strictEqual(seen.get("g"), 1);
});
