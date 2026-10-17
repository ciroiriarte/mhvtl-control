package apply

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"mhvtl-control/internal/conffile"
	"mhvtl-control/internal/config"
)

// mhvtl.conf keys a change may set (plan 3.2, guard.go; AC 30). A change
// to MHVTL_HOME_PATH is let through to the home rules (checkHomes); any
// other key is the operator's to edit by hand.
var editableKeys = map[string]bool{"CAPACITY": true, "VERBOSE": true, "VTL_DEBUG": true}

const homeKey = "MHVTL_HOME_PATH"

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
// at d65d25f,
// https://github.com/markh794/mhvtl/blob/d65d25f350f66bbb8e9d572a0cb718f3052ec7da/usr/vtllib.c#L279-L308),
// and their units load mhvtl.conf as an EnvironmentFile too
// (etc/vtllibrary@.service.in:16, etc/vtltape@.service.in:13). The helper's
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

// checkMhvtlConfKeys refuses a change to any mhvtl.conf key but CAPACITY,
// VERBOSE, VTL_DEBUG and MHVTL_HOME_PATH.
func checkMhvtlConfKeys(cur, next *conffile.MhvtlConf) error {
	keys := append(cur.Keys(), next.Keys()...)
	for _, k := range keys {
		cv, cok := cur.Get(k)
		nv, nok := next.Get(k)
		if cv == nv && cok == nok || editableKeys[k] || k == homeKey {
			continue
		}
		return refuse(ErrMhvtlConfKey, "mhvtl.conf: %s cannot be changed here; edit it by hand", k)
	}
	return nil
}

// checkHomes applies the home rules of plan 4.3 (AC 30): a library that
// stays in device.conf may change its media home, through its own "Home
// directory" or MHVTL_HOME_PATH, only when the old home holds no cartridge
// and no other library uses it. Otherwise the move is mhvtl-move-home's. A
// new library may have any home; validate checks its barcodes against the
// cartridges already there.
func checkHomes(cur, next config.Inventory, curDefault, nextDefault string) error {
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
// usr/raw_bay_ops.c:96-132 at d65d25f), or
// cannot be listed. A missing dir is empty.
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
