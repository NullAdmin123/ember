package payment

import (
	"github.com/konghang/ember/backend/internal/models"
	"testing"
)

// TestWatchRetentionConfiguration validates complete rules and preserves configuration on invalid edits.
func TestWatchRetentionConfiguration(t *testing.T) {
	enabled := true
	days := 14
	minutes := 60
	group := models.PlanGroup{WatchRetentionDays: 30}
	if err := applyWatchRetentionPolicy(&group, &enabled, &days, &minutes); err != nil {
		t.Fatal(err)
	}
	if !group.WatchRetentionEnabled || group.WatchRetentionDays != days || group.WatchRetentionMinMinutes != minutes {
		t.Fatalf("rule not applied: %+v", group)
	}
	days = 15
	if err := applyWatchRetentionPolicy(&group, nil, &days, nil); err != nil || group.WatchRetentionDays != days {
		t.Fatalf("period not changed: %+v err=%v", group, err)
	}
	minutes = 0
	if err := applyWatchRetentionPolicy(&group, nil, nil, &minutes); err == nil {
		t.Fatal("accepted enabled zero")
	}
	if group.WatchRetentionMinMinutes != 60 {
		t.Fatal("invalid edit changed the persisted rule")
	}
	enabled = false
	if err := applyWatchRetentionPolicy(&group, &enabled, nil, &minutes); err != nil || group.WatchRetentionEnabled {
		t.Fatalf("disable failed: %+v err=%v", group, err)
	}
}
