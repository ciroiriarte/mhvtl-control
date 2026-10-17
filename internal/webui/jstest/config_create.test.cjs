// Tests of the create wizard and system settings helpers in core.js (plan
// 4.2): personality resolution, the element limits, the defaults, the
// request bodies and the mhvtl.conf form.
//
// Run: node --test internal/webui/jstest
const test = require("node:test");
const assert = require("node:assert");
const core = require("../assets/static/js/core.js");

// pers is a GET /config/personalities answer in miniature: rules grouped
// by personality, each with its place in vtllibrary's evaluation.
const pers = {
  libraries: [
    { init_fn: "init_stkl20", name: "STK L20/L40/L80", vpd83: false, limits: { pickers: 9, maps: 490, drives: 500, slots: 64536 },
      rules: [{ vendor: "stk", product_prefix: "L20", order: 1 }, { vendor: "stk", product_prefix: "L80", order: 3 }] },
    { init_fn: "init_stklxx", name: "STK L-series (L180/L700)", vpd83: false, limits: { pickers: 9, maps: 490, drives: 500, slots: 64536 },
      rules: [{ vendor: "stk", product_prefix: "", order: 4, source: "usr/cmd/vtllibrary.c:1866" }] },
    { init_fn: "init_stkslxx", name: "STK SL500", vpd83: false, limits: { pickers: 9, maps: 490, drives: 500, slots: 64536 },
      rules: [{ vendor: "stk", product_prefix: "SL500", order: 0 }] },
    { init_fn: "init_ibm3584", name: "IBM 03584", vpd83: true, limits: { pickers: 256, maps: 255, drives: 512, slots: 64512 },
      rules: [{ vendor: "IBM", product_prefix: "03584", order: 6 }] },
    { init_fn: "init_default_smc", name: "mhVTL default library", vpd83: true, limits: { pickers: 16, maps: 256, drives: 751, slots: 64512 },
      rules: [{ vendor: "IBM", product_prefix: "", order: 7 }, { vendor: "", product_prefix: "", order: 20 }] },
    { init_fn: "init_scalar_smc", name: "Quantum Scalar", vpd83: true, limits: { pickers: 1, maps: 3, drives: 2, slots: 5 },
      rules: [{ vendor: "", product_prefix: "Scalar", order: 15 }] },
  ],
  drives: [{ product: "ULT3580-TD8" }, { product: "Ultrium 6-SCSI" }, { product: "T10000C" }, { product: "SDLT600" }, { product: "SDX-700C" }],
  target_apps: [{ id: "pbs", name: "Proxmox Backup Server" }],
};

test("resolvePersonality matches as vtllibrary does: first rule in order, prefixes case-insensitive", () => {
  const name = (v, p) => (core.resolvePersonality(pers, v, p) || {}).init_fn;
  assert.strictEqual(name("STK", "L700"), "init_stklxx");
  assert.strictEqual(name("STK", "SL500"), "init_stkslxx"); // order 0 beats the stk catch-all
  assert.strictEqual(name("StK", "l80"), "init_stkl20");
  assert.strictEqual(name("IBM", "03584L32"), "init_ibm3584");
  assert.strictEqual(name("IBM", "3576"), "init_default_smc");
  assert.strictEqual(name("ACME", "Scalar i6"), "init_scalar_smc");
  assert.strictEqual(name("ACME", "box"), "init_default_smc");
  assert.strictEqual(name("ST", "L700"), "init_default_smc"); // a vendor shorter than the prefix never matches
  assert.strictEqual(core.resolvePersonality({}, "STK", "L700"), null);
});

test("modelPresets: one per rule naming both vendor and product, in rule order", () => {
  assert.deepStrictEqual(core.modelPresets(pers), [
    { vendor: "STK", product: "SL500", name: "STK SL500" },
    { vendor: "STK", product: "L20", name: "STK L20/L40/L80" },
    { vendor: "STK", product: "L80", name: "STK L20/L40/L80" },
    { vendor: "IBM", product: "03584", name: "IBM 03584" },
  ]);
});

test("createLimits: the personality's windows, within what one create request adds", () => {
  const p = pers.libraries.find((l) => l.init_fn === "init_ibm3584");
  assert.deepStrictEqual(core.createLimits(p), { slots: 4096, maps: 255, drives: 512 });
  assert.deepStrictEqual(core.createLimits(pers.libraries.find((l) => l.init_fn === "init_scalar_smc")), { slots: 5, maps: 3, drives: 2 });
  assert.deepStrictEqual(core.createLimits(null), { slots: 0, maps: 0, drives: 0 });
  // Drive IDs follow the library's: no more drives than IDs up to 1023.
  assert.deepStrictEqual(core.createLimits(p, 1020), { slots: 4096, maps: 255, drives: 3 });
});

