package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/jclement/meshflash/internal/flash"
	"github.com/jclement/meshflash/internal/store"
	"github.com/jclement/meshflash/internal/theme"
)

// FlashJob is one board to write, as presented to the progress view.
type FlashJob struct {
	// Title is the board label, e.g. "RAK4631 on /dev/cu.usbmodem1101".
	Title string
	// Detail is the firmware being written.
	Detail string
	// Run performs the work. It reports progress through the supplied
	// callbacks and returns the result.
	Run func(ctx context.Context, onFlash flash.ProgressFunc, onStore store.ProgressFunc, onLog func(string)) (*flash.Result, error)
}

// JobOutcome is the recorded result of one job.
type JobOutcome struct {
	Title    string
	Result   *flash.Result
	Err      error
	Duration time.Duration
}

// Succeeded reports whether the job completed.
func (o JobOutcome) Succeeded() bool { return o.Err == nil }

// jobState tracks live progress for the running job.
type jobState struct {
	stage    string
	message  string
	current  int64
	total    int64
	logLines []string
}

// flashModel drives the progress screen.
type flashModel struct {
	jobs     []FlashJob
	index    int
	state    jobState
	outcomes []JobOutcome

	spin    spinner.Model
	width   int
	started time.Time

	ctx    context.Context
	cancel context.CancelFunc

	// updates and result belong to the currently running job. They are
	// recreated by runCurrent for each job and drained by waitCmd.
	updates chan jobState
	result  chan JobOutcome

	done     bool
	quitting bool
}

// maxLogLines bounds the scrollback shown under the progress bar. The full
// detail is always in the session log file; this pane is for reassurance.
const maxLogLines = 8

type progressMsg struct{ state jobState }
type jobDoneMsg struct {
	outcome JobOutcome
}
type allDoneMsg struct{}

// RunFlash presents a progress view for a sequence of flash jobs and returns
// their outcomes. It is safe to pass a single job.
func RunFlash(ctx context.Context, jobs []FlashJob) ([]JobOutcome, error) {
	if len(jobs) == 0 {
		return nil, nil
	}

	ctx, cancel := context.WithCancel(ctx)
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(theme.Colors().Accent)

	m := &flashModel{
		jobs:    jobs,
		spin:    sp,
		width:   80,
		started: time.Now(),
		ctx:     ctx,
		cancel:  cancel,
	}

	p := tea.NewProgram(m, tea.WithContext(ctx))
	final, err := p.Run()
	cancel()
	if err != nil {
		return nil, err
	}

	fm, ok := final.(*flashModel)
	if !ok {
		return nil, fmt.Errorf("unexpected final model")
	}
	return fm.outcomes, nil
}

func (m *flashModel) Init() tea.Cmd {
	return tea.Batch(m.spin.Tick, m.runCurrent())
}

// runCurrent launches the active job on a goroutine and funnels its progress
// back through the Bubble Tea message loop.
func (m *flashModel) runCurrent() tea.Cmd {
	if m.index >= len(m.jobs) {
		return func() tea.Msg { return allDoneMsg{} }
	}
	job := m.jobs[m.index]
	ctx := m.ctx

	// Bubble Tea delivers one message per command, so a long-running job needs
	// its updates pushed through a channel that waitCmd drains one at a time.
	updates := make(chan jobState, 64)
	result := make(chan JobOutcome, 1)
	m.updates, m.result = updates, result

	go func() {
		start := time.Now()
		st := jobState{}

		emit := func() {
			// Non-blocking: a stalled UI must never stall a flash in progress.
			select {
			case updates <- st:
			default:
			}
		}

		res, err := job.Run(ctx,
			func(p flash.Progress) {
				st.stage, st.message = p.Stage, p.Message
				st.current, st.total = p.Current, p.Total
				emit()
			},
			func(p store.Progress) {
				st.stage = p.Stage
				st.message = capitalize(p.Stage) + " " + p.Name
				st.current, st.total = p.Current, p.Total
				emit()
			},
			func(line string) {
				st.logLines = append(st.logLines, line)
				if len(st.logLines) > maxLogLines {
					st.logLines = st.logLines[len(st.logLines)-maxLogLines:]
				}
				emit()
			},
		)
		close(updates)
		result <- JobOutcome{Title: job.Title, Result: res, Err: err, Duration: time.Since(start)}
	}()

	return m.waitCmd()
}

