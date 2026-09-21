// checkin.go owns the plugin's single scheduler loop and the per-account
// auth-operation mutexes.
//
// The scheduler wakes the keepalive cadence (token refresh at 22:00 local).
// v0.8.7: daily check-in was removed with the switch to the enterprise
// (CodeBuddy 企业版) usage model — enterprise quotas are granted by the org,
// not by personal check-in, so there is nothing to schedule at 09:00/21:00.
// The loop is retained because token keepalive still needs a daily tick.
package main

import (
	"sync"
	"time"
)

var (
	schedulerStop chan struct{}
	schedulerMu   sync.Mutex
)

func ensureScheduler() {
	schedulerMu.Lock()
	defer schedulerMu.Unlock()
	if schedulerStop != nil {
		return // already running
	}
	schedulerStop = make(chan struct{})
	go schedulerLoop(schedulerStop)
}

// Note: there is deliberately no stopScheduler. The plugin shutdown
// export is a no-op (see cliproxyPluginShutdown) because the host invokes it
// during its own runtime teardown, where touching Go sync primitives from the
// plugin's c-shared runtime caused SIGSEGV on every restart.

func schedulerLoop(stop chan struct{}) {
	for {
		next := nextScheduledTime(time.Now())
		// Personal check-in tick (09:00/21:00) shares the single scheduler
		// loop: wake for whichever fires first. Enterprise keepalive path
		// below is unchanged.
		if cn := nextPersonalCheckinTime(time.Now()); cn.Before(next) {
			next = cn
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-stop:
			timer.Stop()
			return
		case <-timer.C:
			// Personal daily check-in (CN personal only; enterprise/Global
			// skip internally). Gated by checkin_auto.
			if scheduledInCurrentHour(time.Now(), checkinHours) {
				runAutoCheckin()
			}
			// 22:00 local: token keepalive (runs the reconcile lifecycle too
			// so exhausted enterprise quotas are disabled/deleted).
			if shouldRunKeepaliveNow(time.Now()) {
				runTokenKeepalive()
			}
			if lifecycleEnabled() {
				reconcileAllAccounts(true)
			}
		}
	}
}

// nextScheduledTime returns the next keepalive slot (22:00 local). Personal
// check-in hours (09:00/21:00, see personal.go nextPersonalCheckinTime) share
// the same schedulerLoop and are merged at wake-up time.
func nextScheduledTime(now time.Time) time.Time {
	var earliest time.Time
	for _, h := range keepaliveHours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour) // slot already passed today → tomorrow
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// -----------------------------------------------------------------------------
// Per-account auth-operation locks
// -----------------------------------------------------------------------------

// authLockFor returns the per-account mutex serializing auth-file mutations
// (disable/reenable/delete). Entries are pruned during dashboard prune to
// avoid unbounded growth when auth accounts are deleted/rotated.
var authLocks sync.Map // auth_index -> *sync.Mutex

func authLockFor(authIndex string) *sync.Mutex {
	v, _ := authLocks.LoadOrStore(authIndex, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// pruneAuthLocks removes lock entries for auth indices that no longer
// exist in hostAuthList. Call after dashboard prune.
// Lock keys are auth_index (used for host RPC), so live map needs auth_index too.
func pruneAuthLocks() {
	files, err := hostAuthList()
	if err != nil {
		return
	}
	live := make(map[string]struct{}, len(files))
	for _, f := range files {
		live[f.ID] = struct{}{}
		live[f.AuthIndex] = struct{}{} // authLockFor uses auth_index as key
	}
	authLocks.Range(func(key, _ any) bool {
		idx, _ := key.(string)
		if _, ok := live[idx]; !ok {
			authLocks.Delete(key)
		}
		return true
	})
}
