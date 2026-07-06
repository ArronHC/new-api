package model

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/setting/operation_setting"
)

const (
	autoCheckinTickInterval = time.Minute
	defaultAutoCheckinCron  = "0 0 * * *"
)

type AutoCheckinSummary struct {
	TotalChannels          int                    `json:"total_channels"`
	ChannelsCheckedIn      int                    `json:"channels_checked_in"`
	ChannelsAlreadyChecked int                    `json:"channels_already_checked"`
	ChannelsFailed         int                    `json:"channels_failed"`
	ChannelResults         []ChannelCheckinResult `json:"channel_results"`
	StartedAt              int64                  `json:"started_at"`
	FinishedAt             int64                  `json:"finished_at"`
	DurationSeconds        int64                  `json:"duration_seconds"`
	Trigger                string                 `json:"trigger"`
}

type ChannelCheckinResult struct {
	ChannelID      int    `json:"channel_id"`
	ChannelName    string `json:"channel_name"`
	BaseURL         string `json:"base_url"`
	Success         bool   `json:"success"`
	QuotaAwarded    int64  `json:"quota_awarded"`
	Error           string `json:"error,omitempty"`
	AlreadyChecked  bool   `json:"already_checked"`
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
	autoCheckinHTTPClient  = &http.Client{Timeout: 30 * time.Second}
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

func TriggerAutoCheckinAllChannels() (*AutoCheckinSummary, error) {
	return runAutoCheckin("manual")
}

func AutoCheckinAllChannels() (*AutoCheckinSummary, error) {
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

	var channels []Channel
	err := DB.Model(&Channel{}).
		Where("status = ?", common.ChannelStatusEnabled).
		Order("id asc").
		Find(&channels).Error
	if err == nil {
		configs := loadChannelCheckinConfigs()
		summary.TotalChannels = len(channels)
		summary.ChannelResults = make([]ChannelCheckinResult, 0, len(channels))
		for _, channel := range channels {
			result := checkinChannel(ctx, channel, configs)
			summary.ChannelResults = append(summary.ChannelResults, result)
			switch {
			case result.AlreadyChecked:
				summary.ChannelsAlreadyChecked++
			case result.Success:
				summary.ChannelsCheckedIn++
			default:
				summary.ChannelsFailed++
				logger.LogWarn(ctx, fmt.Sprintf("auto check-in: channel_id=%d name=%s failed: %s", result.ChannelID, result.ChannelName, result.Error))
			}
		}
	}

	recordAutoCheckinResult(summary, err)
	if err != nil {
		logger.LogError(ctx, fmt.Sprintf("auto check-in failed: %v", err))
		return summary, err
	}

	logger.LogInfo(ctx, fmt.Sprintf(
		"auto check-in completed: trigger=%s total_channels=%d checked_in=%d already_checked=%d failed=%d",
		trigger,
		summary.TotalChannels,
		summary.ChannelsCheckedIn,
		summary.ChannelsAlreadyChecked,
		summary.ChannelsFailed,
	))
	return summary, nil
}

type upstreamCheckinResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Data    struct {
		QuotaAwarded int64 `json:"quota_awarded"`
	} `json:"data"`
}

type channelCheckinConfig struct {
	UserID      string `json:"user_id"`
	AccessToken string `json:"access_token"`
}

func loadChannelCheckinConfigs() map[int]channelCheckinConfig {
	configs := make(map[int]channelCheckinConfig)
	option := Option{}
	if err := DB.Where("`key` = ?", "checkin_channel_configs").First(&option).Error; err != nil {
		return configs
	}
	_ = common.UnmarshalJsonStr(option.Value, &configs)
	return configs
}

func checkinChannel(ctx context.Context, channel Channel, configs map[int]channelCheckinConfig) ChannelCheckinResult {
	baseURL := ""
	if channel.BaseURL != nil {
		baseURL = strings.TrimSpace(*channel.BaseURL)
	}
	result := ChannelCheckinResult{
		ChannelID:   channel.Id,
		ChannelName: channel.Name,
		BaseURL:     baseURL,
	}

	if baseURL == "" {
		result.Error = "channel base URL is empty"
		return result
	}

	checkinURL := strings.TrimRight(baseURL, "/") + "/api/user/checkin"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checkinURL, nil)
	if err != nil {
		result.Error = err.Error()
		return result
	}

	// Use access_token from config, fall back to channel key
	cfg, hasConfig := configs[channel.Id]
	token := ""
	if hasConfig && cfg.AccessToken != "" {
		token = cfg.AccessToken
	} else {
		token = strings.TrimSpace(channel.Key)
	}
	if token == "" {
		result.Error = "no checkin credentials configured and channel key is empty"
		return result
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if hasConfig && cfg.UserID != "" {
		req.Header.Set("New-Api-User", cfg.UserID)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Accept", "application/json")

	resp, err := autoCheckinHTTPClient.Do(req)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		result.Error = err.Error()
		return result
	}

	// Check if already checked in via GET status
	var statusResp struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
		Data    struct {
			Enabled bool `json:"enabled"`
			Stats   struct {
				CheckedInToday bool  `json:"checked_in_today"`
				Records        []struct {
					CheckinDate   string `json:"checkin_date"`
					QuotaAwarded  int64  `json:"quota_awarded"`
				} `json:"records"`
			} `json:"stats"`
		} `json:"data"`
	}
	if len(strings.TrimSpace(string(body))) > 0 {
		_ = common.Unmarshal(body, &statusResp)
	}

	if statusResp.Data.Stats.CheckedInToday {
		result.Success = true
		result.AlreadyChecked = true
		today := time.Now().Format("2006-01-02")
		for _, rec := range statusResp.Data.Stats.Records {
			if rec.CheckinDate == today {
				result.QuotaAwarded = rec.QuotaAwarded
				break
			}
		}
		return result
	}

	// Not checked in yet, do POST checkin
	postReq, err := http.NewRequestWithContext(ctx, http.MethodPost, checkinURL, nil)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	postReq.Header = req.Header.Clone()

	postResp, err := autoCheckinHTTPClient.Do(postReq)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	defer postResp.Body.Close()

	postBody, err := io.ReadAll(io.LimitReader(postResp.Body, 1<<20))
	if err != nil {
		result.Error = err.Error()
		return result
	}

	var postResult upstreamCheckinResponse
	if len(strings.TrimSpace(string(postBody))) > 0 {
		if err := common.Unmarshal(postBody, &postResult); err != nil {
			result.Error = fmt.Sprintf("invalid response: %v", err)
			return result
		}
	}

	message := strings.TrimSpace(postResult.Message)
	if isAlreadyCheckedMessage(message) {
		result.Success = true
		result.AlreadyChecked = true
		result.QuotaAwarded = postResult.Data.QuotaAwarded
		return result
	}
	if postResp.StatusCode < http.StatusOK || postResp.StatusCode >= http.StatusMultipleChoices {
		if message == "" {
			message = postResp.Status
		}
		result.Error = message
		return result
	}
	if !postResult.Success {
		if message == "" {
			message = "upstream check-in failed"
		}
		result.Error = message
		return result
	}

	result.Success = true
	result.QuotaAwarded = postResult.Data.QuotaAwarded
	return result
}

func isAlreadyCheckedMessage(message string) bool {
	message = strings.ToLower(message)
	return strings.Contains(message, "已经签到") ||
		strings.Contains(message, "今日已签到") ||
		strings.Contains(message, "今天已签到") ||
		strings.Contains(message, "今天已经签到") ||
		strings.Contains(message, "already checked") ||
		strings.Contains(message, "already check")
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