// waitCmd blocks on the running job's channels and turns the next event into a
// message. Re-issued after every progressMsg so updates keep flowing until the
// update channel closes, at which point the outcome is delivered.
func (m *flashModel) waitCmd() tea.Cmd {
	updates, result := m.updates, m.result
	return func() tea.Msg {
		if st, ok := <-updates; ok {
			return progressMsg{state: st}
		}
		return jobDoneMsg{outcome: <-result}
	}
}

func (m *flashModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil

	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			// Cancelling mid-write can leave a half-written device, so the
			// cancel is signalled to the job rather than tearing down the UI.
			m.quitting = true
			m.cancel()
			if m.done {
				return m, tea.Quit
			}
			return m, nil
		}
		if m.done {
			return m, tea.Quit
		}
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd

	case progressMsg:
		m.state = msg.state
		return m, m.waitCmd()

	case jobDoneMsg:
		m.outcomes = append(m.outcomes, msg.outcome)
		m.index++
		m.state = jobState{}
		if m.index >= len(m.jobs) {
			return m, func() tea.Msg { return allDoneMsg{} }
		}
		return m, m.runCurrent()

	case allDoneMsg:
		m.done = true
		return m, tea.Quit
	}
	return m, nil
}

func (m *flashModel) View() tea.View {
	var b strings.Builder

	b.WriteString(Title().Render(" meshflash "))
	b.WriteString("  ")
	b.WriteString(Subtitle().Render(fmt.Sprintf("job %d of %d", min(m.index+1, len(m.jobs)), len(m.jobs))))
	b.WriteString("\n\n")

	for i, o := range m.outcomes {
		glyph := OK().Render(GlyphOK)
		detail := ""
		if o.Err != nil {
			glyph = Error().Render(GlyphFail)
			detail = Error().Render(" " + firstLine(o.Err.Error()))
		} else if o.Result != nil {
			detail = Muted().Render(fmt.Sprintf(" %s in %s", store.FormatBytes(o.Result.BytesWritten), o.Duration.Round(time.Millisecond)))
		}
		fmt.Fprintf(&b, "%s %s%s\n", glyph, m.jobs[i].Title, detail)
	}

	if !m.done && m.index < len(m.jobs) {
		job := m.jobs[m.index]
		b.WriteString("\n")
		fmt.Fprintf(&b, "%s %s\n", m.spin.View(), lipgloss.NewStyle().Bold(true).Render(job.Title))
		fmt.Fprintf(&b, "  %s\n", Muted().Render(job.Detail))

		msg := m.state.message
		if msg == "" {
			msg = "Starting…"
		}
		b.WriteString("\n  " + msg + "\n")

		width := clamp(m.width-20, 20, 60)
		pct := -1.0
		if m.state.total > 0 {
			pct = float64(m.state.current) / float64(m.state.total) * 100
		}
		b.WriteString("  " + Bar(width, pct))
		if pct >= 0 {
			fmt.Fprintf(&b, " %5.1f%%  %s", pct, Muted().Render(byteRatio(m.state.current, m.state.total)))
		}
		b.WriteString("\n")

		if len(m.state.logLines) > 0 {
			b.WriteString("\n")
			for _, l := range m.state.logLines {
				b.WriteString(Muted().Render("  │ "+Truncate(l, max(m.width-6, 20))) + "\n")
			}
		}
	}

	if m.quitting && !m.done {
		b.WriteString("\n" + Warn().Render("  Cancelling — waiting for the current step to stop safely…") + "\n")
	}

	b.WriteString("\n")
	if m.done {
		b.WriteString(Help().Render("Press any key to exit."))
	} else {
		b.WriteString(Help().Render("ctrl+c cancel"))
	}

	v := tea.NewView(b.String())
	// The alternate screen keeps this from smearing down the scrollback.
	// Inline rendering repaints in place only while the frame height is
	// stable, and this view grows as jobs complete and log lines arrive, so
	// each growth left the previous frame stranded above the new one. The
	// summary is reprinted after the program exits, so nothing is lost.
	v.AltScreen = true
	return v
}

// capitalize upper-cases the first letter of an ASCII stage name.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func byteRatio(cur, total int64) string {
	if total <= 0 {
		return ""
	}
	return fmt.Sprintf("%s / %s", store.FormatBytes(cur), store.FormatBytes(total))
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