test("nextLibraryID: the first ten that is free with the IDs of its drives", () => {
  assert.strictEqual(core.nextLibraryID([10, 11, 12, 30, 31], 2), 20);
  assert.strictEqual(core.nextLibraryID([10, 11, 12, 20, 22], 2), 30); // 20 taken, 30..32 free
  assert.strictEqual(core.nextLibraryID([10, 20, 30, 41], 2), 50); // 40 is free but 41 is a drive
  assert.strictEqual(core.nextLibraryID([], 0), 10);
  assert.strictEqual(core.nextLibraryID([], 12), 10); // 10..22 overlap the next ten, still free
  const full = [];
  for (let i = 10; i <= 1020; i += 10) { full.push(i); }
  assert.strictEqual(core.nextLibraryID(full, 0), 1); // no ten left: the first free run
  assert.strictEqual(core.nextLibraryID(Array.from({ length: 1023 }, (_, i) => i + 1), 0), null);
});

test("freeTargets: targets on a channel whose LUN 0 is free, lowest first", () => {
  const used = [{ channel: 0, target: 0, lun: 0 }, { channel: 0, target: 1, lun: 0 }, { channel: 0, target: 3, lun: 0 }, { channel: 1, target: 2, lun: 0 }, { channel: 0, target: 4, lun: 1 }];
  assert.deepStrictEqual(core.freeTargets(used, 3, 0), [2, 4, 5]);
  assert.deepStrictEqual(core.freeTargets([], 2, 0), [0, 1]);
  assert.deepStrictEqual(core.freeTargets(used, 0, 0), []);
});

test("usedAddresses and usedIDs read device.conf as GET /config gives it", () => {
  const config = {
    libraries: [{ id: 10, channel: 0, target: 0, lun: 0, serial: "XYZZY_A", drives: [{ id: 11, channel: 0, target: 1, lun: 0, serial: "XYZZY_A1" }] }],
    standalone_drives: [{ id: 90, channel: 0, target: 9, lun: 0, serial: "SA" }],
  };
  assert.deepStrictEqual(core.usedIDs(config), [10, 11, 90]);
  assert.deepStrictEqual(core.usedAddresses(config), [{ channel: 0, target: 0, lun: 0 }, { channel: 0, target: 1, lun: 0 }, { channel: 0, target: 9, lun: 0 }]);
  assert.deepStrictEqual(core.usedSerials(config), ["XYZZY_A", "XYZZY_A1", "SA"]);
  assert.deepStrictEqual(core.usedIDs(null), []);
  assert.deepStrictEqual(core.usedNAAs({ libraries: [{ id: 10, naa: "10:22:33:44:ab:00:00:00", drives: [{ id: 11, naa: "" }] }] }), ["10:22:33:44:ab:00:00:00"]);
});

// The stage's device.conf, a second wizard run in the same stage avoids
// what the first staged (4b pre-review M1).
const stagedConf = "VERSION: 5\n\nLibrary: 20 CHANNEL: 00 TARGET: 02 LUN: 00\n Vendor identification: IBM\n Unit serial number: MHV0020\n" +
  " NAA: 20:22:33:44:ab:00:02:00\n\nDrive: 21 CHANNEL: 00 TARGET: 03 LUN: 00\n Library ID: 20 Slot: 01\n Unit serial number: MHV002001\n" +
  " NAA: 20:22:33:44:ab:00:03:00\n# Drive: 99 CHANNEL: 00 TARGET: 15 LUN: 00\n";

