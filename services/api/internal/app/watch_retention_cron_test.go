package app

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"

	entitlementpkg "github.com/konghang/ember/backend/internal/services/entitlement"
	"github.com/robfig/cron/v3"
)

// TestWatchRetentionCronRegistrationLogs validates the real scheduler entry without starting services or timers.
func TestWatchRetentionCronRegistrationLogs(t *testing.T) {
	for _, expression := range []string{"0 2 * * *", "invalid"} {
		t.Run(expression, func(t *testing.T) {
			var output bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&output)
			t.Cleanup(func() { log.SetOutput(previous) })
			location, _ := time.LoadLocation("Asia/Shanghai")
			now := time.Date(2026, 10, 11, 12, 0, 0, 0, location)
			scheduler := cron.New(cron.WithLocation(location))
			worker := &entitlementpkg.WatchRetentionWorker{Location: location, Schedule: expression}
			registered := registerWatchRetentionCron(scheduler, worker, now)
			if registered != (expression != "invalid") {
				t.Fatalf("registered=%t for %q", registered, expression)
			}
			if !registered {
				if len(scheduler.Entries()) != 0 || !strings.Contains(output.String(), "调度注册失败") || strings.Contains(output.String(), "调度注册成功") {
					t.Fatalf("failed registration logged as success: %s", output.String())
				}
				return
			}
			if len(scheduler.Entries()) != 1 {
				t.Fatal("expected one registered job")
			}
			for _, field := range []string{"调度注册成功", `schedule="0 2 * * *"`, "timezone=Asia/Shanghai", "nextRun=2026-10-12T02:00:00+08:00"} {
				if !strings.Contains(output.String(), field) {
					t.Errorf("missing %q in %s", field, output.String())
				}
			}
			// The callback must surface worker failure; the unconfigured worker makes no DB or network calls.
			scheduler.Entries()[0].Job.Run()
			if !strings.Contains(output.String(), "任务未全部完成") {
				t.Errorf("worker failure was swallowed: %s", output.String())
			}
		})
	}
}
