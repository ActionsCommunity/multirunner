package control

import "testing"

func TestCommandStateTransitions(t *testing.T) {
	valid := map[State][]State{
		StateReceived:             {StateValidated, StateFailed, StateCancelled},
		StateValidated:            {StateAwaitingConfirmation, StateQueued, StateFailed, StateCancelled},
		StateAwaitingConfirmation: {StateQueued, StateFailed, StateCancelled},
		StateQueued:               {StateClaimed, StateCancelled, StateInterrupted},
		StateClaimed:              {StateRunning, StateReconciling, StateFailed, StateInterrupted, StateUnknownOutcome},
		StateRunning:              {StateSucceeded, StateFailed, StateCancelled, StateInterrupted, StateReconciling, StateUnknownOutcome},
		StateReconciling:          {StateReconciling, StateSucceeded, StateFailed, StateInterrupted, StateUnknownOutcome},
	}
	states := []State{
		StateReceived, StateValidated, StateAwaitingConfirmation, StateQueued,
		StateClaimed, StateRunning, StateReconciling, StateSucceeded, StateFailed,
		StateCancelled, StateInterrupted, StateUnknownOutcome,
	}
	for _, from := range states {
		for _, to := range states {
			want := containsState(valid[from], to)
			if got := CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%q, %q) = %v, want %v", from, to, got, want)
			}
		}
	}
}

func containsState(states []State, target State) bool {
	for _, state := range states {
		if state == target {
			return true
		}
	}
	return false
}
