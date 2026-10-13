package handler

import (
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// BackupFileHandler serves local-backup payload downloads on a public,
// self-authenticating route: the URL is HMAC-signed by GetBackupDownloadURL
// with a short expiry, so no session is required (the download must work from
// a plain <a href> navigation, like an S3 presigned URL).
type BackupFileHandler struct {
	backupService *service.BackupService
}

func NewBackupFileHandler(backupService *service.BackupService) *BackupFileHandler {
	return &BackupFileHandler{backupService: backupService}
}

func (h *BackupFileHandler) Download(c *gin.Context) {
	req := service.BackupDownloadRequest{
		RecordID: c.Query("id"),
		Sig:      c.Query("sig"),
	}
	if raw := c.Query("part"); raw != "" {
		part, err := strconv.Atoi(raw)
		if err != nil || part < 0 {
			response.BadRequest(c, "invalid part parameter")
			return
		}
		req.Part = part
	}
	if raw := c.Query("exp"); raw != "" {
		exp, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || exp < 0 {
			response.BadRequest(c, "invalid exp parameter")
			return
		}
		req.Exp = exp
	}

	if err := h.backupService.VerifyBackupDownload(req); err != nil {
		response.ErrorFrom(c, err)
		return
	}

	body, fileName, err := h.backupService.OpenBackupPayload(c.Request.Context(), req)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	defer func() { _ = body.Close() }()

	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fileName))
	c.Header("Content-Type", "application/octet-stream")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Cache-Control", "no-store")
	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, body); err != nil {
		// 客户端可能中途取消下载；此时写失败是预期行为，gin 已记录。
		_ = err
	}
}

// RegisterBackupFileRoutes registers the public backup download route. The
// route is outside the admin group on purpose: authentication is carried by
// the HMAC signature and expiry in the query string.
func RegisterBackupFileRoutes(v1 *gin.RouterGroup, h *BackupFileHandler) {
	backups := v1.Group("/backups")
	backups.GET("/download", h.Download)
}
