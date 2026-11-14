package api

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// staticTags maps each web UI file (its path in StaticFS) to an ETag of its
// content. embed.FS files have no modification time, so without an ETag a
// browser has nothing to revalidate against and may keep running a cached
// app.js against a newer daemon after an upgrade (#81).
type staticTags map[string]string

// newStaticTags hashes every regular file in fsys. A file that cannot be
// read gets no ETag; it is still served with no-cache.
func newStaticTags(fsys fs.FS) staticTags {
	tags := staticTags{}
	_ = fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		b, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil
		}
		sum := sha256.Sum256(b)
		tags[name] = `"` + hex.EncodeToString(sum[:16]) + `"`
		return nil
	})
	return tags
}

// set makes the browser revalidate name on every load (no-cache) and gives
// it the ETag, so an unchanged file costs a 304 and a changed one is fetched.
// http.ServeContent answers If-None-Match from the ETag header set here.
func (t staticTags) set(w http.ResponseWriter, name string) {
	w.Header().Set("Cache-Control", "no-cache")
	if tag, ok := t[name]; ok {
		w.Header().Set("ETag", tag)
	}
}

// files wraps a handler serving StaticFS by URL path.
func (t staticTags) files(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.set(w, path.Clean(strings.TrimPrefix(r.URL.Path, "/")))
		h.ServeHTTP(w, r)
	})
}
