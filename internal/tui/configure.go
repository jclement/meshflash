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
	// Attached marks boards that could plausibly be the hardware currently
	// plugged in. USB identification is weak, so this is a candidate set
	// rather than a definite match.
	Attached bool
}

// searchText is the haystack the filter matches against, precomputed because
// it is rebuilt on every keystroke across a couple of hundred rows.
func (r DeviceRow) searchText() string {
	return strings.ToLower(strings.Join([]string{
		r.Name, r.ID, r.Vendor, r.Platform, strings.Join(r.Projects, " "),
	}, " "))
}

// viewMode narrows which boards the list shows.
type viewMode int

const (
	viewAll viewMode = iota
	viewAttached
	viewSelected
)

func (v viewMode) label() string {
	switch v {
	case viewAttached:
		return "attached"
	case viewSelected:
		return "selected"
	default:
		return "all"
	}
}

func (v viewMode) next() viewMode { return (v + 1) % 3 }

// Configure presents the device multi-select and returns the chosen IDs.
func Configure(rows []DeviceRow, selected []string) ([]string, error) {
	if len(rows) == 0 {
		return nil, fmt.Errorf("the catalog lists no devices; run `meshflash update` first")
	}

	sel := map[string]bool{}
	initial := map[string]bool{}
	for _, id := range selected {
		sel[id] = true
		initial[id] = true
	}

	SortDeviceRows(rows)

	filter := textinput.New()
	filter.Placeholder = "name, vendor or chip"
	filter.Prompt = "  search: "
	filter.CharLimit = 40

	m := &configureModel{
		rows:     rows,
		selected: sel,
		initial:  initial,
		filter:   filter,
		width:    90,
		height:   24,
	}
	m.applyView()

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
	visible  []int // indexes into rows, after the view mode and filter
	selected map[string]bool
	// initial is the selection on entry, used to detect unsaved changes.
	initial map[string]bool

	mode viewMode
	// attachedShown is how many of the leading visible rows are attached
	// boards, which is where the section break goes.
	attachedShown int
	filter        textinput.Model
	// filtering is true while the search box has focus.
	filtering bool
	// confirming is true while the save/discard prompt is up.
	confirming bool

	cursor    int
	offset    int
	width     int
	height    int
	cancelled bool
}

func (m *configureModel) Init() tea.Cmd { return textinput.Blink }

// resetView recomputes the visible rows and returns to the top.
//
// Used when the scope changes — a new search or view mode — because landing
// mid-way down a fresh result set is disorienting.
func (m *configureModel) resetView() {
	m.cursor = 0
	m.offset = 0
	m.applyView()
}

// applyView recomputes the visible rows for the current mode and filter,
// keeping the cursor where it is.
//
// Correcting the scroll here is essential, not cosmetic: narrowing a long list
// while scrolled down otherwise leaves offset past the end of the new result
// set, and the render loop draws nothing at all — so a board that plainly
// matches the search appears to be missing.
func (m *configureModel) applyView() {
	terms := strings.Fields(strings.ToLower(m.filter.Value()))

	// Boards that are plugged in go first, in their own section.
	//
	// The overwhelmingly common task is "configure what I am holding", and
	// hunting for those few boards in a list of two hundred is the whole
	// friction. Everything else keeps its alphabetical order below.
	m.visible = m.visible[:0]
	var rest []int
	for i, r := range m.rows {
		switch m.mode {
		case viewAttached:
			if !r.Attached {
				continue
			}
		case viewSelected:
			if !m.selected[r.ID] {
				continue
			}
		}
		if !matchesAll(r.searchText(), terms) {
			continue
		}
		if r.Attached {
			m.visible = append(m.visible, i)
		} else {
			rest = append(rest, i)
		}
	}
	m.attachedShown = len(m.visible)
	m.visible = append(m.visible, rest...)

	if m.cursor >= len(m.visible) {
		m.cursor = max(len(m.visible)-1, 0)
	}
	if m.offset > m.cursor {
		m.offset = m.cursor
	}
	m.scrollToCursor()
}

// matchesAll requires every term to appear, in any order, so "heltec v4" and
// "v4 heltec" both find the same board.
func matchesAll(haystack string, terms []string) bool {
	for _, t := range terms {
		if !strings.Contains(haystack, t) {
			return false
		}
	}
	return true
}

// listHeight is how many rows fit, leaving room for the header and footer.
//
// The section headers are drawn inside the list, so they have to come out of
// the same budget or the footer gets pushed off the bottom of the screen.
func (m *configureModel) listHeight() int {
	h := m.height - 10
	if m.splitSections() {
		h -= 2
	}
	return max(h, 3)
}

