package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestKeyNames pins the strings the views switch on.
//
// This exists because Bubble Tea v2 renamed the space key from " " to "space",
// which silently broke selection in the configure list: the help text still
// said "space toggle" and pressing space simply did nothing. Nothing in the
// compiler or the test suite caught it, because a key switch that matches no
// case is perfectly valid code.
func TestKeyNames(t *testing.T) {
	cases := []struct {
		key  tea.KeyPressMsg
		want string
	}{
		{tea.KeyPressMsg{Code: tea.KeySpace}, "space"},
		{tea.KeyPressMsg{Code: tea.KeyEnter}, "enter"},
		{tea.KeyPressMsg{Code: tea.KeyEscape}, "esc"},
		{tea.KeyPressMsg{Code: tea.KeyUp}, "up"},
		{tea.KeyPressMsg{Code: tea.KeyDown}, "down"},
		{tea.KeyPressMsg{Code: tea.KeyPgUp}, "pgup"},
		{tea.KeyPressMsg{Code: tea.KeyPgDown}, "pgdown"},
		{tea.KeyPressMsg{Code: tea.KeyHome}, "home"},
		{tea.KeyPressMsg{Code: tea.KeyEnd}, "end"},
		{tea.KeyPressMsg{Code: 'q'}, "q"},
		{tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}, "ctrl+c"},
	}

	for _, c := range cases {
		if got := c.key.String(); got != c.want {
			t.Errorf("key %v stringifies as %q, but the views switch on %q",
				c.key.Code, got, c.want)
		}
	}
}

// TestConfigureTogglesOnSpace drives the model the way a terminal would, so a
// key rename breaks a test rather than a field kit.
func TestConfigureTogglesOnSpace(t *testing.T) {
	rows := []DeviceRow{
		{ID: "heltec-v3", Name: "Heltec V3", Platform: "esp32s3"},
		{ID: "rak4631", Name: "RAK WisBlock 4631", Platform: "nrf52840"},
	}
	m := &configureModel{rows: rows, selected: map[string]bool{}, width: 90, height: 24}
	m.applyFilter()

	press := func(k tea.KeyPressMsg) {
		next, _ := m.Update(k)
		m = next.(*configureModel)
	}

	press(tea.KeyPressMsg{Code: tea.KeySpace})
	if !m.selected["heltec-v3"] {
		t.Fatal("space did not select the row under the cursor")
	}

	press(tea.KeyPressMsg{Code: tea.KeySpace})
	if m.selected["heltec-v3"] {
		t.Fatal("space did not deselect on the second press")
	}

	// Move down, then select, to confirm the cursor is what gets toggled.
	press(tea.KeyPressMsg{Code: tea.KeyDown})
	press(tea.KeyPressMsg{Code: tea.KeySpace})
	if !m.selected["rak4631"] {
		t.Error("space did not select the second row after moving down")
	}
	if m.selected["heltec-v3"] {
		t.Error("moving the cursor should not have selected the first row")
	}
}

// Sorting by name is what makes a vendor's boards findable; platform-first
// grouping scatters them with no visible headers.
func TestSortDeviceRowsKeepsVendorsTogether(t *testing.T) {
	// Deliberately spread across platforms: under the old platform-first sort
	// these three Heltec boards ended up in three different parts of the list.
	rows := []DeviceRow{
		{ID: "rak4631", Name: "RAK WisBlock 4631", Platform: "nrf52840"},
		{ID: "heltec-v3", Name: "Heltec V3", Platform: "esp32s3"},
		{ID: "heltec-t114", Name: "Heltec T114", Platform: "nrf52840"},
		{ID: "heltec-v2", Name: "Heltec V2", Platform: "esp32"},
	}
	SortDeviceRows(rows)

	var names []string
	for _, r := range rows {
		names = append(names, r.Name)
	}
	want := []string{"Heltec T114", "Heltec V2", "Heltec V3", "RAK WisBlock 4631"}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("sorted %v,\n    want %v (a vendor's boards must stay adjacent)", names, want)
		}
	}
}
