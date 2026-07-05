package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAutoCheckinAllUsersChecksInEnabledUsersOnce(t *testing.T) {
	truncateTables(t)
	require.NoError(t, DB.AutoMigrate(&Checkin{}))
	require.NoError(t, DB.Exec("DELETE FROM checkins").Error)

	setting := operation_setting.GetCheckinSetting()
	original := *setting
	t.Cleanup(func() {
		*setting = original
	})
	setting.Enabled = true
	setting.MinQuota = 10
	setting.MaxQuota = 10

	enabledUser := &User{Id: 1, Username: "enabled", Status: common.UserStatusEnabled}
	anotherEnabledUser := &User{Id: 2, Username: "another", Status: common.UserStatusEnabled}
	disabledUser := &User{Id: 3, Username: "disabled", Status: common.UserStatusDisabled}
	require.NoError(t, DB.Create(enabledUser).Error)
	require.NoError(t, DB.Create(anotherEnabledUser).Error)
	require.NoError(t, DB.Create(disabledUser).Error)

	first, err := AutoCheckinAllUsers()
	require.NoError(t, err)
	assert.Equal(t, 2, first.TotalUsers)
	assert.Equal(t, 2, first.CheckedIn)
	assert.Equal(t, 0, first.AlreadyChecked)
	assert.Equal(t, 0, first.Failed)

	second, err := AutoCheckinAllUsers()
	require.NoError(t, err)
	assert.Equal(t, 2, second.TotalUsers)
	assert.Equal(t, 0, second.CheckedIn)
	assert.Equal(t, 2, second.AlreadyChecked)
	assert.Equal(t, 0, second.Failed)

	var users []User
	require.NoError(t, DB.Order("id asc").Find(&users).Error)
	require.Len(t, users, 3)
	assert.Equal(t, 10, users[0].Quota)
	assert.Equal(t, 10, users[1].Quota)
	assert.Equal(t, 0, users[2].Quota)
}

