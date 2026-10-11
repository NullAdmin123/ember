package policy

import (
	"errors"
	"log"
	"strings"

	"github.com/konghang/ember/backend/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// EnqueueUserPolicySyncTx persists a new Policy intent in the caller's user-state transaction.
// Replacing an active task's ID fences its old worker; its batch membership remains intact.
// No external requests occur here. A committed pending task is recoverable after process exit.
func EnqueueUserPolicySyncTx(tx *gorm.DB, user *models.User, reason string) error {
	if tx == nil || user == nil || user.ID == "" {
		return errors.New("Policy 同步任务缺少事务或用户")
	}
	if user.IsAdmin() || strings.TrimSpace(user.EmbyID) == "" {
		return nil
	}
	group := ""
	if user.PlanGroup != nil {
		group = *user.PlanGroup
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "unspecified"
	}
	task := models.EmbyPolicySyncTask{UserID: user.ID, EmbyID: user.EmbyID,
		PlanGroupKey: group, Reason: reason, Status: SyncStatusPending}
	if err := tx.Clauses(clause.OnConflict{
		Columns:     []clause.Column{{Name: "user_id"}},
		TargetWhere: clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: "status IN ('pending', 'processing')"}}},
		// Keep batch_id: an older batch still needs to track this user's final synchronization.
		DoUpdates: clause.AssignmentColumns([]string{"id", "emby_id", "plan_group_key", "reason", "status", "attempts", "last_error", "next_retry_at", "created_at", "updated_at"}),
	}).Create(&task).Error; err != nil {
		return err
	}
	log.Printf("[Policy] 同步任务已写入事务 userId=%s taskId=%s group=%s reason=%s", user.ID, task.ID, group, reason)
	return nil
}
