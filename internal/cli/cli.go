// Package cli implements the meshflash command line.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/jclement/meshflash/internal/bindings"
	"github.com/jclement/meshflash/internal/buildinfo"
	"github.com/jclement/meshflash/internal/catalog"
	"github.com/jclement/meshflash/internal/config"
	"github.com/jclement/meshflash/internal/logging"
	"github.com/jclement/meshflash/internal/store"
	"github.com/jclement/meshflash/internal/theme"
	"github.com/jclement/meshflash/internal/tui"
)

// App carries state shared by every subcommand.
type App struct {
	Paths    config.Paths
	Cfg      config.Config
	Log      *slog.Logger
	Session  *logging.Session
	Store    *store.Store
	Bindings *bindings.Store

	// Catalog is nil when none is on disk yet; CatalogErr says why. Commands
	// that can work without it (doctor, upgrade) must tolerate this.
	Catalog    *catalog.Catalog
	CatalogErr error

	Out io.Writer
	Err io.Writer

	// Global flags.
	Verbose bool
	NoColor bool
	JSON    bool
	Offline bool
	Yes     bool
}

// Run parses arguments and dispatches, returning a process exit code.
func Run(ctx context.Context, args []string) int {
	app := &App{Out: os.Stdout, Err: os.Stderr}

	global := flag.NewFlagSet("meshflash", flag.ContinueOnError)
	global.SetOutput(io.Discard) // usage is printed by our own help
	var homeOverride string
	global.BoolVar(&app.Verbose, "verbose", false, "verbose logging")
	global.BoolVar(&app.Verbose, "v", false, "verbose logging")
	global.BoolVar(&app.NoColor, "no-color", false, "disable colour output")
	global.BoolVar(&app.JSON, "json", false, "machine-readable output where supported")
	global.BoolVar(&app.Offline, "offline", false, "never touch the network")
	global.BoolVar(&app.Yes, "yes", false, "assume yes for confirmations")
	global.BoolVar(&app.Yes, "y", false, "assume yes for confirmations")
	global.StringVar(&homeOverride, "home", "", "override the meshflash home directory")

	if err := global.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(app.Out)
			return 0
		}
		fmt.Fprintln(app.Err, "error:", err)
		return 2
	}

	rest := global.Args()
	if len(rest) == 0 {
		printUsage(app.Out)
		return 0
	}

	cmd, cmdArgs := rest[0], rest[1:]

	switch cmd {
	case "help", "-h", "--help":
		if len(cmdArgs) > 0 {
			printCommandHelp(app.Out, cmdArgs[0])
		} else {
			printUsage(app.Out)
		}
		return 0
	case "version", "--version":
		fmt.Fprintln(app.Out, buildinfo.String())
		return 0
	}

	if homeOverride != "" {
		os.Setenv("MESHFLASH_HOME", homeOverride)
	}
	if os.Getenv("NO_COLOR") != "" {
		app.NoColor = true
	}
	// Settle colour before anything renders. Without an explicit --no-color the
	// theme auto-detects, which also covers piping to a file or a pager.
	if app.NoColor {
		theme.SetColor(false)
	}

	if err := app.init(); err != nil {
		fmt.Fprintln(app.Err, "error:", err)
		return 1
	}
	defer app.Session.Close()

	var err error
	switch cmd {
	case "update":
		err = app.cmdUpdate(ctx, cmdArgs)
	case "upgrade":
		err = app.cmdUpgrade(ctx, cmdArgs)
	case "doctor":
		err = app.cmdDoctor(ctx, cmdArgs)
	case "configure":
		err = app.cmdConfigure(ctx, cmdArgs)
	case "flash":
		err = app.cmdFlash(ctx, cmdArgs)
	case "auto":
		err = app.cmdAuto(ctx, cmdArgs)
	case "devices":
		err = app.cmdDevices(ctx, cmdArgs)
	default:
		fmt.Fprintf(app.Err, "unknown command %q\n\n", cmd)
		printUsage(app.Err)
		return 2
	}

	if err != nil {
		if errors.Is(err, context.Canceled) || errors.As(err, new(tui.ErrCancelled)) {
			fmt.Fprintln(app.Err, "cancelled")
			return 130
		}
		fmt.Fprintln(app.Err)
		fmt.Fprintln(app.Err, tui.Error().Render("error: ")+err.Error())
		if app.Session != nil && app.Session.Path != "" {
			fmt.Fprintln(app.Err, tui.Muted().Render("full log: "+app.Session.Path))
		}
		return 1
	}
	return 0
}

// init prepares paths, logging, config, catalog and stores.
func (a *App) init() error {
	paths, err := config.Discover()
	if err != nil {
		return err
	}
	a.Paths = paths
	if err := paths.EnsureDirs(); err != nil {
		return err
	}

	level := slog.LevelInfo
	if a.Verbose {
		level = slog.LevelDebug
	}
	// Log lines go to stderr so stdout stays clean for --json consumers.
	session, err := logging.Setup(logging.Options{
		Dir:          paths.LogDir(),
		ConsoleLevel: level,
		Console:      a.Err,
		NoColor:      a.NoColor,
	})
	if err != nil {
		return err
	}
	a.Session = session
	a.Log = session.Logger.With("version", buildinfo.Version)

	if a.Cfg, err = config.Load(paths); err != nil {
		return err
	}

	a.Store = store.New(paths, a.Log)
	a.Store.Offline = a.Offline
	a.Store.UserAgent = buildinfo.UserAgent()

	if a.Bindings, err = bindings.Load(paths); err != nil {
		return err
	}

	// A missing catalog is normal on first run and must not be fatal: doctor
	// and upgrade both need to work before anything has been fetched.
	if cat, err := catalog.Load(paths.CatalogFile()); err != nil {
		a.CatalogErr = err
	} else {
		a.Catalog = cat
	}

	return nil
}

