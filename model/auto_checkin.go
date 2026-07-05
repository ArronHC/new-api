package model

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"gorm.io/gorm"
)

const (
	autoCheckinBatchSize    = 300
	autoCheckinTickInterval = time.Minute
	defaultAutoCheckinCron  = "0 0 * * *"
)

type AutoCheckinSummary struct {
	TotalUsers      int    `json:"total_users"`
	CheckedIn       int    `json:"checked_in"`
	AlreadyChecked  int    `json:"already_checked"`
	Failed          int    `json:"failed"`
	ErrorSamples    []int  `json:"error_samples,omitempty"`
	StartedAt       int64  `json:"started_at"`
	FinishedAt      int64  `json:"finished_at"`
	DurationSeconds int64  `json:"duration_seconds"`
	Trigger         string `json:"trigger"`
}

type AutoCheckinSchedulerStatus struct {
	Enabled       bool                 `json:"enabled"`
	Running       bool                 `json:"running"`
	Cron          string               `json:"cron"`
	LastRunDate   string               `json:"last_run_date"`
	LastRunAt     int64                `json:"last_run_at"`
	NextRunAt     int64                `json:"next_run_at"`
	LastSummary   *AutoCheckinSummary  `json:"last_summary,omitempty"`
	LastError     string               `json:"last_error,omitempty"`
	SchedulerLive bool                 `json:"scheduler_live"`
	IsMasterNode  bool                 `json:"is_master_node"`
}

var (
	autoCheckinOnce        sync.Once
	autoCheckinRunning     atomic.Bool
	autoCheckinStatusMu    sync.RWMutex
	autoCheckinLastRunDate string
	autoCheckinLastSummary *AutoCheckinSummary
	autoCheckinLastError   string
	autoCheckinLive        atomic.Bool
)

func StartAutoCheckinScheduler() {
	autoCheckinOnce.Do(func() {
		if !common.IsMasterNode {
			return
		}

		autoCheckinLive.Store(true)
		go func() {
			ctx := context.Background()
			logger.LogInfo(ctx, fmt.Sprintf("auto check-in scheduler started: tick=%s", autoCheckinTickInterval))

			ticker := time.NewTicker(autoCheckinTickInterval)
			defer ticker.Stop()

			runAutoCheckinIfDue(time.Now())
			for now := range ticker.C {
				runAutoCheckinIfDue(now)
			}
		}()
	})
}

func AutoCheckinSchedulerStatusSnapshot() AutoCheckinSchedulerStatus {
	setting := operation_setting.GetCheckinSetting()
	cronExpr := normalizedAutoCheckinCron(setting.AutoCheckinCron)

	autoCheckinStatusMu.RLock()
	lastRunDate := autoCheckinLastRunDate
	lastSummary := autoCheckinLastSummary
	lastError := autoCheckinLastError
	autoCheckinStatusMu.RUnlock()

	var lastRunAt int64
	if lastSummary != nil {
		summaryCopy := *lastSummary
		lastSummary = &summaryCopy
		lastRunAt = summaryCopy.FinishedAt
	}

	return AutoCheckinSchedulerStatus{
		Enabled:       setting.Enabled && setting.AutoCheckinEnabled,
		Running:       autoCheckinRunning.Load(),
		Cron:          cronExpr,
		LastRunDate:   lastRunDate,
		LastRunAt:     lastRunAt,
		NextRunAt:     nextAutoCheckinRun(time.Now(), cronExpr).Unix(),
		LastSummary:   lastSummary,
		LastError:     lastError,
		SchedulerLive: autoCheckinLive.Load(),
		IsMasterNode:  common.IsMasterNode,
	}
}

func TriggerAutoCheckinAllUsers() (*AutoCheckinSummary, error) {
	return runAutoCheckin("manual")
}

func AutoCheckinAllUsers() (*AutoCheckinSummary, error) {
	return runAutoCheckin("auto")
}

func runAutoCheckinIfDue(now time.Time) {
	setting := operation_setting.GetCheckinSetting()
	if !setting.Enabled || !setting.AutoCheckinEnabled {
		return
	}

	cronExpr := normalizedAutoCheckinCron(setting.AutoCheckinCron)
	if !autoCheckinTimeMatches(now, cronExpr) {
		return
	}

	runDate := now.Format("2006-01-02")
	autoCheckinStatusMu.RLock()
	lastRunDate := autoCheckinLastRunDate
	autoCheckinStatusMu.RUnlock()
	if lastRunDate == runDate {
		return
	}

	if _, err := runAutoCheckin("scheduled"); err != nil {
		logger.LogWarn(context.Background(), fmt.Sprintf("auto check-in scheduled run failed: %v", err))
	}
}

