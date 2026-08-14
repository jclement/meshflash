// Package nrfdfu implements the Nordic legacy serial DFU protocol spoken by
// the Adafruit nRF52 bootloader, which is what ships on the nRF52840 boards
// Meshtastic and MeshCore target (RAK4631, Heltec T114, GAT562, …).
//
// This is deliberately the *legacy* protocol (manifest dfu_version 0.5), not
// Nordic's Secure DFU. The two share a name and nothing else on the wire; the
// Adafruit bootloader only speaks the older one. Ported from
// Adafruit_nRF52_nrfutil's dfu_transport_serial.py.
package nrfdfu

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.bug.st/serial"
)

// Protocol timing. These are transcribed from Adafruit's transport and are not
// arbitrary: the bootloader blocks its CPU while erasing and writing flash and
// does not flow-control, so the host must simply wait it out.
const (
	// flashPageSize is the nRF52 flash page.
	flashPageSize = 4096
	// pageEraseTime is the worst-case erase for one page (nRF52840 ~85 ms).
	pageEraseTime = 89700 * time.Microsecond
	// wordWriteTime is the worst-case single-word write.
	wordWriteTime = 100 * time.Microsecond
	// pageWriteTime is how long a full page takes to write, and therefore how
	// long to pause after each 4 KB of data packets.
	pageWriteTime = (flashPageSize / 4) * wordWriteTime

	defaultBaudRate  = 115200
	ackTimeout       = 2 * time.Second
	portSettleTime   = 100 * time.Millisecond
	dtrResetWaitTime = 100 * time.Millisecond
	minEraseWait     = 500 * time.Millisecond
)

// Options configures a DFU session.
type Options struct {
	// Port is the serial device path (/dev/cu.usbmodem…, COM7).
	Port string
	// BaudRate defaults to 115200. The bootloader is fixed-rate; changing this
	// is only useful for unusual adapters.
	BaudRate int
	// TouchBaud, when non-zero, opens the port at that rate and immediately
	// closes it before starting DFU. Opening at 1200 baud is the conventional
	// signal to an Adafruit/Arduino USB stack to reboot into its bootloader,
	// which is how meshflash enters DFU without asking for a double-tap.
	//
	// Leave zero when the device is already in DFU mode; the transport then
	// falls back to toggling DTR.
	TouchBaud int
	// TouchSettle is how long to wait after touching for the bootloader to
	// enumerate. Defaults to 1.5s, matching adafruit-nrfutil.
	TouchSettle time.Duration
	// Logger receives protocol-level detail. Nil discards.
	Logger *slog.Logger
	// Progress, if set, is called with bytes sent and total bytes.
	Progress func(sent, total int)
}

func (o *Options) applyDefaults() {
	if o.BaudRate == 0 {
		o.BaudRate = defaultBaudRate
	}
	if o.TouchSettle == 0 {
		o.TouchSettle = 1500 * time.Millisecond
	}
	if o.Logger == nil {
		o.Logger = slog.New(discardHandler{})
	}
}

// ErrNoAck reports that the bootloader never answered a reliable packet. In
// the field this almost always means the device is not actually in DFU mode.
var ErrNoAck = errors.New("no acknowledgement from bootloader")

// Session is an open DFU conversation with one device.
type Session struct {
	opts Options
	port serial.Port
	seq  sequencer
	log  *slog.Logger

	// totalSize is the byte count declared in the start packet, which sets how
	// long the bootloader spends erasing and therefore how long we wait.
	totalSize int
	sdSize    uint32
	// singleBank is true when the bootloader has one application bank and so
	// skips the bank1→bank0 copy on activation.
	singleBank bool
}

// Open touches the device into DFU mode if requested and opens the port.
func Open(opts Options) (*Session, error) {
	opts.applyDefaults()
	s := &Session{opts: opts, log: opts.Logger.With("port", opts.Port)}

	if opts.TouchBaud > 0 {
		if err := s.touch(); err != nil {
			return nil, err
		}
	}

	port, err := serial.Open(opts.Port, &serial.Mode{BaudRate: opts.BaudRate})
	if err != nil {
		return nil, fmt.Errorf("open %s for DFU: %w", opts.Port, err)
	}
	s.port = port

	if err := port.SetReadTimeout(ackTimeout); err != nil {
		port.Close()
		return nil, fmt.Errorf("set read timeout: %w", err)
	}
	time.Sleep(portSettleTime)

	if opts.TouchBaud == 0 {
		// Without a touch, pulse DTR to reset the board into its bootloader.
		_ = port.SetDTR(false)
		time.Sleep(50 * time.Millisecond)
		_ = port.SetDTR(true)
		time.Sleep(dtrResetWaitTime)
	}

	_ = port.ResetInputBuffer()
	s.seq.reset()
	return s, nil
}