// dirty reports whether the selection differs from the one we started with.
func (m *configureModel) dirty() bool {
	count := 0
	for id, on := range m.selected {
		if !on {
			continue
		}
		count++
		if !m.initial[id] {
			return true
		}
	}
	return count != len(m.initial)
}

func (m *configureModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.scrollToCursor()
		return m, nil

	case tea.KeyPressMsg:
		switch {
		case m.confirming:
			return m.updateConfirm(msg)
		case m.filtering:
			return m.updateFilter(msg)
		default:
			return m.updateList(msg)
		}
	}
	return m, nil
}

// updateConfirm handles the save/discard prompt shown when leaving with
// unsaved changes.
func (m *configureModel) updateConfirm(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y", "enter":
		return m, tea.Quit // cancelled stays false: save
	case "n", "N":
		m.cancelled = true
		return m, tea.Quit
	case "esc", "c", "ctrl+c":
		// Back to editing rather than picking for them.
		m.confirming = false
		return m, nil
	}
	return m, nil
}

func (m *configureModel) updateFilter(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.filtering = false
		m.filter.SetValue("")
		m.filter.Blur()
		m.resetView()
		return m, nil
	case "enter", "down", "up":
		// Leave the box but keep the query, so the results stay narrowed
		// while you arrow through them.
		m.filtering = false
		m.filter.Blur()
		return m, nil
	}

	var cmd tea.Cmd
	m.filter, cmd = m.filter.Update(msg)
	m.resetView()
	return m, cmd
}

func (m *configureModel) updateList(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		m.cancelled = true
		return m, tea.Quit

	case "esc", "q":
		// Escape must not silently discard work. With changes pending it asks;
		// with none it just leaves.
		if m.dirty() {
			m.confirming = true
			return m, nil
		}
		m.cancelled = true
		return m, tea.Quit

	case "enter":
		// Enter is the unambiguous "save and go" — no prompt.
		return m, tea.Quit

	case "/":
		m.filtering = true
		m.filter.Focus()
		return m, textinput.Blink

	case "t":
		m.mode = m.mode.next()
		m.resetView()

	case "d":
		// Jump straight to what is plugged in. Combined with `a` this is how a
		// kit gets configured: d to narrow, a to take them all.
		m.mode = viewAttached
		m.resetView()

	case "s":
		m.mode = viewSelected
		m.resetView()

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

	case "space", " ", "x":
		m.toggleCursor()
		// Deselecting inside the selected-only view would leave the row
		// stranded, so rebuild the list.
		if m.mode == viewSelected {
			m.applyView()
		}

	case "a":
		for _, i := range m.visible {
			m.selected[m.rows[i].ID] = true
		}
	case "n":
		for _, i := range m.visible {
			delete(m.selected, m.rows[i].ID)
		}
		if m.mode == viewSelected {
			m.applyView()
		}
	}

	m.scrollToCursor()
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

