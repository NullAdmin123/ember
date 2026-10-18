package app

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/konghang/ember/backend/internal/models"
	entitlementpkg "github.com/konghang/ember/backend/internal/services/entitlement"
	userpkg "github.com/konghang/ember/backend/internal/services/user"
)

// TestIntegrationWatchRetentionRuleChangesKeepStart exercises real group writes, entitlement DTOs and a fake-only assessment.
func TestIntegrationWatchRetentionRuleChangesKeepStart(t *testing.T) {
	h := newIntegrationHarness(t)
	loc, _ := time.LoadLocation("Asia/Shanghai")
	now := time.Now().In(loc).Truncate(time.Microsecond)
	start := now.AddDate(0, 0, -40)
	user := h.seedUser(t, models.User{Username: "retention-rules", Email: "retention-rules@example.com", EmbyID: "retention-fixture"})
	if err := h.database.Model(&models.UserEntitlement{}).Where("user_id = ?", user.ID).Update("watch_retention_started_at", start).Error; err != nil {
		t.Fatal(err)
	}
	h.setSetting(t, "CRON_ENABLED", "true")
	h.setSetting(t, "CRON_TIMEZONE", loc.String())
	h.setSetting(t, "WATCH_RETENTION_SCHEDULE", "0 2 * * *")
	for _, tc := range []struct {
		name, body, state string
	}{
		{"enable", `{"watchRetentionEnabled":true,"watchRetentionDays":30,"watchRetentionMinMinutes":5}`, "checking"},
		{"change_minutes", `{"watchRetentionMinMinutes":10}`, "checking"},
		{"longer_period", `{"watchRetentionDays":60}`, "grace"},
		{"disable", `{"watchRetentionEnabled":false}`, ""},
		{"enable_again", `{"watchRetentionEnabled":true,"watchRetentionDays":30}`, "checking"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := h.performAdminRequest(http.MethodPut, "/api/v1/admin/plan-groups/"+*user.PlanGroup, []byte(tc.body))
			if response.Code != http.StatusOK {
				t.Fatalf("group update returned %d", response.Code)
			}
			var holding models.UserEntitlement
			if err := h.database.Where("user_id = ?", user.ID).First(&holding).Error; err != nil || holding.WatchRetentionStartedAt == nil || !holding.WatchRetentionStartedAt.Equal(start) {
				t.Fatalf("configuration moved activation: %+v err=%v", holding, err)
			}
			response = h.performAdminRequest(http.MethodGet, "/api/v1/admin/users/"+user.ID+"/entitlements", nil)
			var result struct {
				Data []userpkg.EntitlementView `json:"data"`
			}
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || len(result.Data) != 1 {
				t.Fatalf("entitlement response failed: status=%d", response.Code)
			}
			rule := result.Data[0].WatchRetention
			if tc.state == "" {
				if rule != nil {
					t.Fatal("disabled rule still displayed")
				}
			} else if rule == nil || rule.State != tc.state || rule.FirstCheckAt == nil {
				t.Fatalf("displayed eligibility disagrees with activation: %+v", rule)
			}
		})
	}
	queries := 0
	worker := entitlementpkg.WatchRetentionWorker{DB: h.database, Location: loc, Schedule: "0 2 * * *", Query: func(_ context.Context, id string, from, to time.Time, location *time.Location) (int64, error) {
		queries++
		if id != user.EmbyID || !from.Equal(now.AddDate(0, 0, -30)) || !to.Equal(now) || location != loc {
			t.Fatal("wrong assessment window")
		}
		return 3600, nil
	}}
	if err := worker.Run(context.Background(), now); err != nil || queries != 1 {
		t.Fatalf("mature holding skipped: queries=%d err=%v", queries, err)
	}
	var check models.WatchRetentionCheck
	if err := h.database.Where("user_id = ?", user.ID).First(&check).Error; err != nil || !check.StartedAt.Equal(start) || check.Invalidated {
		t.Fatalf("wrong committed assessment: %+v err=%v", check, err)
	}
}

