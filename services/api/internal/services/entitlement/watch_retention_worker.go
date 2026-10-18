package entitlement

import (
	"context"
	"errors"
	"fmt"
	"github.com/konghang/ember/backend/internal/models"
	policypkg "github.com/konghang/ember/backend/internal/services/policy"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"log"
	"time"
)

type RetentionQuery func(context.Context, string, time.Time, time.Time, *time.Location) (int64, error)

// WatchRetentionWorker keeps HTTP outside transactions and revalidates state before persisting a decision.
type WatchRetentionWorker struct {
	DB       *gorm.DB
	Query    RetentionQuery
	Sync     func(string) error
	Location *time.Location
	Schedule string
}

type retentionSnapshot struct {
	User    models.User
	Holding models.UserEntitlement
	Group   models.PlanGroup
	Start   time.Time
}

// retentionApplyResult describes only committed work; skipped decisions never count as passed checks.
type retentionApplyResult struct {
	check      *models.WatchRetentionCheck
	changed    bool
	skipReason string
}

type retentionRunStats struct {
	candidates, attempted, passed, invalidated, failed            int
	skippedInactive, skippedGrace, skippedUnbound, skippedChanged int
	skippedDuplicate, syncRequested, syncDispatchFailed           int
}

// RetentionStart uses only a holding's actual activation; missing clocks and invalidated grants are not eligible.
func RetentionStart(holding models.UserEntitlement) *time.Time {
	if holding.WatchRetentionInvalidatedAt != nil || holding.WatchRetentionStartedAt == nil || holding.WatchRetentionStartedAt.IsZero() {
		return nil
	}
	return holding.WatchRetentionStartedAt
}