test("stagedDevices reads the stage's device.conf headers, serials and NAAs", () => {
  assert.deepStrictEqual(core.stagedDevices({ files: { "device.conf": stagedConf } }), [
    { id: 20, channel: 0, target: 2, lun: 0, serial: "MHV0020", naa: "20:22:33:44:ab:00:02:00" },
    { id: 21, channel: 0, target: 3, lun: 0, serial: "MHV002001", naa: "20:22:33:44:ab:00:03:00" },
  ]);
  assert.deepStrictEqual(core.stagedDevices({ files: {} }), []);
  assert.deepStrictEqual(core.stagedDevices(null), []);
  const config = { libraries: [{ id: 10, channel: 0, target: 0, lun: 0, serial: "XYZZY_A", naa: "10:22:33:44:ab:00:00:00", drives: [] }] };
  const stage = { files: { "device.conf": stagedConf } };
  assert.deepStrictEqual(core.usedIDs(config, stage), [10, 20, 21]);
  assert.deepStrictEqual(core.usedSerials(config, stage), ["XYZZY_A", "MHV0020", "MHV002001"]);
  assert.deepStrictEqual(core.usedAddresses(config, stage).map((a) => a.target), [0, 2, 3]);
  assert.deepStrictEqual(core.usedNAAs(config, stage), ["10:22:33:44:ab:00:00:00", "20:22:33:44:ab:00:02:00", "20:22:33:44:ab:00:03:00"]);
  const ibm = pers.libraries.find((l) => l.init_fn === "init_ibm3584");
  assert.strictEqual(core.wizardDefaults(config, ibm, stage).id, 30);
});

test("wizardDefaults clamp to the personality and avoid what is taken", () => {
  const config = { libraries: [{ id: 10, channel: 0, target: 0, lun: 0, serial: "XYZZY_A", drives: [{ id: 11, channel: 0, target: 1, lun: 0 }] }] };
  const ibm = pers.libraries.find((l) => l.init_fn === "init_ibm3584");
  assert.deepStrictEqual(core.wizardDefaults(config, ibm), { id: 20, slots: 20, maps: 4, drives: 2 });
  const scalar = pers.libraries.find((l) => l.init_fn === "init_scalar_smc");
  assert.deepStrictEqual(core.wizardDefaults(config, scalar), { id: 20, slots: 5, maps: 3, drives: 2 });
  assert.deepStrictEqual(core.wizardDefaults(null, null), { id: 10, slots: 0, maps: 0, drives: 0 });
});

test("serials and NAA: within the 10-character policy, unique by ID and address", () => {
  assert.strictEqual(core.librarySerial(20), "MHV0020");
  assert.strictEqual(core.driveSerial(20, 1, 2), "MHV002001");
  assert.strictEqual(core.driveSerial(1020, 3, 120), "MHV1020003");
  assert.ok(core.driveSerial(1023, 512, 512).length <= 10);
  // Decimal, as generate_device_conf's printf %02d writes it.
  assert.strictEqual(core.naaFor(20, 0, 10, 0), "20:22:33:44:ab:00:10:00");
  assert.strictEqual(core.naaFor(5, 1, 2, 3), "05:22:33:44:ab:01:02:03");
  assert.strictEqual(core.naaFor(300, 0, 12, 0), "300:22:33:44:ab:00:12:00");
});

test("driveVendorFor: the vendor a drive product is sold under", () => {
  assert.strictEqual(core.driveVendorFor("ULT3580-TD8"), "IBM");
  assert.strictEqual(core.driveVendorFor("ULTRIUM-HH7"), "IBM");
  assert.strictEqual(core.driveVendorFor("03592E07"), "IBM");
  assert.strictEqual(core.driveVendorFor("Ultrium 6-SCSI"), "HP");
  assert.strictEqual(core.driveVendorFor("T10000C"), "STK");
  assert.strictEqual(core.driveVendorFor("T9840D"), "STK");
  assert.strictEqual(core.driveVendorFor("SDLT600"), "QUANTUM");
  assert.strictEqual(core.driveVendorFor("DLT7000"), "QUANTUM");
  assert.strictEqual(core.driveVendorFor("SDX-700C"), "SONY");
  assert.strictEqual(core.driveVendorFor("other"), "");
});

const wiz = {
  id: 20, vendor: "IBM", product: "03584L32", serial: "MHV0020", home: "",
  slots: 20, maps: 4, drives: 2, driveProduct: "ULT3580-TD8", driveVendor: "IBM",
  targets: [2, 3, 4],
};

