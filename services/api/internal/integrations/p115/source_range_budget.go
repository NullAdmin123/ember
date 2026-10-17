package p115

import (
	"context"
	"errors"
	"time"
)

const sourceRangePhaseTimeout = 5 * time.Second

// SourceRangeReadTimeout is the nominal sum of the independent 5s URL/read
// windows, not an additional outer timer that could erase failed-stage details.
const SourceRangeReadTimeout = 2 * sourceRangePhaseTimeout

// sourcePhaseContext keeps production deadlines on the standard context clock
// while allowing read-only budget tests to advance time without sleeping.
func (a *CookieHTTPAdapter) sourcePhaseContext(ctx context.Context, limit time.Duration) (context.Context, context.CancelFunc) {
	if a.sourceReadTimeout != nil {
		return a.sourceReadTimeout(ctx, limit)
	}
	return context.WithTimeout(ctx, limit)
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
