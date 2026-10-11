package models

import "time"

// WatchRetentionScanState tracks scheduling progress independently of decision evidence.
// Failed or interrupted attempts advance fairness without inventing a viewing result.
type WatchRetentionScanState struct {
	UserID        string    `gorm:"column:user_id;type:varchar(25);primaryKey"`
	LastAttemptAt time.Time `gorm:"column:last_attempt_at;not null"`
}

// TableName fixes the SQL contract for durable retention scan progress.
func (WatchRetentionScanState) TableName() string { return "watch_retention_scan_states" }