// TestIntegrationWatchRetentionResetRemoval verifies legacy upgrade, replay and preservation of all holding evidence.
func TestIntegrationWatchRetentionResetRemoval(t *testing.T) {
	h := newIntegrationHarness(t)
	if h.database.Migrator().HasColumn("plan_groups", "watch_retention_reset_at") {
		t.Fatal("fresh install retained group reset column")
	}
	legacy, err := os.ReadFile(filepath.Join(integrationMigrationsDir(t), "20261008_01_watch_retention.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.database.Exec("ALTER TABLE plan_groups DROP CONSTRAINT plan_groups_watch_retention_valid").Error; err != nil {
		t.Fatal(err)
	}
	if err := h.database.Exec(string(legacy)).Error; err != nil {
		t.Fatal(err)
	}
	user := h.seedUser(t, models.User{Username: "retention-upgrade", Email: "retention-upgrade@example.com"})
	start := time.Date(2026, 9, 1, 2, 0, 0, 0, time.UTC)
	invalidated := start.AddDate(0, 0, 31)
	if err := h.database.Model(&models.UserEntitlement{}).Where("user_id = ?", user.ID).Updates(map[string]any{"watch_retention_started_at": start, "watch_retention_invalidated_at": invalidated}).Error; err != nil {
		t.Fatal(err)
	}
	if err := h.database.Model(&models.PlanGroup{}).Where("key = ?", *user.PlanGroup).Updates(map[string]any{"watch_retention_enabled": true, "watch_retention_min_minutes": 5, "watch_retention_reset_at": invalidated.AddDate(0, 0, 10)}).Error; err != nil {
		t.Fatal(err)
	}
	check := models.WatchRetentionCheck{UserID: user.ID, PlanGroup: *user.PlanGroup, CheckedAt: invalidated, WindowStart: start.AddDate(0, 0, 1), StartedAt: start, PeriodDays: 30, MinMinutes: 5, Invalidated: true}
	if err := h.database.Create(&check).Error; err != nil {
		t.Fatal(err)
	}
	missing := h.seedUser(t, models.User{Username: "retention-missing-start", Email: "retention-missing-start@example.com"})
	migration, err := os.ReadFile(filepath.Join(integrationMigrationsDir(t), "20261018_01_remove_watch_retention_reset.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := h.database.Exec(string(migration)).Error; err != nil {
			t.Fatal(err)
		}
	}
	if h.database.Migrator().HasColumn("plan_groups", "watch_retention_reset_at") {
		t.Fatal("upgrade retained reset column")
	}
	var holding models.UserEntitlement
	if err := h.database.Where("user_id = ?", user.ID).First(&holding).Error; err != nil || holding.WatchRetentionStartedAt == nil || !holding.WatchRetentionStartedAt.Equal(start) || holding.WatchRetentionInvalidatedAt == nil || !holding.WatchRetentionInvalidatedAt.Equal(invalidated) {
		t.Fatalf("upgrade changed holding evidence: %+v err=%v", holding, err)
	}
	var unknown models.UserEntitlement
	if err := h.database.Where("user_id = ?", missing.ID).First(&unknown).Error; err != nil || unknown.WatchRetentionStartedAt != nil {
		t.Fatalf("upgrade invented activation: %+v err=%v", unknown, err)
	}
	var count int64
	if err := h.database.Model(&models.WatchRetentionCheck{}).Where("user_id = ?", user.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("audit lost: count=%d err=%v", count, err)
	}
	if err := h.database.Model(&models.PlanGroup{}).Where("key = ?", *user.PlanGroup).Update("watch_retention_min_minutes", 0).Error; err == nil {
		t.Fatal("enabled zero threshold accepted")
	}
	if err := h.database.Model(&models.PlanGroup{}).Where("key = ?", *user.PlanGroup).Update("watch_retention_min_minutes", 10).Error; err != nil {
		t.Fatalf("rule update still needs reset column: %v", err)
	}
	created := h.performAdminRequest(http.MethodPost, "/api/v1/admin/plan-groups", []byte(`{"key":"RETENTION_NEW","name":"Retention fixture","watchRetentionEnabled":true,"watchRetentionDays":30,"watchRetentionMinMinutes":5}`))
	if created.Code != http.StatusOK {
		t.Fatalf("creating an enabled group still needs the removed column: status=%d", created.Code)
	}
}