func (m *configureModel) selectedCount() int {
	n := 0
	for _, on := range m.selected {
		if on {
			n++
		}
	}
	return n
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

func (m *configureModel) attachedCount() int {
	n := 0
	for _, r := range m.rows {
		if r.Attached {
			n++
		}
	}
	return n
}

func (m *configureModel) View() tea.View {
	var b strings.Builder

	b.WriteString(Title().Render(" meshflash configure "))
	b.WriteString("  ")
	b.WriteString(Subtitle().Render("choose the boards you carry — only these get cached for offline flashing"))
	b.WriteString("\n\n")

	fmt.Fprintf(&b, "  %s selected · about %s per release",
		Selected().Render(fmt.Sprintf("%d boards", m.selectedCount())),
		Selected().Render(store.FormatBytes(m.selectedBytes())))
	if m.dirty() {
		b.WriteString(Warn().Render("  · unsaved"))
	}
	b.WriteString("\n")

	// With a couple of hundred boards the list runs well past the screen, so
	// state plainly what is being shown. A board below the fold otherwise
	// reads as missing.
	b.WriteString("  " + Muted().Render(m.scopeLine()) + "\n")

	if m.filtering || m.filter.Value() != "" {
		b.WriteString(m.filter.View() + "\n")
	}
	b.WriteString("\n")

	h := m.listHeight()
	end := min(m.offset+h, len(m.visible))

	if len(m.visible) == 0 {
		b.WriteString("  " + Muted().Render(m.emptyMessage()) + "\n")
	}

	for i := m.offset; i < end; i++ {
		r := m.rows[m.visible[i]]

		// A section header goes at each boundary, and also at the top of the
		// window when it opens mid-section, so you can always tell which half
		// of the list you are looking at.
		if h := m.sectionHeader(i); h != "" {
			b.WriteString("  " + h + "\n")
		}

		check := Muted().Render("[ ]")
		if m.selected[r.ID] {
			check = OK().Render("[x]")
		}

		name := Truncate(r.Name, 34)
		meta := fmt.Sprintf("%-10s %-18s %8s",
			r.Platform, strings.Join(r.Projects, "+"), store.FormatBytes(r.Bytes))
		row := fmt.Sprintf("%s %-34s %s", check, name, Muted().Render(meta))
		if r.Attached {
			row += "  " + OK().Render("attached")
		}

		if i == m.cursor {
			b.WriteString(Selected().Render(GlyphArrow+" ") + row + "\n")
		} else {
			b.WriteString("  " + row + "\n")
		}
	}

	if len(m.visible) > h {
		fmt.Fprintf(&b, "\n  %s\n", Muted().Render(
			fmt.Sprintf("showing %d–%d of %d", m.offset+1, end, len(m.visible))))
	}

	b.WriteString("\n")
	b.WriteString(m.footer())

	v := tea.NewView(b.String())
	// A long scrollable list takes over the screen and restores the scrollback
	// on exit.
	v.AltScreen = true
	return v
}

// splitSections reports whether the list is showing both attached and
// non-attached boards, and so needs headers to separate them.
func (m *configureModel) splitSections() bool {
	return m.attachedShown > 0 && m.attachedShown < len(m.visible)
}

// sectionHeader returns the header to draw above visible row i, or "".
func (m *configureModel) sectionHeader(i int) string {
	if !m.splitSections() {
		return ""
	}
	switch {
	case i == 0 || (i == m.offset && i < m.attachedShown):
		return OK().Render(fmt.Sprintf("connected now (%d)", m.attachedShown))
	case i == m.attachedShown || (i == m.offset && i > m.attachedShown):
		return Muted().Render(fmt.Sprintf("all other boards (%d)", len(m.visible)-m.attachedShown))
	}
	return ""
}

// scopeLine describes what the list is currently showing.
func (m *configureModel) scopeLine() string {
	scope := fmt.Sprintf("%d boards", len(m.rows))
	switch m.mode {
	case viewAttached:
		scope = fmt.Sprintf("attached only · %d of %d boards", len(m.visible), len(m.rows))
	case viewSelected:
		scope = fmt.Sprintf("selected only · %d of %d boards", len(m.visible), len(m.rows))
	}
	if q := m.filter.Value(); q != "" {
		return fmt.Sprintf("%s · matching %q · %d shown", scope, q, len(m.visible))
	}
	if m.mode == viewAll {
		return scope + " · press / to search by name, vendor or chip"
	}
	return scope
}

func (m *configureModel) emptyMessage() string {
	switch {
	case m.filter.Value() != "":
		return fmt.Sprintf("no boards match %q — press esc to clear the search", m.filter.Value())
	case m.mode == viewAttached && m.attachedCount() == 0:
		return "no boards are plugged in right now — press t to show all boards"
	case m.mode == viewSelected:
		return "nothing selected yet — press t to show all boards"
	default:
		return "no boards to show"
	}
}

func (m *configureModel) footer() string {
	if m.confirming {
		return Danger().Render(
			Warn().Render("Save your changes?") + "\n" +
				fmt.Sprintf("%d boards selected, about %s per release.\n\n",
					m.selectedCount(), store.FormatBytes(m.selectedBytes())) +
				"y save and exit · n discard and exit · esc keep editing")
	}
	if m.filtering {
		return Help().Render("type to search · enter/↓ accept · esc clear")
	}
	return Help().Render(
		"space toggle · a all shown · n none shown · / search\n" +
			"t view (" + m.mode.label() + ") · d attached · s selected · enter save · esc exit")
}

// SortDeviceRows orders the configure list by name rather than platform.
//
// Platform-first grouping scatters a vendor's boards across the list — Heltec
// alone spans esp32, esp32c3, esp32s3 and nrf52840 — and since the rows carry
// no group headers the ordering looks arbitrary, so a board you know exists
// reads as missing. Sorting by name keeps "Heltec V2/V3/V4" adjacent, which is
// how people actually look for hardware. The platform is still on every row.
func SortDeviceRows(rows []DeviceRow) {
	sort.Slice(rows, func(i, j int) bool {
		li, lj := strings.ToLower(rows[i].Name), strings.ToLower(rows[j].Name)
		if li != lj {
			return li < lj
		}
		return rows[i].ID < rows[j].ID
	})
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
