# mhVTL Control

mhVTL Control (`mhvtl-control`) is an independent web control plane for
[mhVTL](https://github.com/markh794/mhvtl): a REST API and a web dashboard for
its virtual tape libraries. It is not part of the mhVTL project. It shows each
library's drives, slots and import/export station live from the SCSI medium
changer, mounts and moves cartridges, creates and edits media, reports drive
statistics and device nodes, and formats raw disks as cartridges.

It is a single self-contained binary with the dashboard built in, and uses only
the Go standard library.

The dashboard refreshes itself while its tab is visible: libraries and system
every 10 seconds, media and disks every 30, the configuration every minute. A
cartridge an application loads or moves shows up without clicking Refresh.
It skips a refresh while a dialog is open or a change is still running, keeps
scroll position, focus and expanded lists across refreshes, and redraws a panel
only when its data changed. The Auto-refresh button next to Refresh pauses it
in that browser.

## Install

### From the OBS repositories

mhvtl-control is built on the openSUSE Build Service in
[`home:ciriarte:mhvtl`](https://build.opensuse.org/project/show/home:ciriarte:mhvtl),
the same repositories that carry mhVTL itself, for openSUSE Tumbleweed, Slowroll
and Leap 16.0, Rocky Linux and AlmaLinux 9 and 10, Debian 12 and 13 (Proxmox VE 8
and 9) and Ubuntu 24.04 and 26.04, x86_64 only. These are personal builds, not
packages the mhVTL project supports. With the repository added, `zypper update`,
`dnf upgrade` and `apt upgrade` keep mhVTL and mhvtl-control current.

Add the repository and install mhVTL as described in mhVTL's
[OBS install guide](https://github.com/ciroiriarte/mhvtl/blob/fe28eb2/docs/packaging/install-from-obs.md),
which covers the repository path for each release, the kernel module (KMP or DKMS)
and the Slowroll repository priority. Then, as root:

```sh
zypper install mhvtl-control    # openSUSE
dnf install mhvtl-control       # Rocky, AlmaLinux
apt-get install mhvtl-control   # Debian, Ubuntu, Proxmox VE
```

The package recommends mhVTL, so installing it on its own also pulls in mhVTL's
user-space package, but not the kernel module; follow the guide for that. If dnf
pulled mhVTL in this way, `dnf remove mhvtl-control` removes it again. Install
mhVTL explicitly first, or keep it with `dnf mark install mhvtl-utils mhvtl-dkms`.

### From the release files

Packages for Debian/Ubuntu (`.deb`) and RHEL/Rocky/Alma and openSUSE (`.rpm`),
and static binaries, all for amd64 and arm64, are attached to each
[release](https://github.com/ciroiriarte/mhvtl-control/releases). Use them for
arm64, air-gapped hosts, or a release older than what the repositories carry.

```sh
# Debian, Ubuntu
sudo apt install ./mhvtl-control_0.4.0-1_amd64.deb
# RHEL, Rocky, Alma
sudo dnf install ./mhvtl-control-0.4.0-1.el9.x86_64.rpm
# openSUSE
sudo zypper install ./mhvtl-control-0.4.0-1.x86_64.rpm
```

The package installs `/usr/bin/mhvtl-control`, `mhvtl-control.service` and
`/etc/mhvtl-control/mhvtl-control.env`, and the configuration helper
`/usr/libexec/mhvtl-control/mhvtl-control-apply` with
`mhvtl-control-apply.socket` and its units. Nothing is enabled on install. The
helper's units stay disabled until you turn configuration writes on (see
[Configuration editor](#configuration-editor)). To start mhvtl-control:

```sh
sudo systemctl enable --now mhvtl-control
```

The dashboard is then at <http://127.0.0.1:8080/>. mhvtl-control needs a configured
mhVTL (`/etc/mhvtl/device.conf`) to show anything, but may start before it.

To install the static binary instead, copy it to `/usr/bin/mhvtl-control`,
`dist/mhvtl-control.service` from the source to `/usr/lib/systemd/system/`,
`dist/mhvtl-control.env` to `/etc/mhvtl-control/` (directory mode 0750, file
mode 0600), and `dist/mhvtl-control.tmpfiles` to
`/usr/lib/tmpfiles.d/mhvtl-control.conf`; then create `/var/lib/mhvtl-control-apply`
mode 0700 and run `systemd-tmpfiles --create mhvtl-control.conf`. The unit
refuses to start without those two directories. The static archive holds only
the daemon, not the configuration helper, so the configuration editor stays
read-only with it; for writes, install a package or use `make install`.

`make install` does the same from a source tree, except the `systemd-tmpfiles`
run, and keeps an existing `mhvtl-control.env`. It also installs the
configuration helper and its units, disabled. It does not install
`dist/mhvtl-control.preset`, which the RPM installs, so a later
`systemctl preset-all` may enable them where the preset policy enables by default.

## Configure

Options live in `/etc/mhvtl-control/mhvtl-control.env`. Restart mhvtl-control after changing them.
Up to 0.2.0 they lived in `/etc/mhvtl/mhvtl-control.env`; while the new file is
absent, mhvtl-control still reads the old one and logs that it is deprecated.

```sh
MHVTL_CONTROL_OPTS="--listen 0.0.0.0:8080 --tls-cert /etc/mhvtl/mhvtl-control.crt --tls-key /etc/mhvtl/mhvtl-control.key"
MHVTL_CONTROL_API_TOKEN=change-me
```

mhvtl-control binds the loopback interface by default, and refuses a non-loopback
address without a token (`--insecure` overrides that). `mhvtl-control -h` and
`man mhvtl-control` list every option.

Browsers can't make changes from another site: POST, PATCH and DELETE requests
whose `Sec-Fetch-Site` or `Origin` header names another origin get 403, unless
that origin is the `--cors-origin`. Scripts and curl send neither header and are
unaffected. `--cors-origin '*'` lets every origin through only when a token is
set. On loopback without a token, requests must also address the daemon
as `localhost` or a loopback IP, or they get 421; this stops DNS rebinding. Set a
token to serve it under another name, for example behind a reverse proxy that
passes the original `Host`, or have the proxy send `Host: localhost`.

The service runs as root, as the mhVTL message queue, the `/dev/sg*` nodes and
the disks it formats are root's. It drops every capability except
`CAP_DAC_OVERRIDE` and runs with systemd's sandboxing; `dist/mhvtl-control.service`
explains each exception. The sandbox hides `/home`, so media homes there aren't
supported.

## Configuration editor

The dashboard's Configuration view and the `/api/v1/config` routes show
mhVTL's configuration (`device.conf`, `library_contents.<id>` and
`mhvtl.conf`) and check it against the device personalities and the live
state of the libraries. With writes turned on, they also change it: add, edit
and delete libraries, drives, storage slots and MAP slots, and set `CAPACITY`,
`VERBOSE` and `VTL_DEBUG` in `mhvtl.conf`. A change takes three steps:

1. Stage: edits collect in a stage that mhvtl-control keeps in memory. A stage
   expires 30 minutes after its last use, and is lost when mhvtl-control
   restarts.
2. Check: a dry run sends the stage to the helper, which validates it and
   answers with the plan, the `vtllibrary@` and `vtltape@` units it would
   restart. Nothing changes yet.
3. Apply: the helper applies the stage as checked. An edit after the check
   needs a new check.

mhvtl-control itself cannot write `/etc/mhvtl` or restart units. The helper,
`mhvtl-control-apply`, does that: `mhvtl-control-apply.socket` starts one
instance per request, as root, and the helper accepts only root processes of
`mhvtl-control.service`. It logs each change to the journal and to
`/var/lib/mhvtl-control-apply/audit.jsonl`.

### Turn writes on

Writes are off by default. To turn them on:

1. Set `MHVTL_CONTROL_API_TOKEN` in `/etc/mhvtl-control/mhvtl-control.env` and
   restart mhvtl-control. Writes need a token, on loopback too.
2. If mhvtl-control listens on a non-loopback address, give it `--tls-cert`
   and `--tls-key`. Writes are refused there without TLS, `--insecure` or not.
3. Enable the helper's socket and its boot recovery:

   ```sh
   sudo systemctl enable --now mhvtl-control-apply.socket
   sudo systemctl enable mhvtl-control-apply-recover.service
   ```

`GET /api/v1/config/capabilities` reports in `writes` whether writes are
accepted. The first condition that holds wins:

| `writes`         | Condition |
|------------------|-----------|
| `helper_absent`  | No helper listens on `/run/mhvtl-control-apply.sock`: the socket unit is not running. |
| `tls_required`   | mhvtl-control listens on a non-loopback address without `--tls-cert` and `--tls-key`. |
| `token_required` | No API token is set. |
| `helper_blocked` | The helper refuses every change until you act: `/etc/mhvtl/mhvtl-control.env`, the 0.2.0 options file, still exists (`helper` is `legacy_env_blocks`; merge its settings into `/etc/mhvtl-control/mhvtl-control.env` and remove it), or an interrupted apply left an intent it cannot read (`intent_unreadable`, below). |
| `ok`             | Writes are accepted, and the dashboard offers edits. |

`helper` is `in_progress` while an apply runs and `incomplete_apply` after one
died; neither stops writes. It is `unreachable` when the socket exists but the
helper gave no usable answer; `writes` can still say `ok`, but the write routes
then answer `502 helper_unreachable`.

### What an apply does to running libraries

The check lists the units an apply restarts (`plan.units`) and the libraries
that go offline (`plan.offline_libraries`). For each, the helper takes the
library offline, reads where every cartridge is, stops the library's units,
writes the files, and starts the units again. Each library stays offline until
every restarted unit is active, then comes back online. Hosts cannot use an
affected library during the apply, so let its backup jobs finish first.

A check that finds a cartridge in a drive the apply would stop answers
`drive_loaded`, with a force token valid for 120 seconds. Unload the drive and
check again, or apply with that `force_token`: the helper then puts each
cartridge back in the slot or MAP slot it came from and restarts that drive's
`vtltape@` too. A standalone drive is stopped and its cartridge unloaded. The token
is bound to the change and to the cartridge positions, and works once.

The helper also refuses an apply while mhvtl-control is moving cartridges or
writing media (`busy`), while a media-home move journal under
`/var/lib/mhvtl-move-home` is planned or unfinished, and when the apply's
worst-case time, including a rollback, would exceed 1700 seconds (`budget`). If a step fails after a unit was stopped, it rolls back: it puts
the old files back and restarts the units that were running.

### Interrupted applies

The helper keeps its state in `/var/lib/mhvtl-control-apply`: `intent.json`,
the record of an apply under way; `rollback/`, the files each apply replaced,
for the last 50 applies; and `audit.jsonl`, rotated at 16 MiB.
`mhvtl-control.service` cannot read it.

If an apply dies, for instance with the host, `intent.json` stays behind:

- At boot, `mhvtl-control-apply-recover.service` puts the old files back
  before mhVTL starts. The drop-ins
  `vtllibrary@.service.d/50-mhvtl-control-apply-recover.conf` and
  `vtltape@.service.d/50-mhvtl-control-apply-recover.conf` order mhVTL's
  instances after it. It acts only on an apply from an earlier boot, and only
  while no `vtllibrary@` or `vtltape@` instance is active.
- Otherwise, the next check or apply recovers first, does nothing else, and
  answers `recovered`; check again.
- If `intent.json` cannot be read, `helper` is `intent_unreadable` and the
  helper refuses every change. Boot recovery fails too, and mhVTL may already be
  running on the partly written files: stop `mhvtl.target`, restore the files
  from `rollback/` by hand, then remove `intent.json`.

### Turn writes off

First check that `helper` in `GET /api/v1/config/capabilities` is neither
`incomplete_apply` nor `intent_unreadable`: with both units off, nothing
recovers an interrupted apply.

```sh
sudo systemctl disable --now mhvtl-control-apply.socket
sudo systemctl disable mhvtl-control-apply-recover.service
```

Stopping the socket removes `/run/mhvtl-control-apply.sock`; `writes` then
reports `helper_absent` and the Configuration view is read-only again.
Removing the Debian package keeps `/var/lib/mhvtl-control-apply`; purging it
deletes it. Removing the RPM package keeps it and its contents; delete it by
hand if you don't need the audit log or the rollback copies.

### Routes

All under `/api/v1`, with the API token when one is set (writes always need
one):

- Read and check, never write: `GET /config`, `GET /config/capabilities`,
  `GET /config/personalities`, `GET /config/libraries`,
  `GET /config/libraries/{id}`, `GET /config/libraries/{id}/drives`,
  `GET /config/libraries/{id}/slots`, `POST /config/validate`.
- Stage an edit (`stage` in the body names the stage; empty starts one):
  `POST /config/libraries`, `PATCH` and `DELETE /config/libraries/{id}`,
  `POST /config/libraries/{id}/drives`, `PATCH` and
  `DELETE /config/libraries/{id}/drives/{drive}`,
  `POST /config/libraries/{id}/slots`, `PATCH` and
  `DELETE /config/libraries/{id}/slots/{n}`, `PATCH /config/system`.
- `POST /config/apply`: `{"stage": "...", "dry_run": true}` checks; without
  `dry_run` it applies, with `force_token` after a `drive_loaded` check.
- `GET /config/stages/{id}` shows a stage and its last apply;
  `DELETE /config/stages/{id}` discards it.

## Monitoring

mhvtl-control serves three endpoints for monitoring:

- `GET /metrics`: library, media, home filesystem and drive metrics in the
  [Prometheus text format](https://prometheus.io/docs/instrumenting/exposition_formats/),
  plus mhvtl-control's own request counters. A collection is reused for 10
  seconds, so several scrapers don't each query the changers and drives.
  A scrape never waits behind a cartridge move: a changer busy with one
  reports `mhvtl_library_busy 1` and its last known state, and while a
  collection is still running the previous one is served, its age in
  `mhvtl_control_metrics_age_seconds`.
- `GET /readyz`: `200` when `device.conf` parses, every library's changer
  answers (or is busy with a move), every attached raw-disk cartridge has
  a readable superblock, and no running library holds a file-backed cartridge
  whose media is missing or incomplete; `503` otherwise. It reads the same cached collection,
  so probes add no SCSI or disk I/O of their own. The JSON body lists each
  check and why it failed. The message queue is not checked.
- `GET /healthz`: `200` while mhvtl-control is running, without checking
  mhVTL.

`/metrics` and `/readyz` need the API token when one is set, because they name
libraries, media homes and barcodes; `/healthz` never does. A scrape job:

```yaml
scrape_configs:
  - job_name: mhvtl-control
    authorization:
      credentials_file: /etc/prometheus/mhvtl-control.token
    static_configs:
      - targets: ["vtl.example.com:8080"]
```

Media metrics are labelled by home directory, since libraries can share one;
`mhvtl_library_info` maps each library to its home.

`mhvtl_media_missing_cartridges{library,reason}` counts the cartridges a
running library holds whose media is missing or incomplete; a library holds
what its changer reports. The media list marks each such cartridge with
`missing` and `missing_why`. When the changer can't be read, the list goes by
the file the library loads at its next start (`library_contents.<id>`, or its
`.persist`), and the metric and `/readyz` skip that library. The reasons:

- `directory_absent`: nothing at `<home>/<barcode>`. A drive cannot load it,
  and if `library_contents.<id>` lists the barcode in a slot, `make_vtl_media`
  creates it **blank** when the library next starts, so the backup
  application finds an empty tape where its catalog expects data. Restore the
  directory before restarting the library.
- `file_absent`: the directory lacks `data.0`, or the `indx` or `meta` file
  of a partition it has. Without `data.0`, `indx.0` or `meta.0` a drive
  cannot load it; with a later partition's file gone it loads, but that
  partition can't be read.
- `disk_absent`: a raw-disk cartridge whose disk is not attached, normal for
  a removable bay.
- `superblock_unreadable`: a raw-disk cartridge whose disk has no readable
  MVTL superblock.

The first two fail `/readyz` (check `cartridges:<id>`). The raw-disk reasons
don't fail that check: a detached disk is normal, and an unreadable
superblock already fails the home's `media:<home>` check. Drive statistics come from
LOG SENSE and never read the TapeAlert page, which would clear the flags for
the backup application. Example alert rules are in
`dist/prometheus/mhvtl-control.rules.yml`.

## The kernel ch driver

Linux's `ch` driver (`/dev/sch*`) leaks references. Each time it attaches to a
changer, which happens whenever mhVTL starts a library with `ch` loaded, it
takes a reference to every drive the changer reports and never releases it.
A drive mhVTL later removes (on the kernel-module backend, `vtltape` removes
its device at exit) then stays allocated until a reboot:

- `mhvtl.ko` can't be unloaded (`rmmod`: in use);
- `target_core_pscsi` refuses a new device at that address.

Linux 7.3-rc7 fixes it
([27c0f718d3b3](https://git.kernel.org/torvalds/c/27c0f718d3b3)). Older
kernels may carry a backport, which mhvtl-control can't tell from the version.
mhVTL's changers work through `/dev/sg*`, as PBS and Bareos use them, so the
simplest fix is not to load `ch`:

```sh
echo "blacklist ch" | sudo tee /etc/modprobe.d/mhvtl-no-ch.conf
sudo rmmod ch   # or reboot
```

`GET /api/v1/system` reports it in `kernel_ch`: whether `ch` is loaded
(`ch_loaded`), the libraries whose changer it is bound to (`changers`),
whether the kernel release is 7.3-rc7 or later (`kernel_fixed`), and
`mhvtl.ko`'s reference count next to the number of mhVTL devices. A count
with no device left (`leaked`) is what leaked references look like; an
application that still holds a `/dev/sg*` of a removed drive open shows the
same until it closes it. The dashboard's system panel shows a warning for
these, and a configuration check or apply that starts, stops or restarts a
library while `ch` is loaded warns about it. The TCMU backend never removes
a device, so none of this applies to it.

## Upgrade and roll back

Upgrade by installing the newer package. The .deb restarts mhvtl-control if it is
enabled; the RPM restarts it if it is running. An edited `/etc/mhvtl-control/mhvtl-control.env` is kept. If the
packaged default changed too, dpkg asks which to keep, and RPM keeps yours and
writes the new default next to it as `mhvtl-control.env.rpmnew`.

Upgrading from 0.2.0 moves an edited `/etc/mhvtl/mhvtl-control.env` to
`/etc/mhvtl-control/mhvtl-control.env` and restarts a running mhvtl-control on
it. The new packaged default is kept beside it as `mhvtl-control.env.dpkg-new`
or `mhvtl-control.env.rpmnew`. An unedited old file is simply replaced by the
new default.

mhvtl-control keeps no state of its own: libraries, media and configuration belong to
mhVTL. Rolling back is therefore just installing the older package. 0.3.1 and
earlier have no configuration helper: before rolling back to them, turn writes
off (see [Configuration editor](#turn-writes-off)) and check that `helper` in
`GET /api/v1/config/capabilities` is neither `incomplete_apply` nor
`intent_unreadable`. 0.2.0
reads its options from `/etc/mhvtl/mhvtl-control.env`, so before rolling back
to it, copy `/etc/mhvtl-control/mhvtl-control.env` there (mode 0600):

```sh
# Debian, Ubuntu
sudo apt install ./mhvtl-control_<old-version>_amd64.deb
# RHEL, Rocky, Alma
sudo dnf downgrade ./mhvtl-control-<old-version>.el9.x86_64.rpm
# openSUSE
sudo zypper install --oldpackage ./mhvtl-control-<old-version>.x86_64.rpm
```

`mhvtl-control --version`, and `version` in `GET /api/v1/system`, report the running
version.

## Build

```sh
make build        # bin/mhvtl-control and bin/mhvtl-control-apply, version from git describe
make test         # Go tests and the dashboard's JavaScript tests (Node 18+)
make install DESTDIR=/tmp/stage
```

Packaging is in `debian/` and `rpm/mhvtl-control.spec`. Releases are built by
`.github/workflows/release.yml` when a `v*` tag is pushed.

## License

Apache License 2.0; see [LICENSE](LICENSE).
