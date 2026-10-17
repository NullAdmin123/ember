package directplay

import (
	"context"
	"errors"
	"time"

	p115 "github.com/konghang/ember/backend/internal/integrations/p115"
)

// sourcePathResolveLimit gives ResolveFileByPath the same 30s parent-plus-pages
// budget as the shared directory flight. The test seam shortens it without
// creating a deployment setting.
func (s *Service) sourcePathResolveLimit() time.Duration {
	if s.sourceReadBudget > 0 {
		return s.sourceReadBudget
	}
	return p115.SourceDirectoryResolveTimeout
}

// sourcePathContext lets tests advance directory deadlines without wall-clock
// sleeps. Range reads use the Provider's two independent phase budgets instead.
func (s *Service) sourcePathContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if s.sourceReadTimeout != nil {
		return s.sourceReadTimeout(ctx, s.sourcePathResolveLimit())
	}
	return context.WithTimeout(ctx, s.sourcePathResolveLimit())
}

// sourceReadResult distinguishes Ember's optional acceleration budget from a
// real provider failure and preserves caller cancellation before any fallback.
func sourceReadResult(parent, read context.Context, err error) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	if errors.Is(read.Err(), context.DeadlineExceeded) || errors.Is(err, p115.ErrSourceDirectoryTimeout) {
		return ErrSourceReadTimeout
	}
	// An adapter phase timeout remains typed until reportProviderFailure maps
	// it, so its safe download_url/source_range detail survives into Gateway logs.
	return err
}
