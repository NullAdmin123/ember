package entitlement

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// captureRetentionLogs restores the process logger after each non-parallel logging test.
func captureRetentionLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	return &output
}

// TestRetentionRunSummary distinguishes committed decisions, skips, failures and interrupted work.
func TestRetentionRunSummary(t *testing.T) {
	for _, scenario := range []string{"empty", "read_failure", "grace", "inactive", "passed", "duplicate", "state_changed", "query_failure", "canceled", "invalidated", "sync_failure"} {
		t.Run(scenario, func(t *testing.T) {
			output := captureRetentionLogs(t)
			database, mock := mockDatabase(t)
			location, _ := time.LoadLocation("Asia/Shanghai")
			start := time.Date(2026, 9, 1, 2, 0, 0, 0, location)
			at := start.AddDate(0, 0, 31)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := []string{"本轮结束", "checkedAt=" + at.Format(time.RFC3339Nano), "duration=", "status=completed"}
			rows := sqlmock.NewRows([]string{"id"})
			if scenario != "empty" && scenario != "read_failure" {
				rows.AddRow("u1")
			}
			if scenario == "canceled" {
				rows.AddRow("u2")
			}
			query := mock.ExpectQuery(`SELECT .*users.*JOIN plan_groups`)
			if scenario == "read_failure" {
				query.WillReturnError(errors.New("fixture database unavailable"))
				want[3] = "status=failed"
			} else {
				query.WillReturnRows(rows)
			}
			if scenario != "empty" && scenario != "read_failure" {
				mock.ExpectExec(`INSERT INTO watch_retention_scan_states`).WillReturnResult(sqlmock.NewResult(0, 1))
				policyStart := start
				if scenario == "grace" {
					policyStart = at
				}
				expectRetentionSnapshot(mock, policyStart, scenario != "inactive")
			}
			switch scenario {
			case "empty":
				want = append(want, "candidates=0", "attempted=0", "remaining=0")
			case "grace":
				want = append(want, "skippedGrace=1", "passed=0")
			case "inactive":
				want = append(want, "skippedInactive=1", "passed=0")
			case "passed", "duplicate", "state_changed":
				mock.ExpectBegin()
				expectRetentionSnapshot(mock, start, scenario != "state_changed")
				if scenario != "state_changed" {
					count := int64(1)
					if scenario == "duplicate" {
						count = 0
					}
					mock.ExpectExec(`INSERT INTO "watch_retention_checks"`).WillReturnResult(sqlmock.NewResult(0, count))
				}
				mock.ExpectCommit()
				if scenario == "passed" {
					want = append(want, "passed=1", "invalidated=0", "考核完成", "committed=true")
				} else if scenario == "duplicate" {
					want = append(want, "skippedDuplicate=1", "passed=0")
				} else {
					want = append(want, "skippedChanged=1", "passed=0")
				}
			case "query_failure":
				want[3] = "status=partial_failure"
				want = append(want, "failed=1", "passed=0")
			case "canceled":
				want[3] = "status=canceled"
				want = append(want, "candidates=2", "attempted=1", "remaining=1")
			case "invalidated", "sync_failure":
				mock.ExpectBegin()
				expectRetentionSnapshot(mock, start, true)
				mock.ExpectExec(`INSERT INTO "watch_retention_checks"`).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec(`UPDATE "user_entitlements" SET "watch_retention_invalidated_at"`).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectQuery(`SELECT .*plan_groups`).WillReturnRows(sqlmock.NewRows([]string{"key", "entitlement_rank"}).AddRow("A", 20).AddRow("B", 10))
				mock.ExpectQuery(`SELECT .*user_entitlements`).WillReturnRows(sqlmock.NewRows([]string{"plan_group", "validity_type", "watch_retention_invalidated_at"}).AddRow("A", "permanent", at).AddRow("B", "permanent", nil))
				mock.ExpectExec(`UPDATE "user_entitlements" SET "watch_retention_started_at"`).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec(`UPDATE "users"`).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec(`INSERT INTO "emby_policy_sync_tasks"`).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectCommit()
				want = append(want, "invalidated=1", "passed=0", "syncRequested=1", "committed=true")
				if scenario == "sync_failure" {
					want[3] = "status=partial_failure"
					want = append(want, "failed=1", "syncDispatchFailed=1")
				}
			}
			worker := WatchRetentionWorker{DB: database, Location: location, Query: func(context.Context, string, time.Time, time.Time, *time.Location) (int64, error) {
				if scenario == "canceled" {
					cancel()
					return 0, ctx.Err()
				}
				if scenario == "query_failure" {
					return 0, errors.New("fixture upstream unavailable")
				}
				if scenario == "invalidated" || scenario == "sync_failure" {
					return 0, nil
				}
				if scenario != "passed" && scenario != "duplicate" && scenario != "state_changed" {
					t.Fatalf("unexpected playback query in %s", scenario)
				}
				return 3600, nil
			}, Sync: func(id string) error {
				if id != "u1" || !strings.Contains(output.String(), "committed=true") {
					t.Fatal("sync dispatched before committed result was logged")
				}
				if scenario == "sync_failure" {
					return errors.New("fixture sync failed")
				}
				return nil
			}}
			err := worker.Run(ctx, at)
			wantError := scenario == "read_failure" || scenario == "query_failure" || scenario == "canceled" || scenario == "sync_failure"
			if (err != nil) != wantError {
				t.Fatalf("Run error=%v wantError=%t", err, wantError)
			}
			for _, field := range want {
				if !strings.Contains(output.String(), field) {
					t.Errorf("missing %q in logs: %s", field, output.String())
				}
			}
			if scenario != "passed" && scenario != "invalidated" && scenario != "sync_failure" && strings.Contains(output.String(), "考核完成") {
				t.Errorf("non-committed decision logged as complete: %s", output.String())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
