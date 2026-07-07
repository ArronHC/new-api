package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
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
	defaultOhMyCaptchaURL   = "http://ohmycaptcha:8000"
	ohMyCaptchaDummySitekey = "0x4AAAAAAAJ0j7oRWglsPwXM"
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
	BaseURL        string `json:"base_url"`
	Success        bool   `json:"success"`
	QuotaAwarded   int64  `json:"quota_awarded"`
	Error          string `json:"error,omitempty"`
	AlreadyChecked bool   `json:"already_checked"`
}

type AutoCheckinSchedulerStatus struct {
	Enabled       bool                `json:"enabled"`
	Running       bool                `json:"running"`
	Cron          string              `json:"cron"`
	LastRunDate   string              `json:"last_run_date"`
	LastRunAt     int64               `json:"last_run_at"`
	NextRunAt     int64               `json:"next_run_at"`
	LastSummary   *AutoCheckinSummary `json:"last_summary,omitempty"`
	LastError     string              `json:"last_error,omitempty"`
	SchedulerLive bool                `json:"scheduler_live"`
	IsMasterNode  bool                `json:"is_master_node"`
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
	ohMyCaptchaHTTPClient  = &http.Client{Timeout: 30 * time.Second}
	ohMyCaptchaPollEvery   = 2 * time.Second
	ohMyCaptchaMaxWait     = 90 * time.Second

	// FlareSolverr is retained only as a cf_clearance fallback for Cloudflare block pages.
	flaresolverrURL     string
	flaresolverrOnce    sync.Once
	flaresolverrEnabled bool
	// Cache cf_clearance cookies per domain (domain -> cookie header value)
	cfClearanceCache   = make(map[string]cfClearanceEntry)
	cfClearanceCacheMu sync.RWMutex
)

type cfClearanceEntry struct {
	cookie    string
	userAgent string
	expiresAt time.Time
}

type turnstileEntry struct {
	token     string
	expiresAt time.Time
}

type ohMyCaptchaCreateTaskResponse struct {
	ErrorID          int             `json:"errorId"`
	ErrorCode        string          `json:"errorCode"`
	ErrorDescription string          `json:"errorDescription"`
	TaskID           json.RawMessage `json:"taskId"`
}

type ohMyCaptchaTaskResultResponse struct {
	ErrorID          int    `json:"errorId"`
	ErrorCode        string `json:"errorCode"`
	ErrorDescription string `json:"errorDescription"`
	Status           string `json:"status"`
	Solution         struct {
		Token string `json:"token"`
	} `json:"solution"`
}

var (
	turnstileCache   = make(map[string]turnstileEntry)
	turnstileCacheMu sync.RWMutex
)

func getFlareSolverrURL() string {
	flaresolverrOnce.Do(func() {
		flaresolverrURL = strings.TrimRight(os.Getenv("FLARESOLVERR_URL"), "/")
		if flaresolverrURL == "" {
			flaresolverrURL = "http://flaresolverr:8191"
		}
		// Quick health check
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, flaresolverrURL+"/v1", nil)
		if err == nil {
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				resp.Body.Close()
				flaresolverrEnabled = true
				logger.LogInfo(context.Background(), fmt.Sprintf("FlareSolverr available at %s", flaresolverrURL))
			}
		}
		if !flaresolverrEnabled {
			logger.LogInfo(context.Background(), "FlareSolverr not available, Cloudflare bypass disabled")
		}
	})
	return flaresolverrURL
}

