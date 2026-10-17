# mhvtl-control is built with the distribution's Go, links only the standard library,
# and is stripped (the Makefile's -s -w): there is no debug information to
# package.
%global debug_package %{nil}

Name:           mhvtl-control
Version:        0.3.1
Release:        1%{?dist}
Summary:        Management daemon and web dashboard for the mhVTL virtual tape library
License:        Apache-2.0
URL:            https://github.com/ciroiriarte/mhvtl-control
Source0:        %{url}/archive/v%{version}/%{name}-%{version}.tar.gz

BuildRequires:  make
BuildRequires:  systemd-rpm-macros
%if 0%{?suse_version}
BuildRequires:  go >= 1.22
%{?systemd_ordering}
%else
BuildRequires:  golang >= 1.22
%{?systemd_requires}
%endif
# mhvtl-utils provides mhvtl, and openSUSE's own package is called mhvtl.
Recommends:     mhvtl

%description
mhvtl-control is a REST API and web dashboard for mhVTL. It shows libraries, drives,
slots and the import/export station live from the SCSI medium changer; loads
and unloads cartridges and moves them between slots and drives; creates and
edits media; reports drive statistics and device nodes; and formats raw disks
as cartridges.

It is a single binary with the dashboard built in. It listens on 127.0.0.1 by
default and is not enabled on install.

%prep
%autosetup

%build
# No network and no writes outside the build tree: mhvtl-control uses only the Go
# standard library.
export GOCACHE=%{_builddir}/.gocache GOTOOLCHAIN=local GOPROXY=off
# PIE, as distributions expect; it links no libraries, only needs ld.so.
# LDFLAGS is passed explicitly: EL10's %%build exports the C linker's flags
# in LDFLAGS (-Wl,-z,relro ...), which the Makefile would hand to Go's linker.
make build GO=go VERSION=%{version} LDFLAGS="-s -w" GOBUILDFLAGS="-trimpath -buildvcs=false -buildmode=pie"

%check
export GOCACHE=%{_builddir}/.gocache GOTOOLCHAIN=local GOPROXY=off
go test ./...

%install
# LIBEXECDIR is /usr/libexec, which the helper's units name, rather than
# %%{_libexecdir}: that is /usr/lib on openSUSE Leap.
make install DESTDIR=%{buildroot} BINDIR=%{_bindir} UNITDIR=%{_unitdir} \
    TMPFILESDIR=%{_tmpfilesdir} SYSCONFDIR=%{_sysconfdir} \
    LOCALSTATEDIR=%{_localstatedir} MANDIR=%{_mandir} LIBEXECDIR=%{_prefix}/libexec
# Keep the config-apply helper disabled whatever the vendor presets say.
install -Dpm 0644 dist/mhvtl-control.preset %{buildroot}%{_presetdir}/50-mhvtl-control.preset

# The options file moved from /etc/mhvtl to /etc/mhvtl-control after 0.2.0.
# rpm decides each file's fate before %%pre runs, so a copy %%pre put at the
# new path would be overwritten by the new default. Instead %%pre stages the
# old file, only when the new path is absent (an upgrade from 0.2.0, never a
# reinstall or a later upgrade), and %%posttrans, after the old package's
# erase has removed the old file or kept an edited one as .rpmsave, puts the
# staged copy in place. The new default stays beside it as .rpmnew if they
# differ, and the .rpmsave is dropped when it is the staged copy, so no stray
# copy of the token is left in /etc/mhvtl. Where the systemd macros restart
# in %%postun rather than deferring it to systemd-update-helper, that restart
# ran on the default file, so a running daemon is restarted again once its
# options are in place.
%pre
if [ -f %{_sysconfdir}/mhvtl/mhvtl-control.env ] && [ ! -e %{_sysconfdir}/mhvtl-control/mhvtl-control.env ]; then
    mkdir -p -m 0750 %{_sysconfdir}/mhvtl-control
    cp -p %{_sysconfdir}/mhvtl/mhvtl-control.env %{_sysconfdir}/mhvtl-control/mhvtl-control.env.rpm-migrate
fi
%if 0%{?suse_version}
%service_add_pre mhvtl-control.service mhvtl-control-apply.socket mhvtl-control-apply-recover.service
%endif

