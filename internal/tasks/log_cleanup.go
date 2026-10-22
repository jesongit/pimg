package tasks

// Edits wake scheduling; cleanup only runs at persisted next_run.
func (e *Engine) WakeLogCleanup() {
	select {
	case e.logCleanupWake <- struct{}{}:
	default:
	}
}
func (e *Engine) WakeSchedules() { e.WakeLogCleanup() }
