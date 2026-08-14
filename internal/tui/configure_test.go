package tui

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

// bigList is long enough to scroll, with a vendor whose boards span several
// platforms — the shape that exposed both the search and sorting bugs.
func bigList() []DeviceRow {
	rows := []DeviceRow{
		{ID: "heltec-v2", Name: "Heltec V2", Platform: "esp32"},
		{ID: "heltec-v3", Name: "Heltec V3", Platform: "esp32s3"},
		{ID: "heltec-v4", Name: "Heltec V4", Platform: "esp32s3"},
		{ID: "heltec-t114", Name: "Heltec T114", Platform: "nrf52840", Attached: true},
		{ID: "rak4631", Name: "RAK WisBlock 4631", Platform: "nrf52840", Attached: true},
		{ID: "tbeam", Name: "LILYGO T-Beam", Platform: "esp32"},
	}
	// Pad past a screenful so the scroll offset is non-zero in tests.
	for i := 0; i < 60; i++ {
		rows = append(rows, DeviceRow{
			ID:       "filler-" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
			Name:     "Zzz Filler " + string(rune('a'+i%26)) + string(rune('0'+i/26)),
			Platform: "esp32",
		})
	}
	SortDeviceRows(rows)
	return rows
}

func newModel(rows []DeviceRow) *configureModel {
	m := &configureModel{
		rows:     rows,
		selected: map[string]bool{},
		initial:  map[string]bool{},
		filter:   newTestInput(),
		width:    90,
		height:   24,
	}
	m.applyView()
	return m
}

func (m *configureModel) press(k tea.KeyPressMsg) *configureModel {
	next, _ := m.Update(k)
	return next.(*configureModel)
}