func runAutoCheckin(trigger string) (*AutoCheckinSummary, error) {
	if !autoCheckinRunning.CompareAndSwap(false, true) {
		return nil, errors.New("auto check-in is already running")
	}
	defer autoCheckinRunning.Store(false)

	ctx := context.Background()
	started := time.Now()
	summary := &AutoCheckinSummary{
		StartedAt: started.Unix(),
		Trigger:   trigger,
	}

	setting := operation_setting.GetCheckinSetting()
	if !setting.Enabled {
		err := errors.New("签到功能未启用")
		recordAutoCheckinResult(summary, err)
		return summary, err
	}

	var batchUsers []User
	err := DB.Model(&User{}).
		Select("id").
		Where("status = ?", common.UserStatusEnabled).
		Order("id asc").
		FindInBatches(&batchUsers, autoCheckinBatchSize, func(tx *gorm.DB, batchNum int) error {
			for _, user := range batchUsers {
				summary.TotalUsers++
				checkin, err := UserCheckin(user.Id)
				if err != nil {
					if strings.Contains(err.Error(), "今日已签到") {
						summary.AlreadyChecked++
						continue
					}
					summary.Failed++
					if len(summary.ErrorSamples) < 10 {
						summary.ErrorSamples = append(summary.ErrorSamples, user.Id)
					}
					logger.LogWarn(ctx, fmt.Sprintf("auto check-in: user_id=%d failed: %v", user.Id, err))
					continue
				}
				if checkin != nil {
					summary.CheckedIn++
				}
			}
			return nil
		}).Error

	recordAutoCheckinResult(summary, err)
	if err != nil {
		logger.LogError(ctx, fmt.Sprintf("auto check-in failed: %v", err))
		return summary, err
	}

	logger.LogInfo(ctx, fmt.Sprintf(
		"auto check-in completed: trigger=%s total=%d checked_in=%d already_checked=%d failed=%d",
		trigger,
		summary.TotalUsers,
		summary.CheckedIn,
		summary.AlreadyChecked,
		summary.Failed,
	))
	return summary, nil
}

func recordAutoCheckinResult(summary *AutoCheckinSummary, err error) {
	now := time.Now()
	summary.FinishedAt = now.Unix()
	summary.DurationSeconds = int64(now.Sub(time.Unix(summary.StartedAt, 0)).Seconds())

	autoCheckinStatusMu.Lock()
	defer autoCheckinStatusMu.Unlock()

	autoCheckinLastSummary = summary
	if err != nil {
		autoCheckinLastError = err.Error()
		return
	}
	autoCheckinLastError = ""
	autoCheckinLastRunDate = now.Format("2006-01-02")
}

func normalizedAutoCheckinCron(cronExpr string) string {
	cronExpr = strings.TrimSpace(cronExpr)
	if cronExpr == "" {
		return defaultAutoCheckinCron
	}
	return cronExpr
}

func autoCheckinTimeMatches(now time.Time, cronExpr string) bool {
	minute, hour, ok := autoCheckinCronHourMinute(cronExpr)
	if !ok {
		minute, hour, _ = autoCheckinCronHourMinute(defaultAutoCheckinCron)
	}
	return now.Minute() == minute && now.Hour() == hour
}

func nextAutoCheckinRun(now time.Time, cronExpr string) time.Time {
	minute, hour, ok := autoCheckinCronHourMinute(cronExpr)
	if !ok {
		minute, hour, _ = autoCheckinCronHourMinute(defaultAutoCheckinCron)
	}

	next := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next
}

func autoCheckinCronHourMinute(cronExpr string) (int, int, bool) {
	parts := strings.Fields(cronExpr)
	if len(parts) != 5 || parts[2] != "*" || parts[3] != "*" || parts[4] != "*" {
		return 0, 0, false
	}

	minute, err := strconv.Atoi(parts[0])
	if err != nil || minute < 0 || minute > 59 {
		return 0, 0, false
	}
	hour, err := strconv.Atoi(parts[1])
	if err != nil || hour < 0 || hour > 23 {
		return 0, 0, false
	}
	return minute, hour, true
}