// Run examines current groups and reports committed outcomes and skips for this checkedAt.
// Its deferred summary also covers early failure/cancellation; abrupt process termination may leave only a start log.
func (w *WatchRetentionWorker) Run(ctx context.Context, at time.Time) (runErr error) {
	started := time.Now()
	checkedAt := at.Format(time.RFC3339Nano)
	if w.Location != nil {
		checkedAt = at.In(w.Location).Format(time.RFC3339Nano)
	}
	stats := retentionRunStats{}
	status := "interrupted"
	defer func() {
		if ctx.Err() != nil {
			status = "canceled"
		}
		log.Printf("[WatchRetention] 本轮结束 checkedAt=%s status=%s candidates=%d attempted=%d remaining=%d passed=%d invalidated=%d skippedInactive=%d skippedGrace=%d skippedUnbound=%d skippedChanged=%d skippedDuplicate=%d failed=%d syncRequested=%d syncDispatchFailed=%d duration=%s errorType=%T",
			checkedAt, status, stats.candidates, stats.attempted, stats.candidates-stats.attempted,
			stats.passed, stats.invalidated, stats.skippedInactive, stats.skippedGrace, stats.skippedUnbound,
			stats.skippedChanged, stats.skippedDuplicate, stats.failed, stats.syncRequested, stats.syncDispatchFailed, time.Since(started), runErr)
	}()
	if w.DB == nil || w.Query == nil || w.Location == nil {
		status = "failed"
		log.Printf("[WatchRetention] 任务配置不完整 checkedAt=%s", checkedAt)
		return errors.New("保号任务未配置")
	}
	var users []models.User
	if err := w.DB.WithContext(ctx).Model(&models.User{}).Select("users.*").
		Joins("JOIN plan_groups g ON g.key = users.plan_group").
		Joins("LEFT JOIN watch_retention_scan_states scan ON scan.user_id = users.id").
		Where("users.role = 'user' AND users.resource_access_granted = true AND g.watch_retention_enabled = true").
		Order("scan.last_attempt_at ASC NULLS FIRST, users.id").Find(&users).Error; err != nil {
		status = "failed"
		log.Printf("[WatchRetention] 候选用户读取失败 checkedAt=%s errorType=%T", checkedAt, err)
		return err
	}
	stats.candidates = len(users)
	log.Printf("[WatchRetention] 开始考核 candidates=%d checkedAt=%s", len(users), checkedAt)
	for _, user := range users {
		if err := ctx.Err(); err != nil {
			return err
		}
		stats.attempted++
		if err := w.recordAttempt(ctx, user.ID, at); err != nil {
			stats.failed++
			log.Printf("[WatchRetention] 扫描进度保存失败，跳过考核 checkedAt=%s userId=%s errorType=%T", checkedAt, user.ID, err)
			continue
		}
		snapshot, err := w.snapshot(ctx, user.ID, false)
		if err != nil {
			stats.failed++
			log.Printf("[WatchRetention] 读取失败 checkedAt=%s userId=%s errorType=%T", checkedAt, user.ID, err)
			continue
		}
		if snapshot == nil {
			stats.skippedInactive++
			continue
		}
		schedule := w.Schedule
		if schedule == "" {
			schedule = "0 2 * * *"
		}
		first, err := FirstWatchRetentionCheck(snapshot.Start, snapshot.Group.WatchRetentionDays, schedule, w.Location)
		if err != nil {
			stats.failed++
			log.Printf("[WatchRetention] 调度规则无效 checkedAt=%s userId=%s", checkedAt, user.ID)
			continue
		}
		if at.Before(first) {
			stats.skippedGrace++
			continue
		}
		if snapshot.User.EmbyID == "" {
			stats.skippedUnbound++
			log.Printf("[WatchRetention] 跳过未绑定用户 checkedAt=%s userId=%s", checkedAt, user.ID)
			continue
		}
		start := at.In(w.Location).AddDate(0, 0, -snapshot.Group.WatchRetentionDays)
		seconds, err := w.Query(ctx, snapshot.User.EmbyID, start, at, w.Location)
		if err != nil || seconds < 0 {
			stats.failed++
			log.Printf("[WatchRetention] 统计失败，保留权益 checkedAt=%s userId=%s group=%s errorType=%T", checkedAt, user.ID, snapshot.Group.Key, err)
			continue
		}
		result, err := w.apply(ctx, snapshot, start, at, seconds)
		if err != nil {
			stats.failed++
			log.Printf("[WatchRetention] 考核保存失败 checkedAt=%s userId=%s errorType=%T", checkedAt, user.ID, err)
			continue
		}
		switch {
		case result.skipReason == "state_changed":
			stats.skippedChanged++
		case result.skipReason == "duplicate":
			stats.skippedDuplicate++
		case result.check != nil && result.check.Invalidated:
			stats.invalidated++
		case result.check != nil:
			stats.passed++
		}
		if result.changed {
			stats.syncRequested++
		}
		if result.changed && w.Sync != nil {
			if err := w.Sync(user.ID); err != nil {
				stats.failed++
				stats.syncDispatchFailed++
				log.Printf("[WatchRetention] 权限同步失败 checkedAt=%s userId=%s errorType=%T", checkedAt, user.ID, err)
			}
		}
	}
	if stats.failed > 0 {
		status = "partial_failure"
		return fmt.Errorf("观看保号存在 %d 个处理失败", stats.failed)
	}
	status = "completed"
	return nil
}

// recordAttempt commits progress before reading/querying evidence, including attempts that later fail.
// A single upsert needs no outer transaction; older overlapping runs cannot move progress backwards.
func (w *WatchRetentionWorker) recordAttempt(ctx context.Context, userID string, at time.Time) error {
	return w.DB.WithContext(ctx).Exec(`INSERT INTO watch_retention_scan_states (user_id, last_attempt_at)
 VALUES (?, ?) ON CONFLICT (user_id) DO UPDATE
 SET last_attempt_at = GREATEST(watch_retention_scan_states.last_attempt_at, EXCLUDED.last_attempt_at)`, userID, at).Error
}

