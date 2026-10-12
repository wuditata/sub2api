//go:build unit

package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

func newLocalBackupTestService(t *testing.T) (*BackupService, *mockSettingRepo, *mockObjectStore) {
	t.Helper()
	repo := newMockSettingRepo()
	store := newMockObjectStore()
	cfg := &config.Config{
		Database: config.DatabaseConfig{Host: "localhost", Port: 5432, User: "test", DBName: "testdb"},
		Totp:     config.TotpConfig{EncryptionKeyConfigured: true},
		JWT:      config.JWTConfig{Secret: "test-jwt-secret"},
	}
	factory := func(_ context.Context, _ *BackupS3Config) (BackupObjectStore, error) {
		return store, nil
	}
	return NewBackupService(repo, cfg, &plainEncryptor{}, factory, &mockDumper{}), repo, store
}

func seedLocalBackupConfig(t *testing.T, repo *mockSettingRepo) {
	t.Helper()
	cfg := BackupS3Config{StorageType: BackupStorageTypeLocal}
	data, _ := json.Marshal(cfg)
	require.NoError(t, repo.Set(context.Background(), settingKeyBackupS3Config, string(data)))
}

func seedCompletedLocalBackup(t *testing.T, repo *mockSettingRepo, store *mockObjectStore) BackupRecord {
	t.Helper()
	seedLocalBackupConfig(t, repo)
	record := BackupRecord{
		ID:          "local01",
		Status:      "completed",
		BackupType:  "postgres",
		FileName:    "testdb_20261012_000000_local01.sql.gz",
		S3Key:       "backups/2026/10/12/testdb_20261012_000000_local01.sql.gz",
		TriggeredBy: "manual",
		StartedAt:   time.Now().Format(time.RFC3339),
	}
	records, _ := json.Marshal([]BackupRecord{record})
	require.NoError(t, repo.Set(context.Background(), settingKeyBackupRecords, string(records)))
	if store != nil {
		_, err := store.Upload(context.Background(), record.S3Key, strings.NewReader("gz-bytes"), "application/gzip")
		require.NoError(t, err)
	}
	return record
}

func TestBackupS3Config_StorageTypeValidation(t *testing.T) {
	repo := newMockSettingRepo()
	svc := newTestBackupService(repo, &mockDumper{}, newMockObjectStore())

	_, err := svc.UpdateS3Config(context.Background(), BackupS3Config{StorageType: "webdav"})
	require.ErrorIs(t, err, ErrInvalidBackupStorageType)
}

func TestBackupS3Config_LocalModeAllowsEmptyS3Fields(t *testing.T) {
	svc, repo, _ := newLocalBackupTestService(t)

	// local 模式不需要任何 S3 凭证即可保存。
	cfg, err := svc.UpdateS3Config(context.Background(), BackupS3Config{StorageType: BackupStorageTypeLocal})
	require.NoError(t, err)
	require.True(t, cfg.IsLocal())
	require.True(t, cfg.IsConfigured())

	// 落库后回读仍为 local。
	raw, err := repo.GetValue(context.Background(), settingKeyBackupS3Config)
	require.NoError(t, err)
	var stored BackupS3Config
	require.NoError(t, json.Unmarshal([]byte(raw), &stored))
	require.Equal(t, BackupStorageTypeLocal, stored.StorageType)
}

func TestBackupS3Config_LegacyConfigDefaultsToS3(t *testing.T) {
	svc, repo, _ := newLocalBackupTestService(t)

	// 旧客户端保存的配置没有 storage_type，必须继续按 S3 解释。
	_, err := svc.UpdateS3Config(context.Background(), BackupS3Config{
		Bucket: "b", AccessKeyID: "ak", SecretAccessKey: "sk",
	})
	require.NoError(t, err)

	// GetS3Config 会脱敏 secret，因此用落库原文校验 storage_type。
	raw, err := repo.GetValue(context.Background(), settingKeyBackupS3Config)
	require.NoError(t, err)
	var stored BackupS3Config
	require.NoError(t, json.Unmarshal([]byte(raw), &stored))
	require.Equal(t, BackupStorageTypeS3, stored.StorageType)
	require.False(t, (&stored).IsLocal())
}

