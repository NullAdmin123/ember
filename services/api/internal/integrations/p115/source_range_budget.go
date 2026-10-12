package p115

import (
	"context"
	"errors"
	"time"
)

const sourceRangePhaseTimeout = 5 * time.Second

// SourceRangeReadTimeout bounds signed-URL preparation plus one CDN range
// read; each phase retains its own 5s limit and honors any earlier caller deadline.
const SourceRangeReadTimeout = 2 * sourceRangePhaseTimeout

// sourceRangePhaseLimit permits short mock deadlines without exposing a new
// deployment setting or changing the budgets of retained provider writes.
func (a *CookieHTTPAdapter) sourceRangePhaseLimit() time.Duration {
	if a.sourceRangePhaseBudget > 0 {
		return a.sourceRangePhaseBudget
	}
	return sourceRangePhaseTimeout
}

// sourceRangeStageResult distinguishes a local read deadline from caller
// cancellation and retains the failed HTTP stage without upstream error text.
func sourceRangeStageResult(parent, phase context.Context, err error) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	if errors.Is(phase.Err(), context.DeadlineExceeded) {
		stage, _, status := FailureDiagnostic(err)
		if stage != "" {
			return &operationFailure{cause: ErrSourceRangeTimeout, stage: stage, reason: "request_deadline", status: status}
		}
		return ErrSourceRangeTimeout
	}
	return err
}
