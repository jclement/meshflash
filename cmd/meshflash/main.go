// Command meshflash flashes Meshtastic and MeshCore firmware onto LoRa boards,
// including from a machine that has been offline since the last `update`.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/jclement/meshflash/internal/cli"
)

func main() {
	// Interrupts are delivered to the command as a context cancellation so a
	// flash in progress can stop at a safe point rather than being killed
	// mid-write. A second interrupt takes the default action and kills us.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	os.Exit(cli.Run(ctx, os.Args[1:]))
}
