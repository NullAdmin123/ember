package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/konghang/ember/backend/internal/models"
	entitlementpkg "github.com/konghang/ember/backend/internal/services/entitlement"
	policypkg "github.com/konghang/ember/backend/internal/services/policy"
)

// retentionPolicyClient records managed Policy writes without making external requests.
type retentionPolicyClient struct {
	policy map[string]any
	err    error
}

// GetUserPolicyRaw provides the ordinary-user policy used by the recovery fixture.
func (c *retentionPolicyClient) GetUserPolicyRaw(string) (map[string]any, error) {
	return map[string]any{"IsAdministrator": false}, nil
}

// PatchUserPolicyFields captures the final remote state for disable and fallback assertions.
func (c *retentionPolicyClient) PatchUserPolicyFields(_ string, policy map[string]any, _ []string) error {
	c.policy = policy
	return c.err
}

// seedRetentionRecoveryUser creates an eligible permanent holding in an isolated schema.
func seedRetentionRecoveryUser(t *testing.T, h *integrationHarness, id string) (models.User, time.Time, *time.Location) {
	t.Helper()
	loc, _ := time.LoadLocation("Asia/Shanghai")
	start := time.Date(2026, 9, 1, 2, 0, 0, 0, loc)
	group := "DEFAULT"
	if err := h.database.Model(&models.PlanGroup{}).Where("key = ?", group).Updates(map[string]any{
		"entitlement_rank": 20, "watch_retention_enabled": true, "watch_retention_days": 30,
		"watch_retention_min_minutes": 60,
	}).Error; err != nil {
		t.Fatal(err)
	}
	user := h.seedUser(t, models.User{ID: id, Username: id, Email: id + "@example.com", PlanGroup: &group, EmbyID: id})
	if err := h.database.Model(&models.UserEntitlement{}).Where("user_id = ? AND plan_group = ?", user.ID, group).Update("watch_retention_started_at", start).Error; err != nil {
		t.Fatal(err)
	}
	return user, start.AddDate(0, 0, 31), loc
}

// TestIntegrationWatchRetentionRecoversInterruptedSync covers commit-to-sync interruption
// for complete disable and fallback, including an already active older Policy task.
func TestIntegrationWatchRetentionRecoversInterruptedSync(t *testing.T) {
	for _, tc := range []struct {
		name, oldStatus string
		fallback        bool
	}{
		{name: "disable"},
		{name: "fallback_pending", oldStatus: "pending", fallback: true},
		{name: "fallback_processing", oldStatus: "processing", fallback: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newIntegrationHarness(t)
			user, at, loc := seedRetentionRecoveryUser(t, h, "retention-recovery")
			if tc.fallback {
				rank := 10
				h.seedPlanGroup(t, models.PlanGroup{Key: "FALLBACK", Name: "Fallback", EntitlementRank: &rank})
				h.seedPlanGroupTemplate(t, models.PlanGroupEmbyPolicyTemplate{PlanGroupKey: "FALLBACK", SimultaneousStreamLimit: 1})
				if err := h.database.Create(&models.UserEntitlement{UserID: user.ID, PlanGroup: "FALLBACK", ValidityType: "permanent"}).Error; err != nil {
					t.Fatal(err)
				}
				h.seedPlanGroupLibraries(t, models.PlanGroupMediaLibrary{PlanGroupKey: "FALLBACK", LibraryID: "fallback-library", LibraryName: "Fallback", LibraryType: "movies"})
			}
			oldTask := models.EmbyPolicySyncTask{UserID: user.ID, EmbyID: user.EmbyID, PlanGroupKey: "DEFAULT", Reason: "older_change", Status: tc.oldStatus}
			var batch models.EmbyPolicySyncBatch
			if tc.oldStatus != "" {
				batch = models.EmbyPolicySyncBatch{PlanGroupKey: "DEFAULT", Reason: "older_batch", Status: tc.oldStatus, TotalCount: 1}
				if err := h.database.Create(&batch).Error; err != nil {
					t.Fatal(err)
				}
				oldTask.BatchID = &batch.ID
				if err := h.database.Create(&oldTask).Error; err != nil {
					t.Fatal(err)
				}
			}
			worker := entitlementpkg.WatchRetentionWorker{DB: h.database, Location: loc,
				Query: func(context.Context, string, time.Time, time.Time, *time.Location) (int64, error) { return 0, nil },
				Sync:  func(string) error { panic("simulated interruption before Policy sync") },
			}
			func() {
				defer func() {
					if recover() != "simulated interruption before Policy sync" {
						t.Fatal("did not reach the post-commit interruption")
					}
				}()
				if err := worker.Run(context.Background(), at); err != nil {
					t.Fatal(err)
				}
			}()
			var pending models.EmbyPolicySyncTask
			if err := h.database.Where("user_id = ? AND status = ?", user.ID, "pending").First(&pending).Error; err != nil {
				t.Fatalf("committed invalidation lost its recoverable Policy task: %v", err)
			}
			if tc.oldStatus != "" && pending.ID == oldTask.ID {
				t.Fatal("old worker still owns the new Policy intent")
			}
			if tc.oldStatus != "" && (pending.BatchID == nil || *pending.BatchID != batch.ID) {
				t.Fatal("replacing the intent lost its batch membership")
			}
			// A late completion by the old worker must not consume the new pending intent.
			if tc.oldStatus == "processing" {
				result := h.database.Model(&models.EmbyPolicySyncTask{}).Where("id = ? AND status = ?", oldTask.ID, "processing").Update("status", "synced")
				if result.Error != nil || result.RowsAffected != 0 {
					t.Fatalf("stale completion affected new work: rows=%d err=%v", result.RowsAffected, result.Error)
				}
			}
			client := &retentionPolicyClient{}
			service := policypkg.NewServiceWithDB(h.database, client)
			result, err := service.ProcessPendingEmbyPolicySyncTasks(context.Background(), 1)
			if err != nil || result.Succeeded != 1 {
				t.Fatalf("recovery result=%+v err=%v", result, err)
			}
			if client.policy["IsDisabled"] != !tc.fallback {
				t.Fatalf("wrong recovered access state: %+v", client.policy)
			}
			if tc.fallback {
				folders, ok := client.policy["EnabledFolders"].([]string)
				if !ok || len(folders) != 1 || folders[0] != "fallback-library" {
					t.Fatalf("fallback libraries were not applied: %v", client.policy["EnabledFolders"])
				}
			}
			if err := h.database.First(&pending, "id = ?", pending.ID).Error; err != nil || pending.Status != "synced" {
				t.Fatalf("recovered task not completed: %+v err=%v", pending, err)
			}
			if tc.oldStatus != "" {
				if err := h.database.First(&batch, "id = ?", batch.ID).Error; err != nil || batch.Status != "synced" || batch.TotalCount != 1 || batch.SyncedCount != 1 {
					t.Fatalf("batch did not follow the replaced task: %+v err=%v", batch, err)
				}
			}
		})
	}
}

