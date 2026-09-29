package ctxengine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// FileSystemArchive 是 ArchiveStore 的文件实现：
//
//	<baseDir>/<sessionID>/block-<seq>-<fingerprint8>.json   归档块（含消息原文）
//	<baseDir>/<sessionID>/summary-<summaryID>.json          摘要
//
// 会话目录的 ModTime 即该会话归档的最后更新时间，供 TTL 清理判断。
type FileSystemArchive struct {
	mu      sync.Mutex
	baseDir string
}

// NewFileSystemArchive 创建 FS 归档后端，自动创建根目录。
func NewFileSystemArchive(baseDir string) (*FileSystemArchive, error) {
	if baseDir == "" {
		return nil, fmt.Errorf("ctxengine: 归档目录不能为空")
	}
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, fmt.Errorf("ctxengine: 创建归档目录失败: %w", err)
	}
	return &FileSystemArchive{baseDir: baseDir}, nil
}

func (f *FileSystemArchive) sessionDir(sessionID string) string {
	return filepath.Join(f.baseDir, sessionID)
}

func (f *FileSystemArchive) blockPath(sessionID, blockID string) string {
	return filepath.Join(f.sessionDir(sessionID), "block-"+blockID+".json")
}

func (f *FileSystemArchive) summaryPath(sessionID, summaryID string) string {
	return filepath.Join(f.sessionDir(sessionID), "summary-"+summaryID+".json")
}

// Fingerprint 计算块的确定性指纹：同一批消息重复归档得到同一指纹，
// 归档重试 / 崩溃恢复时据此去重。
func Fingerprint(b Block) string {
	h := sha256.New()
	first := b.FirstUserText()
	h.Write([]byte(first))
	fmt.Fprintf(h, "|%d|", len(b.Messages))
	for _, m := range b.Messages {
		h.Write([]byte(m.Role))
		h.Write([]byte{'|'})
		for _, c := range m.Content {
			h.Write([]byte(c.Text))
			h.Write([]byte{'\x00'})
		}
		h.Write([]byte{'\x01'})
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:8])
}

// NewBlockID 生成块 ID = "b" + 指纹。ID 与 seq 无关——重试时同一批消息
// 得到相同 ID，AppendBlocks 的 Stat 去重才能命中。
func NewBlockID(b Block) string {
	return "b" + Fingerprint(b)
}

func (f *FileSystemArchive) AppendBlocks(ctx context.Context, sessionID string, blocks []ArchivedBlock) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	dir := f.sessionDir(sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("ctxengine: 创建会话归档目录失败: %w", err)
	}
	// 指纹去重：已存在的块静默跳过，保证归档重试幂等。
	for _, b := range blocks {
		path := f.blockPath(sessionID, b.ID)
		if _, err := os.Stat(path); err == nil {
			continue
		}
		data, err := json.MarshalIndent(b, "", "  ")
		if err != nil {
			return fmt.Errorf("ctxengine: 序列化归档块失败: %w", err)
		}
		if err := writeFileAtomic(path, data); err != nil {
			return fmt.Errorf("ctxengine: 写入归档块失败: %w", err)
		}
	}
	return nil
}

func (f *FileSystemArchive) GetBlock(ctx context.Context, sessionID, blockID string) (*ArchivedBlock, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	data, err := os.ReadFile(f.blockPath(sessionID, blockID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("ctxengine: 读取归档块失败: %w", err)
	}
	var b ArchivedBlock
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("ctxengine: 解析归档块失败: %w", err)
	}
	return &b, nil
}

func (f *FileSystemArchive) ListBlocks(ctx context.Context, sessionID string) ([]ArchivedBlock, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	entries, err := os.ReadDir(f.sessionDir(sessionID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("ctxengine: 读取归档目录失败: %w", err)
	}
	var blocks []ArchivedBlock
	for _, e := range entries {
		if e.IsDir() || !isJSON(e.Name()) || !hasPrefix(e.Name(), "block-") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(f.sessionDir(sessionID), e.Name()))
		if err != nil {
			continue
		}
		var b ArchivedBlock
		if json.Unmarshal(data, &b) == nil {
			blocks = append(blocks, b)
		}
	}
	sort.Slice(blocks, func(i, j int) bool { return blocks[i].Seq < blocks[j].Seq })
	return blocks, nil
}

func (f *FileSystemArchive) SaveSummary(ctx context.Context, sessionID string, s *Summary) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	dir := f.sessionDir(sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("ctxengine: 创建会话归档目录失败: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("ctxengine: 序列化摘要失败: %w", err)
	}
	if err := writeFileAtomic(f.summaryPath(sessionID, s.ID), data); err != nil {
		return fmt.Errorf("ctxengine: 写入摘要失败: %w", err)
	}
	return nil
}

func (f *FileSystemArchive) ListSummaries(ctx context.Context, sessionID string) ([]*Summary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	entries, err := os.ReadDir(f.sessionDir(sessionID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("ctxengine: 读取归档目录失败: %w", err)
	}
	var summaries []*Summary
	for _, e := range entries {
		if e.IsDir() || !isJSON(e.Name()) || !hasPrefix(e.Name(), "summary-") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(f.sessionDir(sessionID), e.Name()))
		if err != nil {
			continue
		}
		var s Summary
		if json.Unmarshal(data, &s) == nil {
			cp := s
			summaries = append(summaries, &cp)
		}
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].CreatedAt.Before(summaries[j].CreatedAt) })
	return summaries, nil
}

func (f *FileSystemArchive) DeleteSession(ctx context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := os.RemoveAll(f.sessionDir(sessionID)); err != nil {
		return fmt.Errorf("ctxengine: 删除会话归档失败: %w", err)
	}
	return nil
}

func (f *FileSystemArchive) ListSessions(ctx context.Context) ([]SessionMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	entries, err := os.ReadDir(f.baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("ctxengine: 读取归档根目录失败: %w", err)
	}
	var metas []SessionMeta
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		metas = append(metas, SessionMeta{ID: e.Name(), UpdatedAt: fi.ModTime()})
	}
	return metas, nil
}

// writeFileAtomic 临时文件 + rename，避免读到半截 JSON。
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func isJSON(name string) bool {
	return len(name) >= 5 && name[len(name)-5:] == ".json"
}

func hasPrefix(name, prefix string) bool {
	return len(name) >= len(prefix) && name[:len(prefix)] == prefix
}

var _ ArchiveStore = (*FileSystemArchive)(nil)
