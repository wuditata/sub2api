package repository

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func newLocalStoreForTest(t *testing.T) (*LocalBackupStore, string) {
	t.Helper()
	root := t.TempDir()
	return NewLocalBackupStore(root), root
}

func TestLocalBackupStore_UploadDownloadRoundtrip(t *testing.T) {
	store, root := newLocalStoreForTest(t)
	ctx := context.Background()

	payload := "hello local backup store"
	size, err := store.Upload(ctx, "backups/2026/10/12/db.sql.gz", strings.NewReader(payload), "application/gzip")
	require.NoError(t, err)
	require.EqualValues(t, len(payload), size)

	// 文件应落在 root 下且权限收紧
	target := filepath.Join(root, "backups", "2026", "10", "12", "db.sql.gz")
	info, err := os.Stat(target)
	require.NoError(t, err)
	require.EqualValues(t, len(payload), info.Size())

	body, err := store.Download(ctx, "backups/2026/10/12/db.sql.gz")
	require.NoError(t, err)
	data, err := io.ReadAll(body)
	require.NoError(t, err)
	require.NoError(t, body.Close())
	require.Equal(t, payload, string(data))
}

func TestLocalBackupStore_UploadFile(t *testing.T) {
	store, root := newLocalStoreForTest(t)
	ctx := context.Background()

	src, err := os.CreateTemp(t.TempDir(), "upload-src-*")
	require.NoError(t, err)
	payload := bytes.Repeat([]byte("x"), 4096)
	_, err = src.Write(payload)
	require.NoError(t, err)
	require.NoError(t, src.Close())

	size, err := store.UploadFile(ctx, "backups/parts/payload.part-000001", src.Name(), "application/octet-stream")
	require.NoError(t, err)
	require.EqualValues(t, len(payload), size)

	data, err := os.ReadFile(filepath.Join(root, "backups", "parts", "payload.part-000001"))
	require.NoError(t, err)
	require.Equal(t, payload, data)
}

func TestLocalBackupStore_DeleteIdempotent(t *testing.T) {
	store, _ := newLocalStoreForTest(t)
	ctx := context.Background()

	_, err := store.Upload(ctx, "backups/a/b.sql.gz", strings.NewReader("x"), "application/gzip")
	require.NoError(t, err)
	require.NoError(t, store.Delete(ctx, "backups/a/b.sql.gz"))
	// 与 S3 DeleteObject 语义对齐：删除不存在的对象幂等成功。
	require.NoError(t, store.Delete(ctx, "backups/a/b.sql.gz"))
}

func TestLocalBackupStore_RejectsTraversalKeys(t *testing.T) {
	store, _ := newLocalStoreForTest(t)
	ctx := context.Background()

	for _, key := range []string{
		"../escape.sql.gz",
		"a/../../escape.sql.gz",
		"/absolute/path.sql.gz",
		`C:\windows\evil.sql.gz`,
		`back\slash.sql.gz`,
		"..",
		"",
	} {
		_, err := store.Upload(ctx, key, strings.NewReader("x"), "application/gzip")
		require.Error(t, err, "key %q should be rejected", key)
		_, err = store.Download(ctx, key)
		require.Error(t, err, "key %q should be rejected on download", key)
		require.Error(t, store.Delete(ctx, key))
	}
}

func TestLocalBackupStore_RejectsSymlinkEscape(t *testing.T) {
	store, root := newLocalStoreForTest(t)
	ctx := context.Background()

	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.sql.gz")
	require.NoError(t, os.WriteFile(victim, []byte("secret"), 0o600))

	// 在 root 内种一个指向外部的目录符号链接，尝试借它把对象写到 root 外。
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "leak")))

	_, err := store.Upload(ctx, "leak/victim.sql.gz", strings.NewReader("pwned"), "application/gzip")
	require.Error(t, err, "upload through symlink must be rejected")
	data, err := os.ReadFile(victim)
	require.NoError(t, err)
	require.Equal(t, "secret", string(data), "victim file must be untouched")
}

func TestLocalBackupStore_PresignUnsupported(t *testing.T) {
	store, _ := newLocalStoreForTest(t)
	_, err := store.PresignURL(context.Background(), "k", time.Hour)
	require.Error(t, err)
}

func TestLocalBackupStore_HeadBucketCreatesWritableRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "nested", "backups")
	store := NewLocalBackupStore(root)
	require.NoError(t, store.HeadBucket(context.Background()))
	info, err := os.Stat(root)
	require.NoError(t, err)
	require.True(t, info.IsDir())
}

func TestBackupStoreFactory_DispatchesByStorageType(t *testing.T) {
	ctx := context.Background()

	factory := NewS3BackupStoreFactory(nil)

	// local 配置应得到本地实现，root 落在 ./data/backups（默认回退）。
	store, err := factory(ctx, &service.BackupS3Config{StorageType: service.BackupStorageTypeLocal})
	require.NoError(t, err)
	_, ok := store.(*LocalBackupStore)
	require.True(t, ok)

	// 空 StorageType（旧配置）与显式 s3 都走 S3 分支：本地没有网络也能构造出
	// 客户端对象（错误只在请求时发生），这里只验证返回的不是 LocalBackupStore。
	s3Store, err := factory(ctx, &service.BackupS3Config{})
	require.NoError(t, err)
	_, isLocal := s3Store.(*LocalBackupStore)
	require.False(t, isLocal)
}

func TestBackupStoreFactory_LocalRootFromConfig(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	cfg := &config.Config{Backup: config.BackupConfig{LocalDir: dir}}
	factory := NewS3BackupStoreFactory(cfg)
	store, err := factory(ctx, &service.BackupS3Config{StorageType: service.BackupStorageTypeLocal})
	require.NoError(t, err)

	// 写入再读取，确认 root 确实来自 cfg 指定的目录。
	_, err = store.Upload(ctx, "probe/key.sql.gz", strings.NewReader("cfg"), "application/gzip")
	require.NoError(t, err)
	data, err := os.ReadFile(filepath.Join(dir, "probe", "key.sql.gz"))
	require.NoError(t, err)
	require.Equal(t, "cfg", string(data))
}