test("wizardErrors: counts within the limits, IDs, serials and home", () => {
  const ibm = pers.libraries.find((l) => l.init_fn === "init_ibm3584");
  const lim = core.createLimits(ibm, 20);
  assert.deepStrictEqual(core.wizardErrors(wiz, lim, { ids: [10, 11], serials: ["XYZZY_A"] }), []);
  const errs = core.wizardErrors(Object.assign({}, wiz, { slots: 0, maps: 256, drives: 513, serial: "TOO-LONG-SERIAL", home: "relative/dir", vendor: "" }), lim, { ids: [], serials: [] });
  assert.deepStrictEqual(errs, [
    "Vendor identification is required.",
    "Storage slots must be 1 to 4096.",
    "MAP slots must be 0 to 255.",
    "Drives must be 0 to 512.",
    "The library serial is longer than 10 characters.",
    "The home directory must be an absolute path.",
  ]);
  assert.deepStrictEqual(core.wizardErrors(Object.assign({}, wiz, { id: 0 }), lim, { ids: [], serials: [] }), ["The library ID must be 1 to 1023."]);
  assert.deepStrictEqual(core.wizardErrors(wiz, lim, { ids: [21], serials: [] }), ["ID 21, for drive 1, is taken."]);
  assert.deepStrictEqual(core.wizardErrors(wiz, lim, { ids: [], serials: ["MHV002002"] }), ["Serial MHV002002, for drive 2, is taken."]);
  assert.deepStrictEqual(core.wizardErrors(Object.assign({}, wiz, { driveProduct: "" }), lim, { ids: [], serials: [] }), ["Choose a drive model."]);
  assert.deepStrictEqual(core.wizardErrors(Object.assign({}, wiz, { targets: [2] }), lim, { ids: [], serials: [] }), ["Not enough free SCSI targets on channel 0."]);
  // NAAs: the library's and each drive's (4b pre-review L1).
  assert.deepStrictEqual(core.wizardErrors(wiz, lim, { ids: [], serials: [], naas: ["20:22:33:44:ab:00:03:00"] }), ["NAA 20:22:33:44:ab:00:03:00, for drive 1, is taken."]);
  assert.deepStrictEqual(core.wizardErrors(wiz, lim, { ids: [], serials: [], naas: ["20:22:33:44:ab:00:02:00"] }), ["NAA 20:22:33:44:ab:00:02:00, for the library, is taken."]);
  // Drives need a vendor (4b pre-review L2).
  assert.deepStrictEqual(core.wizardErrors(Object.assign({}, wiz, { driveVendor: " ", driveProduct: "other" }), lim, { ids: [], serials: [] }), ["Drive vendor identification is required."]);
  assert.deepStrictEqual(core.wizardErrors(Object.assign({}, wiz, { driveVendor: "", drives: 0 }), lim, { ids: [], serials: [] }), []);
});

test("wizardRequests: the library, then each drive, as the write routes take them", () => {
  const reqs = core.wizardRequests(wiz);
  assert.strictEqual(reqs.length, 3);
  assert.deepStrictEqual(reqs[0], {
    method: "POST", path: "/api/v1/config/libraries",
    body: { id: 20, channel: 0, target: 2, lun: 0, slots: 20, maps: 4, attrs: [
      { key: "Vendor identification", value: "IBM" },
      { key: "Product identification", value: "03584L32" },
      { key: "Unit serial number", value: "MHV0020" },
      { key: "NAA", value: "20:22:33:44:ab:00:02:00" },
    ] },
  });
  assert.deepStrictEqual(reqs[2], {
    method: "POST", path: "/api/v1/config/libraries/20/drives",
    body: { id: 22, channel: 0, target: 4, lun: 0, slot: 2, attrs: [
      { key: "Vendor identification", value: "IBM" },
      { key: "Product identification", value: "ULT3580-TD8" },
      { key: "Unit serial number", value: "MHV002002" },
      { key: "NAA", value: "20:22:33:44:ab:00:04:00" },
    ] },
  });
  const homed = core.wizardRequests(Object.assign({}, wiz, { home: "/srv/vtl20", drives: 0 }));
  assert.strictEqual(homed.length, 1);
  assert.deepStrictEqual(homed[0].body.attrs[4], { key: "Home directory", value: "/srv/vtl20" });
});

