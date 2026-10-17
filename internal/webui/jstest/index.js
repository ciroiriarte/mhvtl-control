// Entry point so `node --test internal/webui/jstest` works on Node builds whose
// test runner resolves a directory argument to its index module rather than
// recursively discovering *.test.cjs files. Requiring the test file registers
// its node:test cases in this process.
//
// This file contains no tests of its own; the suites live in core.test.cjs
// and config_edit.test.cjs.
require("./core.test.cjs");
require("./config_edit.test.cjs");
