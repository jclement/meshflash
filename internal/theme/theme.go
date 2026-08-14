// Package theme holds meshflash's visual language: one palette and one set of
// styles, shared by the console log handler and the full-screen views so both
// agree on what "warning" looks like.
//
// Lip Gloss v2 dropped adaptive colors in favour of resolving light/dark once
// against the actual terminal, so the palette is built lazily on first use and
// then fixed for the process.
package theme

import (
	"image/color"
	"os"
	"sync"

	"charm.land/lipgloss/v2"
)

// Palette is the resolved colour set.
type Palette struct {
	Accent color.Color
	OK     color.Color
	Warn   color.Color
	Err    color.Color
	Info   color.Color
	Muted  color.Color
	Debug  color.Color

	// TitleFG is the text colour used on an accent-filled bar.
	TitleFG color.Color
}

var (
	mu       sync.Mutex
	override *bool // nil means auto-detect
	cached   *Styles
	cachedC  *Palette
)

// SetColor forces colour output on or off, overriding auto-detection. Call it
// before anything renders — the CLI does so while parsing global flags.
func SetColor(enabled bool) {
	mu.Lock()
	defer mu.Unlock()
	override = &enabled
	cached, cachedC = nil, nil
}

// colorEnabled reports whether styled output should be produced.
//
// Lip Gloss v2 does not degrade automatically the way v1 did, so this has to
// be decided here or piped output ends up full of escape sequences.
func colorEnabled() bool {
	if override != nil {
		return *override
	}
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("CLICOLOR_FORCE") != "" {
		return true
	}
	return isTerminal(os.Stdout)
}

// resolve builds the palette for the detected terminal background.
//
// Querying the terminal is only meaningful on a TTY; when output is piped the
// query would either hang or leak an escape sequence into the stream, so a
// dark palette is assumed instead.
func resolve() Palette {
	dark := true
	if isTerminal(os.Stdout) {
		dark = lipgloss.HasDarkBackground(os.Stdin, os.Stdout)
	}
	pick := lipgloss.LightDark(dark)

	return Palette{
		Accent:  pick(lipgloss.Color("#8839EF"), lipgloss.Color("#CBA6F7")),
		OK:      pick(lipgloss.Color("#40A02B"), lipgloss.Color("#A6E3A1")),
		Warn:    pick(lipgloss.Color("#DF8E1D"), lipgloss.Color("#F9E2AF")),
		Err:     pick(lipgloss.Color("#D20F39"), lipgloss.Color("#F38BA8")),
		Info:    pick(lipgloss.Color("#1E66F5"), lipgloss.Color("#89B4FA")),
		Muted:   pick(lipgloss.Color("#8C8FA1"), lipgloss.Color("#7F849C")),
		Debug:   pick(lipgloss.Color("#6C7086"), lipgloss.Color("#6C7086")),
		TitleFG: pick(lipgloss.Color("#FFFFFF"), lipgloss.Color("#1E1E2E")),
	}
}

// Colors returns the resolved palette.
func Colors() Palette {
	mu.Lock()
	defer mu.Unlock()
	if cachedC == nil {
		p := resolve()
		cachedC = &p
	}
	return *cachedC
}

// Styles bundles the rendered styles.
type Styles struct {
	Title    lipgloss.Style
	Subtitle lipgloss.Style
	Heading  lipgloss.Style
	Muted    lipgloss.Style
	OK       lipgloss.Style
	Warn     lipgloss.Style
	Error    lipgloss.Style
	Info     lipgloss.Style
	Debug    lipgloss.Style
	Selected lipgloss.Style
	Help     lipgloss.Style
	Box      lipgloss.Style
	Danger   lipgloss.Style
	Key      lipgloss.Style
	Accent   lipgloss.Style
}

// plainStyles is the unstyled set used when colour is off. Returning real
// styles with no colour still emits reset sequences, so this returns bare
// styles that render text through untouched.
func plainStyles() Styles {
	s := lipgloss.NewStyle()
	return Styles{
		Title: s, Subtitle: s, Heading: s, Muted: s, OK: s, Warn: s,
		Error: s, Info: s, Debug: s, Selected: s, Help: s, Box: s,
		Danger: s, Key: s, Accent: s,
	}
}

func buildStylesFrom(c Palette) Styles {
	return Styles{
		Title: lipgloss.NewStyle().Bold(true).
			Foreground(c.TitleFG).Background(c.Accent).Padding(0, 1),
		Subtitle: lipgloss.NewStyle().Foreground(c.Muted),
		// No vertical margin: Lip Gloss pads margin lines out to the block
		// width with spaces, which leaves trailing whitespace all through
		// piped output. Callers add their own blank lines.
		Heading:  lipgloss.NewStyle().Bold(true).Foreground(c.Accent),
		Muted:    lipgloss.NewStyle().Foreground(c.Muted),
		OK:       lipgloss.NewStyle().Foreground(c.OK),
		Warn:     lipgloss.NewStyle().Foreground(c.Warn),
		Error:    lipgloss.NewStyle().Foreground(c.Err).Bold(true),
		Info:     lipgloss.NewStyle().Foreground(c.Info),
		Debug:    lipgloss.NewStyle().Foreground(c.Debug),
		Selected: lipgloss.NewStyle().Foreground(c.Accent).Bold(true),
		Help:     lipgloss.NewStyle().Foreground(c.Muted),
		Key:      lipgloss.NewStyle().Foreground(c.Accent),
		Accent:   lipgloss.NewStyle().Foreground(c.Accent),
		Box: lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
			BorderForeground(c.Muted).Padding(0, 1),
		Danger: lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
			BorderForeground(c.Warn).Padding(0, 1),
	}
}

// S returns the resolved styles.
func S() Styles {
	mu.Lock()
	defer mu.Unlock()
	if cached == nil {
		// Built outside the Colors() call path would deadlock on mu, so the
		// palette is resolved inline here.
		st := buildStylesLocked()
		cached = &st
	}
	return *cached
}

// buildStylesLocked assumes mu is held.
func buildStylesLocked() Styles {
	if !colorEnabled() {
		return plainStyles()
	}
	if cachedC == nil {
		p := resolve()
		cachedC = &p
	}
	return buildStylesFrom(*cachedC)
}

// isTerminal reports whether f is attached to a terminal.
func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}
