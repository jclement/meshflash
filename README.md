# meshflash

Flash [Meshtastic](https://meshtastic.org) and [MeshCore](https://meshcore.co.uk)
firmware onto LoRa boards — including from a laptop that has been offline since
the last time it saw a network.

Built for field work: load a machine up while you have connectivity, then take
it somewhere without any and keep flashing.

```
meshflash auto          flash every attached board with what it had last time
meshflash flash         choose a board and firmware, and write it
meshflash doctor        show what's attached, diagnose drivers and permissions
meshflash configure     choose which boards to keep firmware for
meshflash update        refresh the firmware catalog and cache (needs network)
meshflash upgrade       update meshflash itself (needs network)
meshflash devices       list and manage remembered boards
```

## Why it works offline

meshflash never scrapes upstream at flash time. A scheduled CI job resolves
Meshtastic and MeshCore releases into a single normalised `catalog.json` and
publishes it as a release asset. `meshflash update` fetches that, downloads the
firmware for the boards you selected, and extracts what it needs.

That indirection is the point. When an upstream renames an asset, the generator
breaks in CI where it gets fixed once — not on a Toughbook in a field.

Upstream ships firmware in large per-platform archives (one Meshtastic release
is a few hundred megabytes; the esp32s3 archive alone is 170 MB). meshflash
downloads an archive once, extracts only the images your boards need, and
`update --prune` throws the archive away:

```
✓ pruned 48.6 MB (2 archives)
• cache: 5.6 MB firmware, 0 B source archives
```

That 5.6 MB is fully flashable with no network.

## `meshflash auto`

Identifying a board model from USB is mostly impossible. Nearly all these
boards expose a generic USB-UART bridge — a CP2102 or CH340 — whose VID/PID
identifies the bridge, not the board. Nothing distinguishes a Heltec V3 from a
T-Beam.

Board *identity* is a different and much easier problem. meshflash fingerprints
each board and remembers what it wrote:

| Source | Quality | Cost |
|---|---|---|
| ESP32 eFuse base MAC | unique, permanent, survives a full erase | one bootloader handshake |
| USB `iSerialNumber` | unique on native-USB parts (nRF52840, ESP32-S3, RP2040) | free during enumeration |
| `INFO_UF2.TXT` `Board-ID` | names the board model exactly | free when in a UF2 bootloader |

So you answer "which board is this?" once per physical board, ever. After that:

```console
$ meshflash auto
Will flash 3 board(s)
  RAK WisBlock 4631 (rak4631)
    target       RAK4631 (UF2 bootloader at /Volumes/RAK4631)
    firmware     MeshCore 1.17.1
    variant      companion_radio_ble
    why          remembered board (usb:d8f3a1029b44), last flashed 1.17.0
  ...
```

Boards it has never seen are reported and skipped rather than guessed at.
`meshflash auto --probe` will read the eFuse MAC of unrecognised ESP32 boards
to identify them, which costs a reset and so stays opt-in.

Name them if you like:

```console
$ meshflash devices name usb:d8f3a1029b44 "north tower repeater"
```

## Auto-DFU

You should not have to double-tap a reset button. meshflash opens the port at
1200 baud and closes it, which is the conventional signal for an
Adafruit/Arduino USB stack to reboot into its bootloader, then waits for the
volume to mount. If that does not take, it asks for the double-tap and keeps
watching, so a manual entry is still picked up.

For ESP32 it uses `ResetAuto`, trying the classic DTR/RTS sequence and then the
USB-JTAG one, since boards in this space use both.

If a UF2 bootloader never mounts — automount disabled, headless machine —
meshflash falls back to serial DFU when the build ships a package.

## The wipe is optional, and off by default

Upstream ships two forms of the same firmware and its own scripts treat them
very differently:

- **application image at `0x10000`** (`device-update.sh`). Only the app
  partition is touched. NVS at `0x9000` survives, so the node keeps its private
  key, region and channels. **This is the default.**
- **factory image at `0x0`** (`device-install.sh`, after `erase_flash`). It
  spans NVS, so the node comes back with a new identity.

Pass `--erase` for the second. meshflash spells out the consequence first:

> This wipes NVS along with the firmware. On Meshtastic that destroys the
> node's private key: it comes back with a new identity, remote admin stops
> working, and encrypted direct messages to it fail until peers re-learn it.

Flash offsets are never hardcoded. They come from the `.mt.json` manifest
shipped inside each Meshtastic archive, so per-board partition layouts are
correct — the littlefs partition sits at `0x300000` on a T-Lora C6 and
`0x670000` on a Heltec V3.

## Supported hardware

| Family | Method | Notes |
|---|---|---|
| ESP32, S2, S3, C3, C6, H2 | serial bootloader | via [`tinygo.org/x/espflasher`](https://github.com/tinygo-org/espflasher) |
| nRF52840 | UF2 mass storage | primary path; auto-entered |
| nRF52840 | Nordic legacy serial DFU | fallback; no mass storage needed |
| RP2040 / RP2350 | UF2 mass storage | BOOTSEL |

The serial DFU implementation is a port of `adafruit-nrfutil`'s transport —
SLIP/HCI framing, CRC-16, the legacy `dfu_version 0.5` protocol the Adafruit
nRF52 bootloader speaks (*not* Nordic Secure DFU, which shares the name and
nothing else on the wire). Its frame encoder is pinned by golden vectors
generated from the reference Python implementation.

## Install

Grab a build from [releases](https://github.com/jclement/meshflash/releases), or:

```console
$ go install github.com/jclement/meshflash/cmd/meshflash@latest
```

Then:

```console
$ meshflash update        # while online
$ meshflash configure     # pick the boards you carry
$ meshflash update        # cache their firmware
$ meshflash doctor        # confirm you're ready
```

`meshflash upgrade` updates the binary in place, verifying the release
checksum first.

## Setting up a field machine

Do this while you still have a network:

1. `meshflash configure` — select only the boards you carry. Selecting
   everything means hundreds of megabytes.
2. `meshflash update --prune` — cache firmware, discard source archives.
3. `meshflash doctor --drivers` — install the USB-UART drivers. **On Windows a
   board with no driver never appears as a serial port at all**, which is the
   single largest source of "it doesn't show up" in the field. CH340 is the
   usual culprit.
4. On Linux, add yourself to `dialout` (`doctor` checks this).
5. Flash one of each board type so `auto` learns them.

Everything lives under one relocatable directory (`~/.meshflash`, or
`MESHFLASH_HOME`), so a prepared kit copies to another machine as a plain
directory copy.

## Logging

Every run writes a debug-level session log to `~/.meshflash/logs/`, retaining
the last 20. The console shows what you need; the file has everything,
including full protocol traces. Failures print the log path.

## Layout

```
cmd/meshflash            entry point
internal/catalog         normalised firmware index (schema + loader)
internal/store           download, extract, verify, prune
internal/device          serial enumeration, UF2 volumes, 1200-baud touch
internal/fingerprint     per-board identity (eFuse MAC, USB serial)
internal/bindings        what was last flashed to which board
internal/plan            resolves board + catalog + bindings into a flash
internal/flash           esp32 / uf2 / nrf-dfu drivers
internal/doctor          diagnostics and driver links
internal/tui             Bubble Tea views
tools/catalog-gen        builds catalog.json from upstream (CI only)
```

## Development

Tooling is managed with [mise](https://mise.jdx.dev):

```console
$ mise install          # go + goreleaser
$ mise run dev          # build and run against an isolated ./.meshflash-dev home
$ mise run dev flash    # ...with arguments
$ mise run test         # go test -race ./...
$ mise run check        # lint, test, and a cross-compile sweep
$ mise run snapshot     # build release archives locally, no tag, no publish
$ mise run catalog      # regenerate catalog.json from upstream
```

`mise run dev` runs the built binary rather than `go run`, so Ctrl-C reaches
meshflash — which matters when a flash is in progress and has to stop at a safe
point.

### Releasing

```console
$ mise run release          # prompts for patch / minor / major
$ mise run release minor    # or say which
```

That validates the tree is clean and on `main`, shows the commits since the
last tag, then tags and pushes. Pushing the tag is the only trigger: the
`release` workflow runs GoReleaser, which builds all seven targets and
publishes the archives with a `checksums.txt`.

Releases build on a **macOS** runner, which looks odd but is the only single
runner that can produce everything: `go.bug.st/serial`'s port enumerator binds
IOKit, so darwin needs cgo and a macOS toolchain, while Linux and Windows are
pure Go and cross-compile from there. Linux cannot build darwin; macOS can
build both.

Archive names are load-bearing — `meshflash upgrade` looks up
`meshflash_<version>_<goos>_<goarch>` and `checksums.txt` by exact name. A test
and a CI step both pin that contract against `.goreleaser.yaml`.

## License

MIT
