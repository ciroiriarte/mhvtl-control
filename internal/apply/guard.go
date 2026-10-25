package apply

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"mhvtl-control/internal/conffile"
	"mhvtl-control/internal/config"
	"mhvtl-control/internal/validate"
)

// mhvtl.conf keys a change may set (plan 3.2, guard.go; AC 30), each with
// the pattern a new value must match. MHVTL_HOME_PATH also goes through
// the home rules (CheckHomes, validate.HomeProblem).
var editableKeys = []struct {
	key   string
	valid func(string) bool
}{
	{"CAPACITY", func(v string) bool {
		n, err := strconv.ParseUint(v, 10, 64)
		return capacityRE.MatchString(v) && err == nil && n <= maxCapacityMB
	}},
	{"VERBOSE", regexp.MustCompile(`^[0-3]$`).MatchString},
	{"VTL_DEBUG", regexp.MustCompile(`^[01]$`).MatchString},
	// mhvtl and tcmu are the production transports (usr/transport.c); userland
	// is a build/test-only backend and is not offered here.
	{"MHVTL_BACKEND", regexp.MustCompile(`^(mhvtl|tcmu)$`).MatchString},
	{homeKey, func(v string) bool { return homeProblem(v) == "" }},
}

const homeKey = "MHVTL_HOME_PATH"

// capacityRE is a positive decimal without leading zeros: one spelling
// per value. mktape reads it as decimal (sscanf PRId64, usr/cmd/mktape.c:164
// at 852b78a,
// https://github.com/markh794/mhvtl/blob/852b78a7ae3add53ffdc418add5f14999762d788/usr/cmd/mktape.c#L164),
// but shell arithmetic would read 0600 as octal.
var capacityRE = regexp.MustCompile(`^[1-9][0-9]*$`)

// maxCapacityMB bounds CAPACITY, in MiB, as maxSizeMB in internal/api
// bounds a media request: mktape multiplies it by 2^20 in 64 bits.
const maxCapacityMB = 1 << 32

