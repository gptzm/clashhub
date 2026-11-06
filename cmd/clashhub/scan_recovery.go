package main

import "time"

// Only a committed, freshly healthy ordinary selector may shorten backoff.
// This schedules the normal recovery path; it never switches routes itself.
func (a *App) recoverAfterHealthyScan(engineGeneration, routingGeneration uint64, selection ...uint64) {
	a.engineMu.Lock()
	a.routingGate.RLock()
	if (len(selection) > 0 && selection[0] != a.selectionGeneration) || a.engineGeneration != engineGeneration || !a.routingProbeValidLocked(routingGeneration) || a.shuttingDown.Load() {
		a.routingGate.RUnlock()
		a.engineMu.Unlock()
		return
	}
	a.mu.Lock()
	wake := a.mode == ModeFallback && !a.nextRecovery.IsZero()
	if wake {
		earliest := a.lastSwitch.Add(time.Duration(a.cfg.RecoveryInitSec) * time.Second)
		if now := time.Now(); earliest.Before(now) {
			earliest = now
		}
		if earliest.Before(a.nextRecovery) {
			a.nextRecovery = earliest
		}
	}
	a.mu.Unlock()
	a.routingGate.RUnlock()
	a.engineMu.Unlock()
	if wake {
		go a.attemptRecovery()
	}
}
