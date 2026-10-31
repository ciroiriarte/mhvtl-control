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

// Service actions (POST /api/v1/service/..., mhvtl-control#14 phase 4).
const okCaps = { writes: "ok", helper: "present" };
const svcOf = (lib10, drives, extra) => Object.assign({
  manager: "systemd",
  target: unit("mhvtl.target", "active", "active"),
  libraries: [{ id: 10, unit: lib10, persist: false, drives: drives || [] }],
  standalone_drives: [],
}, extra);
const drive = (id, active) => ({ id, unit: unit("vtltape@" + id + ".service", active, active === "active" ? "running" : "dead") });

test("serviceActionAllowed needs writes ok for every action", () => {
  const svc = svcOf(unit("vtllibrary@10.service", "active", "running"), [drive(11, "active")]);
  for (const writes of ["tls_required", "token_required", "helper_absent", "unreachable", "legacy_env_blocks", "intent_unreadable", "incomplete_apply", "in_progress", ""]) {
    for (const action of ["start", "stop", "restart"]) {
      for (const scope of ["target", 10]) {
        const got = core.serviceActionAllowed({ writes }, svc, scope, action);
        assert.strictEqual(got.ok, false, writes + " " + action + " " + scope);
        assert.match(got.reason, /configuration writes/);
      }
    }
  }
  assert.strictEqual(core.serviceActionAllowed(null, svc, 10, "stop").ok, false);
  assert.strictEqual(core.serviceActionAllowed(okCaps, svc, 10, "stop").ok, true);
  assert.strictEqual(core.serviceActionAllowed(okCaps, svc, 10, "restart").ok, true);
  assert.strictEqual(core.serviceActionAllowed(okCaps, svc, 10, "reload").ok, false);
  assert.strictEqual(core.serviceActionAllowed(okCaps, svc, 99, "stop").ok, false);
  assert.strictEqual(core.serviceActionAllowed(okCaps, Object.assign({}, svc, { manager: "none" }), 10, "stop").ok, false);
});

test("serviceActionAllowed follows D6 for start", () => {
  const running = svcOf(unit("vtllibrary@10.service", "active", "running"), [drive(11, "active")]);
  assert.deepStrictEqual(core.serviceActionAllowed(okCaps, running, 10, "start"), { ok: false, reason: "Library 10 already runs", suggest: "" });
  const drivesDown = svcOf(unit("vtllibrary@10.service", "active", "running"), [drive(11, "active"), drive(12, "failed")]);
  const got = core.serviceActionAllowed(okCaps, drivesDown, 10, "start");
  assert.strictEqual(got.ok, false);
  assert.strictEqual(got.suggest, "restart");
  assert.match(got.reason, /drives 12 down: restart it/);
  assert.strictEqual(core.serviceActionAllowed(okCaps, drivesDown, 10, "restart").ok, true);
  const down = svcOf(unit("vtllibrary@10.service", "failed", "failed"), [drive(11, "inactive")]);
  assert.strictEqual(core.serviceActionAllowed(okCaps, down, 10, "start").ok, true);
  assert.deepStrictEqual(core.serviceActionAllowed(okCaps, down, 10, "stop"), { ok: false, reason: "Library 10 is already stopped", suggest: "" });
  const busy = svcOf(unit("vtllibrary@10.service", "activating", "start"), []);
  assert.match(core.serviceActionAllowed(okCaps, busy, 10, "restart").reason, /starting or stopping/);

  // The target: start needs something down, and no running library with
  // drives down; stop needs something up.
  assert.strictEqual(core.serviceActionAllowed(okCaps, running, "target", "start").reason, "Everything already runs");
  assert.strictEqual(core.serviceActionAllowed(okCaps, drivesDown, "target", "start").suggest, "restart");
  const all = svcOf(unit("vtllibrary@10.service", "inactive", "dead"), [drive(11, "inactive")], { target: unit("mhvtl.target", "inactive", "dead") });
  assert.strictEqual(core.serviceActionAllowed(okCaps, all, "target", "start").ok, true);
  assert.strictEqual(core.serviceActionAllowed(okCaps, all, "target", "stop").reason, "Everything is already stopped");
  assert.strictEqual(core.serviceActionAllowed(okCaps, all, "target", "restart").ok, true);
  assert.strictEqual(core.serviceActionAllowed(okCaps, running, "target", "stop").ok, true);
});

const planOf = (units, extra) => Object.assign({ result: "validated", plan: { units, offline_libraries: [10] } }, extra);
const libUnits = ["vtllibrary@10.service", "vtltape@11.service"];

test("serviceConfirmText says what a restart writes, with PERSIST on and off", () => {
  const svc = svcOf(unit("vtllibrary@10.service", "active", "running"), []);
  let c = core.serviceConfirmText("restart", 10, planOf(libUnits), svc);
  assert.strictEqual(c.ok, true);
  assert.strictEqual(c.title, "Restart library 10");
  assert.strictEqual(c.okLabel, "Restart");
  assert.match(c.text, /^2 units stop and start\./);
  assert.match(c.text, /PERSIST off: the current placement is written to library_contents\.10 first\./);
  assert.match(c.text, /Library 10 is offline to hosts until all its units run\./);
  svc.libraries[0].persist = true;
  c = core.serviceConfirmText("restart", 10, planOf(libUnits), svc);
  assert.match(c.text, /PERSIST on: the placement library 10 saves at its stop is written to library_contents\.10\./);
  assert.doesNotMatch(c.text, /PERSIST off/);
});