func TestBackupService_LocalDownloadURLIsSignedRelative(t *testing.T) {
	svc, repo, store := newLocalBackupTestService(t)
	record := seedCompletedLocalBackup(t, repo, store)

	download, err := svc.GetBackupDownloadURL(context.Background(), record.ID)
	require.NoError(t, err)
	require.NotEmpty(t, download.URL)
	require.True(t, strings.HasPrefix(download.URL, "/api/v1/backups/download?"), "local download must use the signed public route, got %q", download.URL)
	require.Contains(t, download.URL, "id=local01")
	require.Contains(t, download.URL, "part=0")
	require.Contains(t, download.URL, "sig=")
	require.Contains(t, download.URL, "exp=")
}

func TestBackupService_VerifyBackupDownload(t *testing.T) {
	svc, repo, store := newLocalBackupTestService(t)
	record := seedCompletedLocalBackup(t, repo, store)

	download, err := svc.GetBackupDownloadURL(context.Background(), record.ID)
	require.NoError(t, err)
	sig := extractQueryParam(t, download.URL, "sig")
	exp := extractQueryParam(t, download.URL, "exp")

	// 完整、未过期的签名应通过。
	require.NoError(t, svc.VerifyBackupDownload(BackupDownloadRequest{RecordID: record.ID, Part: 0, Exp: parseInt64(t, exp), Sig: sig}))

	// 篡改任意字段都应拒绝。
	require.Error(t, svc.VerifyBackupDownload(BackupDownloadRequest{RecordID: "other01", Part: 0, Exp: parseInt64(t, exp), Sig: sig}))
	require.Error(t, svc.VerifyBackupDownload(BackupDownloadRequest{RecordID: record.ID, Part: 3, Exp: parseInt64(t, exp), Sig: sig}))
	require.Error(t, svc.VerifyBackupDownload(BackupDownloadRequest{RecordID: record.ID, Part: 0, Exp: parseInt64(t, exp), Sig: "deadbeef" + sig}))
	// 过期签名拒绝。
	require.Error(t, svc.VerifyBackupDownload(BackupDownloadRequest{RecordID: record.ID, Part: 0, Exp: time.Now().Add(-time.Minute).Unix(), Sig: sig}))
	// 非法输入拒绝。
	require.Error(t, svc.VerifyBackupDownload(BackupDownloadRequest{RecordID: "", Part: 0, Exp: 1, Sig: "ab"}))
	require.Error(t, svc.VerifyBackupDownload(BackupDownloadRequest{RecordID: record.ID, Part: 0, Exp: 1, Sig: "zz-not-hex"}))
}

func TestBackupService_SignatureBindsToHMACKey(t *testing.T) {
	repo := newMockSettingRepo()
	record := seedCompletedLocalBackup(t, repo, nil)

	// 两个不同 jwt secret 的服务实例：A 签的 URL 在 B 处必须无效。
	cfgA := &config.Config{Database: config.DatabaseConfig{DBName: "d"}, JWT: config.JWTConfig{Secret: "secret-a"}}
	cfgB := &config.Config{Database: config.DatabaseConfig{DBName: "d"}, JWT: config.JWTConfig{Secret: "secret-b"}}
	factory := func(_ context.Context, _ *BackupS3Config) (BackupObjectStore, error) { return newMockObjectStore(), nil }
	svcA := NewBackupService(repo, cfgA, &plainEncryptor{}, factory, &mockDumper{})
	svcB := NewBackupService(repo, cfgB, &plainEncryptor{}, factory, &mockDumper{})

	download, err := svcA.GetBackupDownloadURL(context.Background(), record.ID)
	require.NoError(t, err)
	sig := extractQueryParam(t, download.URL, "sig")
	exp := parseInt64(t, extractQueryParam(t, download.URL, "exp"))

	require.NoError(t, svcA.VerifyBackupDownload(BackupDownloadRequest{RecordID: record.ID, Part: 0, Exp: exp, Sig: sig}))
	require.Error(t, svcB.VerifyBackupDownload(BackupDownloadRequest{RecordID: record.ID, Part: 0, Exp: exp, Sig: sig}))
}

