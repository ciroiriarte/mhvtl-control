// The Transport Map's iSCSI export (fabrics.conf through
// /api/v1/transports/iscsi): request bodies, rows, action text, outcomes.
// Run: node --test internal/webui/jstest
const test = require("node:test");
const assert = require("node:assert");
const core = require("../assets/static/js/core.js");

const SECRET = "s3cr3t-Pa55word!";
const MUTUAL = "MuTuAl-s3cret-99";

test("iscsiList trims lines and drops blank ones", () => {
  assert.deepStrictEqual(core.iscsiList(" iqn.a \r\n\n  iqn.b\n   \n"), ["iqn.a", "iqn.b"]);
  assert.deepStrictEqual(core.iscsiList(""), []);
  assert.deepStrictEqual(core.iscsiList(null), []);
});

test("buildISCSISettingsBody sends only what changed", () => {
  const cur = { present: true, portals: ["10.0.0.1:3260"], base_name: "iqn.2026-01.org.example" };
  assert.deepStrictEqual(core.buildISCSISettingsBody(cur, { portals: "10.0.0.1:3260\n", base_name: " iqn.2026-01.org.example " }, true),
    { dry_run: true });
  assert.deepStrictEqual(core.buildISCSISettingsBody(cur, { portals: "10.0.0.1:3260\n10.0.0.2:3260", base_name: "iqn.2026-01.org.example" }, false),
    { portals: ["10.0.0.1:3260", "10.0.0.2:3260"], dry_run: false });
  assert.deepStrictEqual(core.buildISCSISettingsBody(cur, { portals: "10.0.0.1:3260", base_name: "iqn.2026-02.org.other" }, true),
    { base_name: "iqn.2026-02.org.other", dry_run: true });
  // No current settings: both are sent.
  assert.deepStrictEqual(core.buildISCSISettingsBody(null, { portals: "", base_name: "iqn.x" }, true),
    { portals: [], base_name: "iqn.x", dry_run: true });
});

test("buildISCSIBindBody for a new export", () => {
  const r = core.buildISCSIBindBody(10, { initiators: " iqn.1994-05.com.redhat:a \n\niqn.b\n", target_name: "  ", chap_name: "", chap_secret: "" }, null, true);
  assert.deepStrictEqual(r, { body: { library: 10, initiators: ["iqn.1994-05.com.redhat:a", "iqn.b"], dry_run: true } });
  // No initiators listed on a new export: the field is left to the daemon.
  assert.deepStrictEqual(core.buildISCSIBindBody(50, {}, null, false), { body: { library: 50, dry_run: false } });
  const withChap = core.buildISCSIBindBody(10, { initiators: "iqn.a", target_name: "iqn.t", chap_name: " user ", chap_secret: SECRET,
    mutual_name: "tgt", mutual_secret: MUTUAL }, null, true);
  assert.deepStrictEqual(withChap.body, { library: 10, initiators: ["iqn.a"], target_name: "iqn.t",
    chap: { name: "user", secret: SECRET }, mutual_chap: { name: "tgt", secret: MUTUAL }, dry_run: true });
});

test("buildISCSIBindBody on an existing record: empty CHAP is unchanged, clear flags, lists", () => {
  const existing = { target_name: "iqn.t", initiators: ["iqn.a"], chap: true, mutual_chap: true };
  // Unchanged form: only the initiators (sent as listed) and dry_run.
  assert.deepStrictEqual(core.buildISCSIBindBody(10, { initiators: "iqn.a", target_name: "iqn.t", chap_name: "", chap_secret: "" }, existing, true).body,
    { library: 10, initiators: ["iqn.a"], dry_run: true });
  // Clearing the target name sends "".
  assert.deepStrictEqual(core.buildISCSIBindBody(10, { initiators: "iqn.a", target_name: "" }, existing, true).body,
    { library: 10, initiators: ["iqn.a"], target_name: "", dry_run: true });
  // Emptying the initiators of a record is refused here, never sent as [].
  const emptied = core.buildISCSIBindBody(10, { initiators: "\n", target_name: "iqn.t" }, existing, true);
  assert.strictEqual(emptied.body, undefined);
  assert.strictEqual(emptied.field, "initiators");
  assert.ok(/exports nothing/.test(emptied.error) && /Stop exporting/.test(emptied.error));
  // Remove CHAP wins over typed CHAP fields, and sends no credentials.
  const cleared = core.buildISCSIBindBody(10, { initiators: "iqn.a", target_name: "iqn.t", chap_name: "x", chap_secret: SECRET,
    clear_chap: true, clear_mutual_chap: true }, existing, false).body;
  assert.deepStrictEqual(cleared, { library: 10, initiators: ["iqn.a"], clear_chap: true, clear_mutual_chap: true, dry_run: false });
  assert.ok(!JSON.stringify(cleared).includes(SECRET));
});

