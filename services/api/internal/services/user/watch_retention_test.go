package user

import (
	"github.com/konghang/ember/backend/internal/models"
	"testing"
	"time"
)

// TestWatchRetentionDisplay uses server state for disabled, waiting, first-check and invalidated copy.
func TestWatchRetentionDisplay(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	start := time.Date(2026, 10, 8, 15, 0, 0, 0, loc)
	for _, tc := range []struct {
		name                      string
		enabled, current, invalid bool
		want                      string
	}{
		{"disabled", false, true, false, ""}, {"waiting", true, false, false, "waiting"}, {"grace", true, true, false, "grace"}, {"invalidated", true, false, true, "invalidated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := EntitlementView{UserEntitlement: models.UserEntitlement{ValidityType: "permanent", WatchRetentionStartedAt: &start}, RetentionEnabled: tc.enabled, RetentionDays: 30, RetentionMinutes: 60, Current: tc.current}
			if tc.invalid {
				row.WatchRetentionInvalidatedAt = &start
			}
			if err := attachWatchRetentionView(&row, start, "0 2 * * *", loc, true); err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if row.WatchRetention != nil {
					t.Fatal("disabled display")
				}
				return
			}
			if row.WatchRetention == nil || row.WatchRetention.State != tc.want {
				t.Fatalf("%+v", row.WatchRetention)
			}
			if tc.want == "grace" && !row.WatchRetention.FirstCheckAt.Equal(time.Date(2026, 11, 8, 2, 0, 0, 0, loc)) {
				t.Fatal("wrong first check")
			}
		})
	}
}

// TestWatchRetentionSuspendedScheduleKeepsEntitlementsReadable covers existing unsupported cron settings.
func TestWatchRetentionSuspendedScheduleKeepsEntitlementsReadable(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	now := time.Date(2026, 10, 11, 15, 0, 0, 0, loc)
	for _, current := range []bool{false, true} {
		row := EntitlementView{UserEntitlement: models.UserEntitlement{ValidityType: "permanent", WatchRetentionStartedAt: &now}, RetentionEnabled: true, RetentionDays: 30, RetentionMinutes: 60, Current: current}
		if err := attachWatchRetentionView(&row, now, "@every 24h", loc, false); err != nil {
			t.Fatal(err)
		}
		if row.WatchRetention.State != "suspended" || row.WatchRetention.FirstCheckAt != nil {
			t.Fatalf("suspended schedule advertises checks: %+v", row.WatchRetention)
		}
	}
}

// TestWatchRetentionRuleChangesKeepActivation verifies that current rules recalculate dates from the original clock.
func TestWatchRetentionRuleChangesKeepActivation(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	start := time.Date(2026, 9, 1, 15, 0, 0, 0, loc)
	now := time.Date(2026, 10, 19, 2, 0, 0, 0, loc)
	for _, tc := range []struct {
		name          string
		days, minutes int
		state         string
		first         time.Time
	}{
		{"enabled_mature_holding", 30, 5, "checking", time.Date(2026, 10, 2, 2, 0, 0, 0, loc)},
		{"new_minute_threshold", 30, 90, "checking", time.Date(2026, 10, 2, 2, 0, 0, 0, loc)},
		{"shorter_period", 14, 5, "checking", time.Date(2026, 9, 16, 2, 0, 0, 0, loc)},
		{"longer_period", 60, 5, "grace", time.Date(2026, 11, 1, 2, 0, 0, 0, loc)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := EntitlementView{UserEntitlement: models.UserEntitlement{ValidityType: "permanent", WatchRetentionStartedAt: &start}, RetentionEnabled: true, RetentionDays: tc.days, RetentionMinutes: tc.minutes, Current: true}
			if err := attachWatchRetentionView(&row, now, "0 2 * * *", loc, true); err != nil {
				t.Fatal(err)
			}
			if row.WatchRetention.State != tc.state || row.WatchRetention.FirstCheckAt == nil || !row.WatchRetention.FirstCheckAt.Equal(tc.first) || row.WatchRetention.MinMinutes != tc.minutes || !row.WatchRetentionStartedAt.Equal(start) {
				t.Fatalf("rule change moved activation or advertised wrong check: %+v", row)
			}
		})
	}
}
