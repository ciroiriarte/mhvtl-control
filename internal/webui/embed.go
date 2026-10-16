// Package webui embeds the mhvtl-control dashboard (HTML/CSS/JS) into the binary so
// the daemon is self-contained and serves its web UI from any working
// directory, with no external asset files.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:assets
var embedded embed.FS

// FS returns the dashboard asset tree rooted so that index.html is at the FS
// root and the static/ subtree holds css/js. This matches the api server's
// static routes: "/" serves index.html and "/static/..." serves the rest.
func FS() fs.FS {
	sub, err := fs.Sub(embedded, "assets")
	if err != nil {
		// embed paths are fixed at build time; a failure here is a programmer
		// error, not a runtime condition.
		panic(err)
	}
	return sub
}