test("buildISCSIBindBody refuses half a CHAP pair, naming the field", () => {
  const a = core.buildISCSIBindBody(10, { chap_name: "user", chap_secret: "" }, null, true);
  assert.strictEqual(a.field, "chap");
  assert.ok(/CHAP needs both a name and a secret/.test(a.error));
  assert.strictEqual(a.body, undefined);
  const b = core.buildISCSIBindBody(10, { mutual_name: "", mutual_secret: MUTUAL }, null, true);
  assert.strictEqual(b.field, "mutual_chap");
  assert.ok(!b.error.includes(MUTUAL));
  assert.strictEqual(core.buildISCSIBindBody(0, {}, null, true).field, "library");
});

test("iscsiApplyBody sends the previewed body with base_sha256", () => {
  const previewed = { library: 10, initiators: ["iqn.a"], chap: { name: "u", secret: SECRET }, dry_run: true };
  const body = core.iscsiApplyBody(previewed, "abc123");
  assert.deepStrictEqual(body, { library: 10, initiators: ["iqn.a"], chap: { name: "u", secret: SECRET }, dry_run: false, base_sha256: "abc123" });
  assert.strictEqual(previewed.dry_run, true); // the preview is left as it was
  // No file when previewed: base_sha256 "" (present, not omitted).
  assert.deepStrictEqual(core.iscsiApplyBody({ library: 30, dry_run: true }, ""), { library: 30, dry_run: false, base_sha256: "" });
  assert.deepStrictEqual(core.iscsiApplyBody({ portals: [], dry_run: true }, undefined), { portals: [], dry_run: false, base_sha256: "" });
});

test("buildISCSIUnbindBody", () => {
  assert.deepStrictEqual(core.buildISCSIUnbindBody("30", true), { library: 30, dry_run: true });
  assert.deepStrictEqual(core.buildISCSIUnbindBody(30, false), { library: 30, dry_run: false });
});

const config = {
  libraries: [{ id: 10, vendor: "STK" }, { id: 30, vendor: "IBM" }],
  standalone_drives: [{ id: 50 }],
};
const iscsiCfg = {
  exists: true, sha256: "abc", iscsi: { present: true, portals: ["0.0.0.0:3260"], base_name: "iqn.2026-01.org.example" },
  targets: [
    { id: 10, target_name: "", wwn: "iqn.2026-01.org.example:lib10", initiators: ["iqn.a", "iqn.b"], chap: true, chap_name: "user",
      mutual_chap: false, mutual_chap_name: "", secret: SECRET, chap_secret: SECRET, device: { kind: "library", id: 10 } },
    { id: 50, target_name: "iqn.custom:d50", wwn: "iqn.custom:d50", initiators: [], chap: false, mutual_chap: true,
      mutual_chap_name: "tgt", mutual_chap_secret: MUTUAL, device: { kind: "drive", id: 50 } },
    { id: 70, wwn: "iqn.2026-01.org.example:lib70", initiators: [], chap: false, mutual_chap: false, device: null },
  ],
  problems: [{ line: 4, message: "unknown key Foo" }], warnings: ["Portal 0.0.0.0:3260 listens on every address"],
};
const transports = {
  configfs: true,
  fabrics: [{ name: "iscsi", transport: "iSCSI", targets: [
    { wwn: "iqn.2026-01.org.example:lib10", tpgs: [{ tag: 1, enabled: true, luns: [{ lun: 0 }, { lun: 1 }],
      acls: [{ initiator: "iqn.a", session: { state: "TARG_SESS_STATE_LOGGED_IN" } }, { initiator: "iqn.b" }], dynamic_sessions: [] }] },
  ] }],
};

