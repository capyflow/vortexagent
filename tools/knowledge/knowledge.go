// Package knowledge 是框架内置的"本地文档知识库"扩展工具：目录扫描 + 全文关键词检索。
//
// 它是可选的扩展而非框架核心——不注册其工具时，Agent 就是一个不依赖任何
// 知识库的通用 agent。第一版采用纯文本子串检索（大小写不敏感），不引入
// bleve / 向量库等第三方依赖，仅使用标准库；类型边界（Result / KB / Tool）
// 为后续升级为索引化或向量检索预留了演进空间。
package knowledge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// 默认配置常量。
const (
	// DefaultMaxFileSize 单个文件的检索大小上限（512KB），超限文件直接跳过。
	DefaultMaxFileSize int64 = 512 * 1024
	// DefaultMaxResults 检索结果的默认返回条数。
	DefaultMaxResults = 5
	// MaxSearchResults 检索结果的硬上限：模型可以传 limit，但不能超过该值，
	// 防止一次检索把整个知识库塞进上下文。
	MaxSearchResults = 50
	// MaxReadDocumentSize read_document 的全文截断上限（100KB）。
	MaxReadDocumentSize = 100 * 1024
	// binaryCheckSize 二进制检测读取的文件前缀字节数。
	binaryCheckSize = 512
)

// skipDirs 扫描时整棵跳过的目录名（各类生成目录、版本控制目录、隐藏目录）。
var skipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	".idea":        true,
	"vendor":       true,
	"dist":         true,
	"build":        true,
	".omo":         true,
	".vscode":      true,
}

// defaultExtensions 默认收录的文本文件扩展名，可通过 SetExtensions 覆盖。
var defaultExtensions = []string{".md", ".txt", ".go", ".json", ".yaml", ".yml", ".toml"}

// Result 是单条检索命中：某个文件的某一个匹配行及其上下文。
type Result struct {
	Path    string // 相对各自知识库根目录的路径（POSIX 分隔符），不携带绝对路径
	Line    int    // 命中行号（从 1 开始）
	Text    string // 命中行内容（不含换行符）
	Context string // 前后各 1 行上下文（带行号、换行分隔），命中行本身不重复
}

// KB 是本地文档知识库，管理一组根目录并对外提供全文检索与文档读取能力。
//
// 字段均为私有，行为通过构造函数与以下方法配置，避免外部直接篡改不变量。
type KB struct {
	rootDirs    []string        // 知识库根目录（支持多个）
	extensions  map[string]bool // 允许收录的扩展名集合（小写）
	maxFileSize int64           // 单文件检索大小上限
	maxResults  int             // limit<=0 时的默认返回条数
}

// NewKB 以一组根目录构造知识库，使用默认扩展名与默认上限。
func NewKB(rootDirs []string) *KB {
	exts := make(map[string]bool, len(defaultExtensions))
	for _, e := range defaultExtensions {
		exts[strings.ToLower(e)] = true
	}
	return &KB{
		rootDirs:    append([]string{}, rootDirs...),
		extensions:  exts,
		maxFileSize: DefaultMaxFileSize,
		maxResults:  DefaultMaxResults,
	}
}

// AddRoot 追加一个知识库根目录。
func (k *KB) AddRoot(dir string) {
	k.rootDirs = append(k.rootDirs, dir)
}

// SetExtensions 替换允许收录的扩展名列表，如 SetExtensions([]string{".md", ".txt"})。
// 扩展名大小写不敏感。
func (k *KB) SetExtensions(exts []string) {
	k.extensions = make(map[string]bool, len(exts))
	for _, e := range exts {
		k.extensions[strings.ToLower(e)] = true
	}
}

