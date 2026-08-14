// Package tui holds the interactive views.
package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/jclement/meshflash/internal/theme"
)

// Styles are re-exported from the shared theme so call sites can stay short.
// They are functions rather than variables because the palette is resolved
// against the terminal on first use, which must not happen at init time.
func Title() lipgloss.Style    { return theme.S().Title }
func Subtitle() lipgloss.Style { return theme.S().Subtitle }
func Heading() lipgloss.Style  { return theme.S().Heading }
func Muted() lipgloss.Style    { return theme.S().Muted }
func OK() lipgloss.Style       { return theme.S().OK }
func Warn() lipgloss.Style     { return theme.S().Warn }
func Error() lipgloss.Style    { return theme.S().Error }
func Info() lipgloss.Style     { return theme.S().Info }
func Selected() lipgloss.Style { return theme.S().Selected }
func Help() lipgloss.Style     { return theme.S().Help }
func Box() lipgloss.Style      { return theme.S().Box }
func Danger() lipgloss.Style   { return theme.S().Danger }

// Status glyphs. Plain ASCII fallbacks are not used: every terminal meshflash
// targets handles these, and they carry meaning at a glance in a field log.
const (
	GlyphOK      = "✓"
	GlyphWarn    = "!"
	GlyphFail    = "✗"
	GlyphInfo    = "•"
	GlyphPending = "◦"
	GlyphArrow   = "→"
)

// StatusGlyph renders a coloured status marker.
func StatusGlyph(status string) string {
	switch status {
	case "ok":
		return OK().Render(GlyphOK)
	case "warn":
		return Warn().Render(GlyphWarn)
	case "fail":
		return Error().Render(GlyphFail)
	default:
		return Info().Render(GlyphInfo)
	}
}

// Bar renders a progress bar of the given width.
//
// A negative percent means indeterminate, which is the honest rendering for
// phases like "erasing flash" where the device goes silent and reports nothing.
func Bar(width int, percent float64) string {
	if width < 4 {
		width = 4
	}
	if percent < 0 {
		return Muted().Render(strings.Repeat("░", width))
	}
	if percent > 100 {
		percent = 100
	}
	filled := int(float64(width) * percent / 100)
	if filled > width {
		filled = width
	}
	// Use the themed style rather than building one from the palette directly:
	// the palette is always populated, so constructing a style from it emits
	// colour even when colour is switched off or output is piped.
	return theme.S().Accent.Render(strings.Repeat("█", filled)) +
		Muted().Render(strings.Repeat("░", width-filled))
}

// Truncate shortens a string to width, adding an ellipsis.
//
// Callers must truncate plain text before styling it: slicing a rendered
// string would cut through an ANSI escape and corrupt the rest of the screen.
func Truncate(s string, width int) string {
	if width <= 1 || lipgloss.Width(s) <= width {
		return s
	}
	runes := []rune(s)
	if len(runes) <= width {
		return s
	}
	return string(runes[:width-1]) + "…"
}

// Indent prefixes every line of a block.
func Indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}
