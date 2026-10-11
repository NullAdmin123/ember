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

// sourceRangeReadLimit keeps HashFileRange on the original 5s source-range
// budget. The same test seam shortens it for local timeout tests.
func (s *Service) sourceRangeReadLimit() time.Duration {
	if s.sourceReadBudget > 0 {
		return s.sourceReadBudget
	}
	return sourceReadTimeout
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
	return err
}
