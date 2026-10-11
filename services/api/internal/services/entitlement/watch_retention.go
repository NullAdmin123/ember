package entitlement

import (
	"errors"
	"log"
	"sync/atomic"
	"time"

	configpkg "github.com/konghang/ember/backend/internal/config"
)

// FirstWatchRetentionCheck returns the first configured run at or after a full business-calendar period.
func FirstWatchRetentionCheck(start time.Time, days int, expression string, location *time.Location) (time.Time, error) {
	if location == nil || days < 1 || days > 3650 {
		return time.Time{}, errors.New("观看保号周期须为 1..3650 天")
	}
	schedule, err := configpkg.ParseWatchRetentionSchedule(expression)
	if err != nil {
		return time.Time{}, err
	}
	ready := start.In(location).AddDate(0, 0, days)
	next := schedule.Next(ready.Add(-time.Nanosecond))
	if next.IsZero() {
		return time.Time{}, errors.New("观看保号计划没有下一次执行时间")
	}
	return next, nil
}

// RetentionSchedule is the process's registered schedule, not a pending settings change requiring restart.
type RetentionSchedule struct {
	Expression string
	Location   *time.Location
	Enabled    bool
}

var registeredRetentionSchedule atomic.Pointer[RetentionSchedule]

// RegisterRetentionSchedule publishes the cron contract, suspending unsupported stored configurations.
// Other cron jobs remain enabled; users must not be shown invented first-check dates.
func RegisterRetentionSchedule(expression string, location *time.Location, enabled bool) {
	if _, err := configpkg.ParseWatchRetentionSchedule(expression); err != nil {
		enabled = false
		log.Printf("[WatchRetention] 调度配置无效，暂停考核；请在设置中心改用日历 cron 并重启 API: %v", err)
	}
	registeredRetentionSchedule.Store(&RetentionSchedule{Expression: expression, Location: location, Enabled: enabled})
}

// RegisteredRetentionSchedule returns nil before process assembly (for isolated service tests).
func RegisteredRetentionSchedule() *RetentionSchedule { return registeredRetentionSchedule.Load() }