// Search 在知识库全部根目录内执行大小写不敏感的全文关键词检索。
//
// query 支持空格分隔的多个关键词：所有关键词都在同一行出现才算命中
// （如 "vortex agent" 会匹配同时包含两个词的任意行，而非整串连续匹配）。
// 返回的每个 Result 对应一个命中行；文件按命中行数降序、路径字典序升序稳定排序后，
// 逐文件展平并按 limit 截断。limit<=0 时使用默认条数（DefaultMaxResults），
// 超过 MaxSearchResults 时钳制到该上限。
// 检索过程中检查 ctx，取消时提前退出并返回 ctx 的错误。
func (k *KB) Search(ctx context.Context, query string, limit int) ([]Result, error) {
	if limit <= 0 {
		limit = k.maxResults
	}
	if limit > MaxSearchResults {
		limit = MaxSearchResults
	}
	tokens := strings.Fields(strings.ToLower(strings.TrimSpace(query)))
	if len(tokens) == 0 {
		return nil, errors.New("检索关键词不能为空")
	}

	// fileHits 聚合一个文件的全部命中行，用于排序与展平。
	type fileHits struct {
		path string // 相对各自 root 的展示路径
		hits []Result
	}

	var matched []fileHits
	var firstErr error

	for _, root := range k.rootDirs {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
			// ctx 取消：立即中止扫描并向上传播。
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			// 单个文件/目录不可访问时跳过，不中断整体检索。
			if walkErr != nil {
				return nil
			}
			if d.IsDir() {
				if path != root && skipDirs[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			ext := strings.ToLower(filepath.Ext(path))
			if !k.extensions[ext] {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return nil
			}
			if hits := k.searchFile(ctx, path, filepath.ToSlash(rel), tokens); len(hits) > 0 {
				matched = append(matched, fileHits{path: filepath.ToSlash(rel), hits: hits})
			}
			return nil
		})
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			// 单个根目录整体不可读（如路径不存在）时记录错误，继续检索其余根目录；
			// 仅在无任何结果时上报，避免一个坏根目录拖垮整个检索。
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	// 整树扫描后无结果且存在根目录错误时，向调用方说明原因。
	if len(matched) == 0 && firstErr != nil {
		return nil, firstErr
	}

	// 命中行数降序、路径字典序升序，稳定排序保证同命中数的文件顺序确定。
	sort.SliceStable(matched, func(i, j int) bool {
		if len(matched[i].hits) != len(matched[j].hits) {
			return len(matched[i].hits) > len(matched[j].hits)
		}
		return matched[i].path < matched[j].path
	})

	// 逐文件展平并按 limit 截断。
	results := make([]Result, 0, limit)
	for _, f := range matched {
		results = append(results, f.hits...)
		if len(results) >= limit {
			results = results[:limit]
			break
		}
	}
	return results, nil
}

// searchFile 在单个文件中检索关键词，返回命中行列表；文件超限、二进制、
// 读取失败或无命中时返回 nil。absPath 用于读取，relPath 用于结果展示。
// tokens 为小写化后的关键词列表，命中行必须包含全部关键词。
func (k *KB) searchFile(ctx context.Context, absPath, relPath string, tokens []string) []Result {
	if ctx.Err() != nil {
		return nil
	}
	fi, err := os.Stat(absPath)
	if err != nil || fi.IsDir() {
		return nil
	}
	if fi.Size() > k.maxFileSize {
		return nil // 超过大小上限，跳过
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil
	}
	if looksBinary(data) {
		return nil // 二进制文件，跳过
	}
	// 整文快速预检：所有关键词都出现才逐行匹配，避免对不相关文件逐行做小写转换。
	lower := bytes.ToLower(data)
	for _, tok := range tokens {
		if !bytes.Contains(lower, []byte(tok)) {
			return nil
		}
	}

	lines := strings.Split(string(data), "\n")
	hits := make([]Result, 0)
	for i, line := range lines {
		if !lineContainsAll(line, tokens) {
			continue
		}
		hits = append(hits, Result{
			Path:    relPath,
			Line:    i + 1,
			Text:    strings.TrimSuffix(line, "\r"),
			Context: buildContext(lines, i),
		})
	}
	return hits
}

// lineContainsAll 判断一行（小写化后）是否包含全部关键词。
func lineContainsAll(line string, tokens []string) bool {
	lower := strings.ToLower(line)
	for _, tok := range tokens {
		if !strings.Contains(lower, tok) {
			return false
		}
	}
	return true
}

// buildContext 拼接命中行前后各 1 行上下文，带行号、换行分隔；
// 命中行本身不重复包含（已由 Result.Text 承载），越界侧自动裁剪。
func buildContext(lines []string, idx int) string {
	start, end := idx-1, idx+1
	if start < 0 {
		start = 0
	}
	if end >= len(lines) {
		end = len(lines) - 1
	}
	var b strings.Builder
	for i := start; i <= end; i++ {
		if i == idx {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString(": ")
		b.WriteString(strings.TrimSuffix(lines[i], "\r"))
	}
	return b.String()
}

// Read 返回知识库内指定文档的全文。
//
// path 支持相对知识库根目录的相对路径或位于根目录内的绝对路径；只接受
// 知识库 rootDir 之内的文件（含符号链接解析后的最终位置），防止任意文件读取。
// 全文超过 MaxReadDocumentSize 时截断，并在末尾追加截断提示。
func (k *KB) Read(ctx context.Context, path string) (string, error) {
	abs, err := k.resolvePath(path)
	if err != nil {
		return "", err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", ctxErr
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", fmt.Errorf("读取文件失败: %w", err)
	}
	if looksBinary(data) {
		return "", errors.New("目标文件是二进制文件，无法作为文档读取")
	}
	if len(data) > MaxReadDocumentSize {
		return string(data[:MaxReadDocumentSize]) + "\n\n[提示：文档超过 100KB，已截断，仅展示前 100KB]", nil
	}
	return string(data), nil
}

// resolvePath 将用户传入的路径解析为知识库根目录内的绝对路径。
//
// 双重防线防目录穿越：
//  1. filepath.Clean + 相对路径拼接后，用 filepath.Rel 校验结果不携带 ".." 前缀；
//  2. 符号链接可能指向根目录之外，故对最终路径做 EvalSymlinks 后再校验一次
//     ".." 前缀，确保符号链接逃逸也无法越出知识库。
func (k *KB) resolvePath(p string) (string, error) {
	clean := filepath.Clean(p)
	if clean == "" || clean == "." || clean == string(filepath.Separator) {
		return "", errors.New("路径不合法")
	}
	for _, root := range k.rootDirs {
		rootReal, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue // 根目录不可解析，尝试下一个
		}
		candidate := clean
		if !filepath.IsAbs(clean) {
			candidate = filepath.Join(root, clean)
		}
		candidateReal, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			continue // 文件不存在或无法解析（拒绝通过断裂的符号链接越界）
		}
		rel, err := filepath.Rel(rootReal, candidateReal)
		if err != nil {
			continue
		}
		// 相对路径带 ".." 前缀意味着越出了根目录，拒绝。
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			continue
		}
		fi, err := os.Stat(candidateReal)
		if err != nil || fi.IsDir() {
			continue
		}
		return candidateReal, nil
	}
	return "", fmt.Errorf("路径 %q 不在知识库目录内", p)
}