test("systemFields and buildSystemPatch: only CAPACITY, VERBOSE and VTL_DEBUG, changed values only", () => {
  assert.deepStrictEqual(core.systemFields({ values: { CAPACITY: "600", VERBOSE: "2" } }), [
    { key: "CAPACITY", value: "600", set: true, shipped: "500" },
    { key: "VERBOSE", value: "2", set: true, shipped: "1" },
    { key: "VTL_DEBUG", value: "", set: false, shipped: "0" },
  ]);
  assert.deepStrictEqual(core.systemFields(undefined).map((f) => f.value), ["", "", ""]);
  const fields = core.systemFields({ values: { CAPACITY: "500", VERBOSE: "1", VTL_DEBUG: "0" } });
  assert.deepStrictEqual(core.buildSystemPatch(fields, { CAPACITY: " 1024 ", VERBOSE: "1", VTL_DEBUG: "1", MHVTL_HOME_PATH: "/x" }), { CAPACITY: "1024", VTL_DEBUG: "1" });
  assert.strictEqual(core.buildSystemPatch(fields, { CAPACITY: "500" }), null);
  // A key the file lacks stays out until given a value.
  const none = core.systemFields({ values: {} });
  assert.strictEqual(core.buildSystemPatch(none, { CAPACITY: "", VERBOSE: " ", VTL_DEBUG: "" }), null);
  assert.deepStrictEqual(core.buildSystemPatch(none, { CAPACITY: "", VERBOSE: "1", VTL_DEBUG: "" }), { VERBOSE: "1" });
  // The route sets values; it removes none.
  assert.throws(() => core.buildSystemPatch(fields, { VERBOSE: "" }), RangeError);
  [["CAPACITY", "0"], ["CAPACITY", "0600"], ["CAPACITY", "4294967297"], ["CAPACITY", "1e3"], ["VERBOSE", "4"], ["VTL_DEBUG", "2"]].forEach(([k, v]) => {
    const form = {};
    form[k] = v;
    assert.throws(() => core.buildSystemPatch(fields, form), RangeError, k + "=" + v);
  });
  assert.deepStrictEqual(core.buildSystemPatch(fields, { CAPACITY: "4294967296" }), { CAPACITY: "4294967296" });
});

test("presetWarning is the preset finding for a target app the personality does not suit", () => {
  assert.strictEqual(core.presetWarning(pers, "pbs", "STK", "L700", 40),
    "library 40 is set up for Proxmox Backup Server, but it finds changers by udev's ID_SCSI_SERIAL, which comes from VPD page 83h, " +
    "and the STK L-series (L180/L700) personality (init_stklxx, selected at usr/cmd/vtllibrary.c:1866) has no such page; " +
    "use a model that has it, such as IBM 03584 or Quantum Scalar");
  assert.strictEqual(core.presetWarning(pers, "pbs", "IBM", "03584L32", 40), "");
  assert.strictEqual(core.presetWarning(pers, "", "STK", "L700", 40), "");
  assert.strictEqual(core.presetWarning(pers, "pbs", "ST", "L700", 40), ""); // the default library has page 83h
  assert.strictEqual(core.presetWarning(pers, "other", "STK", "L700", 40), "");
  assert.strictEqual(core.presetWarning({}, "pbs", "STK", "L700", 40), "");
});

test("wizardDefaultHome is the home the helper gives a blank Home directory", () => {
  const config = { default_home: "/srv/served", default_home_effective: "/var/lib/mhvtl" };
  assert.strictEqual(core.wizardDefaultHome(config, null), "/var/lib/mhvtl");
  assert.strictEqual(core.wizardDefaultHome(config, { default_home_effective: "/srv/staged" }), "/srv/staged");
  assert.strictEqual(core.wizardDefaultHome(config, { files: {} }), "/var/lib/mhvtl");
  assert.strictEqual(core.wizardDefaultHome({ default_home: "/srv/served" }, null), "/srv/served"); // an older daemon
  assert.strictEqual(core.wizardDefaultHome(null, null), "");
  // mhvtl.conf exists but cannot be read: the home is unknown, not --home-dir.
  assert.strictEqual(core.wizardDefaultHome({ default_home: "/srv/served", default_home_effective: "", system: { file_error: "EACCES" } }, null), "");
});

test("wizardHomeHint names the default home, or why it is unknown", () => {
  const tail = " Another must be a new, empty directory.";
  assert.strictEqual(core.wizardHomeHint({ default_home_effective: "/var/lib/mhvtl" }, null), "Blank: the default home, /var/lib/mhvtl." + tail);
  // mhvtl.conf unreadable and nothing staged: unknown, with the read error.
  const unreadable = { default_home: "/srv/served", default_home_effective: "", system: { file_error: "open /etc/mhvtl/mhvtl.conf: permission denied" } };
  assert.strictEqual(core.wizardHomeHint(unreadable, null), "Blank: the default home, unknown (mhvtl.conf: open /etc/mhvtl/mhvtl.conf: permission denied)." + tail);
  // A staged mhvtl.conf decides it, whatever the file on disk.
  assert.strictEqual(core.wizardHomeHint(unreadable, { default_home_effective: "/srv/staged" }), "Blank: the default home, /srv/staged." + tail);
  assert.strictEqual(core.wizardHomeHint(null, null), "Blank: the default home, unknown." + tail);
});