test("iscsiRows merges device.conf, fabrics.conf targets and configfs", () => {
  const rows = core.iscsiRows(config, iscsiCfg, transports);
  assert.deepStrictEqual(rows.map((r) => [r.key, r.label, r.exported, r.orphan]), [
    ["iscsi-library-10", "Library 10", true, false],
    ["iscsi-library-30", "Library 30", false, false],
    ["iscsi-drive-50", "Drive 50 (standalone)", true, false],
    ["iscsi-library-70", "Library 70 (not in device.conf)", true, true],
  ]);
  const [l10, l30, d50, o70] = rows;
  assert.strictEqual(l10.wwn, "iqn.2026-01.org.example:lib10");
  assert.deepStrictEqual(l10.initiators, ["iqn.a", "iqn.b"]);
  assert.strictEqual(l10.chap, "CHAP: on (user)");
  assert.strictEqual(l10.mutualChap, "Mutual CHAP: off");
  assert.strictEqual(l10.present, true);
  assert.strictEqual(l10.enabled, true);
  assert.strictEqual(l10.luns, 2);
  assert.strictEqual(l10.sessions, 1);
  assert.deepStrictEqual(l10.target, { target_name: "", initiators: ["iqn.a", "iqn.b"], chap: true, mutual_chap: false });
  assert.strictEqual(l30.present, null);
  assert.strictEqual(l30.target, null);
  assert.strictEqual(d50.present, false);
  assert.strictEqual(d50.targetName, "iqn.custom:d50");
  assert.strictEqual(d50.mutualChap, "Mutual CHAP: on (tgt)");
  assert.strictEqual(o70.present, false);
});

test("iscsiRows without the transport map or device.conf", () => {
  const rows = core.iscsiRows(config, iscsiCfg, null);
  assert.strictEqual(rows[0].present, null);
  const only = core.iscsiRows(null, iscsiCfg, transports);
  assert.deepStrictEqual(only.map((r) => r.key), ["iscsi-library-10", "iscsi-drive-50", "iscsi-library-70"]);
  assert.ok(only.every((r) => r.orphan));
  assert.deepStrictEqual(core.iscsiRows(config, null, null).map((r) => r.exported), [false, false, false]);
});

test("no derived row carries a secret", () => {
  const text = JSON.stringify(core.iscsiRows(config, iscsiCfg, transports));
  assert.ok(!text.includes(SECRET));
  assert.ok(!text.includes(MUTUAL));
});

test("iscsiNotes lists fabrics.conf problems and warnings", () => {
  assert.deepStrictEqual(core.iscsiNotes(iscsiCfg), [
    "fabrics.conf:4: unknown key Foo",
    "fabrics.conf: Portal 0.0.0.0:3260 listens on every address",
  ]);
  assert.deepStrictEqual(core.iscsiNotes(null), []);
  assert.deepStrictEqual(core.iscsiNotes({ warnings: [{ message: "m" }] }), ["fabrics.conf: m"]);
});

