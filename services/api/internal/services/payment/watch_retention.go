package payment

import (
	"errors"
	"github.com/konghang/ember/backend/internal/models"
)

var ErrWatchRetentionInvalid = errors.New("观看保号周期须为 1..3650 天，开启时最低观看分钟数须为 1..5256000")

// applyWatchRetentionPolicy validates group rules without changing any entitlement's actual activation clock.
func applyWatchRetentionPolicy(group *models.PlanGroup, enabled *bool, days, minutes *int) error {
	next := *group
	if next.WatchRetentionDays == 0 {
		next.WatchRetentionDays = 30
	}
	if enabled != nil {
		next.WatchRetentionEnabled = *enabled
	}
	if days != nil {
		next.WatchRetentionDays = *days
	}
	if minutes != nil {
		next.WatchRetentionMinMinutes = *minutes
	}
	if next.WatchRetentionDays < 1 || next.WatchRetentionDays > 3650 || next.WatchRetentionMinMinutes < 0 || next.WatchRetentionMinMinutes > 5256000 || next.WatchRetentionEnabled && next.WatchRetentionMinMinutes == 0 {
		return ErrWatchRetentionInvalid
	}
	*group = next
	return nil
}
