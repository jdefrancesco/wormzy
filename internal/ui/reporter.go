package ui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jdefrancesco/internal/transport"
)

// Reporter dispatches transport updates into the Bubble Tea program.
type Reporter struct {
	prog *tea.Program
}

// NewReporter returns a transport.Reporter that streams messages into prog.
func NewReporter(prog *tea.Program) *Reporter {
	return &Reporter{prog: prog}
}

func (r *Reporter) Logf(format string, args ...any) {
	if r == nil || r.prog == nil {
		return
	}
	r.prog.Send(logMsg{text: fmt.Sprintf(format, args...)})
}

func (r *Reporter) Stage(stage transport.Stage, state transport.StageState, detail string) {
	if r == nil || r.prog == nil {
		return
	}
	r.prog.Send(stageMsg{stage: stage, state: state, detail: detail})
}