// checkLegacyEnv refuses while the daemon's options file is still at its
// 0.2.0 path: it holds the API token and sits in the directory the helper
// writes (spec, "Token file"). The helper's unit makes it inaccessible
// (InaccessiblePaths=), but Lstat still sees it.
func (h *Helper) checkLegacyEnv() error {
	if _, err := os.Lstat(h.cfg.LegacyEnv); err == nil {
		return refuse(ErrLegacyEnv, "%s exists; move it to /etc/mhvtl-control/mhvtl-control.env first", h.cfg.LegacyEnv)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// checkConfigPath refuses when mhVTL would read its configuration from a
// directory other than --config-dir. vtllibrary and vtltape take it from
// MHVTL_CONFIG_PATH in the environment, else from the first
// MHVTL_CONFIG_PATH line of mhvtl.conf (markh794/mhvtl usr/vtllib.c:279-308
// at 852b78a,
// https://github.com/markh794/mhvtl/blob/852b78a7ae3add53ffdc418add5f14999762d788/usr/vtllib.c#L279-L308),
// and their units load mhvtl.conf as an EnvironmentFile too
// (etc/vtllibrary@.service.in:16,
// https://github.com/markh794/mhvtl/blob/852b78a7ae3add53ffdc418add5f14999762d788/etc/vtllibrary@.service.in#L16,
// and etc/vtltape@.service.in:13,
// https://github.com/markh794/mhvtl/blob/852b78a7ae3add53ffdc418add5f14999762d788/etc/vtltape@.service.in#L13). The helper's
// own environment stands for the one systemd gives it; mhvtl.conf is checked
// as it is and as the request leaves it. A mhvtl.conf with a Problem leaves
// the path mhVTL reads unclear (conffile.MhvtlConf), so it is refused too.
func (h *Helper) checkConfigPath(cur, next *conffile.MhvtlConf) error {
	dir := filepath.Clean(h.cfg.ConfigDir)
	if v := h.cfg.Getenv("MHVTL_CONFIG_PATH"); v != "" && filepath.Clean(v) != dir {
		return refuse(ErrConfigPath, "MHVTL_CONFIG_PATH=%s in the environment, not %s", v, dir)
	}
	for _, c := range []struct {
		which string
		conf  *conffile.MhvtlConf
	}{{"mhvtl.conf", cur}, {"the new mhvtl.conf", next}} {
		if err := c.conf.Writable(); err != nil {
			return refuse(ErrNotWritable, "%s: %v", c.which, err)
		}
		if v, ok := c.conf.Get("MHVTL_CONFIG_PATH"); ok && filepath.Clean(v) != dir {
			return refuse(ErrConfigPath, "%s sets MHVTL_CONFIG_PATH=%s, not %s", c.which, v, dir)
		}
	}
	return nil
}

// DeriveMhvtlConf refuses a new mhvtl.conf unless it is the current one
// with only the values of the editable keys changed: what BuildMhvtlConf
// makes of the current one and the new values.
// make_vtl_media sources mhvtl.conf as bash, as root, from
// mhvtl-media.service (markh794/mhvtl usr/cmd/make_vtl_media.in:307 at
// 852b78a,
// https://github.com/markh794/mhvtl/blob/852b78a7ae3add53ffdc418add5f14999762d788/usr/cmd/make_vtl_media.in#L307),
// so any other line or value would be code run as root: the request's
// bytes are never trusted, only compared with the helper's derivation.
func DeriveMhvtlConf(cur, next []byte) error {
	c, n := conffile.ParseMhvtlConf(cur), conffile.ParseMhvtlConf(next)
	values := map[string]string{}
	for _, k := range editableKeys {
		cv, cok := c.Get(k.key)
		nv, nok := n.Get(k.key)
		if cv == nv && cok == nok {
			continue
		}
		if !nok {
			return refuse(ErrMhvtlConfKey, "mhvtl.conf: %s cannot be removed here", k.key)
		}
		values[k.key] = nv
	}
	derived, err := BuildMhvtlConf(cur, values)
	if err != nil {
		return err
	}
	if !bytes.Equal(derived, next) {
		return refuse(ErrMhvtlConfDerived, "the new mhvtl.conf may differ from the current one only in the values of CAPACITY, VERBOSE, VTL_DEBUG and MHVTL_BACKEND, and of MHVTL_HOME_PATH through mhvtl-move-home; change other lines by hand")
	}
	return nil
}

// BuildMhvtlConf is cur with the editable keys of values set, each through
// conffile's round-trip setter and each matching its pattern. The keys are
// set in editableKeys order, so the result does not depend on how an edit
// was split into requests: a key cur lacks is appended. A value cur
// already has changes nothing.
func BuildMhvtlConf(cur []byte, values map[string]string) ([]byte, error) {
	for k := range values {
		known := false
		for _, e := range editableKeys {
			known = known || e.key == k
		}
		if !known {
			return nil, refuse(ErrMhvtlConfKey, "mhvtl.conf: %s cannot be set here", k)
		}
	}
	c := conffile.ParseMhvtlConf(cur)
	for _, k := range editableKeys {
		v, ok := values[k.key]
		if cv, cok := c.Get(k.key); !ok || cok && cv == v {
			continue
		}
		if !k.valid(v) {
			return nil, refuse(ErrMhvtlConfValue, "mhvtl.conf: %s=%q is not a valid value", k.key, v)
		}
		if err := c.Set(k.key, v); err != nil {
			return nil, refuse(ErrMhvtlConfValue, "%v", err)
		}
	}
	return c.Render(), nil
}

// homeProblem is validate.HomeProblem on home as written and as it
// resolves: the longest ancestor of home that exists, with its symbolic
// links followed, then the rest. Only an ancestor that does not exist is
// climbed past; one that cannot be looked at is refused. A home through a link into /usr, say,
// is refused like /usr itself. A link planted after the check is not
// caught; the home's parents are root's to write.
func homeProblem(home string) string {
	if p := validate.HomeProblem(home); p != "" {
		return p
	}
	resolved, err := resolveHome(home)
	if err != nil {
		return err.Error()
	}
	if p := validate.HomeProblem(resolved); p != "" {
		return "resolves to " + resolved + ", " + p
	}
	return ""
}

// resolveHome is home with the symbolic links of its longest existing
// ancestor followed, and the rest appended. Only an ancestor that does not
// exist is climbed past; one that cannot be looked at is an error.
func resolveHome(home string) (string, error) {
	dir, rest := filepath.Clean(home), ""
	for dir != "/" {
		_, err := os.Lstat(dir)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", errors.New("cannot resolve " + dir + ": " + err.Error())
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = filepath.Dir(dir)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", errors.New("cannot resolve " + dir + ": " + err.Error())
	}
	return filepath.Join(resolved, rest), nil
}

// CheckHomes applies the home rules of plan 4.3 (AC 30): a library that
// stays in device.conf may change its media home, through its own "Home
// directory" or MHVTL_HOME_PATH, only when the old home holds no cartridge
// and no other library uses it. Otherwise the move is mhvtl-move-home's. A
// new library may have any home validate.HomeProblem allows; validate
// checks its barcodes against the cartridges already there. A "Home
// directory" the change sets, for a new library or an old one, must pass
// validate.HomeProblem: mktape and make_vtl_media create the home as root.
func CheckHomes(cur, next config.Inventory, curDefault, nextDefault string) error {
	homeOf := func(l config.Library, def string) string {
		if l.HomeDir != "" {
			return l.HomeDir
		}
		return def
	}
	for _, nl := range next.Libraries {
		var cl *config.Library
		for i := range cur.Libraries {
			if cur.Libraries[i].ID == nl.ID {
				cl = &cur.Libraries[i]
			}
		}
		if nl.HomeDir != "" && (cl == nil || cl.HomeDir != nl.HomeDir) {
			if p := homeProblem(nl.HomeDir); p != "" {
				return refuse(ErrHomeForbidden, "library %d: Home directory %q cannot be a media home: %s", nl.ID, nl.HomeDir, p)
			}
		}
		if cl == nil {
			continue
		}
		old, home := homeOf(*cl, curDefault), homeOf(nl, nextDefault)
		if old == home || old == "" {
			continue
		}
		for _, o := range cur.Libraries {
			if o.ID != nl.ID && homeOf(o, curDefault) == old {
				return refuse(ErrHomeInUse, "library %d: its home %s is shared with library %d; use mhvtl-move-home", nl.ID, old, o.ID)
			}
		}
		if err := homeEmpty(old); err != nil {
			return refuse(ErrHomeInUse, "library %d: %v; use mhvtl-move-home", nl.ID, err)
		}
	}
	return nil
}

// homeEmpty fails when dir holds a directory (a cartridge) or a symbolic
// link (a raw-disk cartridge, ensure_symlink in markh794/mhvtl
// usr/raw_bay_ops.c:96-132 at 852b78a,
// https://github.com/markh794/mhvtl/blob/852b78a7ae3add53ffdc418add5f14999762d788/usr/raw_bay_ops.c#L96-L132),
// or cannot be listed. A missing dir is empty. A dir under /tmp or
// /var/tmp, as written or as its links resolve (resolveHome), is never
// judged empty: both services run with PrivateTmp=yes
// (dist/mhvtl-control.service, dist/mhvtl-control-apply@.service), so they
// see an empty /tmp whatever the real one holds.
func homeEmpty(dir string) error {
	if t, err := sandboxedRoot(dir); err != nil {
		return err
	} else if t != "" {
		return errors.New("the service sandbox hides " + t + ", so the old home " + dir + " cannot be seen empty")
	}
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range ents {
		if e.IsDir() || e.Type()&fs.ModeSymlink != 0 {
			return errors.New("the old home " + dir + " holds " + e.Name())
		}
	}
	return nil
}

// sandboxedRoot is the SandboxedTmp directory dir is in, as written or
// through its links, or "". A link into the real /tmp dangles inside the
// sandbox, so a link whose target is missing is followed as written
// (os.Readlink), a relative target from the link's real directory, at most
// 40 times as the kernel's ELOOP limit.
func sandboxedRoot(dir string) (string, error) {
	p := filepath.Clean(dir)
	for hop := 0; ; hop++ {
		for _, t := range SandboxedTmp {
			if p == t || strings.HasPrefix(p, t+"/") {
				return t, nil
			}
		}
		if hop == 40 {
			return "", errors.New("cannot resolve " + dir + ": too many links")
		}
		resolved, err := resolveHome(p)
		if err == nil {
			if resolved == p {
				return "", nil
			}
			p = resolved
			continue
		}
		// The longest existing ancestor dangles: follow it as written.
		a, rest := p, ""
		for a != "/" {
			if _, err := os.Lstat(a); err == nil {
				break
			}
			rest = filepath.Join(filepath.Base(a), rest)
			a = filepath.Dir(a)
		}
		target, lerr := os.Readlink(a)
		if lerr != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			// The kernel resolves it against the link's real directory,
			// which differs from the written one when a parent is a link.
			parent, err := filepath.EvalSymlinks(filepath.Dir(a))
			if err != nil {
				return "", errors.New("cannot resolve " + dir + ": " + err.Error())
			}
			target = filepath.Join(parent, target)
		}
		p = filepath.Join(target, rest)
	}
}

// SandboxedTmp are the directories PrivateTmp=yes replaces with the
// service's own (systemd.exec(5),
// https://www.freedesktop.org/software/systemd/man/latest/systemd.exec.html#PrivateTmp=).
// Tests whose homes are under t.TempDir() clear it: tests only; never
// assign in production.
var SandboxedTmp = []string{"/tmp", "/var/tmp"}

// defaultHome is the home of a library without a "Home directory":
// MHVTL_HOME_PATH from mhvtl.conf, else the compiled-in default.
func (h *Helper) defaultHome(c *conffile.MhvtlConf) string {
	if v, ok := c.Get(homeKey); ok && v != "" {
		return v
	}
	return h.cfg.DefaultHome
}