func TestBackupService_OpenBackupPayloadLocalMode(t *testing.T) {
	svc, repo, store := newLocalBackupTestService(t)
	record := seedCompletedLocalBackup(t, repo, store)

	download, err := svc.GetBackupDownloadURL(context.Background(), record.ID)
	require.NoError(t, err)
	req := BackupDownloadRequest{
		RecordID: record.ID,
		Part:     0,
		Exp:      parseInt64(t, extractQueryParam(t, download.URL, "exp")),
		Sig:      extractQueryParam(t, download.URL, "sig"),
	}
	require.NoError(t, svc.VerifyBackupDownload(req))

	body, fileName, err := svc.OpenBackupPayload(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, record.FileName, fileName)
	require.NotNil(t, body)
	require.NoError(t, body.Close())

	// 不存在的分卷返回错误而不是空流。
	_, _, err = svc.OpenBackupPayload(context.Background(), BackupDownloadRequest{RecordID: record.ID, Part: 9, Exp: req.Exp, Sig: "x"})
	require.Error(t, err)
}

func TestBackupService_OpenBackupPayloadRejectsWhenStorageIsS3(t *testing.T) {
	// S3 模式下公开路由不可用：预签名 URL 才是下载通道。
	repo := newMockSettingRepo()
	svc := newTestBackupService(repo, &mockDumper{}, newMockObjectStore())
	seedS3Config(t, repo)
	record := BackupRecord{
		ID: "s3rec001", Status: "completed", FileName: "f.sql.gz",
		S3Key: "backups/f.sql.gz", StartedAt: time.Now().Format(time.RFC3339),
	}
	records, _ := json.Marshal([]BackupRecord{record})
	require.NoError(t, repo.Set(context.Background(), settingKeyBackupRecords, string(records)))

	_, _, err := svc.OpenBackupPayload(context.Background(), BackupDownloadRequest{RecordID: record.ID, Part: 0, Exp: 1, Sig: "ab"})
	require.Error(t, err)
}

func TestImageStorage_ReuseRejectedWhenBackupIsLocal(t *testing.T) {
	backupSvc, repo, _ := newLocalBackupTestService(t)
	// 备份存储切到 local 后，图片存储不能再"复用备份 S3"。
	seedLocalBackupConfig(t, repo)

	factory := func(_ context.Context, _ *config.ImageStorageConfig) (ImageStorage, error) {
		return nil, nil
	}
	imageSvc := NewImageStorageSettingService(repo, &plainEncryptor{}, backupSvc, factory, config.ImageStorageConfig{})

	_, err := imageSvc.Update(context.Background(), ImageStorageSettings{Enabled: true, ReuseBackupS3: true, Bucket: "images"})
	require.ErrorIs(t, err, ErrImageStorageReuseNotS3)
}

func extractQueryParam(t *testing.T, rawURL, key string) string {
	t.Helper()
	idx := strings.Index(rawURL, key+"=")
	require.NotEqual(t, -1, idx, "query param %q missing in %q", key, rawURL)
	rest := rawURL[idx+len(key)+1:]
	if end := strings.IndexByte(rest, '&'); end >= 0 {
		return rest[:end]
	}
	return rest
}

func parseInt64(t *testing.T, raw string) int64 {
	t.Helper()
	var v int64
	require.NoError(t, json.Unmarshal([]byte(raw), &v))
	return v
}
