package tui

import (
	"fmt"
	"sort"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/jclement/meshflash/internal/catalog"
	"github.com/jclement/meshflash/internal/store"
)

// DeviceRow is one selectable board in the configure view.
type DeviceRow struct {
	ID       string
	Name     string
	Vendor   string
	Platform string
	// Projects lists which upstreams ship firmware for this board.
	Projects []string
	// Bytes is the estimated download this board pulls in, which is what
	// makes the size consequence of a selection visible before committing.
	Bytes int64
	// Attached marks boards currently plugged in, so the common case of
	// "select what I'm holding" is one keystroke.
	Attached bool
}

// Configure presents the device multi-select and returns the chosen IDs.
func Configure(rows []DeviceRow, selected []string) ([]string, error) {
	if len(rows) == 0 {
		return nil, fmt.Errorf("the catalog lists no devices; run `meshflash update` first")
	}

	sel := map[string]bool{}
	for _, id := range selected {
		sel[id] = true
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Platform != rows[j].Platform {
			return rows[i].Platform < rows[j].Platform
		}
		return rows[i].Name < rows[j].Name
	})

	filter := textinput.New()
	filter.Placeholder = "type to filter"
	filter.Prompt = "  / "
	filter.CharLimit = 40

	m := &configureModel{rows: rows, selected: sel, filter: filter, width: 90, height: 24}
	m.applyFilter()

	final, err := tea.NewProgram(m).Run()
	if err != nil {
		return nil, err
	}
	fm := final.(*configureModel)
	if fm.cancelled {
		return nil, ErrCancelled{}
	}

	out := make([]string, 0, len(fm.selected))
	for id, on := range fm.selected {
		if on {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out, nil
}

type configureModel struct {
	rows     []DeviceRow
	visible  []int // indexes into rows, after filtering
	selected map[string]bool

	filter    textinput.Model
	filtering bool

	cursor    int
	offset    int
	width     int
	height    int
	cancelled bool
}

func (m *configureModel) Init() tea.Cmd { return textinput.Blink }

func (m *configureModel) applyFilter() {
	q := strings.ToLower(strings.TrimSpace(m.filter.Value()))
	m.visible = m.visible[:0]
	for i, r := range m.rows {
		if q == "" || strings.Contains(strings.ToLower(r.Name+" "+r.ID+" "+r.Vendor+" "+r.Platform), q) {
			m.visible = append(m.visible, i)
		}
	}
	if m.cursor >= len(m.visible) {
		m.cursor = max(len(m.visible)-1, 0)
	}
}

// listHeight is how many rows fit, leaving room for the header and footer.
func (m *configureModel) listHeight() int { return max(m.height-9, 3) }

func (m *configureModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.KeyPressMsg:
		if m.filtering {
			switch msg.String() {
			case "esc":
				m.filtering = false
				m.filter.SetValue("")
				m.filter.Blur()
				m.applyFilter()
				return m, nil
			case "enter":
				m.filtering = false
				m.filter.Blur()
				return m, nil
			}
			var cmd tea.Cmd
			m.filter, cmd = m.filter.Update(msg)
			m.applyFilter()
			return m, cmd
		}

		switch msg.String() {
		case "ctrl+c", "esc":
			m.cancelled = true
			return m, tea.Quit
		case "q":
			m.cancelled = true
			return m, tea.Quit
		case "/":
			m.filtering = true
			m.filter.Focus()
			return m, textinput.Blink
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.visible)-1 {
				m.cursor++
			}
		case "pgup":
			m.cursor = max(m.cursor-m.listHeight(), 0)
		case "pgdown":
			m.cursor = min(m.cursor+m.listHeight(), max(len(m.visible)-1, 0))
		case "home", "g":
			m.cursor = 0
		case "end", "G":
			m.cursor = max(len(m.visible)-1, 0)
		case " ", "x":
			m.toggleCursor()
		case "a":
			// Select everything currently visible, which combined with the
			// filter is how "all nRF52 boards" gets selected quickly.
			for _, i := range m.visible {
				m.selected[m.rows[i].ID] = true
			}
		case "n":
			for _, i := range m.visible {
				delete(m.selected, m.rows[i].ID)
			}
		case "d":
			// Select exactly what is plugged in right now.
			for _, r := range m.rows {
				if r.Attached {
					m.selected[r.ID] = true
				}
			}
		case "enter":
			return m, tea.Quit
		}
		m.scrollToCursor()
	}
	return m, nil
}

