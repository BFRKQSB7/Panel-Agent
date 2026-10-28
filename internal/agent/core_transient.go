package agent

import "context"

// stopTransient is only used when a caller explicitly requests parent-bound
// cores for a disposable runner. Normal Agent shutdown retains Xray continuity.
func (m *CoreManager) stopTransient(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.Process == nil {
		return nil
	}
	boundary, err := m.captureBoundary(ctx, false)
	if err != nil {
		return err
	}
	if err := stopCoreProcess(ctx, *m.state.Process, m.binary); err != nil {
		return err
	}
	if err := m.finishBoundary(boundary, false); err != nil {
		return err
	}
	next := m.state
	next.Process = nil
	next.Phase = "stable"
	return m.save(next)
}
