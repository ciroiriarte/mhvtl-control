// The System panel's Service table (GET /api/v1/service, mhvtl-control#14).
// Run: node --test internal/webui/jstest
const test = require("node:test");
const assert = require("node:assert");
const core = require("../assets/static/js/core.js");

test("fmtUptime keeps the two largest units", () => {
  assert.strictEqual(core.fmtUptime(0), "");
  assert.strictEqual(core.fmtUptime(null), "");
  assert.strictEqual(core.fmtUptime(999999), "");
  assert.strictEqual(core.fmtUptime(42e6), "42 s");
  assert.strictEqual(core.fmtUptime(125e6), "2 min");
  assert.strictEqual(core.fmtUptime(3600e6), "1 h");
  assert.strictEqual(core.fmtUptime(3725e6), "1 h 2 min");
  assert.strictEqual(core.fmtUptime((2 * 86400 + 5 * 3600 + 7) * 1e6), "2 d 5 h");
  assert.strictEqual(core.fmtUptime(86400e6 + 59e6), "1 d");
});

const unit = (name, active, sub, extra) => Object.assign({ name, load_state: "loaded", active_state: active, sub_state: sub }, extra);

test("serviceRows lists the target, support units, libraries and standalone drives", () => {
  const rows = core.serviceRows({
    manager: "systemd",
    target: unit("mhvtl.target", "active", "active", { uptime_us: 90e6 }),
    support: [
      unit("mhvtl-load-modules.service", "active", "exited"),
      { name: "mhvtl-tcmu-handler.service", load_state: "not-found", active_state: "inactive", sub_state: "dead" },
      unit("mhvtl-bay-monitor.service", "activating", "start"),
    ],
    libraries: [
      { id: 10, unit: unit("vtllibrary@10.service", "active", "running", { main_pid: 4711, uptime_us: 3725e6 }), persist: true, ch_exposed: true,
        drives: [{ id: 11, unit: unit("vtltape@11.service", "active", "running") }, { id: 12, unit: unit("vtltape@12.service", "failed", "failed") }] },
      { id: 30, unit: unit("vtllibrary@30.service", "failed", "failed", { result: "exit-code" }), drives: [] },
    ],
    standalone_drives: [{ id: 50, unit: unit("vtltape@50.service", "inactive", "dead") }],
  });
  assert.deepStrictEqual(rows.map((r) => r.key), ["svc-target", "svc-mhvtl-load-modules.service", "svc-mhvtl-tcmu-handler.service",
    "svc-mhvtl-bay-monitor.service", "svc-lib-10", "svc-lib-30", "svc-drive-50"]);
  assert.deepStrictEqual(rows[0], { key: "svc-target", name: "mhvtl.target", state: "active", cls: "st-active", pid: "", uptime: "1 min", notes: [] });
  assert.strictEqual(rows[1].name, "mhvtl-load-modules");
  assert.strictEqual(rows[1].state, "active (exited)");
  assert.strictEqual(rows[2].state, "not installed");
  assert.strictEqual(rows[2].cls, "");
  assert.strictEqual(rows[3].cls, "st-transition");
  assert.deepStrictEqual(rows[4], { key: "svc-lib-10", name: "library 10", state: "active (running)", cls: "st-active", pid: "4711",
    uptime: "1 h 2 min", notes: ["drives 1/2 active", "PERSIST", "ch exposed"] });
  assert.strictEqual(rows[5].cls, "st-failed");
  assert.deepStrictEqual(rows[5].notes, ["drives 0/0 active"]);
  assert.deepStrictEqual(rows[6].notes, ["standalone"]);
  assert.strictEqual(rows[6].state, "inactive (dead)");
});

test("serviceRows without a service manager lists the libraries only, state unknown", () => {
  const rows = core.serviceRows({ manager: "unavailable", libraries: [{ id: 10, drives: [{ id: 11 }] }] });
  assert.deepStrictEqual(rows, [{ key: "svc-lib-10", name: "library 10", state: "unknown", cls: "", pid: "", uptime: "", notes: ["1 drive"] }]);
  assert.deepStrictEqual(core.serviceRows(null), []);
  assert.deepStrictEqual(core.serviceRows({ manager: "systemd", libraries: "x" }), [{ key: "svc-target", name: "mhvtl.target", state: "unknown", cls: "", pid: "", uptime: "", notes: [] }]);
});

test("serviceBackendText says what is configured, loaded and whether the queue exists", () => {
  assert.strictEqual(core.serviceBackendText({ backend: { configured: "mhvtl", kernel_module: { loaded: true, refcount: 9 }, tcmu: {} }, queue: { exists: true } }),
    "Backend mhvtl; loaded: mhvtl.ko (9 refs); queue present");
  assert.strictEqual(core.serviceBackendText({ backend: { configured: "tcmu", kernel_module: { loaded: true }, tcmu: { target_core_user: true } }, queue: {} }),
    "Backend tcmu; loaded: mhvtl.ko, target_core_user; queue absent");
  assert.strictEqual(core.serviceBackendText({}), "Backend mhvtl; loaded: none; queue absent");
});
