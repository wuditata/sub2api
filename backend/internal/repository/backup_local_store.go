package repository

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/servertiming"
)

// LocalBackupStore implements service.BackupObjectStore on the local filesystem.
// Object keys are treated as slash-separated relative paths under root.
type LocalBackupStore struct {
	root string
}

// NewLocalBackupStore returns a local-filesystem store rooted at dir. The
// directory is created lazily on first write.
func NewLocalBackupStore(dir string) *LocalBackupStore {
	return &LocalBackupStore{root: filepath.Clean(dir)}
}

// resolve maps an object key to a path under root, rejecting traversal and
// absolute keys. Symlink escape is blocked by verifying the real path of every
// parent plus the final path stays inside the real root.
func (s *LocalBackupStore) resolve(key string) (string, error) {
	rel, err := cleanObjectKey(key)
	if err != nil {
		return "", err
	}
	cleanedRoot := filepath.Clean(s.root)
	target := filepath.Join(cleanedRoot, rel)
	if !pathWithinDir(target, cleanedRoot) {
		return "", fmt.Errorf("backup object key escapes local root: %q", key)
	}
	realRoot, err := filepath.EvalSymlinks(cleanedRoot)
	if err != nil {
		// Root may not exist yet on first write; a missing root cannot contain
		// an escaping symlink, and the join below stays under it by construction.
		if os.IsNotExist(err) {
			return target, nil
		}
		return "", fmt.Errorf("resolve backup root: %w", err)
	}
	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		if os.IsNotExist(err) {
			// The leaf (and possibly parents) does not exist yet. Verify the
			// deepest existing ancestor stays inside the real root so a symlink
			// planted mid-path cannot redirect the write.
			parent := filepath.Dir(target)
			for {
				realParent, err := filepath.EvalSymlinks(parent)
				if err == nil {
					if !pathWithinDir(realParent, realRoot) {
						return "", fmt.Errorf("backup object path escapes local root: %q", key)
					}
					break
				}
				if !os.IsNotExist(err) {
					return "", fmt.Errorf("resolve backup object path: %w", err)
				}
				next := filepath.Dir(parent)
				if next == parent {
					break
				}
				parent = next
			}
			return target, nil
		}
		return "", fmt.Errorf("resolve backup object: %w", err)
	}
	if !pathWithinDir(realTarget, realRoot) {
		return "", fmt.Errorf("backup object path escapes local root: %q", key)
	}
	return realTarget, nil
}

// cleanObjectKey normalizes a slash-separated object key into a relative
// filesystem path, rejecting absolute paths, backslashes and traversal.
func cleanObjectKey(key string) (string, error) {
	if key == "" {
		return "", errors.New("backup object key is empty")
	}
	if strings.ContainsRune(key, 0) {
		return "", errors.New("backup object key contains NUL")
	}
	if strings.HasPrefix(key, "/") || strings.HasPrefix(key, "\\") || filepath.IsAbs(key) {
		return "", fmt.Errorf("backup object key must be relative: %q", key)
	}
	// Keys use forward slashes; on Windows also reject native separators so a
	// key cannot smuggle drive letters or alternative separators.
	if strings.Contains(key, "\\") {
		return "", fmt.Errorf("backup object key must not contain backslashes: %q", key)
	}
	parts := make([]string, 0)
	for _, part := range strings.Split(key, "/") {
		switch part {
		case "", ".":
			continue
		case "..":
			return "", fmt.Errorf("backup object key must not traverse upward: %q", key)
		default:
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return "", errors.New("backup object key resolves to empty path")
	}
	rel := filepath.Join(parts...)
	if filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" {
		return "", fmt.Errorf("backup object key must stay relative: %q", key)
	}
	return rel, nil
}

func pathWithinDir(path, dir string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (s *LocalBackupStore) Upload(ctx context.Context, key string, body io.Reader, contentType string) (int64, error) {
	target, err := s.resolve(key)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return 0, fmt.Errorf("create backup object dir: %w", err)
	}
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, fmt.Errorf("create backup object: %w", err)
	}
	finish := servertiming.ObserveDependency(ctx, "local-store")
	size, copyErr := io.Copy(file, body)
	closeErr := file.Close()
	finish()
	if copyErr != nil {
		_ = os.Remove(target)
		return 0, fmt.Errorf("write backup object: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(target)
		return 0, fmt.Errorf("close backup object: %w", closeErr)
	}
	return size, nil
}

func (s *LocalBackupStore) UploadFile(ctx context.Context, key string, filePath string, contentType string) (int64, error) {
	info, err := os.Stat(filePath)
	if err != nil {
		return 0, fmt.Errorf("stat upload file: %w", err)
	}
	target, err := s.resolve(key)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return 0, fmt.Errorf("create backup object dir: %w", err)
	}
	// 备份归档是刚写完的临时文件，优先原子改名；跨设备（tmp 与备份卷不在同一
	// 文件系统）时回退为拷贝。
	if err := os.Rename(filePath, target); err == nil {
		return info.Size(), nil
	} else if !errors.Is(err, syscall.EXDEV) {
		// Windows rename-overwrite 只在目标已存在时失败；删除后重试一次。
		if _, statErr := os.Stat(target); statErr == nil {
			if rmErr := os.Remove(target); rmErr == nil {
				if err := os.Rename(filePath, target); err == nil {
					return info.Size(), nil
				}
			}
		}
		return 0, fmt.Errorf("move backup object: %w", err)
	}

	src, err := os.Open(filePath)
	if err != nil {
		return 0, fmt.Errorf("open upload file: %w", err)
	}
	defer func() { _ = src.Close() }()
	dst, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, fmt.Errorf("create backup object: %w", err)
	}
	finish := servertiming.ObserveDependency(ctx, "local-store")
	size, copyErr := io.Copy(dst, src)
	closeErr := dst.Close()
	finish()
	if copyErr == nil && closeErr == nil && size != info.Size() {
		copyErr = fmt.Errorf("short write: got %d, want %d", size, info.Size())
	}
	if copyErr != nil {
		_ = os.Remove(target)
		return 0, fmt.Errorf("copy backup object: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(target)
		return 0, fmt.Errorf("close backup object: %w", closeErr)
	}
	return size, nil
}

func (s *LocalBackupStore) Download(ctx context.Context, key string) (io.ReadCloser, error) {
	target, err := s.resolve(key)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(target)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("backup object %q not found: %w", key, err)
		}
		return nil, fmt.Errorf("open backup object: %w", err)
	}
	return file, nil
}

func (s *LocalBackupStore) Delete(ctx context.Context, key string) error {
	target, err := s.resolve(key)
	if err != nil {
		return err
	}
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		// S3 DeleteObject 对不存在的对象幂等；本地对齐该语义。
		return fmt.Errorf("delete backup object: %w", err)
	}
	return nil
}

func (s *LocalBackupStore) PresignURL(ctx context.Context, key string, expiry time.Duration) (string, error) {
	return "", fmt.Errorf("local backup storage does not support presigned URLs")
}

func (s *LocalBackupStore) HeadBucket(ctx context.Context) error {
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return fmt.Errorf("create backup dir: %w", err)
	}
	probe := filepath.Join(s.root, ".probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		return fmt.Errorf("backup dir is not writable: %w", err)
	}
	_ = os.Remove(probe)
	return nil
}
