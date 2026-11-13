// app.js and core.js are each one closure: two declarations with the same
// name don't fail for function/var, the later one silently replaces the
// earlier everywhere. That turned the media list into renderMedia ->
// drawMedia -> drawPanel -> renderMedia and a "Maximum call stack size
// exceeded" error. Keep the names unique.
//
// Checked, at closure level (a statement starting at exactly two-space
// indent), every declared name:
//   function name(      async function name(      function* name(
//   var / let / const name, including every comma-separated declarator
//   (`var a = 1, b = 2;`, `var a, b;`) and declarators continued on later
//   lines up to the statement's `;`.
// Not checked: destructuring declarators (`var {a} = x`), declarations
// nested deeper than two spaces, and statements without `;` that end only
// by the next closure-level line (those end there, which is the usual case).
//
// Run: node --test internal/webui/jstest
const test = require("node:test");
const assert = require("node:assert");
const fs = require("node:fs");
const path = require("node:path");

const IDENT = /[A-Za-z_$][\w$]*/y;

// skipQuoted returns the index after the string literal starting at i.
function skipQuoted(src, i) {
  const q = src[i];
  for (i++; i < src.length && src[i] !== q; i++) if (src[i] === "\\") i++;
  return i + 1;
}

// skipTemplate returns the index after the template literal starting at i,
// including any ${...} expressions.
function skipTemplate(src, i) {
  for (i++; i < src.length && src[i] !== "`"; i++) {
    if (src[i] === "\\") i++;
    else if (src[i] === "$" && src[i + 1] === "{") {
      let depth = 0;
      for (i++; i < src.length; i++) {
        const c = src[i];
        if (c === "\"" || c === "'") i = skipQuoted(src, i) - 1;
        else if (c === "`") i = skipTemplate(src, i) - 1;
        else if (c === "{") depth++;
        else if (c === "}" && --depth === 0) break;
      }
    }
  }
  return i + 1;
}

// REGEX_AFTER are the keywords after which a / starts a regex literal.
const REGEX_AFTER = new Set(["return", "typeof", "instanceof", "in", "of", "new", "delete", "void", "throw", "case", "yield", "await"]);

// BLOCK_AFTER are the keywords a { after which opens a block, not an object
// literal; a / after them starts a regex too.
const BLOCK_AFTER = new Set(["else", "do", "try", "finally"]);

// declaratorNames returns the declared names of the var/let/const statement
// whose declarator list starts at index i, and the index where it ends.
function declaratorNames(src, i) {
  const names = [];
  let depth = 0;
  let want = true; // next identifier at depth 0 is a declarator name
  let last = ""; // last significant character, to tell a regex from a division
  const braces = []; // what each open { started: "object" or "block"
  while (i < src.length) {
    const c = src[i];
    if (/\s/.test(c)) {
      // A new closure-level line ends a statement that lacks its `;`.
      if (depth === 0 && c === "\n" && /^  \S/.test(src.slice(i + 1, i + 4)) && last !== "," && last !== "=") break;
      i++;
      continue;
    }
    if (c === "/" && src[i + 1] === "/") { while (i < src.length && src[i] !== "\n") i++; continue; }
    if (c === "/" && src[i + 1] === "*") { const e = src.indexOf("*/", i + 2); i = e < 0 ? src.length : e + 2; continue; }
    if (c === "\"" || c === "'") { i = skipQuoted(src, i); last = "s"; continue; }
    if (c === "`") { i = skipTemplate(src, i); last = "s"; continue; }
    if ((c === "+" || c === "-") && src[i + 1] === c) { i += 2; last = "i"; continue; } // ++/--: a / after it divides
    if (depth !== 0 || !want) {
      // A word: after a keyword such as return a / starts a regex, after a
      // name or a property (.return) it divides.
      IDENT.lastIndex = i;
      const w = /[A-Za-z_$]/.test(c) && IDENT.exec(src);
      if (w && w.index === i) {
        i += w[0].length;
        last = last === "." ? "i" : BLOCK_AFTER.has(w[0]) ? "b" : REGEX_AFTER.has(w[0]) ? "k" : "i";
        continue;
      }
    }
    if (c === "/" && (last === "" || last === "k" || last === "b" || "=(,:[!&|?{;+-*%<>~^".includes(last))) {
      // regex literal
      for (i++; i < src.length && src[i] !== "/" && src[i] !== "\n"; i++) {
        if (src[i] === "\\") i++;
        else if (src[i] === "[") while (i < src.length && src[i] !== "]") { if (src[i] === "\\") i++; i++; }
      }
      i++;
      last = "s";
      continue;
    }
    if (c === "{") {
      // An object literal opens where an expression is expected; any other
      // brace opens a block (function body, if, arrow body).
      braces.push(last === "" || last === "k" || "=(,:[!&|?".includes(last) ? "object" : "block");
      depth++;
    } else if (c === "}") {
      depth--;
      // A / after an object literal divides; after a block it starts a
      // statement, so a regex.
      last = braces.pop() === "object" ? "i" : ";";
      i++;
      continue;
    } else if (c === "(" || c === "[") depth++;
    else if (c === ")" || c === "]") depth--;
    else if (depth === 0 && c === ";") return { names, end: i };
    else if (depth === 0 && c === ",") want = true;
    else if (depth === 0 && want) {
      IDENT.lastIndex = i;
      const m = IDENT.exec(src);
      if (m) {
        names.push(m[0]);
        want = false;
        i += m[0].length;
        last = "i";
        continue;
      }
      want = false; // destructuring pattern: not checked
    }
    last = c;
    i++;
  }
  return { names, end: i };
}

