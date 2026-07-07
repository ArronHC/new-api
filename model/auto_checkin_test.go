package model

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAutoCheckinAllChannelsChecksActiveUpstreamChannels(t *testing.T) {
	truncateTables(t)

	setting := operation_setting.GetCheckinSetting()
	original := *setting
	t.Cleanup(func() {
		*setting = original
	})
	setting.Enabled = true

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/user/checkin", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")

		// Determine which channel by cookie or auth header
		cookie := r.Header.Get("Cookie")
		auth := r.Header.Get("Authorization")

		if r.Method == http.MethodGet {
			// Status check - return not checked in
			if cookie == "session=sess-ok" || auth == "Bearer success-token" {
				_, _ = w.Write([]byte(`{"success":true,"data":{"enabled":true,"stats":{"checked_in_today":false}}}`))
			} else if cookie == "session=sess-already" || auth == "Bearer already-token" {
				body := `{"success":true,"data":{"enabled":true,"stats":{"checked_in_today":true,"records":[{"checkin_date":"` + time.Now().Format("2006-01-02") + `","quota_awarded":999}]}}}`
				_, _ = w.Write([]byte(body))
			} else if auth == "Bearer failed-token" {
				_, _ = w.Write([]byte(`{"success":true,"data":{"enabled":true,"stats":{"checked_in_today":false}}}`))
			} else {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"success":false,"message":"bad auth"}`))
			}
			return
		}

		// POST - actual checkin
		if cookie == "session=sess-ok" || auth == "Bearer success-token" {
			_, _ = w.Write([]byte(`{"success":true,"message":"签到成功","data":{"quota_awarded":1234}}`))
		} else if cookie == "session=sess-already" || auth == "Bearer already-token" {
			_, _ = w.Write([]byte(`{"success":false,"message":"今天已经签到"}`))
		} else if auth == "Bearer failed-token" {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"success":false,"message":"upstream unavailable"}`))
		} else {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"success":false,"message":"bad token"}`))
		}
	}))
	t.Cleanup(server.Close)

	baseURL := server.URL
	require.NoError(t, DB.Create(&Channel{Id: 1, Name: "success", Key: "success-token", BaseURL: &baseURL, Status: common.ChannelStatusEnabled}).Error)
	require.NoError(t, DB.Create(&Channel{Id: 2, Name: "already", Key: "already-token", BaseURL: &baseURL, Status: common.ChannelStatusEnabled}).Error)
	require.NoError(t, DB.Create(&Channel{Id: 3, Name: "failed", Key: "failed-token", BaseURL: &baseURL, Status: common.ChannelStatusEnabled}).Error)
	require.NoError(t, DB.Create(&Channel{Id: 4, Name: "disabled", Key: "success-token", BaseURL: &baseURL, Status: common.ChannelStatusManuallyDisabled}).Error)
	// Channel 5 is enabled but has no credentials configured: it must be a
	// candidate (loaded) but skipped — not checked in, not in the status list.
	require.NoError(t, DB.Create(&Channel{Id: 5, Name: "no-creds", Key: "success-token", BaseURL: &baseURL, Status: common.ChannelStatusEnabled}).Error)

	// Only channels with both user_id and access_token configured are eligible.
	require.NoError(t, DB.Create(&Option{Key: "checkin_channel_configs", Value: `{"1":{"user_id":"u1","access_token":"success-token"},"2":{"user_id":"u2","access_token":"already-token"},"3":{"user_id":"u3","access_token":"failed-token"}}`}).Error)

	summary, err := AutoCheckinAllChannels()
	require.NoError(t, err)
	assert.Equal(t, 3, summary.TotalChannels)
	assert.Equal(t, 1, summary.ChannelsCheckedIn)
	assert.Equal(t, 1, summary.ChannelsAlreadyChecked)
	assert.Equal(t, 1, summary.ChannelsFailed)
	require.Len(t, summary.ChannelResults, 3)

	for _, r := range summary.ChannelResults {
		assert.NotEqual(t, 5, r.ChannelID, "channel without credentials must not appear in status list")
	}

	assert.Equal(t, ChannelCheckinResult{
		ChannelID:    1,
		ChannelName:  "success",
		BaseURL:      baseURL,
		Success:      true,
		QuotaAwarded: 1234,
	}, summary.ChannelResults[0])
	assert.Equal(t, ChannelCheckinResult{
		ChannelID:      2,
		ChannelName:    "already",
		BaseURL:        baseURL,
		Success:        true,
		AlreadyChecked: true,
		QuotaAwarded:   999,
	}, summary.ChannelResults[1])
	assert.Equal(t, 3, summary.ChannelResults[2].ChannelID)
	assert.False(t, summary.ChannelResults[2].Success)
	assert.Equal(t, "upstream unavailable", summary.ChannelResults[2].Error)
}

func TestAutoCheckinWithSessionConfig(t *testing.T) {
	truncateTables(t)

	setting := operation_setting.GetCheckinSetting()
	original := *setting
	t.Cleanup(func() {
		*setting = original
	})
	setting.Enabled = true

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		auth := r.Header.Get("Authorization")

		if r.Method == http.MethodGet {
			if auth == "Bearer test-token" {
				_, _ = w.Write([]byte(`{"success":true,"data":{"enabled":true,"stats":{"checked_in_today":false}}}`))
			} else {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"success":false,"message":"Unauthorized"}`))
			}
			return
		}

		// POST
		if auth == "Bearer test-token" {
			_, _ = w.Write([]byte(`{"success":true,"message":"签到成功","data":{"quota_awarded":5678}}`))
		} else {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"success":false,"message":"Unauthorized"}`))
		}
	}))
	t.Cleanup(server.Close)

	baseURL := server.URL
	require.NoError(t, DB.Create(&Channel{Id: 10, Name: "session-test", Key: "unused", BaseURL: &baseURL, Status: common.ChannelStatusEnabled}).Error)

	// Store access_token config in options
	require.NoError(t, DB.Create(&Option{Key: "checkin_channel_configs", Value: `{"10":{"user_id":"42","access_token":"test-token"}}`}).Error)

	summary, err := AutoCheckinAllChannels()
	require.NoError(t, err)
	assert.Equal(t, 1, summary.TotalChannels)
	assert.Equal(t, 1, summary.ChannelsCheckedIn)
	require.Len(t, summary.ChannelResults, 1)
	assert.True(t, summary.ChannelResults[0].Success)
	assert.Equal(t, int64(5678), summary.ChannelResults[0].QuotaAwarded)
}

func TestAutoCheckinSkipsChannelsWithoutBothCredentials(t *testing.T) {
	truncateTables(t)

	setting := operation_setting.GetCheckinSetting()
	original := *setting
	t.Cleanup(func() {
		*setting = original
	})
	setting.Enabled = true

	// Track whether the upstream checkin endpoint is hit; channels without
	// both credentials must never reach it.
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`{"success":true,"data":{"enabled":true,"stats":{"checked_in_today":false}}}`))
	}))
	t.Cleanup(server.Close)

	baseURL := server.URL
	require.NoError(t, DB.Create(&Channel{Id: 100, Name: "both", BaseURL: &baseURL, Status: common.ChannelStatusEnabled}).Error)
	require.NoError(t, DB.Create(&Channel{Id: 101, Name: "user-id-only", BaseURL: &baseURL, Status: common.ChannelStatusEnabled}).Error)
	require.NoError(t, DB.Create(&Channel{Id: 102, Name: "access-token-only", BaseURL: &baseURL, Status: common.ChannelStatusEnabled}).Error)
	require.NoError(t, DB.Create(&Channel{Id: 103, Name: "no-config", BaseURL: &baseURL, Status: common.ChannelStatusEnabled}).Error)

	require.NoError(t, DB.Create(&Option{Key: "checkin_channel_configs", Value: `{"100":{"user_id":"u100","access_token":"tok-100"},"101":{"user_id":"u101","access_token":""},"102":{"user_id":"","access_token":"tok-102"}}`}).Error)

	summary, err := AutoCheckinAllChannels()
	require.NoError(t, err)
	assert.Equal(t, 1, summary.TotalChannels, "only the channel with both credentials is eligible")
	require.Len(t, summary.ChannelResults, 1)
	assert.Equal(t, 100, summary.ChannelResults[0].ChannelID)

	for _, r := range summary.ChannelResults {
		assert.NotEqual(t, 101, r.ChannelID)
		assert.NotEqual(t, 102, r.ChannelID)
		assert.NotEqual(t, 103, r.ChannelID)
	}
	assert.Equal(t, int32(2), atomic.LoadInt32(&hits), "eligible channel should perform status and check-in requests only")
}

func TestGetTurnstileTokenExtractsSolutionTokenAndCachesByDomain(t *testing.T) {
	resetTurnstileTestState(t)

	var createTaskCalls int
	var getTaskResultCalls int
	var upstreamURL string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			t.Errorf("upstream root page should not be fetched for sitekey extraction")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		switch r.URL.Path {
		case "/createTask":
			createTaskCalls++
			var req struct {
				ClientKey string `json:"clientKey"`
				Task      struct {
					Type       string `json:"type"`
					WebsiteURL string `json:"websiteURL"`
					WebsiteKey string `json:"websiteKey"`
				} `json:"task"`
			}
			require.NoError(t, common.DecodeJson(r.Body, &req))
			assert.Equal(t, "test-ohmycaptcha-key", req.ClientKey)
			assert.Equal(t, "TurnstileTaskProxyless", req.Task.Type)
			assert.Equal(t, upstreamURL+"/profile", req.Task.WebsiteURL)
			assert.Equal(t, "0x4AAAAAAAJ0j7oRWglsPwXM", req.Task.WebsiteKey)

			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"errorId":0,"taskId":"task-123"}`))
		case "/getTaskResult":
			getTaskResultCalls++
			var req struct {
				ClientKey string `json:"clientKey"`
				TaskID    string `json:"taskId"`
			}
			require.NoError(t, common.DecodeJson(r.Body, &req))
			assert.Equal(t, "test-ohmycaptcha-key", req.ClientKey)
			assert.Equal(t, "task-123", req.TaskID)

			w.Header().Set("Content-Type", "application/json")
			if getTaskResultCalls == 1 {
				_, _ = w.Write([]byte(`{"errorId":0,"status":"processing"}`))
				return
			}
			_, _ = w.Write([]byte(`{"errorId":0,"status":"ready","solution":{"token":"token-from-ohmycaptcha"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(upstream.Close)
	upstreamURL = upstream.URL
	t.Setenv("OHMYCAPTCHA_URL", upstream.URL)
	t.Setenv("OHMYCAPTCHA_KEY", "test-ohmycaptcha-key")

	token := getTurnstileToken(context.Background(), upstream.URL+"/api")
	require.Equal(t, "token-from-ohmycaptcha", token)

	token = getTurnstileToken(context.Background(), upstream.URL+"/other")
	require.Equal(t, "token-from-ohmycaptcha", token)
	assert.Equal(t, 1, createTaskCalls)
	assert.Equal(t, 2, getTaskResultCalls)
}

func TestAutoCheckinRetriesPostWithTurnstileToken(t *testing.T) {
	truncateTables(t)
	resetTurnstileTestState(t)

	setting := operation_setting.GetCheckinSetting()
	original := *setting
	t.Cleanup(func() {
		*setting = original
	})
	setting.Enabled = true

	var upstreamURL string
	ohmycaptcha := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/createTask":
			var req struct {
				ClientKey string `json:"clientKey"`
				Task      struct {
					Type       string `json:"type"`
					WebsiteURL string `json:"websiteURL"`
					WebsiteKey string `json:"websiteKey"`
				} `json:"task"`
			}
			require.NoError(t, common.DecodeJson(r.Body, &req))
			assert.Equal(t, "test-ohmycaptcha-key", req.ClientKey)
			assert.Equal(t, "TurnstileTaskProxyless", req.Task.Type)
			assert.Equal(t, upstreamURL+"/profile", req.Task.WebsiteURL)
			assert.Equal(t, "0x4AAAAAAAJ0j7oRWglsPwXM", req.Task.WebsiteKey)
			_, _ = w.Write([]byte(`{"errorId":0,"taskId":"retry-task"}`))
		case "/getTaskResult":
			_, _ = w.Write([]byte(`{"errorId":0,"status":"ready","solution":{"token":"retry-token"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(ohmycaptcha.Close)
	t.Setenv("OHMYCAPTCHA_URL", ohmycaptcha.URL)
	t.Setenv("OHMYCAPTCHA_KEY", "test-ohmycaptcha-key")

	var postBodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			t.Errorf("upstream root page should not be fetched for sitekey extraction")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))

		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"success":true,"data":{"enabled":true,"stats":{"checked_in_today":false}}}`))
			return
		}

		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		postBodies = append(postBodies, string(body))

		if len(postBodies) == 1 {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"success":false,"message":"Turnstile token 为空"}`))
			return
		}

		assert.JSONEq(t, `{"turnstile_token":"retry-token"}`, string(body))
		_, _ = w.Write([]byte(`{"success":true,"message":"签到成功","data":{"quota_awarded":2468}}`))
	}))
	t.Cleanup(upstream.Close)
	upstreamURL = upstream.URL

	baseURL := upstream.URL
	require.NoError(t, DB.Create(&Channel{Id: 20, Name: "turnstile", Key: "test-token", BaseURL: &baseURL, Status: common.ChannelStatusEnabled}).Error)
	require.NoError(t, DB.Create(&Option{Key: "checkin_channel_configs", Value: `{"20":{"user_id":"u20","access_token":"test-token"}}`}).Error)

	summary, err := AutoCheckinAllChannels()
	require.NoError(t, err)
	require.Len(t, summary.ChannelResults, 1)
	assert.True(t, summary.ChannelResults[0].Success)
	assert.Equal(t, int64(2468), summary.ChannelResults[0].QuotaAwarded)
	require.Len(t, postBodies, 2)
	assert.Empty(t, postBodies[0])
}

func resetTurnstileTestState(t *testing.T) {
	t.Helper()

	flaresolverrOnce = sync.Once{}
	flaresolverrURL = ""
	flaresolverrEnabled = false
	turnstileCacheMu.Lock()
	turnstileCache = make(map[string]turnstileEntry)
	turnstileCacheMu.Unlock()
	cfClearanceCacheMu.Lock()
	cfClearanceCache = make(map[string]cfClearanceEntry)
	cfClearanceCacheMu.Unlock()

	t.Cleanup(func() {
		flaresolverrOnce = sync.Once{}
		flaresolverrURL = ""
		flaresolverrEnabled = false
		turnstileCacheMu.Lock()
		turnstileCache = make(map[string]turnstileEntry)
		turnstileCacheMu.Unlock()
		cfClearanceCacheMu.Lock()
		cfClearanceCache = make(map[string]cfClearanceEntry)
		cfClearanceCacheMu.Unlock()
	})
}
