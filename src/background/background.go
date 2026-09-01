// Package background embeds the subtle background image drawn behind the
// controls.
package background

import (
	_ "embed"
)

//go:embed bg-avatar.png
var PNG []byte
