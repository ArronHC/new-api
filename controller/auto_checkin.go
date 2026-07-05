package controller

import (
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

// GetAutoCheckinStatus 获取自动签到调度器状态
func GetAutoCheckinStatus(c *gin.Context) {
	status := model.AutoCheckinSchedulerStatusSnapshot()
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    status,
	})
}

// TriggerAutoCheckin 手动触发全量自动签到
func TriggerAutoCheckin(c *gin.Context) {
	summary, err := model.TriggerAutoCheckinAllUsers()
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "自动签到执行完成",
		"data":    summary,
	})
}