func (m *configureModel) type_(s string) *configureModel {
	out := m
	for _, r := range s {
		out = out.press(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	return out
}

func (m *configureModel) visibleNames() []string {
	var out []string
	for _, i := range m.visible {
		out = append(out, m.rows[i].Name)
	}
	return out
}

// TestSearchFindsMatchAfterScrolling is the regression test for "searching for
// V4 doesn't show Heltec V4".
//
// The filter itself was fine; the scroll offset was not reset when the result
// set shrank, so the render loop started past the end of it and drew nothing.
func TestSearchFindsMatchAfterScrolling(t *testing.T) {
	m := newModel(bigList())

	// Scroll well down the list first — this is what made it fail.
	for i := 0; i < 50; i++ {
		m = m.press(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	if m.offset == 0 {
		t.Fatal("precondition failed: expected to be scrolled down")
	}

	m = m.press(tea.KeyPressMsg{Code: '/', Text: "/"})
	m = m.type_("v4")

	names := m.visibleNames()
	if len(names) == 0 {
		t.Fatal(`searching "v4" matched nothing`)
	}
	found := false
	for _, n := range names {
		if n == "Heltec V4" {
			found = true
		}
	}
	if !found {
		t.Errorf(`searching "v4" gave %v, which does not include Heltec V4`, names)
	}

	// The rows must actually be on screen, not merely in the match set.
	if m.offset >= len(m.visible) {
		t.Errorf("offset %d is past the %d visible rows, so nothing would render",
			m.offset, len(m.visible))
	}
	if !strings.Contains(m.View().Content, "Heltec V4") {
		t.Error("Heltec V4 matched the filter but was not rendered")
	}
}

// Terms may be given in any order, so "v4 heltec" works as well as "heltec v4".
func TestSearchIsMultiTerm(t *testing.T) {
	for _, q := range []string{"heltec v4", "v4 heltec", "HELTEC V4"} {
		m := newModel(bigList())
		m = m.press(tea.KeyPressMsg{Code: '/', Text: "/"})
		m = m.type_(q)
		names := m.visibleNames()
		if len(names) != 1 || names[0] != "Heltec V4" {
			t.Errorf("search %q gave %v, want just Heltec V4", q, names)
		}
	}
}

func TestViewModeAttached(t *testing.T) {
	m := newModel(bigList())
	m = m.press(tea.KeyPressMsg{Code: 'd', Text: "d"})

	if m.mode != viewAttached {
		t.Fatalf("d set mode %v, want attached", m.mode)
	}
	names := m.visibleNames()
	if len(names) != 2 {
		t.Fatalf("attached view shows %v, want the 2 attached boards", names)
	}

	// d then a is the intended way to configure a kit from what is plugged in.
	m = m.press(tea.KeyPressMsg{Code: 'a', Text: "a"})
	if !m.selected["heltec-t114"] || !m.selected["rak4631"] {
		t.Error("`a` in the attached view should select exactly the attached boards")
	}
	if m.selected["heltec-v3"] {
		t.Error("`a` selected a board that is not attached")
	}
}

func TestViewModeSelectedAndCycle(t *testing.T) {
	m := newModel(bigList())
	m = m.press(tea.KeyPressMsg{Code: tea.KeySpace}) // select the first row

	m = m.press(tea.KeyPressMsg{Code: 's', Text: "s"})
	if m.mode != viewSelected {
		t.Fatalf("s set mode %v, want selected", m.mode)
	}
	if len(m.visible) != 1 {
		t.Errorf("selected view shows %v, want exactly the one selected board", m.visibleNames())
	}

	// Deselecting inside the selected view must drop the row immediately,
	// rather than leaving a stale entry the cursor can sit on.
	m = m.press(tea.KeyPressMsg{Code: tea.KeySpace})
	if len(m.visible) != 0 {
		t.Errorf("after deselecting, the selected view still shows %v", m.visibleNames())
	}

	// t cycles all -> attached -> selected -> all.
	m.mode = viewAll
	for _, want := range []viewMode{viewAttached, viewSelected, viewAll} {
		m = m.press(tea.KeyPressMsg{Code: 't', Text: "t"})
		if m.mode != want {
			t.Fatalf("t cycled to %v, want %v", m.mode, want)
		}
	}
}

// Escape must not throw away work silently.
func TestEscapePromptsWhenDirty(t *testing.T) {
	m := newModel(bigList())

	// Nothing changed: escape leaves straight away.
	m2 := m.press(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m2.confirming {
		t.Error("escape prompted even though nothing had changed")
	}
	if !m2.cancelled {
		t.Error("escape with no changes should just exit")
	}

	// Now make a change.
	m = newModel(bigList())
	m = m.press(tea.KeyPressMsg{Code: tea.KeySpace})
	if !m.dirty() {
		t.Fatal("selecting a board should mark the model dirty")
	}

	m = m.press(tea.KeyPressMsg{Code: tea.KeyEscape})
	if !m.confirming {
		t.Fatal("escape with unsaved changes should ask before discarding")
	}
	if m.cancelled {
		t.Error("the prompt must not have decided to discard already")
	}

	// esc at the prompt goes back to editing.
	m = m.press(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.confirming || m.cancelled {
		t.Error("esc at the prompt should return to editing, not exit")
	}

	// n discards.
	m = m.press(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = m.press(tea.KeyPressMsg{Code: 'n', Text: "n"})
	if !m.cancelled {
		t.Error("n at the prompt should discard and exit")
	}

	// y saves.
	m = newModel(bigList())
	m = m.press(tea.KeyPressMsg{Code: tea.KeySpace})
	m = m.press(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = m.press(tea.KeyPressMsg{Code: 'y', Text: "y"})
	if m.cancelled {
		t.Error("y at the prompt should save, not discard")
	}
}

// Enter is the unambiguous save, with no prompt in the way.
func TestEnterSavesWithoutPrompting(t *testing.T) {
	m := newModel(bigList())
	m = m.press(tea.KeyPressMsg{Code: tea.KeySpace})
	m = m.press(tea.KeyPressMsg{Code: tea.KeyEnter})

	if m.confirming {
		t.Error("enter should save directly, not raise the prompt")
	}
	if m.cancelled {
		t.Error("enter must not discard the selection")
	}
}

func TestDirtyTracksBothDirections(t *testing.T) {
	rows := bigList()
	m := &configureModel{
		rows:     rows,
		selected: map[string]bool{"heltec-v3": true},
		initial:  map[string]bool{"heltec-v3": true},
		filter:   newTestInput(),
		width:    90, height: 24,
	}
	m.applyView()

	if m.dirty() {
		t.Error("an untouched selection must not read as dirty")
	}

	// Deselecting an initially-selected board is a change too.
	delete(m.selected, "heltec-v3")
	if !m.dirty() {
		t.Error("removing a board should mark the model dirty")
	}
}

func newTestInput() textinput.Model {
	ti := textinput.New()
	ti.CharLimit = 40
	return ti
}
