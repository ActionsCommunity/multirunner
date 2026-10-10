package control

func CanTransition(from, to State) bool {
	if !from.Valid() || !to.Valid() || from.Terminal() {
		return false
	}
	switch from {
	case StateReceived:
		return to == StateValidated || to == StateFailed || to == StateCancelled
	case StateValidated:
		return to == StateAwaitingConfirmation || to == StateQueued ||
			to == StateFailed || to == StateCancelled
	case StateAwaitingConfirmation:
		return to == StateQueued || to == StateFailed || to == StateCancelled
	case StateQueued:
		return to == StateClaimed || to == StateCancelled || to == StateInterrupted
	case StateClaimed:
		return to == StateRunning || to == StateReconciling || to == StateFailed ||
			to == StateInterrupted || to == StateUnknownOutcome
	case StateRunning:
		return to == StateSucceeded || to == StateFailed || to == StateCancelled ||
			to == StateInterrupted || to == StateReconciling || to == StateUnknownOutcome
	case StateReconciling:
		return to == StateSucceeded || to == StateFailed || to == StateInterrupted ||
			to == StateUnknownOutcome || to == StateReconciling
	default:
		return false
	}
}
