package funding

import (
	"fmt"
	"time"
)

// EXIT LIQUIDITY WATCH
//
// THE BUG THIS FIXES
//
// Position.NetBps() computes an open position's net as:
//
//	funding - entryCost - entryCost      // exit ESTIMATED as symmetric
//
// That estimate is made once, at entry, and never revisited. So if a symbol's
// book thins and the real cost of getting out triples, NetBps has no idea --
// and StopLossBps, which reads NetBps, never fires. A stop-loss that cannot
// see the cost of stopping is not a stop-loss.
//
// Measured 2026-08-11: RPLUSDT's spot touch moved between $2.13 and $500.87
// within a single session, inversely with its funding rate. A position opened
// at the $500 end and held into the $2 end would have had its exit cost move
// by orders of magnitude with nothing in the system noticing.
//
// THE RULE
//
// Not "close when the book is thin" -- a level is noisy and arbitrary. The
// question is whether the exit is deteriorating faster than the position is
// earning:
//
//	f = funding earned per day        (bps)
//	e = exit cost drift per day       (bps, measured poll to poll)
//
//	hold  while  f > e
//	close when   e >= f
//
// If the door is closing faster than you are being paid to stand in it, leave.
// That is symmetric with how everything else here reasons, and both terms are
// measured rather than assumed.
//
// WHY DRIFT IS SAMPLED HOURLY, NOT PER POLL
//
// Polls are five minutes apart. Dividing a small cost change by 1/288th of a
// day multiplies noise by 288 and produces drift figures of hundreds of bps
// per day from a book that merely twitched. So drift is computed against a
// baseline that is only re-anchored once an hour, and a close requires the
// drift to have been positive across several consecutive baselines.

// MinDriftInterval is how long a baseline must stand before drift is computed
// from it.
const MinDriftInterval = time.Hour

// ExitWatch tracks what it would cost to close a position, over time.
//
// Embedded in Position, so its fields serialise into positions.json and its
// methods are available on the position directly. Every field is omitempty:
// a book written before this existed loads with a zero ExitWatch, which
// reports Measured=false and falls back to the old behaviour exactly.
type ExitWatch struct {
	// LiveExitCostBps is the most recent measured cost of closing.
	LiveExitCostBps float64 `json:"live_exit_cost_bps,omitempty"`

	// WorstExitCostBps is the highest ever seen. Kept because the peak is
	// what a stress moment would have cost, and it is gone from the live
	// reading by the time anyone looks.
	WorstExitCostBps float64 `json:"worst_exit_cost_bps,omitempty"`

	// DriftBpsPerDay is the smoothed rate at which the exit cost is rising.
	// Negative means the book is improving.
	DriftBpsPerDay float64 `json:"exit_drift_bps_per_day,omitempty"`

	// ConsecutiveDrift counts baselines in a row where the exit got worse.
	// A single adverse reading is noise; several in a row is a trend.
	ConsecutiveDrift int `json:"consecutive_drift,omitempty"`

	// DriftBaseBps and DriftBaseAt anchor the drift calculation.
	DriftBaseBps float64   `json:"drift_base_bps,omitempty"`
	DriftBaseAt  time.Time `json:"drift_base_at,omitempty"`

	LastCheckAt time.Time `json:"last_exit_check_at,omitempty"`

	// Measured is false until the first real reading. Nothing downstream may
	// treat an unmeasured watch as "exit is cheap".
	Measured bool `json:"exit_measured,omitempty"`
}

// Observe records a freshly measured exit cost.
//
// A non-positive reading is IGNORED rather than recorded as zero. Zero would
// mean "free to exit", which is the most dangerous possible wrong answer here.
func (w *ExitWatch) Observe(liveExitBps float64, now time.Time) {
	if liveExitBps <= 0 {
		return
	}

	w.LiveExitCostBps = liveExitBps
	w.LastCheckAt = now
	if liveExitBps > w.WorstExitCostBps {
		w.WorstExitCostBps = liveExitBps
	}

	if !w.Measured {
		// First reading: anchor the baseline, compute nothing. A drift
		// derived from a single point would be invented.
		w.Measured = true
		w.DriftBaseBps = liveExitBps
		w.DriftBaseAt = now
		return
	}

	if w.DriftBaseAt.IsZero() {
		w.DriftBaseBps = liveExitBps
		w.DriftBaseAt = now
		return
	}

	elapsed := now.Sub(w.DriftBaseAt)
	if elapsed < MinDriftInterval {
		return
	}

	days := elapsed.Hours() / 24
	if days <= 0 {
		return
	}
	drift := (liveExitBps - w.DriftBaseBps) / days

	// Exponential smoothing. One bad hour should move the estimate, not
	// dominate it.
	if w.DriftBpsPerDay == 0 {
		w.DriftBpsPerDay = drift
	} else {
		w.DriftBpsPerDay = 0.6*w.DriftBpsPerDay + 0.4*drift
	}

	if drift > 0 {
		w.ConsecutiveDrift++
	} else {
		w.ConsecutiveDrift = 0
	}

	// Re-anchor for the next window.
	w.DriftBaseBps = liveExitBps
	w.DriftBaseAt = now
}

// EffectiveExitBps is the exit cost to use in PnL arithmetic.
//
// Returns the live measurement when one exists, and the caller's fallback
// (normally the entry cost) only before the first reading. This is what makes
// the stop-loss able to see a deteriorating exit.
func (w ExitWatch) EffectiveExitBps(fallback float64) float64 {
	if w.Measured && w.LiveExitCostBps > 0 {
		return w.LiveExitCostBps
	}
	return fallback
}

// Deterioration is how much worse the exit has become since the position was
// opened, as a multiple of the entry cost. 1.0 means unchanged.
func (w ExitWatch) Deterioration(entryCostBps float64) float64 {
	if !w.Measured || entryCostBps <= 0 || w.LiveExitCostBps <= 0 {
		return 1
	}
	return w.LiveExitCostBps / entryCostBps
}

// DoorClosing reports whether the exit is deteriorating faster than the
// position earns, sustained across enough baselines to be a trend.
//
// fundingPerDayBps is what the position currently earns per day. It may be
// negative, in which case any positive drift qualifies -- a position that is
// paying funding AND getting harder to exit has nothing to wait for.
func (w ExitWatch) DoorClosing(fundingPerDayBps float64, minConsecutive int) (bool, string) {
	if !w.Measured {
		return false, ""
	}
	if minConsecutive < 1 {
		minConsecutive = 3
	}
	if w.ConsecutiveDrift < minConsecutive {
		return false, ""
	}
	if w.DriftBpsPerDay <= 0 {
		return false, ""
	}
	if w.DriftBpsPerDay < fundingPerDayBps {
		// Still earning faster than the exit is worsening. Hold.
		return false, ""
	}

	return true, fmt.Sprintf(
		"exit liquidity closing: cost to close is rising %.2f bps/day against %.2f bps/day of funding, "+
			"for %d consecutive windows (now %.2f bps, worst %.2f bps). "+
			"Waiting costs more than staying earns",
		w.DriftBpsPerDay, fundingPerDayBps, w.ConsecutiveDrift,
		w.LiveExitCostBps, w.WorstExitCostBps)
}

// String renders the watch for a log line or the dashboard.
func (w ExitWatch) String() string {
	if !w.Measured {
		return "exit cost UNMEASURED"
	}
	return fmt.Sprintf("exit %.2f bps (worst %.2f, drift %+.2f bps/day, %d adverse windows)",
		w.LiveExitCostBps, w.WorstExitCostBps, w.DriftBpsPerDay, w.ConsecutiveDrift)
}