test("serviceConfirmText for stop and start", () => {
  const svc = svcOf(unit("vtllibrary@10.service", "active", "running"), []);
  let c = core.serviceConfirmText("stop", 10, planOf(libUnits), svc);
  assert.match(c.text, /^2 units stop\./);
  assert.match(c.text, /stop leaves the library down until started; it starts again at boot\./i);
  assert.doesNotMatch(c.text, /offline to hosts/);
  c = core.serviceConfirmText("stop", "target", planOf(["mhvtl.target"].concat(libUnits)), svc);
  assert.strictEqual(c.title, "Stop mhvtl.target");
  assert.match(c.text, /Stop leaves mhVTL down until started; it starts again at boot\./);
  c = core.serviceConfirmText("start", 10, planOf(libUnits), svc);
  assert.match(c.text, /^2 units start\./);
  assert.doesNotMatch(c.text, /PERSIST/);
  assert.doesNotMatch(c.text, /leaves/);
  assert.strictEqual(core.serviceConfirmText("start", 10, planOf(["vtltape@11.service"]), svc).text.indexOf("1 unit start."), 0);
});

test("serviceConfirmText carries ch warnings verbatim and loaded drives", () => {
  const ch = "library 10: the kernel ch driver holds the changer's drives; they stay pinned until a reboot";
  const svc = svcOf(unit("vtllibrary@10.service", "active", "running"), []);
  let c = core.serviceConfirmText("restart", 10, planOf(libUnits, { warnings: [ch] }), svc);
  assert.ok(c.text.endsWith(ch), c.text);
  c = core.serviceConfirmText("restart", 10, planOf(libUnits, { result: "refused", error: { code: "drive_loaded", message: "drive 11 holds E01001L8" }, force_token: "t" }), svc);
  assert.strictEqual(c.ok, true);
  assert.match(c.text, /drive 11 holds E01001L8\. Each cartridge goes back to its slot first\./);
});

test("serviceConfirmText refuses budget, use_restart and other refusals", () => {
  const refused = (code, message) => ({ result: "refused", error: { code, message } });
  let c = core.serviceConfirmText("restart", "target", refused("budget", "restarting 4 libraries takes 41 min, over the 30 min budget"));
  assert.deepStrictEqual(c, { ok: false, title: "Restart mhvtl.target", text: "restarting 4 libraries takes 41 min, over the 30 min budget. Act on the libraries one at a time.", okLabel: "" });
  c = core.serviceConfirmText("start", 10, refused("use_restart", "library 10 is active"));
  assert.strictEqual(c.ok, false);
  assert.match(c.text, /^library 10 is active\. Use Restart instead\.$/);
  c = core.serviceConfirmText("stop", 10, refused("placement", "slot 3 holds two cartridges"));
  assert.strictEqual(c.text, "slot 3 holds two cartridges");
  c = core.serviceConfirmText("stop", 10, { error: { code: "helper_absent", message: "" } });
  assert.strictEqual(c.text, "helper_absent");
});

test("serviceResultText shows applied, rolled_back, recovered and errors", () => {
  assert.deepStrictEqual(core.serviceResultText({ result: "applied", action: "restart", library: 10 }), { tone: "ok", text: "Restart of library 10 done.", tail: "" });
  const forced = core.serviceResultText({ result: "applied", action: "stop", target: true, forced_unloads: [{ drive: 11, barcode: "E01001L8", source_slot: 3 }] });
  assert.strictEqual(forced.tone, "warn");
  assert.match(forced.text, /^Stop of mhvtl\.target done\. E01001L8 went back from drive 11 to slot 3$/);
  const rb = core.serviceResultText({ result: "rolled_back", action: "start", library: 10, failed_unit: "vtltape@11.service", journal_tail: "j", error: { code: "rolled_back", message: "start failed" } });
  assert.deepStrictEqual(rb, { tone: "error", text: "Start of library 10 failed and was rolled back: the units are back as they were. vtltape@11.service did not start. start failed", tail: "j" });
  assert.strictEqual(core.serviceResultText({ result: "recovered", error: { message: "the earlier restart was finished" } }).tone, "warn");
  assert.strictEqual(core.serviceResultText({ result: "error", action: "restart", library: 10, error: { message: "boom" } }).text, "Restart of library 10 failed. boom");
  assert.strictEqual(core.serviceResultText({ action: "stop", library: 10, error: { code: "unreachable", message: "dial" } }).text, "Stop of library 10 failed: dial");
});

test("servicePath builds only /api/v1/service routes", () => {
  assert.strictEqual(core.servicePath("target", "restart"), "/api/v1/service/restart");
  assert.strictEqual(core.servicePath("target", "stop"), "/api/v1/service/stop");
  assert.strictEqual(core.servicePath(10, "start"), "/api/v1/service/libraries/10/start");
  assert.strictEqual(core.servicePath(0, "stop"), "/api/v1/service/libraries/0/stop");
  for (const action of ["reload", "", "../config/apply", "restart/../../config", undefined, "toString"]) {
    assert.throws(() => core.servicePath("target", action), /unknown service action/);
    assert.throws(() => core.servicePath(10, action), /unknown service action/);
  }
  for (const scope of ["10", "../config", -1, 1.5, NaN, Infinity, 1e21, 2 ** 53, null, undefined, "Target"]) {
    assert.throws(() => core.servicePath(scope, "restart"), /bad service scope/);
  }
});
