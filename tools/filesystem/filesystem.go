package filesystem

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	MaxFileSize = 1024 * 1024 // 1MB
)

type ReadFileTool struct {
	rootDir string
}

func NewReadFileTool(rootDir string) *ReadFileTool {
	return &ReadFileTool{rootDir: rootDir}
}

func (t *ReadFileTool) Name() string { return "read_file" }

func (t *ReadFileTool) Description() string {
	return "读取指定文件的内容"
}

func (t *ReadFileTool) Overview() string {
	return "读取文件内容"
}

func (t *ReadFileTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "文件路径（相对于项目根目录）",
			},
		},
		"required": []string{"path"},
	}
}

func (t *ReadFileTool) Call(ctx context.Context, args map[string]any) (string, error) {
	path, _ := args["path"].(string)
	if path == "" {
		return "", fmt.Errorf("path 不能为空")
	}

	absPath, err := t.resolvePath(path)
	if err != nil {
		return "", err
	}

	fi, err := os.Stat(absPath)
	if err != nil {
		return "", fmt.Errorf("文件不存在: %w", err)
	}
	if fi.IsDir() {
		return "", fmt.Errorf("路径是目录，不是文件")
	}
	if fi.Size() > MaxFileSize {
		return "", fmt.Errorf("文件过大（超过 1MB）")
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", fmt.Errorf("读取文件失败: %w", err)
	}

	return string(data), nil
}

func (t *ReadFileTool) resolvePath(path string) (string, error) {
	if filepath.IsAbs(path) {
		return path, nil
	}
	abs := filepath.Join(t.rootDir, path)
	clean := filepath.Clean(abs)
	if !strings.HasPrefix(clean, t.rootDir) {
		return "", fmt.Errorf("路径越界")
	}
	return clean, nil
}

type WriteFileTool struct {
	rootDir string
}

func NewWriteFileTool(rootDir string) *WriteFileTool {
	return &WriteFileTool{rootDir: rootDir}
}

func (t *WriteFileTool) Name() string { return "write_file" }

func (t *WriteFileTool) Description() string {
	return "创建或覆盖写入文件内容"
}

func (t *WriteFileTool) Overview() string {
	return "写入文件"
}

func (t *WriteFileTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "文件路径（相对于项目根目录）",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "要写入的内容",
			},
		},
		"required": []string{"path", "content"},
	}
}

func (t *WriteFileTool) Call(ctx context.Context, args map[string]any) (string, error) {
	path, _ := args["path"].(string)
	content, _ := args["content"].(string)
	if path == "" {
		return "", fmt.Errorf("path 不能为空")
	}

	absPath, err := t.resolvePath(path)
	if err != nil {
		return "", err
	}

	dir := filepath.Dir(absPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("创建目录失败: %w", err)
	}

	if err := os.WriteFile(absPath, []byte(content), 0644); err != nil {
		return "", fmt.Errorf("写入文件失败: %w", err)
	}

	return fmt.Sprintf("文件已写入: %s", path), nil
}

func (t *WriteFileTool) resolvePath(path string) (string, error) {
	if filepath.IsAbs(path) {
		return path, nil
	}
	abs := filepath.Join(t.rootDir, path)
	clean := filepath.Clean(abs)
	if !strings.HasPrefix(clean, t.rootDir) {
		return "", fmt.Errorf("路径越界")
	}
	return clean, nil
}

type EditFileTool struct {
	rootDir string
}

func NewEditFileTool(rootDir string) *EditFileTool {
	return &EditFileTool{rootDir: rootDir}
}

func (t *EditFileTool) Name() string { return "edit_file" }

func (t *EditFileTool) Description() string {
	return "编辑文件：替换指定内容"
}

func (t *EditFileTool) Overview() string {
	return "编辑文件内容"
}

func (t *EditFileTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "文件路径（相对于项目根目录）",
			},
			"old_string": map[string]any{
				"type":        "string",
				"description": "要替换的原内容",
			},
			"new_string": map[string]any{
				"type":        "string",
				"description": "替换后的新内容",
			},
		},
		"required": []string{"path", "old_string", "new_string"},
	}
}

func (t *EditFileTool) Call(ctx context.Context, args map[string]any) (string, error) {
	path, _ := args["path"].(string)
	oldString, _ := args["old_string"].(string)
	newString, _ := args["new_string"].(string)

	if path == "" || oldString == "" {
		return "", fmt.Errorf("path 和 old_string 不能为空")
	}

	absPath, err := t.resolvePath(path)
	if err != nil {
		return "", err
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", fmt.Errorf("读取文件失败: %w", err)
	}

	content := string(data)
	if !strings.Contains(content, oldString) {
		return "", fmt.Errorf("未找到要替换的内容")
	}

	count := strings.Count(content, oldString)
	if count > 1 {
		return "", fmt.Errorf("找到 %d 处匹配，请提供更精确的内容", count)
	}

	newContent := strings.Replace(content, oldString, newString, 1)
	if err := os.WriteFile(absPath, []byte(newContent), 0644); err != nil {
		return "", fmt.Errorf("写入文件失败: %w", err)
	}

	return fmt.Sprintf("文件已编辑: %s", path), nil
}

func (t *EditFileTool) resolvePath(path string) (string, error) {
	if filepath.IsAbs(path) {
		return path, nil
	}
	abs := filepath.Join(t.rootDir, path)
	clean := filepath.Clean(abs)
	if !strings.HasPrefix(clean, t.rootDir) {
		return "", fmt.Errorf("路径越界")
	}
	return clean, nil
}

type ListFilesTool struct {
	rootDir string
}

func NewListFilesTool(rootDir string) *ListFilesTool {
	return &ListFilesTool{rootDir: rootDir}
}

func (t *ListFilesTool) Name() string { return "list_files" }

func (t *ListFilesTool) Description() string {
	return "列出目录下的文件和子目录"
}

func (t *ListFilesTool) Overview() string {
	return "列出目录内容"
}

func (t *ListFilesTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "目录路径（相对于项目根目录，默认为根目录）",
			},
		},
	}
}

func (t *ListFilesTool) Call(ctx context.Context, args map[string]any) (string, error) {
	path, _ := args["path"].(string)
	if path == "" {
		path = "."
	}

	absPath, err := t.resolvePath(path)
	if err != nil {
		return "", err
	}

	entries, err := os.ReadDir(absPath)
	if err != nil {
		return "", fmt.Errorf("读取目录失败: %w", err)
	}

	var result []string
	for _, entry := range entries {
		if entry.IsDir() {
			result = append(result, entry.Name()+"/")
		} else {
			result = append(result, entry.Name())
		}
	}

	if len(result) == 0 {
		return "目录为空", nil
	}
	return strings.Join(result, "\n"), nil
}

func (t *ListFilesTool) resolvePath(path string) (string, error) {
	if filepath.IsAbs(path) {
		return path, nil
	}
	abs := filepath.Join(t.rootDir, path)
	clean := filepath.Clean(abs)
	if !strings.HasPrefix(clean, t.rootDir) {
		return "", fmt.Errorf("路径越界")
	}
	return clean, nil
}
