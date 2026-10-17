package p115account

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestRevokePersonalUIDIsVisibleOnlyAfterCommit exercises the actual GORM
// transaction using SQL fakes, including rollback, failed commit and no account.
func TestRevokePersonalUIDIsVisibleOnlyAfterCommit(t *testing.T) {
	for _, outcome := range []string{"committed", "missing", "update_failed", "commit_failed"} {
		t.Run(outcome, func(t *testing.T) {
			raw, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			database, err := gorm.Open(postgres.New(postgres.Config{Conn: raw}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
			if err != nil {
				t.Fatal(err)
			}
			mock.ExpectBegin()
			rows := sqlmock.NewRows([]string{"id", "owner_user_id", "provider_user_id", "role", "status"})
			if outcome != "missing" {
				rows.AddRow("account-1", "user-1", "12345", "playback", "active")
			}
			mock.ExpectQuery(`SELECT .*FROM "p115_accounts".*FOR UPDATE`).WillReturnRows(rows)
			if outcome != "missing" {
				update := mock.ExpectExec(`UPDATE "p115_accounts"`)
				if outcome == "update_failed" {
					update.WillReturnError(errors.New("fixture update failed"))
				} else {
					update.WillReturnResult(sqlmock.NewResult(0, 1))
				}
			}
			switch outcome {
			case "update_failed":
				mock.ExpectRollback()
			case "commit_failed":
				mock.ExpectCommit().WillReturnError(errors.New("fixture commit failed"))
			default:
				mock.ExpectCommit()
			}
			store := &gormAccountStore{db: database}
			uid, err := store.RevokePersonal(context.Background(), "user-1")
			if outcome == "committed" {
				if err != nil || uid != "12345" {
					t.Fatalf("committed UID=%q error=%v", uid, err)
				}
			} else if outcome == "missing" {
				if err != nil || uid != "" {
					t.Fatalf("missing UID=%q error=%v", uid, err)
				}
			} else if err == nil || uid != "" {
				t.Fatal("uncommitted UID escaped the transaction")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
