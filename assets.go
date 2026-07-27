package main

import "embed"

// runtimeAssets are compiled into the binary. The server does not need the
// source templates/snippets after the binary has been copied there.
//
//go:embed templates/*.tmpl snippets/*.conf sites.example.yaml env.example runtime.example.yaml
var runtimeAssets embed.FS