%posttrans
f=%{_sysconfdir}/mhvtl-control/mhvtl-control.env
if [ -f $f.rpm-migrate ]; then
    if cmp -s $f.rpm-migrate $f; then
        rm -f $f.rpm-migrate
    else
        mv -f $f $f.rpmnew
        mv -f $f.rpm-migrate $f
        if [ -d /run/systemd/system ]; then
            systemctl try-restart mhvtl-control.service >/dev/null 2>&1 || :
        fi
    fi
    if cmp -s %{_sysconfdir}/mhvtl/mhvtl-control.env.rpmsave $f; then
        rm -f %{_sysconfdir}/mhvtl/mhvtl-control.env.rpmsave
    fi
fi

%post
# The unit masks /run/mhvtl-control-apply, so create it before any restart.
# None of the units is enabled on install (50-mhvtl-control.preset
# disables the helper's, the distributions' default presets the daemon):
# enabling mhvtl-control-apply.socket and
# mhvtl-control-apply-recover.service is what turns configuration writes on.
# The helper's template is started by its socket alone.
%tmpfiles_create %{_tmpfilesdir}/mhvtl-control.conf
%{_presetdir}/50-mhvtl-control.preset
%if 0%{?suse_version}
%service_add_post mhvtl-control.service mhvtl-control-apply.socket mhvtl-control-apply-recover.service
%else
%systemd_post mhvtl-control.service mhvtl-control-apply.socket mhvtl-control-apply-recover.service
%endif

%preun
%if 0%{?suse_version}
%service_del_preun mhvtl-control.service mhvtl-control-apply.socket mhvtl-control-apply-recover.service
%else
%systemd_preun mhvtl-control.service mhvtl-control-apply.socket mhvtl-control-apply-recover.service
%endif

%postun
%if 0%{?suse_version}
%service_del_postun mhvtl-control.service mhvtl-control-apply.socket mhvtl-control-apply-recover.service
%else
%systemd_postun_with_restart mhvtl-control.service mhvtl-control-apply.socket
%systemd_postun mhvtl-control-apply-recover.service
%endif

%files
%license LICENSE
%doc README.md
%{_bindir}/mhvtl-control
%dir %{_prefix}/libexec/mhvtl-control
%{_prefix}/libexec/mhvtl-control/mhvtl-control-apply
%{_unitdir}/mhvtl-control.service
%{_unitdir}/mhvtl-control-apply.socket
%{_unitdir}/mhvtl-control-apply@.service
%{_unitdir}/mhvtl-control-apply-recover.service
# Ordering drop-ins for mhVTL's instances (after boot recovery).
%dir %{_unitdir}/vtllibrary@.service.d
%{_unitdir}/vtllibrary@.service.d/50-mhvtl-control-apply-recover.conf
%dir %{_unitdir}/vtltape@.service.d
%{_unitdir}/vtltape@.service.d/50-mhvtl-control-apply-recover.conf
%{_tmpfilesdir}/mhvtl-control.conf
%{_presetdir}/50-mhvtl-control.preset
%{_mandir}/man8/mhvtl-control.8*
%dir %attr(0750,root,root) %{_sysconfdir}/mhvtl-control
# The options file may hold the API token.
%config(noreplace) %attr(0600,root,root) %{_sysconfdir}/mhvtl-control/mhvtl-control.env
# The config-apply helper's state; mhvtl-control.service masks it.
%dir %attr(0700,root,root) %{_sharedstatedir}/mhvtl-control-apply

%changelog
* Fri Oct 16 2026 Ciro Iriarte <ciro.iriarte@gmail.com> - 0.3.1-1
- Release workflow: install pkgconf and systemd-dev for the deb build. v0.3.0's release run failed there, so 0.3.0 has no GitHub release; 0.3.1 is otherwise the same as 0.3.0.

* Fri Oct 16 2026 Ciro Iriarte <ciro.iriarte@gmail.com> - 0.3.0-1
- Read raw-disk cartridges' superblock and MAM: capacity, usage and MAM in the media list (#33).
- Serve Prometheus metrics at /metrics and readiness at /readyz (#34).
- Debian: install the unit where the release's systemd looks for it, and build with golang-1.23-go from bookworm-backports, so the package builds on Debian 12 (#35).
- RPM: keep the C linker flags EL10 exports out of Go's linker.
- Add a lossless model of mhVTL's configuration files and validation of a proposed configuration: personality table, identity generation and checks (#38, #39, #40). Not yet exposed in the API.
- Move the options file to /etc/mhvtl-control/mhvtl-control.env; an upgrade from 0.2.0 moves an edited file (#41).
- Confine mhvtl-control.service away from systemd, D-Bus and the config-apply helper (#41).

* Fri Oct 16 2026 Ciro Iriarte <ciro.iriarte@gmail.com> - 0.2.0-1
- Initial release.