// snapshot optionally locks the user and policy for final revalidation inside a transaction.
func (w *WatchRetentionWorker) snapshot(ctx context.Context, id string, locked bool) (*retentionSnapshot, error) {
	q := w.DB.WithContext(ctx)
	uq := q
	if locked {
		uq = uq.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var user models.User
	if err := uq.Where("id = ?", id).First(&user).Error; err != nil {
		return nil, err
	}
	if user.IsAdmin() || !user.ResourceAccessGranted || user.PlanGroup == nil {
		return nil, nil
	}
	var group models.PlanGroup
	gq := q
	if locked {
		gq = gq.Clauses(clause.Locking{Strength: "SHARE"})
	}
	if err := gq.Where("key = ?", *user.PlanGroup).First(&group).Error; err != nil {
		return nil, err
	}
	if !group.WatchRetentionEnabled {
		return nil, nil
	}
	var holding models.UserEntitlement
	if err := q.Where("user_id = ? AND plan_group = ?", user.ID, group.Key).First(&holding).Error; err != nil {
		return nil, err
	}
	start := RetentionStart(holding)
	if start == nil {
		if holding.WatchRetentionInvalidatedAt == nil {
			log.Printf("[WatchRetention] 考核起点缺失，跳过用户 userId=%s group=%s reason=missing_entitlement_start", user.ID, group.Key)
		}
		return nil, nil
	}
	return &retentionSnapshot{User: user, Holding: holding, Group: group, Start: *start}, nil
}

// sameRetentionSnapshot rejects query results made stale by renewal, identity change, switching or policy edits.
func sameRetentionSnapshot(a, b *retentionSnapshot) bool {
	return a != nil && b != nil && a.User.EmbyID == b.User.EmbyID && a.Group.Key == b.Group.Key && a.Group.WatchRetentionDays == b.Group.WatchRetentionDays && a.Group.WatchRetentionMinMinutes == b.Group.WatchRetentionMinMinutes && a.Start.Equal(b.Start) && sameTime(a.Holding.ExpiresAt, b.Holding.ExpiresAt) && a.Holding.ValidityType == b.Holding.ValidityType
}

// apply atomically records a still-current decision, logging completion only after its transaction commits.
// Commit errors return no successful outcome; stale/duplicate results remain distinguishable in run summaries.
func (w *WatchRetentionWorker) apply(ctx context.Context, old *retentionSnapshot, start, at time.Time, seconds int64) (retentionApplyResult, error) {
	result := retentionApplyResult{}
	err := w.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		locked := *w
		locked.DB = tx
		fresh, err := locked.snapshot(ctx, old.User.ID, true)
		if err != nil {
			return err
		}
		if !sameRetentionSnapshot(old, fresh) {
			result.skipReason = "state_changed"
			log.Printf("[WatchRetention] 状态已变，丢弃旧统计 checkedAt=%s userId=%s", at.In(w.Location).Format(time.RFC3339Nano), old.User.ID)
			return nil
		}
		record := models.WatchRetentionCheck{UserID: fresh.User.ID, PlanGroup: fresh.Group.Key, CheckedAt: at, WindowStart: start, StartedAt: fresh.Start, PeriodDays: fresh.Group.WatchRetentionDays, MinMinutes: fresh.Group.WatchRetentionMinMinutes, WatchedSeconds: seconds, Invalidated: seconds < int64(fresh.Group.WatchRetentionMinMinutes)*60}
		write := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&record)
		if write.Error != nil {
			return write.Error
		}
		if write.RowsAffected == 0 {
			result.skipReason = "duplicate"
			return nil
		}
		if record.Invalidated {
			if err := tx.Model(&models.UserEntitlement{}).Where("user_id = ? AND plan_group = ?", record.UserID, record.PlanGroup).Update("watch_retention_invalidated_at", at).Error; err != nil {
				return err
			}
			ranks, err := Ranks(tx)
			if err != nil {
				return err
			}
			result.changed, err = ReconcileLocked(tx, &fresh.User, ranks, at)
			if err != nil {
				return err
			}
			if result.changed {
				if err := policypkg.EnqueueUserPolicySyncTx(tx, &fresh.User, "watch_retention"); err != nil {
					return err
				}
			}
		}
		result.check = &record
		return nil
	})
	if err != nil {
		return retentionApplyResult{}, err
	}
	if record := result.check; record != nil {
		log.Printf("[WatchRetention] 考核完成 checkedAt=%s userId=%s group=%s start=%s end=%s seconds=%d minMinutes=%d invalidated=%t committed=true policyChanged=%t",
			at.In(w.Location).Format(time.RFC3339Nano), record.UserID, record.PlanGroup, start.In(w.Location).Format(time.RFC3339), at.In(w.Location).Format(time.RFC3339), seconds, record.MinMinutes, record.Invalidated, result.changed)
	}
	return result, nil
}