test("iscsiAllowed follows the write gate and hides actions without a config", () => {
  const ok = { writes: "ok" };
  const clean = { targets: [], problems: [] };
  assert.deepStrictEqual(core.iscsiAllowed({ caps: ok, reachable: true, cfg: clean }), { ok: true, hide: false, reason: "" });
  const old = core.iscsiAllowed({ caps: ok, reachable: true, cfg: null, cfgError: { status: 404, message: "not found" } });
  assert.strictEqual(old.ok, false);
  assert.strictEqual(old.hide, true);
  assert.ok(/does not offer the iSCSI export API/.test(old.reason));
  const failed = core.iscsiAllowed({ caps: ok, reachable: true, cfgError: { status: 500, message: "boom" } });
  assert.deepStrictEqual([failed.hide, failed.reason], [true, "fabrics.conf not read: boom"]);
  assert.strictEqual(core.iscsiAllowed({ caps: ok, reachable: true }).hide, true);
  const ro = core.iscsiAllowed({ caps: { writes: "helper_absent" }, reachable: true, cfg: clean });
  assert.deepStrictEqual([ro.ok, ro.hide], [false, false]);
  assert.ok(/^Read-only: the apply helper is not enabled/.test(ro.reason));
  assert.ok(/writes are weird/.test(core.iscsiAllowed({ caps: { writes: "weird" }, reachable: true, cfg: clean }).reason));
  assert.strictEqual(core.iscsiAllowed({ caps: null, reachable: true, cfg: clean }).ok, false);
  assert.strictEqual(core.iscsiAllowed({ caps: ok, reachable: false, cfg: clean }).reason, "The node is not reachable.");
  assert.ok(/fix them first/.test(core.iscsiAllowed({ caps: ok, reachable: true, cfg: iscsiCfg }).reason));
  assert.strictEqual(core.iscsiAllowed({ caps: ok, reachable: true, cfg: clean, busy: true }).ok, false);
});

test("iscsiActionText", () => {
  assert.strictEqual(core.iscsiActionText("reload"), "reload: sessions to other targets stay up");
  assert.ok(/^start/.test(core.iscsiActionText("start")));
  assert.strictEqual(core.iscsiActionText("none"), "none: applied when mhVTL starts");
  assert.strictEqual(core.iscsiActionText(""), "");
  assert.strictEqual(core.iscsiActionText("other"), "other");
});

test("iscsiDiffLines classifies the diff", () => {
  assert.deepStrictEqual(core.iscsiDiffLines("--- a/fabrics.conf\n+++ b/fabrics.conf\n@@ -1,2 +1,3 @@\n iSCSI:\n-  Portal: a\n+  Portal: b\n"), [
    { cls: "diff-hunk", text: "--- a/fabrics.conf" },
    { cls: "diff-hunk", text: "+++ b/fabrics.conf" },
    { cls: "diff-hunk", text: "@@ -1,2 +1,3 @@" },
    { cls: "diff-ctx", text: " iSCSI:" },
    { cls: "diff-del", text: "-  Portal: a" },
    { cls: "diff-add", text: "+  Portal: b" },
  ]);
  assert.deepStrictEqual(core.iscsiDiffLines(""), []);
  assert.deepStrictEqual(core.iscsiDiffLines(null), []);
});

test("iscsiFieldInput maps request fields to inputs", () => {
  assert.strictEqual(core.iscsiFieldInput("initiators"), "iscsi-initiators");
  assert.strictEqual(core.iscsiFieldInput("initiators[2]"), "iscsi-initiators");
  assert.strictEqual(core.iscsiFieldInput("chap"), "iscsi-chap-name");
  assert.strictEqual(core.iscsiFieldInput("chap.name"), "iscsi-chap-name");
  assert.strictEqual(core.iscsiFieldInput("chap.secret"), "iscsi-chap-secret");
  assert.strictEqual(core.iscsiFieldInput("mutual_chap.secret"), "iscsi-mutual-secret");
  assert.strictEqual(core.iscsiFieldInput("base_name"), "iscsi-base-name");
  assert.strictEqual(core.iscsiFieldInput("portals"), "iscsi-portals");
  assert.strictEqual(core.iscsiFieldInput("library"), "");
  assert.strictEqual(core.iscsiFieldInput(""), "");
  assert.strictEqual(core.iscsiFieldInput("__proto__"), "");
});

