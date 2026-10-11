package policy

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/konghang/ember/backend/internal/models"
	"gorm.io/gorm/logger"
)

// TestPolicyTaskTerminalLogRequiresPersistedResult rejects false success from replaced tasks or failed writes.
func TestPolicyTaskTerminalLogRequiresPersistedResult(t *testing.T) {
	for _, scenario := range []string{"synced", "failed", "replaced", "write_failure", "commit_failure"} {
		t.Run(scenario, func(t *testing.T) {
			var output bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&output)
			t.Cleanup(func() { log.SetOutput(previous) })
			database, mock, closeDB := newPolicySQLMockDB(t, 1)
			defer closeDB()
			database.Logger = logger.Default.LogMode(logger.Silent)
			service := NewServiceWithDB(database, &stubPolicyClient{})
			mock.ExpectBegin()
			update := mock.ExpectExec(`UPDATE "emby_policy_sync_tasks"`)
			if scenario == "write_failure" {
				update.WillReturnError(errors.New("fixture write failed"))
				mock.ExpectRollback()
			} else {
				count := int64(1)
				if scenario == "replaced" {
					count = 0
				}
				update.WillReturnResult(sqlmock.NewResult(0, count))
				commit := mock.ExpectCommit()
				if scenario == "commit_failure" {
					commit.WillReturnError(errors.New("fixture commit failed"))
				}
			}
			status := SyncStatusSynced
			var cause error
			if scenario == "failed" {
				status = SyncStatusFailed
				cause = errors.New("fixture private upstream detail")
			}
			err := service.finishPolicySyncTask(context.Background(), models.EmbyPolicySyncTask{ID: "task1", UserID: "user1", Reason: "watch_retention"}, status, cause)
			wantError := scenario == "write_failure" || scenario == "commit_failure"
			if (err != nil) != wantError {
				t.Fatalf("finish error=%v wantError=%t", err, wantError)
			}
			want := "同步任务已结束"
			if scenario == "replaced" {
				want = "忽略过期任务结果"
			} else if wantError {
				want = "同步终态保存失败"
			}
			for _, field := range []string{want, "taskId=task1", "userId=user1", "reason=watch_retention"} {
				if !strings.Contains(output.String(), field) {
					t.Errorf("missing %q in %s", field, output.String())
				}
			}
			if strings.Contains(output.String(), "同步任务已结束") != (scenario == "synced" || scenario == "failed") {
				t.Errorf("unpersisted result reported as terminal: %s", output.String())
			}
			if (scenario == "synced" || scenario == "failed") && !strings.Contains(output.String(), "status="+status) {
				t.Errorf("terminal status missing: %s", output.String())
			}
			if strings.Contains(output.String(), "fixture private upstream detail") {
				t.Error("raw upstream error leaked into terminal log")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestImmediatePolicySyncWithoutClaimDoesNotLogSuccess distinguishes another worker's ownership from completion.
func TestImmediatePolicySyncWithoutClaimDoesNotLogSuccess(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	database, mock, closeDB := newPolicySQLMockDB(t, 1)
	defer closeDB()
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT .*emby_policy_sync_tasks.*user_id.*FOR UPDATE SKIP LOCKED`).
		WillReturnRows(policyTaskRows())
	mock.ExpectCommit()
	service := NewServiceWithDB(database, &stubPolicyClient{})
	if err := service.ProcessPendingUserPolicySync(context.Background(), "user1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "本次未领取用户任务 userId=user1") || strings.Contains(output.String(), "status=synced") {
		t.Fatalf("unclaimed task must not be reported as synced: %s", output.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