// TestIntegrationWatchRetentionImmediateSync finishes only the requested user's task,
// retaining a failed terminal record for administrator retry when the fake remote write fails.
func TestIntegrationWatchRetentionImmediateSync(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			h := newIntegrationHarness(t)
			user, at, loc := seedRetentionRecoveryUser(t, h, "retention-immediate")
			other := h.seedUser(t, models.User{Username: "other-user", Email: "other@example.com", EmbyID: "other-emby"})
			otherTask := models.EmbyPolicySyncTask{UserID: other.ID, EmbyID: other.EmbyID, PlanGroupKey: "DEFAULT", Reason: "other_change", Status: "pending"}
			if err := h.database.Create(&otherTask).Error; err != nil {
				t.Fatal(err)
			}
			client := &retentionPolicyClient{}
			if fail {
				client.err = errors.New("fixture write failure")
			}
			service := policypkg.NewServiceWithDB(h.database, client)
			worker := entitlementpkg.WatchRetentionWorker{DB: h.database, Location: loc,
				Query: func(_ context.Context, embyID string, _, _ time.Time, _ *time.Location) (int64, error) {
					if embyID == user.EmbyID {
						return 0, nil
					}
					return 3600, nil
				},
				Sync: func(id string) error { return service.ProcessPendingUserPolicySync(context.Background(), id) },
			}
			if err := worker.Run(context.Background(), at); (err != nil) != fail {
				t.Fatalf("Run error=%v fail=%t", err, fail)
			}
			var task models.EmbyPolicySyncTask
			want := "synced"
			if fail {
				want = "failed"
			}
			if err := h.database.Where("user_id = ?", user.ID).First(&task).Error; err != nil || task.Status != want || task.Attempts != 1 {
				t.Fatalf("wrong immediate result: %+v err=%v", task, err)
			}
			if err := h.database.First(&otherTask, "id = ?", otherTask.ID).Error; err != nil || otherTask.Status != "pending" {
				t.Fatalf("consumed unrelated work: %+v err=%v", otherTask, err)
			}
		})
	}
}
