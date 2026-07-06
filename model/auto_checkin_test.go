package model

import (
	"net/http"
	"net/http/httptest"
	"testing"

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
				_, _ = w.Write([]byte(`{"success":true,"data":{"enabled":true,"stats":{"checked_in_today":true,"records":[{"checkin_date":"2006-01-02","quota_awarded":999}]}}}`))
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

	summary, err := AutoCheckinAllChannels()
	require.NoError(t, err)
	assert.Equal(t, 3, summary.TotalChannels)
	assert.Equal(t, 1, summary.ChannelsCheckedIn)
	assert.Equal(t, 1, summary.ChannelsAlreadyChecked)
	assert.Equal(t, 1, summary.ChannelsFailed)
	require.Len(t, summary.ChannelResults, 3)

	assert.Equal(t, ChannelCheckinResult{
		ChannelID:    1,
		ChannelName:  "success",
		BaseURL:      baseURL,
		Success:      true,
		QuotaAwarded: 1234,
	}, summary.ChannelResults[0])
	assert.Equal(t, ChannelCheckinResult{
		ChannelID:     2,
		ChannelName:   "already",
		BaseURL:       baseURL,
		Success:       true,
		AlreadyChecked: true,
		QuotaAwarded:  999,
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
