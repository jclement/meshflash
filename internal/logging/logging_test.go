package logging

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Muting the console must stop terminal writes without losing anything from
// the session log.
//
// The console and a full-screen view share one terminal. Any log line written
// while a view is repainting lands in the middle of a frame and corrupts it —
// which in practice looked like "board=HT-n5262mesh-node-t114er", two strings
// interleaved. The detail still has to reach the file, because that is where
// a failed field flash gets diagnosed from.
func TestMuteConsoleKeepsFileLogging(t *testing.T) {
	dir := t.TempDir()
	var console bytes.Buffer

	s, err := Setup(Options{
		Dir:          dir,
		ConsoleLevel: slog.LevelInfo,
		Console:      &console,
		NoColor:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.Logger.Info("before mute", "n", 1)

	s.MuteConsole(true)
	s.Logger.Info("during mute", "n", 2)
	s.Logger.Error("error during mute", "n", 3)
	// Loggers derived after muting must stay muted too.
	s.Logger.With("device", "rak4631").Info("derived during mute", "n", 4)

	s.MuteConsole(false)
	s.Logger.Info("after mute", "n", 5)

	out := console.String()
	for _, want := range []string{"before mute", "after mute"} {
		if !strings.Contains(out, want) {
			t.Errorf("console is missing %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"during mute", "error during mute", "derived during mute"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("console leaked %q while muted:\n%s", unwanted, out)
		}
	}

	// Everything, muted or not, must be in the file.
	data, err := os.ReadFile(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"before mute", "during mute", "error during mute", "derived during mute", "after mute",
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("session log is missing %q", want)
		}
	}
}

// A logger captured before muting shares the flag, so holding a reference does
// not smuggle output onto the screen.
func TestMuteAppliesToPreExistingDerivedLoggers(t *testing.T) {
	dir := t.TempDir()
	var console bytes.Buffer

	s, err := Setup(Options{Dir: dir, ConsoleLevel: slog.LevelInfo, Console: &console, NoColor: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// This is exactly what the flash layer does: derive once, log throughout.
	derived := s.Logger.With("port", "/dev/cu.usbmodem2101")

	s.MuteConsole(true)
	derived.Info("copying UF2 image", "bytes", 855552)
	s.MuteConsole(false)

	if strings.Contains(console.String(), "copying UF2 image") {
		t.Errorf("a logger derived before muting still wrote to the console:\n%s", console.String())
	}
}

func TestPruneKeepsNewestLogs(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"meshflash-20260101-000001.log",
		"meshflash-20260101-000002.log",
		"meshflash-20260101-000003.log",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	pruneLogs(dir, 2)

	got, _ := filepath.Glob(filepath.Join(dir, "meshflash-*.log"))
	if len(got) != 2 {
		t.Fatalf("kept %d logs, want 2: %v", len(got), got)
	}
	// The oldest is the one that should have gone.
	for _, p := range got {
		if strings.HasSuffix(p, "000001.log") {
			t.Error("pruning removed a newer log and kept the oldest")
		}
	}
}
