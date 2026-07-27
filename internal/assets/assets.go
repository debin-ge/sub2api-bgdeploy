// Package assets contains the runtime files embedded in the bgdeploy binary.
package assets

import "embed"

// Files are compiled into the binary. The server does not need the
// source templates/snippets after the binary has been copied there.
//
//go:embed templates/*.tmpl snippets/*.conf sites.example.yaml env.example runtime.example.yaml
var Files embed.FS
