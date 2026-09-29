package ctxengine

import (
	"os"
	"time"
)

// setDirModTime 调整目录 mtime（TTL 清理测试用）。
func setDirModTime(path string, t time.Time) error {
	return os.Chtimes(path, t, t)
}