// touch opens and immediately closes the port at TouchBaud, then waits for the
// device to re-enumerate as its bootloader.
func (s *Session) touch() error {
	s.log.Debug("touching port to enter DFU", "baud", s.opts.TouchBaud)
	p, err := serial.Open(s.opts.Port, &serial.Mode{BaudRate: s.opts.TouchBaud})
	if err != nil {
		return fmt.Errorf("touch %s at %d baud: %w", s.opts.Port, s.opts.TouchBaud, err)
	}
	time.Sleep(portSettleTime)
	// Dropping DTR before close makes the reboot reliable on stacks that gate
	// the magic-baud reset on the line being deasserted.
	_ = p.SetDTR(false)
	if err := p.Close(); err != nil {
		return fmt.Errorf("close touch port: %w", err)
	}
	time.Sleep(s.opts.TouchSettle)
	return nil
}

// Close releases the serial port.
func (s *Session) Close() error {
	if s == nil || s.port == nil {
		return nil
	}
	err := s.port.Close()
	s.port = nil
	return err
}

// SetSingleBank declares that the target bootloader uses a single application
// bank, which shortens the post-activation wait.
func (s *Session) SetSingleBank(v bool) { s.singleBank = v }

// Program writes every image in a package and activates the result.
func (s *Session) Program(ctx context.Context, pkg *Package) error {
	if err := pkg.VerifyChecksums(); err != nil {
		return err
	}

	total := pkg.TotalBytes()
	sent := 0
	for _, img := range pkg.Images {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.log.Info("programming image", "image", img.Name, "bytes", len(img.Firmware))
		if err := s.programImage(ctx, img, &sent, total); err != nil {
			return fmt.Errorf("%s: %w", img.Name, err)
		}
	}
	return s.activate(ctx)
}

func (s *Session) programImage(ctx context.Context, img Image, sent *int, total int) error {
	var sd, bl, app uint32
	switch img.Mode {
	case ModeSoftDevice:
		sd = uint32(len(img.Firmware))
	case ModeBootloader:
		bl = uint32(len(img.Firmware))
	case ModeApplication:
		app = uint32(len(img.Firmware))
	case ModeSoftDevice | ModeBootloader:
		sd, bl = img.SoftDeviceSize, img.BootloaderSize
	default:
		return fmt.Errorf("unsupported DFU mode %d", img.Mode)
	}

	if err := s.sendStart(img.Mode, sd, bl, app); err != nil {
		return err
	}
	if err := s.sendInitPacket(img.InitPacket); err != nil {
		return err
	}
	return s.sendFirmware(ctx, img.Firmware, sent, total)
}

// sendStart declares image sizes, then waits for the bootloader to erase the
// destination bank. The device is unresponsive for the whole erase.
func (s *Session) sendStart(mode, sdSize, blSize, appSize uint32) error {
	payload := make([]byte, 0, 8+12)
	payload = append(payload, le32(opStartPacket)...)
	payload = append(payload, le32(mode)...)
	payload = append(payload, le32(sdSize)...)
	payload = append(payload, le32(blSize)...)
	payload = append(payload, le32(appSize)...)

	if err := s.sendPacket(payload); err != nil {
		return fmt.Errorf("start packet: %w", err)
	}

	s.sdSize = sdSize
	s.totalSize = int(sdSize) + int(blSize) + int(appSize)

	wait := s.eraseWait()
	s.log.Debug("waiting for flash erase", "duration", wait, "bytes", s.totalSize)
	time.Sleep(wait)
	return nil
}

// sendInitPacket forwards the .dat blob. The trailing uint16 padding is part
// of the legacy framing and the bootloader rejects the packet without it.
func (s *Session) sendInitPacket(init []byte) error {
	payload := make([]byte, 0, 4+len(init)+2)
	payload = append(payload, le32(opInitPacket)...)
	payload = append(payload, init...)
	payload = append(payload, 0x00, 0x00)

	if err := s.sendPacket(payload); err != nil {
		return fmt.Errorf("init packet: %w", err)
	}
	return nil
}

