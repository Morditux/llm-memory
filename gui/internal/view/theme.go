// Package view contains the Fyne widgets of the desktop GUI.
package view

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

// Palette of the web explorer (internal/ui/static/index.html).
var (
	colorBG     = color.NRGBA{R: 0x0f, G: 0x17, B: 0x2a, A: 0xff}
	colorPanel  = color.NRGBA{R: 0x1e, G: 0x29, B: 0x3b, A: 0xff}
	colorBorder = color.NRGBA{R: 0x33, G: 0x41, B: 0x55, A: 0xff}
	colorText   = color.NRGBA{R: 0xe2, G: 0xe8, B: 0xf0, A: 0xff}
	colorMuted  = color.NRGBA{R: 0x94, G: 0xa3, B: 0xb8, A: 0xff}
	colorAccent = color.NRGBA{R: 0x38, G: 0xbd, B: 0xf8, A: 0xff}
	colorGreen  = color.NRGBA{R: 0x34, G: 0xd3, B: 0x99, A: 0xff}
	colorAmber  = color.NRGBA{R: 0xfb, G: 0xbf, B: 0x24, A: 0xff}
	colorRed    = color.NRGBA{R: 0xf8, G: 0x71, B: 0x71, A: 0xff}
)

// Theme is the dark theme reproducing the web explorer palette. It always
// renders the dark variant, whatever the system preference.
type Theme struct{}

var _ fyne.Theme = Theme{}

// Color implements fyne.Theme.
func (Theme) Color(name fyne.ThemeColorName, _ fyne.ThemeVariant) color.Color {
	switch name {
	case theme.ColorNameBackground, theme.ColorNameMenuBackground, theme.ColorNameOverlayBackground:
		return colorBG
	case theme.ColorNameInputBackground, theme.ColorNameHeaderBackground, theme.ColorNameButton:
		return colorPanel
	case theme.ColorNameInputBorder, theme.ColorNameSeparator, theme.ColorNameShadow:
		return colorBorder
	case theme.ColorNameForeground:
		return colorText
	case theme.ColorNamePlaceHolder, theme.ColorNameDisabled:
		return colorMuted
	case theme.ColorNamePrimary, theme.ColorNameFocus, theme.ColorNameHyperlink:
		return colorAccent
	case theme.ColorNameSelection:
		return color.NRGBA{R: 0x38, G: 0xbd, B: 0xf8, A: 0x40}
	case theme.ColorNameHover:
		return color.NRGBA{R: 0x38, G: 0xbd, B: 0xf8, A: 0x20}
	case theme.ColorNameSuccess:
		return colorGreen
	case theme.ColorNameWarning:
		return colorAmber
	case theme.ColorNameError:
		return colorRed
	}
	return theme.DefaultTheme().Color(name, theme.VariantDark)
}

// Font implements fyne.Theme.
func (Theme) Font(s fyne.TextStyle) fyne.Resource { return theme.DefaultTheme().Font(s) }

// Icon implements fyne.Theme.
func (Theme) Icon(n fyne.ThemeIconName) fyne.Resource { return theme.DefaultTheme().Icon(n) }

// Size implements fyne.Theme.
func (Theme) Size(n fyne.ThemeSizeName) float32 { return theme.DefaultTheme().Size(n) }