// requireCatalog returns the catalog or a message telling the operator how to
// get one.
func (a *App) requireCatalog() (*catalog.Catalog, error) {
	if a.Catalog != nil {
		return a.Catalog, nil
	}
	if os.IsNotExist(errors.Unwrap(a.CatalogErr)) || strings.Contains(fmt.Sprint(a.CatalogErr), "no such file") {
		return nil, fmt.Errorf("no firmware catalog yet — run `meshflash update` while online")
	}
	return nil, fmt.Errorf("could not load the firmware catalog: %w", a.CatalogErr)
}

// confirm asks a yes/no question unless --yes was passed.
func (a *App) confirm(prompt string) bool {
	if a.Yes {
		return true
	}
	fmt.Fprintf(a.Out, "%s [y/N] ", prompt)
	var answer string
	if _, err := fmt.Fscanln(os.Stdin, &answer); err != nil {
		return false
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `meshflash — flash Meshtastic and MeshCore firmware in the field

Usage:
  meshflash [flags] <command> [args]

Commands:
  auto        Flash every attached board with whatever it had last time
  flash       Choose a board and firmware, and write it
  doctor      Show attached devices and diagnose drivers and permissions
  configure   Choose which boards to keep firmware for
  update      Refresh the firmware catalog and cache (needs network)
  upgrade     Update meshflash itself (needs network)
  devices     List and manage remembered boards
  version     Print the version

Flags:
  -v, --verbose     verbose logging
      --no-color    disable colour
      --json        machine-readable output where supported
      --offline     never touch the network
  -y, --yes         assume yes for confirmations
      --home DIR    override the meshflash home directory

Run "meshflash help <command>" for details on a command.
`)
}

func printCommandHelp(w io.Writer, cmd string) {
	help := map[string]string{
		"auto": `meshflash auto — flash every attached board with what it had last time

Looks at each connected board, matches it against the boards meshflash has
flashed before (by USB serial number or ESP32 MAC), and writes the newest
cached firmware of the same project and variant.

Boards meshflash has never seen are reported and skipped, unless they identify
themselves — a UF2 bootloader publishes its own Board-ID, which is enough.

Usage:
  meshflash auto [flags]

Flags:
  --probe          read the ESP32 MAC of unrecognised boards to identify them
                   (resets the board; needed for CH340 boards with no serial)
  --version VER    flash a specific version instead of the newest cached
  --erase          full chip erase first (ESP32 only; wipes node identity)
  --dry-run        show what would be flashed and stop
`,
		"flash": `meshflash flash — choose a board and firmware, and write it

With no arguments this is interactive: pick the target, then the firmware.
Supply flags to skip the prompts and script it.

Usage:
  meshflash flash [flags]

Flags:
  --port NAME        serial port or bootloader volume to write
  --device ID        catalog device id (e.g. rak4631)
  --project ID       meshtastic or meshcore
  --variant NAME     build variant (e.g. companion_radio_ble)
  --version VER      firmware version; defaults to the newest cached
  --erase            full chip erase first (ESP32 only)
  --verify           read back and compare after writing
  --no-auto-bootloader
                     do not reboot the board into its bootloader automatically
  --remember=false   do not record this board for ` + "`meshflash auto`" + `
`,
		"doctor": `meshflash doctor — show what is attached and what is wrong

Lists connected boards, whether each identified itself, and the state of the
catalog and firmware cache. Diagnoses the usual failures: missing USB-UART
drivers on Windows, missing dialout group on Linux, and empty caches.

Usage:
  meshflash doctor [flags]

Flags:
  --drivers    list the drivers worth pre-installing on a field machine
  --json       machine-readable output
`,
		"configure": `meshflash configure — choose which boards to keep firmware for

A full Meshtastic release is a few hundred megabytes of platform archives.
Selecting the boards you actually carry keeps the offline cache small.

Usage:
  meshflash configure [flags]

Flags:
  --add ID,ID      add devices without opening the interactive picker
  --remove ID,ID   remove devices
  --list           print the current selection and exit
`,
		"update": `meshflash update — refresh the firmware catalog and cache

Fetches the catalog, then downloads and extracts firmware for your selected
boards. Run this while you have a network; everything after works offline.

Usage:
  meshflash update [flags]

Flags:
  --catalog-only   fetch the catalog but download no firmware
  --prune          delete cached firmware no longer selected
  --force          re-download even when already cached
`,
		"upgrade": `meshflash upgrade — update meshflash itself

Downloads the newest release for this platform, verifies its checksum, and
replaces the running binary.

Usage:
  meshflash upgrade [flags]

Flags:
  --check    report whether an update exists and exit
`,
		"devices": `meshflash devices — list and manage remembered boards

Every successful flash records the board's fingerprint and what was written to
it, which is what lets ` + "`meshflash auto`" + ` work.

Usage:
  meshflash devices [list]
  meshflash devices name <fingerprint> <nickname>
  meshflash devices forget <fingerprint>
`,
	}

	if text, ok := help[cmd]; ok {
		fmt.Fprint(w, text)
		return
	}
	fmt.Fprintf(w, "no help for %q\n", cmd)
}