// closureNames counts every name declared at the closure's top level
// (two-space indent) in src.
function closureNames(src) {
  const seen = new Map();
  const add = (name) => seen.set(name, (seen.get(name) || 0) + 1);
  for (const m of src.matchAll(/^  (?:(?:async\s+)?function\s*\*?\s*([A-Za-z_$][\w$]*)\s*\(|(?:var|let|const)\s+)/gm)) {
    if (m[1]) add(m[1]);
    else declaratorNames(src, m.index + m[0].length).names.forEach(add);
  }
  return seen;
}

// Names each file is known to declare at closure level, functions and var
// declarators (also later in a comma list), so a pattern or tokenizer that
// stops matching fails here rather than passing with fewer names.
const KNOWN = {
  "app.js": ["renderMedia", "drawMediaList", "core", "buildMountBody", "isStale"],
  "core.js": ["coerceArray", "HTML_ESCAPE", "HELPER_ENABLE", "HELPER_STATE"],
};

for (const file of ["app.js", "core.js"]) {
  test(file + " declares each closure-level name once", () => {
    const seen = closureNames(fs.readFileSync(path.join(__dirname, "../assets/static/js", file), "utf8"));
    assert.ok(seen.size > 50, "found only " + seen.size + " declarations; the pattern no longer matches " + file);
    const dups = [...seen].filter(([, n]) => n > 1).map(([name]) => name);
    assert.deepStrictEqual(dups, []);
    for (const name of KNOWN[file]) assert.ok(seen.has(name), file + ": " + name + " not found; the pattern no longer matches");
  });
}

const names = (body) => Object.fromEntries(closureNames("(function () {\n" + body + "})();\n"));

test("closureNames counts a var next to a function of the same name", () => {
  assert.deepStrictEqual(names("  function f() {}\n  var f = function () {};\n  var g = 1;\n"), { f: 2, g: 1 });
});

test("closureNames counts every comma-separated var declarator", () => {
  assert.deepStrictEqual(names("  var a = 1, b = 2;\n  var c, d;\n  var a;\n"), { a: 2, b: 1, c: 1, d: 1 });
});

test("closureNames counts declarators continued on later lines", () => {
  const body = "  var a = f(1, 2),\n      b = { x: 1, y: [2, 3] },\n      c = 'p,q; r' + `s,${t(1, 2)};` + /,;/.source;\n  function b() {}\n";
  assert.deepStrictEqual(names(body), { a: 1, b: 2, c: 1 });
});

test("closureNames ignores commas and semicolons in comments and nested code", () => {
  const body = "  var a = function () { var z = 1, y = 2; }, // x, w;\n      b = 2; /* q, r; */\n  var z;\n";
  assert.deepStrictEqual(names(body), { a: 1, b: 1, z: 1 });
});

test("closureNames counts async and generator functions", () => {
  assert.deepStrictEqual(names("  async function a() {}\n  function* b() {}\n  function * c() {}\n  async function a() {}\n"), { a: 2, b: 1, c: 1 });
});

test("closureNames counts let and const, and a name reused across kinds", () => {
  assert.deepStrictEqual(names("  let a = 1, b;\n  const c = 2, d = 3;\n  function a() {}\n  const c = 4;\n"), { a: 2, b: 1, c: 2, d: 1 });
});

test("closureNames skips deeper declarations and ends a statement lacking its semicolon", () => {
  assert.deepStrictEqual(names("  var a = 1\n  var b = 2;\n    var inner = 3;\n  function f() {\n    var a = 1;\n  }\n"), { a: 1, b: 1, f: 1 });
});

test("closureNames tells a regex from a division after ++, a property, return and a closing brace", () => {
  assert.deepStrictEqual(names("  var i = 0;\n  var a = i++ / 2, b = 1;\n  function b() {}\n").b, 2);
  assert.deepStrictEqual(names("  var a = function () { return /;/.test(x); }, b = 1;\n  function b() {}\n").b, 2);
  assert.deepStrictEqual(names("  var a = o.return / 2, b = 1;\n  function b() {}\n").b, 2);
  assert.deepStrictEqual(names("  var a = {} / 2, b = 1;\n  function b() {}\n").b, 2);
  assert.deepStrictEqual(names("  var a = function () { if (x) {} /}/.test(x); }, b = 1;\n  function b() {}\n").b, 2);
  assert.deepStrictEqual(names("  var a = function () { if (x) {y();} /;/.test(z); }, b = 1;\n  function b() {}\n").b, 2);
  assert.deepStrictEqual(names("  var a = function () { if (x) {} else {} /,;/.test(x); }, b = 1;\n  function b() {}\n").b, 2);
  assert.deepStrictEqual(names("  var a = function () { try {} finally {} /,;/.test(x); }, b = 1;\n  function b() {}\n").b, 2);
  assert.deepStrictEqual(names("  var a = function () { if (x) y(); else /,;/.test(x); }, b = 1;\n  function b() {}\n").b, 2);
  assert.deepStrictEqual(names("  var a = { x: { y: 1 } } / 2, b = (function () {}) / 3, c = 1;\n  function c() {}\n").c, 2);
});