func (m *configureModel) toggleCursor() {
	if m.cursor >= len(m.visible) {
		return
	}
	id := m.rows[m.visible[m.cursor]].ID
	if m.selected[id] {
		delete(m.selected, id)
	} else {
		m.selected[id] = true
	}
}

func (m *configureModel) scrollToCursor() {
	h := m.listHeight()
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+h {
		m.offset = m.cursor - h + 1
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

func (m *configureModel) selectedBytes() int64 {
	var total int64
	for _, r := range m.rows {
		if m.selected[r.ID] {
			total += r.Bytes
		}
	}
	return total
}

func (m *configureModel) View() tea.View {
	var b strings.Builder

	count := 0
	for _, on := range m.selected {
		if on {
			count++
		}
	}

	b.WriteString(Title().Render(" meshflash configure "))
	b.WriteString("  ")
	b.WriteString(Subtitle().Render("choose the boards you carry — only these get cached for offline flashing"))
	b.WriteString("\n\n")

	fmt.Fprintf(&b, "  %s selected · about %s per release\n",
		Selected().Render(fmt.Sprintf("%d boards", count)),
		Selected().Render(store.FormatBytes(m.selectedBytes())))

	if m.filtering || m.filter.Value() != "" {
		b.WriteString(m.filter.View() + "\n")
	}
	b.WriteString("\n")

	h := m.listHeight()
	end := min(m.offset+h, len(m.visible))

	if len(m.visible) == 0 {
		b.WriteString(Muted().Render("  no boards match the filter\n"))
	}

	for i := m.offset; i < end; i++ {
		r := m.rows[m.visible[i]]

		check := Muted().Render("[ ]")
		if m.selected[r.ID] {
			check = OK().Render("[x]")
		}

		name := Truncate(r.Name, 34)
		meta := fmt.Sprintf("%-10s %-18s %8s", r.Platform, strings.Join(r.Projects, "+"), store.FormatBytes(r.Bytes))
		if r.Attached {
			meta += "  " + OK().Render("attached")
		}

		row := fmt.Sprintf("%s %-34s %s", check, name, Muted().Render(meta))
		if i == m.cursor {
			b.WriteString(Selected().Render(GlyphArrow+" ") + row + "\n")
		} else {
			b.WriteString("  " + row + "\n")
		}
	}

	if len(m.visible) > h {
		fmt.Fprintf(&b, "\n  %s\n", Muted().Render(fmt.Sprintf("showing %d–%d of %d", m.offset+1, end, len(m.visible))))
	}

	if m.filtering {
		b.WriteString(Help().Render("type to filter · enter accept · esc clear"))
	} else {
		b.WriteString(Help().Render("space toggle · a all shown · n none shown · d attached · / filter · enter save · esc cancel"))
	}
	v := tea.NewView(b.String())
	// The device picker is a long, scrollable list, so it takes over the screen
	// and restores the scrollback on exit.
	v.AltScreen = true
	return v
}

// BuildDeviceRows turns a catalog into configure rows, estimating the download
// each board implies from its newest release.
func BuildDeviceRows(cat *catalog.Catalog, attached map[string]bool) []DeviceRow {
	type agg struct {
		projects map[string]bool
		bytes    int64
	}
	byDevice := map[string]*agg{}

	for pi := range cat.Projects {
		p := &cat.Projects[pi]
		rel, ok := p.LatestStable()
		if !ok {
			continue
		}
		// Count each source archive once per device: a device whose artifacts
		// all come from one platform zip should not be charged for it twice.
		for _, bld := range rel.Builds {
			a := byDevice[bld.DeviceID]
			if a == nil {
				a = &agg{projects: map[string]bool{}}
				byDevice[bld.DeviceID] = a
			}
			a.projects[p.ID] = true
			a.bytes += store.DownloadBytes(bld.Artifacts)
		}
	}

	rows := make([]DeviceRow, 0, len(byDevice))
	for _, d := range cat.Devices {
		a, ok := byDevice[d.ID]
		if !ok {
			continue // no firmware ships for this board
		}
		projects := make([]string, 0, len(a.projects))
		for id := range a.projects {
			projects = append(projects, id)
		}
		sort.Strings(projects)

		rows = append(rows, DeviceRow{
			ID:       d.ID,
			Name:     d.Name,
			Vendor:   d.Vendor,
			Platform: d.Platform,
			Projects: projects,
			Bytes:    a.bytes,
			Attached: attached[d.ID],
		})
	}
	return rows
}