// flaresolverrSolve navigates to the target URL via FlareSolverr to obtain cf_clearance cookies.
func flaresolverrSolve(ctx context.Context, targetURL string) (*cfClearanceEntry, error) {
	fsURL := getFlareSolverrURL()
	if !flaresolverrEnabled {
		return nil, errors.New("FlareSolverr not available")
	}

	// Check cache first
	domain := extractDomain(targetURL)
	cfClearanceCacheMu.RLock()
	if entry, ok := cfClearanceCache[domain]; ok && time.Now().Before(entry.expiresAt) {
		cfClearanceCacheMu.RUnlock()
		return &entry, nil
	}
	cfClearanceCacheMu.RUnlock()

	reqBody, err := common.Marshal(map[string]any{
		"cmd":        "request.get",
		"url":        targetURL,
		"maxTimeout": 60000,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fsURL+"/v1", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("FlareSolverr request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}

	// Parse FlareSolverr response for cookies
	var fsResp struct {
		Status   string `json:"status"`
		Solution struct {
			Headers map[string]string `json:"headers"`
			Cookies []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"cookies"`
			UserAgent string `json:"userAgent"`
			Status    int    `json:"status"`
		} `json:"solution"`
		Message string `json:"message"`
	}
	if err := common.Unmarshal(body, &fsResp); err != nil {
		return nil, fmt.Errorf("FlareSolverr response parse error: %w", err)
	}
	if fsResp.Status != "ok" {
		return nil, fmt.Errorf("FlareSolverr error: %s", fsResp.Message)
	}

	// Build cookie string from FlareSolverr cookies
	var cookies []string
	for _, c := range fsResp.Solution.Cookies {
		if c.Name == "cf_clearance" || c.Name == "__cf_bm" {
			cookies = append(cookies, c.Name+"="+c.Value)
		}
	}
	if len(cookies) == 0 {
		return nil, fmt.Errorf("FlareSolverr returned no cf_clearance cookie (status=%d)", fsResp.Solution.Status)
	}

	entry := cfClearanceEntry{
		cookie:    strings.Join(cookies, "; "),
		userAgent: fsResp.Solution.UserAgent,
		expiresAt: time.Now().Add(15 * time.Minute), // cache for 15 min
	}

	cfClearanceCacheMu.Lock()
	cfClearanceCache[domain] = entry
	cfClearanceCacheMu.Unlock()

	logger.LogInfo(ctx, fmt.Sprintf("FlareSolverr solved challenge for %s", domain))
	return &entry, nil
}

func extractDomain(rawURL string) string {
	url := strings.TrimPrefix(rawURL, "https://")
	url = strings.TrimPrefix(url, "http://")
	if idx := strings.Index(url, "/"); idx >= 0 {
		url = url[:idx]
	}
	return url
}

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
		// Only channels with both user_id and access_token configured are
		// eligible for auto check-in. Newly added channels still appear as
		// candidates (configured via the settings UI) but are skipped here
		// until both credentials are provided, so they are neither checked
		// in nor surfaced in the status output.
		eligible := make([]Channel, 0, len(channels))
		for _, channel := range channels {
			cfg, ok := configs[channel.Id]
			if !ok || strings.TrimSpace(cfg.UserID) == "" || strings.TrimSpace(cfg.AccessToken) == "" {
				continue
			}
			eligible = append(eligible, channel)
		}
		summary.TotalChannels = len(eligible)
		summary.ChannelResults = make([]ChannelCheckinResult, 0, len(eligible))
		for _, channel := range eligible {
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

	// The credentials come exclusively from the per-channel config. The
	// channel key is no longer used as a fallback, so channels without both
	// user_id and access_token configured are never checked in.
	cfg, hasConfig := configs[channel.Id]
	if !hasConfig || strings.TrimSpace(cfg.UserID) == "" || strings.TrimSpace(cfg.AccessToken) == "" {
		result.Error = "no checkin credentials configured"
		return result
	}
	token := strings.TrimSpace(cfg.AccessToken)

	// Try GET status first, with FlareSolverr retry on Cloudflare block
	statusResp, _, err := doCheckinRequest(ctx, checkinURL, http.MethodGet, token, cfg, hasConfig, baseURL)
	if err != nil {
		result.Error = err.Error()
		return result
	}

	if statusResp != nil && statusResp.Data.Stats.CheckedInToday {
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
	postResult, _, err := doCheckinRequestPost(ctx, checkinURL, token, cfg, hasConfig, baseURL)
	if err != nil {
		if isTurnstileEmptyError(err.Error()) {
			logger.LogInfo(ctx, fmt.Sprintf("Turnstile token required for %s, attempting to obtain via OhMyCaptcha...", extractDomain(baseURL)))
			turnstileToken := getTurnstileToken(ctx, baseURL)
			if turnstileToken == "" {
				result.Error = "Turnstile token required but could not be obtained"
				return result
			}
			postResult, _, err = doCheckinRequestPostWithToken(ctx, checkinURL, token, cfg, hasConfig, baseURL, turnstileToken)
			if err != nil {
				result.Error = err.Error()
				return result
			}
		} else {
			result.Error = err.Error()
			return result
		}
	}

	message := strings.TrimSpace(postResult.Message)
	if isTurnstileEmptyError(message) {
		logger.LogInfo(ctx, fmt.Sprintf("Turnstile token required for %s, attempting to obtain via OhMyCaptcha...", extractDomain(baseURL)))
		turnstileToken := getTurnstileToken(ctx, baseURL)
		if turnstileToken == "" {
			result.Error = "Turnstile token required but could not be obtained"
			return result
		}
		postResult, _, err = doCheckinRequestPostWithToken(ctx, checkinURL, token, cfg, hasConfig, baseURL, turnstileToken)
		if err != nil {
			result.Error = err.Error()
			return result
		}
		message = strings.TrimSpace(postResult.Message)
	}
	if isAlreadyCheckedMessage(message) {
		result.Success = true
		result.AlreadyChecked = true
		result.QuotaAwarded = postResult.Data.QuotaAwarded
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

// doCheckinRequest performs a GET request with automatic FlareSolverr retry on Cloudflare block.
type checkinStatusResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Data    struct {
		Enabled bool `json:"enabled"`
		Stats   struct {
			CheckedInToday bool `json:"checked_in_today"`
			Records        []struct {
				CheckinDate  string `json:"checkin_date"`
				QuotaAwarded int64  `json:"quota_awarded"`
			} `json:"records"`
		} `json:"stats"`
	} `json:"data"`
}

func doCheckinRequest(ctx context.Context, url, method, token string, cfg channelCheckinConfig, hasConfig bool, baseURL string) (*checkinStatusResponse, []byte, error) {
	body, _, httpStatus, err := executeCheckinHTTP(ctx, url, method, token, cfg, hasConfig, baseURL, nil)
	if err != nil {
		return nil, nil, err
	}

	// Check for Cloudflare block
	if isCloudflareBlock(body, httpStatus) {
		logger.LogInfo(ctx, fmt.Sprintf("Cloudflare block detected for %s, trying FlareSolverr...", extractDomain(baseURL)))
		body, _, httpStatus, err = executeCheckinHTTPWithFlareSolverr(ctx, url, method, token, cfg, hasConfig, baseURL, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("FlareSolverr retry failed: %w", err)
		}
		if isCloudflareBlock(body, httpStatus) {
			return nil, nil, fmt.Errorf("still blocked by Cloudflare after FlareSolverr (status=%d)", httpStatus)
		}
	}

	var statusResp checkinStatusResponse
	if len(strings.TrimSpace(string(body))) > 0 {
		_ = common.Unmarshal(body, &statusResp)
	}
	return &statusResp, body, nil
}

func doCheckinRequestPost(ctx context.Context, url, token string, cfg channelCheckinConfig, hasConfig bool, baseURL string) (upstreamCheckinResponse, []byte, error) {
	body, _, httpStatus, err := executeCheckinHTTP(ctx, url, http.MethodPost, token, cfg, hasConfig, baseURL, nil)
	if err != nil {
		return upstreamCheckinResponse{}, nil, err
	}

	if isCloudflareBlock(body, httpStatus) {
		logger.LogInfo(ctx, fmt.Sprintf("Cloudflare block on POST for %s, trying FlareSolverr...", extractDomain(baseURL)))
		body, _, httpStatus, err = executeCheckinHTTPWithFlareSolverr(ctx, url, http.MethodPost, token, cfg, hasConfig, baseURL, nil)
		if err != nil {
			return upstreamCheckinResponse{}, nil, fmt.Errorf("FlareSolverr retry failed: %w", err)
		}
		if isCloudflareBlock(body, httpStatus) {
			return upstreamCheckinResponse{}, nil, fmt.Errorf("still blocked by Cloudflare after FlareSolverr (status=%d)", httpStatus)
		}
	}

	var postResult upstreamCheckinResponse
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := common.Unmarshal(body, &postResult); err != nil {
			return upstreamCheckinResponse{}, body, fmt.Errorf("invalid response: %v", err)
		}
	}
	if httpStatus < http.StatusOK || httpStatus >= http.StatusMultipleChoices {
		message := strings.TrimSpace(postResult.Message)
		if message == "" {
			message = fmt.Sprintf("HTTP %d", httpStatus)
		}
		return upstreamCheckinResponse{}, body, fmt.Errorf("%s", message)
	}
	return postResult, body, nil
}

// doCheckinRequestPostWithToken performs a POST checkin with a Turnstile token in the request body.
func doCheckinRequestPostWithToken(ctx context.Context, url, token string, cfg channelCheckinConfig, hasConfig bool, baseURL string, turnstileToken string) (upstreamCheckinResponse, []byte, error) {
	payload := map[string]string{"turnstile_token": turnstileToken}
	reqBody, err := common.Marshal(payload)
	if err != nil {
		return upstreamCheckinResponse{}, nil, fmt.Errorf("failed to marshal turnstile payload: %w", err)
	}

	body, _, httpStatus, err := executeCheckinHTTP(ctx, url, http.MethodPost, token, cfg, hasConfig, baseURL, reqBody)
	if err != nil {
		return upstreamCheckinResponse{}, nil, err
	}

	if isCloudflareBlock(body, httpStatus) {
		logger.LogInfo(ctx, fmt.Sprintf("Cloudflare block on POST with token for %s, trying FlareSolverr...", extractDomain(baseURL)))
		body, _, httpStatus, err = executeCheckinHTTPWithFlareSolverr(ctx, url, http.MethodPost, token, cfg, hasConfig, baseURL, reqBody)
		if err != nil {
			return upstreamCheckinResponse{}, nil, fmt.Errorf("FlareSolverr retry failed: %w", err)
		}
		if isCloudflareBlock(body, httpStatus) {
			return upstreamCheckinResponse{}, nil, fmt.Errorf("still blocked by Cloudflare after FlareSolverr (status=%d)", httpStatus)
		}
	}

	var postResult upstreamCheckinResponse
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := common.Unmarshal(body, &postResult); err != nil {
			return upstreamCheckinResponse{}, body, fmt.Errorf("invalid response: %v", err)
		}
	}
	if httpStatus < http.StatusOK || httpStatus >= http.StatusMultipleChoices {
		message := strings.TrimSpace(postResult.Message)
		if message == "" {
			message = fmt.Sprintf("HTTP %d", httpStatus)
		}
		return upstreamCheckinResponse{}, body, fmt.Errorf("%s", message)
	}
	return postResult, body, nil
}

func executeCheckinHTTP(ctx context.Context, url, method, token string, cfg channelCheckinConfig, hasConfig bool, baseURL string, body []byte) ([]byte, http.Header, int, error) {
	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if hasConfig && cfg.UserID != "" {
		req.Header.Set("New-Api-User", cfg.UserID)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("Accept", "application/json")
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := autoCheckinHTTPClient.Do(req)
	if err != nil {
		return nil, nil, 0, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, nil, 0, err
	}
	return respBody, resp.Header, resp.StatusCode, nil
}

func executeCheckinHTTPWithFlareSolverr(ctx context.Context, url, method, token string, cfg channelCheckinConfig, hasConfig bool, baseURL string, body []byte) ([]byte, http.Header, int, error) {
	entry, err := flaresolverrSolve(ctx, baseURL)
	if err != nil {
		// Fall back to direct request
		return executeCheckinHTTP(ctx, url, method, token, cfg, hasConfig, baseURL, body)
	}

	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if hasConfig && cfg.UserID != "" {
		req.Header.Set("New-Api-User", cfg.UserID)
	}
	if entry.userAgent != "" {
		req.Header.Set("User-Agent", entry.userAgent)
	} else {
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	}
	req.Header.Set("Cookie", entry.cookie)
	req.Header.Set("Accept", "application/json")
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := autoCheckinHTTPClient.Do(req)
	if err != nil {
		return nil, nil, 0, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, nil, 0, err
	}
	return respBody, resp.Header, resp.StatusCode, nil
}

// isCloudflareBlock detects Cloudflare challenge pages in the response.
func isCloudflareBlock(body []byte, statusCode int) bool {
	trimmedBody := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmedBody, "{") || strings.HasPrefix(trimmedBody, "[") {
		return false
	}

	if statusCode == 403 || statusCode == 503 {
		s := strings.ToLower(trimmedBody)
		if strings.Contains(s, "cf-mitigated") ||
			strings.Contains(s, "cloudflare") ||
			strings.Contains(s, "challenge-platform") ||
			strings.Contains(s, "turnstile") ||
			strings.Contains(s, "cf_chl_opt") ||
			strings.Contains(s, "just a moment") ||
			strings.Contains(s, "checking your browser") {
			return true
		}
	}
	// Also check for HTML response when JSON was expected
	if statusCode == 200 {
		s := strings.ToLower(trimmedBody)
		if strings.Contains(s, "<!doctype html") && (strings.Contains(s, "cloudflare") || strings.Contains(s, "turnstile")) {
			return true
		}
	}
	return false
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

func isTurnstileEmptyError(message string) bool {
	s := strings.ToLower(message)
	return strings.Contains(s, "turnstile token 为空") ||
		strings.Contains(s, "turnstile token为空") ||
		strings.Contains(s, "turnstile token is empty") ||
		(strings.Contains(s, "turnstile token") && strings.Contains(s, "empty"))
}

func getTurnstileToken(ctx context.Context, baseURL string) string {
	domain := extractDomain(baseURL)

	turnstileCacheMu.RLock()
	if entry, ok := turnstileCache[domain]; ok && time.Now().Before(entry.expiresAt) {
		turnstileCacheMu.RUnlock()
		return entry.token
	}
	turnstileCacheMu.RUnlock()

	token, err := ohMyCaptchaGetTurnstileToken(ctx, baseURL)
	if err == nil && token != "" {
		turnstileCacheMu.Lock()
		turnstileCache[domain] = turnstileEntry{token: token, expiresAt: time.Now().Add(2 * time.Minute)}
		turnstileCacheMu.Unlock()
		logger.LogInfo(ctx, fmt.Sprintf("Obtained Turnstile token via OhMyCaptcha for %s", domain))
		return token
	}
	if err != nil {
		logger.LogInfo(ctx, fmt.Sprintf("OhMyCaptcha Turnstile solving failed for %s: %v", domain, err))
	}

	return ""
}

func ohMyCaptchaGetTurnstileToken(ctx context.Context, baseURL string) (string, error) {
	clientKey := strings.TrimSpace(os.Getenv("OHMYCAPTCHA_KEY"))
	if clientKey == "" {
		return "", errors.New("OHMYCAPTCHA_KEY is not configured")
	}

	websiteURL, err := turnstileWebsiteURL(baseURL)
	if err != nil {
		return "", err
	}

	taskID, err := ohMyCaptchaCreateTask(ctx, clientKey, websiteURL, ohMyCaptchaDummySitekey)
	if err != nil {
		return "", err
	}
	return ohMyCaptchaPollTask(ctx, clientKey, taskID)
}

func getOhMyCaptchaURL() string {
	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("OHMYCAPTCHA_URL")), "/")
	if baseURL == "" {
		return defaultOhMyCaptchaURL
	}
	return baseURL
}

func turnstileWebsiteURL(baseURL string) (string, error) {
	pageURL := strings.TrimRight(strings.TrimSpace(baseURL), "/") + "/"
	parsedURL, err := neturl.Parse(pageURL)
	if err != nil {
		return "", err
	}
	if parsedURL.Scheme == "" || parsedURL.Host == "" {
		return "", fmt.Errorf("invalid channel base URL: %s", baseURL)
	}
	parsedURL.Path = "/profile"
	parsedURL.RawQuery = ""
	parsedURL.Fragment = ""
	return parsedURL.String(), nil
}

func ohMyCaptchaCreateTask(ctx context.Context, clientKey, websiteURL, sitekey string) (json.RawMessage, error) {
	reqBody, err := common.Marshal(map[string]any{
		"clientKey": clientKey,
		"task": map[string]string{
			"type":       "TurnstileTaskProxyless",
			"websiteURL": websiteURL,
			"websiteKey": sitekey,
		},
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, getOhMyCaptchaURL()+"/createTask", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := ohMyCaptchaHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("OhMyCaptcha createTask request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("OhMyCaptcha createTask returned HTTP %d", resp.StatusCode)
	}

	var createResp ohMyCaptchaCreateTaskResponse
	if err := common.Unmarshal(body, &createResp); err != nil {
		return nil, fmt.Errorf("OhMyCaptcha createTask response parse error: %w", err)
	}
	if createResp.ErrorID != 0 {
		return nil, fmt.Errorf("OhMyCaptcha createTask error %s: %s", createResp.ErrorCode, createResp.ErrorDescription)
	}
	if len(bytes.TrimSpace(createResp.TaskID)) == 0 {
		return nil, errors.New("OhMyCaptcha createTask returned empty taskId")
	}
	return createResp.TaskID, nil
}

func ohMyCaptchaPollTask(ctx context.Context, clientKey string, taskID json.RawMessage) (string, error) {
	deadline := time.Now().Add(ohMyCaptchaMaxWait)
	for {
		result, err := ohMyCaptchaGetTaskResult(ctx, clientKey, taskID)
		if err != nil {
			return "", err
		}
		switch strings.ToLower(result.Status) {
		case "ready":
			token := strings.TrimSpace(result.Solution.Token)
			if token == "" {
				return "", errors.New("OhMyCaptcha returned ready status without token")
			}
			return token, nil
		case "", "processing":
			if time.Now().After(deadline) {
				return "", errors.New("OhMyCaptcha task timed out")
			}
			timer := time.NewTimer(ohMyCaptchaPollEvery)
			select {
			case <-ctx.Done():
				timer.Stop()
				return "", ctx.Err()
			case <-timer.C:
			}
		default:
			return "", fmt.Errorf("OhMyCaptcha task returned unexpected status: %s", result.Status)
		}
	}
}

func ohMyCaptchaGetTaskResult(ctx context.Context, clientKey string, taskID json.RawMessage) (ohMyCaptchaTaskResultResponse, error) {
	reqBody, err := common.Marshal(map[string]any{
		"clientKey": clientKey,
		"taskId":    taskID,
	})
	if err != nil {
		return ohMyCaptchaTaskResultResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, getOhMyCaptchaURL()+"/getTaskResult", bytes.NewReader(reqBody))
	if err != nil {
		return ohMyCaptchaTaskResultResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := ohMyCaptchaHTTPClient.Do(req)
	if err != nil {
		return ohMyCaptchaTaskResultResponse{}, fmt.Errorf("OhMyCaptcha getTaskResult request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ohMyCaptchaTaskResultResponse{}, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return ohMyCaptchaTaskResultResponse{}, fmt.Errorf("OhMyCaptcha getTaskResult returned HTTP %d", resp.StatusCode)
	}

	var result ohMyCaptchaTaskResultResponse
	if err := common.Unmarshal(body, &result); err != nil {
		return ohMyCaptchaTaskResultResponse{}, fmt.Errorf("OhMyCaptcha getTaskResult response parse error: %w", err)
	}
	if result.ErrorID != 0 {
		return ohMyCaptchaTaskResultResponse{}, fmt.Errorf("OhMyCaptcha getTaskResult error %s: %s", result.ErrorCode, result.ErrorDescription)
	}
	return result, nil
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
