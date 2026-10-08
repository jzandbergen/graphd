// Package webui embeds the entire front end. There is no build step: these are
// plain files committed to the repo, and the vendored libraries are UMD
// single-file builds. If you find yourself looking for a package.json, you have
// made a wrong turn (SPEC §0).
package webui

import "embed"

// FS holds every asset under assets/, including vendor/.
//
//go:embed all:assets
var FS embed.FS