// looksBinary 通过检查文件前缀是否含 NUL 字节判断是否为二进制文件。
func looksBinary(data []byte) bool {
	n := len(data)
	if n > binaryCheckSize {
		n = binaryCheckSize
	}
	return bytes.IndexByte(data[:n], 0) != -1
}

// formatResults 将检索结果格式化为模型可直接阅读的文本。
func formatResults(results []Result) string {
	if len(results) == 0 {
		return "未在知识库中找到匹配内容。"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "在知识库中找到 %d 条匹配结果：\n", len(results))
	lastPath := ""
	for _, r := range results {
		if r.Path != lastPath {
			fmt.Fprintf(&b, "\n文件：%s\n", r.Path)
			lastPath = r.Path
		}
		fmt.Fprintf(&b, "  行 %d：%s\n", r.Line, r.Text)
		if r.Context != "" {
			b.WriteString("    上下文：\n")
			for _, line := range strings.Split(r.Context, "\n") {
				fmt.Fprintf(&b, "      %s\n", line)
			}
		}
	}
	return b.String()
}

// Tool 是知识库工具的结构化实现，方法签名与 agent 包约定的 Tool 接口一致，
// 通过 Go 的结构化接口（duck typing）满足契约，无需 import agent，避免包循环依赖。
type Tool struct {
	name        string
	description string
	schema      map[string]any
	call        func(ctx context.Context, args map[string]any) (string, error)
}

