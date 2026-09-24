// Package content holds the reviewed, portable instruction fragments, skill
// templates, model roster and upstream provenance embedded in the binary.
package content

import "embed"

//go:embed fragments skills roster.json provenance.json
var FS embed.FS
