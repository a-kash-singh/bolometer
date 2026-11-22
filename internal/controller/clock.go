package controller

import "time"

// clock abstracts time so the monitoring schedules and cooldowns can be
// driven deterministically in tests. Production code uses realClock.
type clock interface {
	Now() time.Time
	NewTimer(d time.Duration) timer
}

// timer is the subset of *time.Timer used by the monitors.
type timer interface {
	C() <-chan time.Time
	Stop() bool
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) NewTimer(d time.Duration) timer { return realTimer{time.NewTimer(d)} }

type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time { return r.t.C }
func (r realTimer) Stop() bool          { return r.t.Stop() }
