// Package gui exposes the assets shared by the desktop application.
package gui

import _ "embed"

// Icon is the application icon (512x512 PNG).
//
//go:embed Icon.png
var Icon []byte