test("iscsiOutcome maps write responses", () => {
  const v = core.iscsiOutcome({ dry_run: true, action: "reload", diff: "+x\n", result: "validated" }, 200);
  assert.deepStrictEqual([v.kind, v.action, v.actionText, v.diff], ["validated", "reload", "reload: sessions to other targets stay up", "+x\n"]);
  const a = core.iscsiOutcome({ dry_run: false, action: "start", result: "applied" }, 200);
  assert.strictEqual(a.kind, "applied");
  assert.ok(/^Applied \(start/.test(a.text));
  const rb = core.iscsiOutcome({ dry_run: false, result: "rolled_back", message: "mhvtl-fabrics failed", journal_tail: "line1\nline2",
    rollback_dir: "/var/lib/mhvtl-control-apply/rollback/1" }, 500);
  assert.deepStrictEqual([rb.kind, rb.tail], ["rolled_back", "line1\nline2"]);
  assert.ok(rb.text.includes("mhvtl-fabrics failed") && rb.text.includes("/rollback/1"));
  const inv = core.iscsiOutcome({ result: "refused", error: { code: "invalid", message: "bad IQN", field: "initiators" } }, 422);
  assert.deepStrictEqual([inv.kind, inv.field, inv.text], ["invalid", "initiators", "bad IQN"]);
  const c = core.iscsiOutcome({ result: "conflict", error: { code: "conflict", message: "changed" } }, 409);
  assert.strictEqual(c.kind, "conflict");
  assert.ok(/changed since it was read/.test(c.text));
  const busy = core.iscsiOutcome({ result: "apply_in_progress", error: { code: "apply_in_progress", message: "another apply is running" } }, 409);
  assert.deepStrictEqual([busy.kind, /Another change is being applied/.test(busy.text)], ["conflict", true]);
  const rec = core.iscsiOutcome({ result: "recovered", error: { code: "recovered", message: "m" } }, 409);
  assert.ok(/recovered a change that had been interrupted/.test(rec.text) && /make the change again/.test(rec.text));
  const other = core.iscsiOutcome({ error: { code: "intent_unreadable", message: "intent.json unreadable" } }, 409);
  assert.ok(/refused: intent.json unreadable/.test(other.text));
  const same = core.iscsiOutcome({ dry_run: true, result: "unchanged", diff: "", sha256: "abc" }, 200);
  assert.deepStrictEqual([same.kind, same.text], ["unchanged", "No change to fabrics.conf."]);
  assert.strictEqual(core.iscsiOutcome({ dry_run: false, result: "unchanged" }, 200).kind, "unchanged");
  assert.strictEqual(core.iscsiOutcome({ error: { code: "not_bound", message: "x" } }, 404).kind, "not_bound");
  assert.strictEqual(core.iscsiOutcome({ error: { code: "token_required", message: "token" } }, 403).kind, "readonly");
  assert.strictEqual(core.iscsiOutcome({ error: { code: "unauthorized", message: "no" } }, 401).kind, "readonly");
  assert.strictEqual(core.iscsiOutcome({ error: { code: "helper_absent", message: "h" } }, 503).kind, "readonly");
  const ref = core.iscsiOutcome({ result: "refused", message: "helper refused" }, 400);
  assert.deepStrictEqual([ref.kind, ref.text], ["refused", "helper refused"]);
  const err = core.iscsiOutcome({ result: "error", error: { message: "boom" }, journal_tail: "j" }, 500);
  assert.deepStrictEqual([err.kind, err.text, err.tail], ["error", "boom", "j"]);
  assert.strictEqual(core.iscsiOutcome({ error: { message: "network" } }, 0).kind, "error");
  assert.ok(/No answer/.test(core.iscsiOutcome(null, 0).text));
  assert.strictEqual(core.iscsiOutcome(null, 500).kind, "error");
});

test("no outcome text carries a secret from the request", () => {
  const body = core.buildISCSIBindBody(10, { chap_name: "u", chap_secret: SECRET, mutual_name: "t", mutual_secret: MUTUAL }, null, true).body;
  assert.strictEqual(body.chap.secret, SECRET); // the request has it ...
  const answers = [
    [{ dry_run: true, action: "reload", diff: "+  CHAP secret: ********\n", result: "validated" }, 200],
    [{ dry_run: false, action: "reload", result: "applied" }, 200],
    [{ result: "rolled_back", message: "failed", journal_tail: "t" }, 500],
    [{ error: { code: "invalid", message: "CHAP secret too short", field: "chap.secret" } }, 422],
    [{ error: { code: "conflict" } }, 409],
    [{}, 0],
  ];
  answers.forEach(([res, status]) => {
    const text = JSON.stringify(core.iscsiOutcome(res, status));
    assert.ok(!text.includes(SECRET) && !text.includes(MUTUAL), text); // ... no outcome does
  });
});