// toolLike 是 agent 侧 Tool 接口的本地投影，用于编译期断言签名一致。
type toolLike interface {
	Name() string
	Description() string
	Schema() map[string]any
	Call(ctx context.Context, args map[string]any) (string, error)
}

// 编译期断言：*Tool 满足约定的工具接口。
var _ toolLike = (*Tool)(nil)

// Name 返回工具名称。
func (t *Tool) Name() string { return t.name }

// Description 返回工具描述。
func (t *Tool) Description() string { return t.description }

// Schema 返回参数 JSON Schema（type object / properties / required）。
func (t *Tool) Schema() map[string]any { return t.schema }

// Call 执行工具调用，返回供模型消费的文本结果。
func (t *Tool) Call(ctx context.Context, args map[string]any) (string, error) {
	return t.call(ctx, args)
}

// NewKBTools 构造知识库对外暴露的工具列表：search_knowledge 与 read_document。
func NewKBTools(kb *KB) []*Tool {
	return []*Tool{newSearchKnowledgeTool(kb), newReadDocumentTool(kb)}
}

// newSearchKnowledgeTool 构造检索工具：query 必填（支持空格分隔的多关键词，
// 全部命中才算）、limit 可选（默认 5，最大 50）。
func newSearchKnowledgeTool(kb *KB) *Tool {
	return &Tool{
		name:        "search_knowledge",
		description: "在本地文档知识库中按关键词全文检索（支持空格分隔的多关键词，需全部命中同一行），返回文件路径、行号、匹配行内容与上下文",
		schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "检索关键词（大小写不敏感，可用空格分隔多个词）",
				},
				"limit": map[string]any{
					"type":        "number",
					"description": "返回结果条数上限，默认 5，最大 50",
				},
			},
			"required": []string{"query"},
		},
		call: func(ctx context.Context, args map[string]any) (string, error) {
			query, err := requireString(args, "query")
			if err != nil {
				return "", err
			}
			limit := 0
			if v, ok := args["limit"]; ok {
				n, err := coerceInt(v)
				if err != nil {
					return "", fmt.Errorf("参数 limit 不合法: %w", err)
				}
				limit = n
			}
			if limit > MaxSearchResults {
				limit = MaxSearchResults
			}
			results, err := kb.Search(ctx, query, limit)
			if err != nil {
				return "", fmt.Errorf("知识库检索失败: %w", err)
			}
			return formatResults(results), nil
		},
	}
}

// newReadDocumentTool 构造文档读取工具：path 必填，仅限知识库目录内文件。
func newReadDocumentTool(kb *KB) *Tool {
	return &Tool{
		name:        "read_document",
		description: "读取知识库内指定文档的全文（仅限知识库目录内的文件）",
		schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "知识库内文件的相对路径或绝对路径",
				},
			},
			"required": []string{"path"},
		},
		call: func(ctx context.Context, args map[string]any) (string, error) {
			path, err := requireString(args, "path")
			if err != nil {
				return "", err
			}
			content, err := kb.Read(ctx, path)
			if err != nil {
				return "", err
			}
			return content, nil
		},
	}
}

// requireString 从工具参数中取出必填的字符串参数，并拒绝空串。
func requireString(args map[string]any, key string) (string, error) {
	v, ok := args[key]
	if !ok {
		return "", fmt.Errorf("缺少必需参数 %q", key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("参数 %q 必须为字符串，实际类型为 %T", key, v)
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("参数 %q 不能为空", key)
	}
	return s, nil
}

// coerceInt 兼容 JSON 反序列化后的多种数字类型（float64 / int / int64 / json.Number），
// 统一提取为整数；非整数值报错。
func coerceInt(v any) (int, error) {
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case float64:
		if math.Trunc(n) != n {
			return 0, fmt.Errorf("期望整数，实际为 %v", n)
		}
		return int(n), nil
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, err
		}
		return int(i), nil
	default:
		return 0, fmt.Errorf("期望整数，实际类型为 %T", v)
	}
}
