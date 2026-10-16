# mhVTL Control

mhVTL Control (`mhvtl-control`) is an independent web control plane for
[mhVTL](https://github.com/markh794/mhvtl): a REST API and a web dashboard for
its virtual tape libraries. It is not part of the mhVTL project. It shows each
library's drives, slots and import/export station live from the SCSI medium
changer, mounts and moves cartridges, creates and edits media, reports drive
statistics and device nodes, and formats raw disks as cartridges.

It is a single self-contained binary with the dashboard built in, and uses only
the Go standard library.

## Install

Packages for Debian/Ubuntu (`.deb`) and RHEL/Rocky/Alma and openSUSE (`.rpm`),
and static binaries, all for amd64 and arm64, are attached to each
[release](https://github.com/ciroiriarte/mhvtl-control/releases).

```sh
# Debian, Ubuntu
sudo apt install ./mhvtl-control_0.2.0-1_amd64.deb
# RHEL, Rocky, Alma
sudo dnf install ./mhvtl-control-0.2.0-1.el9.x86_64.rpm
# openSUSE
sudo zypper install ./mhvtl-control-0.2.0-1.x86_64.rpm
```

The package installs `/usr/bin/mhvtl-control`, `mhvtl-control.service` and
`/etc/mhvtl/mhvtl-control.env`. The service is not enabled on install:

```sh
sudo systemctl enable --now mhvtl-control
```

The dashboard is then at <http://127.0.0.1:8080/>. mhvtl-control needs a configured
mhVTL (`/etc/mhvtl/device.conf`) to show anything, but may start before it.

To install the static binary instead, copy it to `/usr/bin/mhvtl-control`, and
`dist/mhvtl-control.service` and `dist/mhvtl-control.env` from the source to
`/usr/lib/systemd/system/` and `/etc/mhvtl/` (mode 0600). `make install` does
the same from a source tree, and keeps an existing `mhvtl-control.env`.

## Configure

Options live in `/etc/mhvtl/mhvtl-control.env`. Restart mhvtl-control after changing them.

```sh
MHVTL_CONTROL_OPTS="--listen 0.0.0.0:8080 --tls-cert /etc/mhvtl/mhvtl-control.crt --tls-key /etc/mhvtl/mhvtl-control.key"
MHVTL_CONTROL_API_TOKEN=change-me
```

mhvtl-control binds the loopback interface by default, and refuses a non-loopback
address without a token (`--insecure` overrides that). `mhvtl-control -h` and
`man mhvtl-control` list every option.

The service runs as root, as the mhVTL message queue, the `/dev/sg*` nodes and
the disks it formats are root's. It drops every capability except
`CAP_DAC_OVERRIDE` and runs with systemd's sandboxing; `dist/mhvtl-control.service`
explains each exception. The sandbox hides `/home`, so media homes there aren't
supported.

## Monitoring

mhvtl-control serves three endpoints for monitoring:

- `GET /metrics`: library, media, home filesystem and drive metrics in the
  [Prometheus text format](https://prometheus.io/docs/instrumenting/exposition_formats/),
  plus mhvtl-control's own request counters. A collection is reused for 10
  seconds, so several scrapers don't each query the changers and drives.
- `GET /readyz`: `200` when `device.conf` parses, every library's changer
  answers, and every attached raw-disk cartridge has a readable superblock;
  `503` otherwise. The JSON body lists each check and why it failed. The
  message queue is not checked.
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
`mhvtl_library_info` maps each library to its home. Drive statistics come from
LOG SENSE and never read the TapeAlert page, which would clear the flags for
the backup application. Example alert rules are in
`dist/prometheus/mhvtl-control.rules.yml`.

## Upgrade and roll back

Upgrade by installing the newer package. The .deb restarts mhvtl-control if it is
enabled; the RPM restarts it if it is running. An edited `/etc/mhvtl/mhvtl-control.env` is kept. If the
packaged default changed too, dpkg asks which to keep, and RPM keeps yours and
writes the new default next to it as `mhvtl-control.env.rpmnew`.

mhvtl-control keeps no state of its own: libraries, media and configuration belong to
mhVTL. Rolling back is therefore just installing the older package:

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
make build        # bin/mhvtl-control, version from git describe
make test         # Go tests and the dashboard's JavaScript tests (Node 18+)
make install DESTDIR=/tmp/stage
```

Packaging is in `debian/` and `rpm/mhvtl-control.spec`. Releases are built by
`.github/workflows/release.yml` when a `v*` tag is pushed.

## License

Apache License 2.0; see [LICENSE](LICENSE).
