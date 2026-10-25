// Package web embeds interim's server-rendered templates and static assets.
package web

import "embed"

//go:embed templates/*.html static/* openapi.yaml
var FS embed.FS