// sendFirmware streams the image in 512-byte data packets, pausing every 4 KB
// so the bootloader can commit a flash page, then closes with a stop packet.
func (s *Session) sendFirmware(ctx context.Context, firmware []byte, sent *int, total int) error {
	if s.opts.Progress != nil {
		s.opts.Progress(*sent, total)
	}

	for off, n := 0, 0; off < len(firmware); off, n = off+maxDFUPayload, n+1 {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(off+maxDFUPayload, len(firmware))

		payload := make([]byte, 0, 4+(end-off))
		payload = append(payload, le32(opDataPacket)...)
		payload = append(payload, firmware[off:end]...)

		if err := s.sendPacket(payload); err != nil {
			return fmt.Errorf("data packet at offset %d: %w", off, err)
		}

		*sent += end - off
		if s.opts.Progress != nil {
			s.opts.Progress(*sent, total)
		}

		// Every 8 packets is one 4 KB flash page. The bootloader stalls to
		// write it and will drop bytes arriving during that window.
		if n%8 == 0 {
			time.Sleep(pageWriteTime)
		}
	}

	time.Sleep(pageWriteTime) // let the final partial page commit

	if err := s.sendPacket(le32(opStopDataPacket)); err != nil {
		return fmt.Errorf("stop packet: %w", err)
	}
	return nil
}

// activate waits out the bootloader's swap of the staged image into bank 0.
// Reopening the port during this window causes a pin reset mid-copy, which is
// the classic way to brick a board with this bootloader.
func (s *Session) activate(ctx context.Context) error {
	wait := s.activateWait()
	s.log.Info("activating firmware", "wait", wait)

	// Release the port first so nothing else can disturb the device.
	if err := s.Close(); err != nil {
		s.log.Debug("closing port before activation", "error", err)
	}

	select {
	case <-time.After(wait):
		return nil
	case <-ctx.Done():
		// Do not report cancellation as failure here: the image is already
		// committed and interrupting only skips the remainder of a wait.
		s.log.Warn("activation wait interrupted; device may need a moment before it reappears")
		return nil
	}
}

// eraseWait scales with image size, one page-erase per 4 KB.
func (s *Session) eraseWait() time.Duration {
	pages := s.totalSize/flashPageSize + 1
	d := time.Duration(pages) * pageEraseTime
	return max(d, minEraseWait)
}

// activateWait covers erasing bank 0 and copying bank 1 over it.
func (s *Session) activateWait() time.Duration {
	if s.singleBank && s.sdSize == 0 {
		// Nothing to copy; only the bootloader settings page is rewritten.
		return pageEraseTime + pageWriteTime
	}
	pages := s.totalSize/flashPageSize + 1
	return s.eraseWait() + time.Duration(pages)*pageWriteTime
}

// sendPacket writes one reliable frame and waits for its acknowledgement,
// retrying on timeout.
func (s *Session) sendPacket(payload []byte) error {
	const attempts = 3

	seq, expectAck := s.seq.next()
	frame, err := buildFrame(seq, payload)
	if err != nil {
		return err
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if _, err := s.port.Write(frame); err != nil {
			return fmt.Errorf("write frame: %w", err)
		}

		ack, err := s.readAck()
		if err == nil {
			// Adafruit's own tool does not enforce the ack value and some
			// bootloader builds number these differently, so a mismatch is
			// recorded but not fatal. A missing ack is what actually matters.
			if ack != expectAck {
				s.log.Debug("unexpected DFU ack", "got", ack, "want", expectAck, "seq", seq)
			}
			return nil
		}
		lastErr = err
		s.log.Debug("retrying DFU packet", "seq", seq, "attempt", attempt, "error", err)
	}
	return fmt.Errorf("after %d attempts: %w", attempts, lastErr)
}

// readAck reads until a complete SLIP frame has arrived or the timeout fires.
func (s *Session) readAck() (uint8, error) {
	deadline := time.Now().Add(ackTimeout)
	buf := make([]byte, 0, 32)
	tmp := make([]byte, 32)

	for time.Now().Before(deadline) {
		n, err := s.port.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if frame, ok := extractFrame(buf); ok {
				decoded, err := slipUnescape(frame)
				if err != nil {
					return 0, err
				}
				return ackNumber(decoded)
			}
		}
		if err != nil {
			return 0, fmt.Errorf("read ack: %w", err)
		}
		if n == 0 {
			// Read timed out with nothing pending.
			break
		}
	}
	return 0, ErrNoAck
}

// extractFrame returns the bytes between the first two 0xC0 delimiters.
func extractFrame(buf []byte) ([]byte, bool) {
	start := -1
	for i, b := range buf {
		if b != slipEnd {
			continue
		}
		if start == -1 {
			start = i
			continue
		}
		if i > start+1 {
			return buf[start+1 : i], true
		}
		// Back-to-back delimiters: treat the second as a fresh frame start.
		start = i
	}
	return nil, false
}

// discardHandler is a no-op slog handler for when no logger is supplied.
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (discardHandler) WithAttrs([]slog.Attr) slog.Handler        { return discardHandler{} }
func (discardHandler) WithGroup(string) slog.Handler             { return discardHandler{} }
