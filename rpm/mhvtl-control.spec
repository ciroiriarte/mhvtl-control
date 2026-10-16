# mhvtl-control is built with the distribution's Go, links only the standard library,
# and is stripped (the Makefile's -s -w): there is no debug information to
# package.
%global debug_package %{nil}

Name:           mhvtl-control
Version:        0.2.0
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
make build GO=go VERSION=%{version} GOBUILDFLAGS="-trimpath -buildvcs=false -buildmode=pie"

%check
export GOCACHE=%{_builddir}/.gocache GOTOOLCHAIN=local GOPROXY=off
go test ./...

%install
make install DESTDIR=%{buildroot} BINDIR=%{_bindir} UNITDIR=%{_unitdir} \
    TMPFILESDIR=%{_tmpfilesdir} SYSCONFDIR=%{_sysconfdir} \
    LOCALSTATEDIR=%{_localstatedir} MANDIR=%{_mandir}

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
%service_add_pre mhvtl-control.service
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
%tmpfiles_create %{_tmpfilesdir}/mhvtl-control.conf
%if 0%{?suse_version}
%service_add_post mhvtl-control.service
%else
%systemd_post mhvtl-control.service
%endif

%preun
%if 0%{?suse_version}
%service_del_preun mhvtl-control.service
%else
%systemd_preun mhvtl-control.service
%endif

%postun
%if 0%{?suse_version}
%service_del_postun mhvtl-control.service
%else
%systemd_postun_with_restart mhvtl-control.service
%endif

%files
%license LICENSE
%doc README.md
%{_bindir}/mhvtl-control
%{_unitdir}/mhvtl-control.service
%{_tmpfilesdir}/mhvtl-control.conf
%{_mandir}/man8/mhvtl-control.8*
%dir %attr(0750,root,root) %{_sysconfdir}/mhvtl-control
# The options file may hold the API token.
%config(noreplace) %attr(0600,root,root) %{_sysconfdir}/mhvtl-control/mhvtl-control.env
# The config-apply helper's state; mhvtl-control.service masks it.
%dir %attr(0700,root,root) %{_sharedstatedir}/mhvtl-control-apply

%changelog
* Fri Oct 16 2026 Ciro Iriarte <ciro.iriarte@gmail.com> - 0.2.0-1
- Initial release.
