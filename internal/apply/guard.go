package apply

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

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
// with only the values of the editable keys changed, each through
// conffile's round-trip setter and each matching its pattern.
// make_vtl_media sources mhvtl.conf as bash, as root, from
// mhvtl-media.service (markh794/mhvtl usr/cmd/make_vtl_media.in:307 at
// 852b78a,
// https://github.com/markh794/mhvtl/blob/852b78a7ae3add53ffdc418add5f14999762d788/usr/cmd/make_vtl_media.in#L307),
// so any other line or value would be code run as root: the request's
// bytes are never trusted, only compared with the helper's derivation.
func DeriveMhvtlConf(cur, next []byte) error {
	c, n := conffile.ParseMhvtlConf(cur), conffile.ParseMhvtlConf(next)
	derived := conffile.ParseMhvtlConf(cur)
	for _, k := range editableKeys {
		cv, cok := c.Get(k.key)
		nv, nok := n.Get(k.key)
		if cv == nv && cok == nok {
			continue
		}
		if !nok {
			return refuse(ErrMhvtlConfKey, "mhvtl.conf: %s cannot be removed here", k.key)
		}
		if !k.valid(nv) {
			return refuse(ErrMhvtlConfValue, "mhvtl.conf: %s=%q is not a valid value", k.key, nv)
		}
		if err := derived.Set(k.key, nv); err != nil {
			return refuse(ErrMhvtlConfValue, "%v", err)
		}
	}
	if !bytes.Equal(derived.Render(), next) {
		return refuse(ErrMhvtlConfDerived, "the new mhvtl.conf may differ from the current one only in the values of CAPACITY, VERBOSE, VTL_DEBUG and MHVTL_HOME_PATH; edit anything else by hand")
	}
	return nil
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
	dir, rest := home, ""
	for dir != "/" {
		_, err := os.Lstat(dir)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "cannot resolve " + dir + ": " + err.Error()
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = filepath.Dir(dir)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "cannot resolve " + dir + ": " + err.Error()
	}
	resolved = filepath.Join(resolved, rest)
	if p := validate.HomeProblem(resolved); p != "" {
		return "resolves to " + resolved + ", " + p
	}
	return ""
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
// or cannot be listed. A missing dir is empty.
func homeEmpty(dir string) error {
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

// defaultHome is the home of a library without a "Home directory":
// MHVTL_HOME_PATH from mhvtl.conf, else the compiled-in default.
func (h *Helper) defaultHome(c *conffile.MhvtlConf) string {
	if v, ok := c.Get(homeKey); ok && v != "" {
		return v
	}
	return h.cfg.DefaultHome
}
