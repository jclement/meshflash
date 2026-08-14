package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// Choice is one selectable row.
type Choice struct {
	// Key is returned to the caller when this row is chosen.
	Key string
	// Title is the primary label.
	Title string
	// Detail is dimmed supporting text shown after the title.
	Detail string
	// Group, when set, sorts rows under a heading.
	Group string
}

// ErrCancelled means the operator backed out of a picker.
type ErrCancelled struct{}

func (ErrCancelled) Error() string { return "cancelled" }

// Pick presents a single-select list and returns the chosen key.
func Pick(title, prompt string, choices []Choice) (string, error) {
	if len(choices) == 0 {
		return "", fmt.Errorf("nothing to choose from")
	}
	if len(choices) == 1 {
		return choices[0].Key, nil
	}

	m := &pickModel{title: title, prompt: prompt, choices: choices, width: 80}
	final, err := tea.NewProgram(m).Run()
	if err != nil {
		return "", err
	}
	fm := final.(*pickModel)
	if fm.cancelled {
		return "", ErrCancelled{}
	}
	return fm.choices[fm.cursor].Key, nil
}

type pickModel struct {
	title     string
	prompt    string
	choices   []Choice
	cursor    int
	width     int
	cancelled bool
	done      bool
}

func (m *pickModel) Init() tea.Cmd { return nil }

func (m *pickModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			m.cancelled = true
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.choices)-1 {
				m.cursor++
			}
		case "home", "g":
			m.cursor = 0
		case "end", "G":
			m.cursor = len(m.choices) - 1
		case "enter", "space", " ":
			m.done = true
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m *pickModel) View() tea.View {
	var b strings.Builder
	b.WriteString(Title().Render(" " + m.title + " "))
	b.WriteString("\n")
	if m.prompt != "" {
		b.WriteString(Subtitle().Render(m.prompt) + "\n")
	}
	b.WriteString("\n")

	lastGroup := ""
	for i, c := range m.choices {
		if c.Group != "" && c.Group != lastGroup {
			b.WriteString(Heading().Render(c.Group) + "\n")
			lastGroup = c.Group
		}

		// Truncate the plain text first: slicing a styled string would cut
		// through an ANSI escape and corrupt the rest of the screen.
		avail := max(m.width-6, 30)
		title := Truncate(c.Title, avail)
		detail := ""
		if c.Detail != "" {
			detail = Muted().Render("  " + Truncate(c.Detail, max(avail-len([]rune(title)), 8)))
		}

		if i == m.cursor {
			b.WriteString(Selected().Render(GlyphArrow+" ") + Selected().Render(title) + detail + "\n")
		} else {
			b.WriteString("    " + title + detail + "\n")
		}
	}

	b.WriteString(Help().Render("↑/↓ move · enter select · esc cancel"))
	return tea.NewView(b.String())
}
