package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/konghang/ember/backend/internal/models"
	entitlementpkg "github.com/konghang/ember/backend/internal/services/entitlement"
)

// TestIntegrationWatchRetentionResumesAfterCancellation verifies that a fresh worker
// reaches untouched users even when a preceding user fails and each run is interrupted.
func TestIntegrationWatchRetentionResumesAfterCancellation(t *testing.T) {
	for _, firstFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "passed_prefix", true: "failed_prefix"}[firstFails], func(t *testing.T) {
			h := newIntegrationHarness(t)
			_, at, loc := seedRetentionRecoveryUser(t, h, "retention-a")
			seedRetentionRecoveryUser(t, h, "retention-b")
			seedRetentionRecoveryUser(t, h, "retention-c")
			visits := map[string]int{}
			for round := 0; round < 2; round++ {
				ctx, cancel := context.WithCancel(context.Background())
				worker := entitlementpkg.WatchRetentionWorker{DB: h.database, Location: loc,
					Query: func(ctx context.Context, id string, _, _ time.Time, _ *time.Location) (int64, error) {
						visits[id]++
						if id == "retention-b" {
							cancel()
							return 0, ctx.Err()
						}
						if id == "retention-a" && firstFails {
							return 0, errors.New("fixture unavailable evidence")
						}
						return 3600, nil
					},
				}
				err := worker.Run(ctx, at.AddDate(0, 0, round))
				cancel()
				if err == nil {
					t.Fatal("expected an interrupted run")
				}
				if round == 0 && visits["retention-c"] != 0 {
					t.Fatal("fixture did not stop before the final user")
				}
				if round == 0 {
					migration, err := os.ReadFile(filepath.Join(integrationMigrationsDir(t), "20261011_01_watch_retention_scan_progress.sql"))
					if err != nil {
						t.Fatal(err)
					}
					for i := 0; i < 2; i++ {
						if err := h.database.Exec(string(migration)).Error; err != nil {
							t.Fatal(err)
						}
					}
					var progress models.WatchRetentionScanState
					if err := h.database.First(&progress, "user_id = ?", "retention-b").Error; err != nil || !progress.LastAttemptAt.Equal(at) {
						t.Fatalf("migration replay lost interrupted progress: %+v err=%v", progress, err)
					}
				}
			}
			if visits["retention-c"] != 1 {
				t.Fatalf("new worker did not resume untouched users: %v", visits)
			}
			var invalidated int64
			if err := h.database.Table("user_entitlements").Where("watch_retention_invalidated_at IS NOT NULL").Count(&invalidated).Error; err != nil || invalidated != 0 {
				t.Fatalf("unknown evidence invalidated a grant: count=%d err=%v", invalidated, err)
			}
		})
	}
}
