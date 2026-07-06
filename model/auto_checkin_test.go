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
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/user/checkin", r.URL.Path)

		switch r.Header.Get("Authorization") {
		case "Bearer success-token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"message":"签到成功","data":{"quota_awarded":1234}}`))
		case "Bearer already-token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":false,"message":"今天已经签到"}`))
		case "Bearer failed-token":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"success":false,"message":"upstream unavailable"}`))
		default:
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
		ChannelID:      2,
		ChannelName:    "already",
		BaseURL:         baseURL,
		Success:         true,
		AlreadyChecked:  true,
		QuotaAwarded:    0,
		Error:           "今天已经签到",
	}, summary.ChannelResults[1])
	assert.Equal(t, 3, summary.ChannelResults[2].ChannelID)
	assert.False(t, summary.ChannelResults[2].Success)
	assert.Equal(t, "upstream unavailable", summary.ChannelResults[2].Error)
}
