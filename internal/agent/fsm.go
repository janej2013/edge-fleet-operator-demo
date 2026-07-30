package agent

import "fmt"

// State is one step of the firmware-upgrade transaction. The whole package
// deliberately imports nothing but the standard library: the agent must ship
// as one small static binary onto constrained edge hardware, and every
// dependency is attack surface + flash footprint there.
type State string

const (
	StateIdle        State = "Idle"
	StateDownloading State = "Downloading"
	StateVerifying   State = "Verifying"
	StateFlashing    State = "Flashing"
	StateConfirming  State = "Confirming"
	StateFailed      State = "Failed"
)

// validTransitions makes the state graph *data*, not control flow buried in
// ifs: the compiler can't check it, but a table-driven test can, and an
// illegal jump (e.g. Idle→Flashing skipping verification) fails loudly at the
// exact line that attempted it instead of corrupting a device.
var validTransitions = map[State][]State{
	StateIdle:        {StateDownloading},
	StateDownloading: {StateVerifying, StateFailed},
	StateVerifying:   {StateFlashing, StateFailed},
	StateFlashing:    {StateConfirming, StateFailed},
	StateConfirming:  {StateIdle, StateFailed},
	// Failed is recoverable: a *new* target from the CR restarts the cycle.
	StateFailed: {StateDownloading},
}

// FSM is a minimal explicit state machine. Not goroutine-safe by design: the
// single-flight guard in Agent guarantees exactly one upgrade transaction
// drives it at a time, so a mutex here would only hide guard bugs.
type FSM struct {
	state State
	// onTransition is the observability hook: every state change is one
	// structured log line, which is how you debug a fleet you can't ssh into.
	onTransition func(from, to State)
}

func NewFSM(onTransition func(from, to State)) *FSM {
	return &FSM{state: StateIdle, onTransition: onTransition}
}

func (f *FSM) State() State { return f.state }

// To moves to next if the transition table allows it.
func (f *FSM) To(next State) error {
	for _, allowed := range validTransitions[f.state] {
		if next == allowed {
			if f.onTransition != nil {
				f.onTransition(f.state, next)
			}
			f.state = next
			return nil
		}
	}
	return fmt.Errorf("illegal state transition %s -> %s", f.state, next)
}
