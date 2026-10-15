# mhvtld is built with the distribution's Go, links only the standard library,
# and is stripped (the Makefile's -s -w): there is no debug information to
# package.
%global debug_package %{nil}

Name:           mhvtld
Version:        0.1.0
Release:        1%{?dist}
Summary:        Management daemon and web dashboard for the mhVTL virtual tape library
License:        Apache-2.0
URL:            https://github.com/ciroiriarte/mhvtld
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
mhvtld is a REST API and web dashboard for mhVTL. It shows libraries, drives,
slots and the import/export station live from the SCSI medium changer; loads
and unloads cartridges and moves them between slots and drives; creates and
edits media; reports drive statistics and device nodes; and formats raw disks
as cartridges.

It is a single binary with the dashboard built in. It listens on 127.0.0.1 by
default and is not enabled on install.

%prep
%autosetup

%build
# No network and no writes outside the build tree: mhvtld uses only the Go
# standard library.
export GOCACHE=%{_builddir}/.gocache GOTOOLCHAIN=local GOPROXY=off
# PIE, as distributions expect; it links no libraries, only needs ld.so.
make build GO=go VERSION=%{version} GOBUILDFLAGS="-trimpath -buildvcs=false -buildmode=pie"

%check
export GOCACHE=%{_builddir}/.gocache GOTOOLCHAIN=local GOPROXY=off
go test ./...

%install
make install DESTDIR=%{buildroot} BINDIR=%{_bindir} UNITDIR=%{_unitdir} \
    SYSCONFDIR=%{_sysconfdir} MANDIR=%{_mandir}

%if 0%{?suse_version}
%pre
%service_add_pre mhvtld.service

%post
%service_add_post mhvtld.service

%preun
%service_del_preun mhvtld.service

%postun
%service_del_postun mhvtld.service
%else
%post
%systemd_post mhvtld.service

%preun
%systemd_preun mhvtld.service

%postun
%systemd_postun_with_restart mhvtld.service
%endif

%files
%license LICENSE
%doc README.md
%{_bindir}/mhvtld
%{_unitdir}/mhvtld.service
%{_mandir}/man8/mhvtld.8*
# /etc/mhvtl is shared with mhVTL.
%dir %{_sysconfdir}/mhvtl
# The options file may hold the API token.
%config(noreplace) %attr(0600,root,root) %{_sysconfdir}/mhvtl/mhvtld.env

%changelog
* Thu Oct 15 2026 Ciro Iriarte <ciro.iriarte@gmail.com> - 0.1.0-1
- Initial release.
