package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestFile 创建测试文件（自动创建父目录）。
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// buildTree 构造一个典型的测试知识库目录树。
//
// 结构说明：
//   - docs/api.md：2 处命中 "登录"（第 1、2 行），用于验证多行匹配与上下文
//   - src/main.go：注释中 1 处命中
//   - notes/todo.txt：1 处命中
//   - docs/usage.txt：英文 "Login"，用于验证大小写不敏感
//   - .git/config 与 node_modules/pkg/index.js：应被整棵跳过
//   - docs/secret.go：允许的扩展名但含 NUL 字节，应被二进制检测跳过
//   - assets/logo.png：不允许的扩展名，应被跳过
func buildTree(t *testing.T, root string) {
	t.Helper()
	writeTestFile(t, filepath.Join(root, "docs", "api.md"),
		"用户登录接口说明\n登录接口需要 Token 鉴权\n请在请求头携带 Authorization\n")
	writeTestFile(t, filepath.Join(root, "docs", "usage.txt"),
		"Login with your account\nAPI KEY 放在环境变量中\n")
	writeTestFile(t, filepath.Join(root, "src", "main.go"),
		"package main\n\nfunc main() {\n\t// 登录逻辑\n}\n")
	writeTestFile(t, filepath.Join(root, "notes", "todo.txt"), "crypto 登录\n")
	// 应被跳过的内容：.git、node_modules、二进制、不支持扩展名
	writeTestFile(t, filepath.Join(root, ".git", "config"), "登录 密码 密钥\n")
	writeTestFile(t, filepath.Join(root, "node_modules", "pkg", "index.js"), "登录 秘密\n")
	writeTestFile(t, filepath.Join(root, "docs", "secret.go"), "PK\x03\x04\x00\x00\x00登录")
	writeTestFile(t, filepath.Join(root, "assets", "logo.png"), "登录图片\n")
}

// TestSearch_sortsByMatchCountAndPath 验证排序、多行匹配与相对路径展示。
//
// 命中数：api.md 2 处、todo.txt 1 处、main.go 1 处；排序后应优先返回 api.md，
// todo.txt 与 main.go 按路径字典序（notes/todo.txt < src/main.go）稳定排序。
func TestSearch_sortsByMatchCountAndPath(t *testing.T) {
	root := t.TempDir()
	buildTree(t, root)
	kb := NewKB([]string{root})

	results, err := kb.Search(context.Background(), "登录", 5)
	if err != nil {
		t.Fatalf("Search 返回错误: %v", err)
	}
	if len(results) != 4 {
		t.Fatalf("期望 4 条结果，实际 %d 条: %+v", len(results), results)
	}
	wantPaths := []string{"docs/api.md", "docs/api.md", "notes/todo.txt", "src/main.go"}
	for i, want := range wantPaths {
		if results[i].Path != want {
			t.Errorf("结果[%d].Path 期望 %q，实际 %q", i, want, results[i].Path)
		}
	}
	if results[0].Line != 1 || results[1].Line != 2 {
		t.Errorf("api.md 期望命中第 1、2 行，实际第 %d、%d 行", results[0].Line, results[1].Line)
	}
	if results[1].Text != "登录接口需要 Token 鉴权" {
		t.Errorf("结果[1].Text 不匹配: %q", results[1].Text)
	}
}

// TestSearch_caseInsensitive 验证大小写不敏感匹配。
func TestSearch_caseInsensitive(t *testing.T) {
	root := t.TempDir()
	buildTree(t, root)
	kb := NewKB([]string{root})

	results, err := kb.Search(context.Background(), "LOGIN", 5)
	if err != nil {
		t.Fatalf("Search 返回错误: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("期望 1 条结果（usage.txt 第 1 行），实际 %d 条", len(results))
	}
	if results[0].Path != "docs/usage.txt" || results[0].Line != 1 {
		t.Errorf("期望命中 docs/usage.txt 第 1 行，实际 %s 第 %d 行", results[0].Path, results[0].Line)
	}
}

// TestSearch_skipsIgnoredContent 验证 .git / node_modules / 二进制 / 不支持扩展名 / 超限文件全部跳过。
func TestSearch_skipsIgnoredContent(t *testing.T) {
	root := t.TempDir()
	buildTree(t, root)
	kb := NewKB([]string{root})

	results, err := kb.Search(context.Background(), "登录", 50)
	if err != nil {
		t.Fatalf("Search 返回错误: %v", err)
	}
	for _, r := range results {
		if strings.HasPrefix(r.Path, ".git/") || strings.HasPrefix(r.Path, "node_modules/") {
			t.Errorf("跳过目录中的文件不应出现在结果中: %q", r.Path)
		}
		if r.Path == "docs/secret.go" {
			t.Errorf("二进制文件不应出现在结果中: %q", r.Path)
		}
		if r.Path == "assets/logo.png" {
			t.Errorf("不支持扩展名的文件不应出现在结果中: %q", r.Path)
		}
	}
}

// TestSearch_skipsOversizedFile 验证超过 maxFileSize 的文件被跳过。
func TestSearch_skipsOversizedFile(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "small.md"), "登录 小文件\n")
	// 构造超过 512KB 的文件，仍使用允许的扩展名。
	writeTestFile(t, filepath.Join(root, "big.md"), strings.Repeat("登录 big\n", 60*1024))
	kb := NewKB([]string{root})

	results, err := kb.Search(context.Background(), "登录", 10)
	if err != nil {
		t.Fatalf("Search 返回错误: %v", err)
	}
	for _, r := range results {
		if r.Path == "big.md" {
			t.Error("超过大小上限的文件不应出现在结果中")
		}
	}
	if len(results) != 1 || results[0].Path != "small.md" {
		t.Errorf("期望仅命中 small.md，实际 %+v", results)
	}
}

// TestSearch_contextLines 验证命中行前后各 1 行的上下文。
func TestSearch_contextLines(t *testing.T) {
	root := t.TempDir()
	buildTree(t, root)
	kb := NewKB([]string{root})

	results, err := kb.Search(context.Background(), "Token", 5)
	if err != nil {
		t.Fatalf("Search 返回错误: %v", err)
	}
	// "Token" 仅命中 api.md 第 2 行。
	if len(results) != 1 {
		t.Fatalf("期望 1 条结果，实际 %d 条", len(results))
	}
	r := results[0]
	wantCtx := "1: 用户登录接口说明\n3: 请在请求头携带 Authorization"
	if r.Context != wantCtx {
		t.Errorf("上下文不匹配:\n期望:\n%s\n实际:\n%s", wantCtx, r.Context)
	}
	if r.Line != 2 || r.Text != "登录接口需要 Token 鉴权" {
		t.Errorf("命中行不匹配: 行 %d %q", r.Line, r.Text)
	}
}

// TestSearch_limitSemantics 验证 limit 默认值与截断逻辑。
func TestSearch_limitSemantics(t *testing.T) {
	root := t.TempDir()
	buildTree(t, root)
	kb := NewKB([]string{root})

	// limit<=0 使用默认值 5。
	def, err := kb.Search(context.Background(), "登录", 0)
	if err != nil {
		t.Fatalf("Search(limit=0) 返回错误: %v", err)
	}
	explicit, err := kb.Search(context.Background(), "登录", 5)
	if err != nil {
		t.Fatalf("Search(limit=5) 返回错误: %v", err)
	}
	if len(def) != len(explicit) || len(def) != 4 {
		t.Errorf("limit=0 应等价于默认 5 条（实际 %d，期望 4）", len(def))
	}

	// limit=2 时只保留命中数最高的文件（api.md 的 2 条）。
	two, err := kb.Search(context.Background(), "登录", 2)
	if err != nil {
		t.Fatalf("Search(limit=2) 返回错误: %v", err)
	}
	if len(two) != 2 {
		t.Fatalf("limit=2 期望 2 条结果，实际 %d", len(two))
	}
	for _, r := range two {
		if r.Path != "docs/api.md" {
			t.Errorf("limit=2 应只保留 api.md，实际出现 %q", r.Path)
		}
	}
}

// TestSearch_multipleRoots 验证多个根目录：路径相对各自 root 展示。
func TestSearch_multipleRoots(t *testing.T) {
	dir := t.TempDir()
	rootA := filepath.Join(dir, "a")
	rootB := filepath.Join(dir, "b")
	writeTestFile(t, filepath.Join(rootA, "alpha.md"), "alpha 登录\n")
	writeTestFile(t, filepath.Join(rootB, "beta.md"), "beta 登录\n")
	kb := NewKB([]string{rootA, rootB})

	results, err := kb.Search(context.Background(), "登录", 5)
	if err != nil {
		t.Fatalf("Search 返回错误: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("期望 2 条结果，实际 %d", len(results))
	}
	if results[0].Path != "alpha.md" || results[1].Path != "beta.md" {
		t.Errorf("多 root 下应展示相对各自 root 的路径，实际 %q / %q", results[0].Path, results[1].Path)
	}
	if strings.HasPrefix(results[0].Path, "/") {
		t.Errorf("结果路径不应是绝对路径: %q", results[0].Path)
	}
}

// TestSearch_emptyQuery 验证空关键词被拒绝。
func TestSearch_emptyQuery(t *testing.T) {
	kb := NewKB([]string{t.TempDir()})
	if _, err := kb.Search(context.Background(), "  ", 5); err == nil {
		t.Error("空关键词应返回错误")
	}
}

// TestSearch_multiTokenQuery 验证多关键词检索：所有词在同一行出现才算命中，
// 且大小写不敏感。
func TestSearch_multiTokenQuery(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "doc.md"),
		"Vortex 是一个文档助手\nAgent 循环调度工具\nVortex agent 支持工具调用\n")

	// "vortex agent"：只有第 3 行同时包含两个词。
	results, err := NewKB([]string{root}).Search(context.Background(), "vortex agent", 5)
	if err != nil {
		t.Fatalf("Search 返回错误: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("期望 1 条结果，实际 %d 条: %+v", len(results), results)
	}
	if results[0].Line != 3 {
		t.Errorf("期望命中第 3 行，实际第 %d 行", results[0].Line)
	}

	// 大小写不敏感仍然成立。
	results, err = NewKB([]string{root}).Search(context.Background(), "AGENT VORTEX", 5)
	if err != nil {
		t.Fatalf("Search 返回错误: %v", err)
	}
	if len(results) != 1 || results[0].Line != 3 {
		t.Errorf("多关键词大小写不敏感失败: %+v", results)
	}
}

// TestSearch_limitHardCap 验证 Search 的 limit 硬上限（防模型传超大 limit 撑爆上下文）。
func TestSearch_limitHardCap(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "many.md"), strings.Repeat("命中行\n", 100))

	results, err := NewKB([]string{root}).Search(context.Background(), "命中", 9999)
	if err != nil {
		t.Fatalf("Search 返回错误: %v", err)
	}
	if len(results) != MaxSearchResults {
		t.Errorf("limit 应被钳制到 %d，实际 %d", MaxSearchResults, len(results))
	}
}

// TestSearch_canceledContext 验证 ctx 取消时提前退出。
func TestSearch_canceledContext(t *testing.T) {
	root := t.TempDir()
	buildTree(t, root)
	kb := NewKB([]string{root})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := kb.Search(ctx, "登录", 5); !errors.Is(err, context.Canceled) {
		t.Errorf("期望 context.Canceled，实际 %v", err)
	}
}

// TestRead_returnsFullContent 验证读取文档全文。
func TestRead_returnsFullContent(t *testing.T) {
	root := t.TempDir()
	buildTree(t, root)
	kb := NewKB([]string{root})

	content, err := kb.Read(context.Background(), "docs/api.md")
	if err != nil {
		t.Fatalf("Read 返回错误: %v", err)
	}
	if !strings.Contains(content, "用户登录接口说明") || !strings.Contains(content, "Authorization") {
		t.Errorf("Read 应返回完整内容，实际: %q", content)
	}
}

// TestRead_allowsDotDotWithinRoot 验证路径清理后仍在根目录内的读取被允许。
func TestRead_allowsDotDotWithinRoot(t *testing.T) {
	root := t.TempDir()
	buildTree(t, root)
	kb := NewKB([]string{root})

	content, err := kb.Read(context.Background(), "docs/../docs/api.md")
	if err != nil {
		t.Fatalf("根目录内的 ../ 应被清理后放行: %v", err)
	}
	if !strings.Contains(content, "用户登录接口说明") {
		t.Errorf("读取内容不完整: %q", content)
	}
}

// TestRead_rejectsPathEscape 验证越界路径（../ 前缀）被拒绝。
func TestRead_rejectsPathEscape(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "kb")
	writeTestFile(t, filepath.Join(root, "ok.md"), "安全内容\n")
	writeTestFile(t, filepath.Join(dir, "secret.md"), "秘密\n")
	kb := NewKB([]string{root})

	escapePaths := []string{
		"../secret.md",
		filepath.Join("..", "secret.md"),
		"docs/../../secret.md",
		"../../etc/passwd",
		"..",
	}
	for _, p := range escapePaths {
		if _, err := kb.Read(context.Background(), p); err == nil {
			t.Errorf("越界路径 %q 应被拒绝", p)
		}
	}
}

// TestRead_rejectsAbsoluteOutside 验证根目录之外的绝对路径被拒绝。
func TestRead_rejectsAbsoluteOutside(t *testing.T) {
	root := t.TempDir()
	buildTree(t, root)
	kb := NewKB([]string{root})

	if _, err := kb.Read(context.Background(), "/etc/passwd"); err == nil {
		t.Error("知识库外的绝对路径应被拒绝")
	}
}

// TestRead_rejectsSymlinkEscape 验证符号链接逃逸（链接指向根目录外）被拒绝。
func TestRead_rejectsSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "kb")
	writeTestFile(t, filepath.Join(root, "docs", "real.md"), "真实内容\n")
	writeTestFile(t, filepath.Join(dir, "secret.txt"), "机密\n")
	link := filepath.Join(root, "docs", "link.md")
	if err := os.Symlink(filepath.Join(dir, "secret.txt"), link); err != nil {
		t.Skipf("无法创建符号链接: %v", err)
	}
	kb := NewKB([]string{root})

	if _, err := kb.Read(context.Background(), "docs/link.md"); err == nil {
		t.Error("指向根目录外的符号链接应被拒绝")
	}
	// 根目录内的符号链接仍应可读。
	innerLink := filepath.Join(root, "docs", "inner.md")
	if err := os.Symlink(filepath.Join(root, "docs", "real.md"), innerLink); err != nil {
		t.Skipf("无法创建符号链接: %v", err)
	}
	content, err := kb.Read(context.Background(), "docs/inner.md")
	if err != nil {
		t.Fatalf("根目录内的符号链接应可读: %v", err)
	}
	if !strings.Contains(content, "真实内容") {
		t.Errorf("读取内容不完整: %q", content)
	}
}

// TestRead_truncatesLargeDocument 验证超过 100KB 的文档被截断并提示。
func TestRead_truncatesLargeDocument(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "large.md"), strings.Repeat("x", MaxReadDocumentSize+1000))
	kb := NewKB([]string{root})

	content, err := kb.Read(context.Background(), "large.md")
	if err != nil {
		t.Fatalf("Read 返回错误: %v", err)
	}
	if !strings.Contains(content, "[提示：文档超过 100KB") {
		t.Error("超限文档应包含截断提示")
	}
	if len(content) >= MaxReadDocumentSize+1000 {
		t.Errorf("超限文档应被截断，实际长度 %d", len(content))
	}
}

// TestRead_rejectsBinary 验证二进制文件读取被拒绝。
func TestRead_rejectsBinary(t *testing.T) {
	root := t.TempDir()
	buildTree(t, root)
	kb := NewKB([]string{root})

	if _, err := kb.Read(context.Background(), "docs/secret.go"); err == nil {
		t.Error("二进制文件应被拒绝读取")
	}
}

// TestRead_afterAddRoot 验证 AddRoot 追加根目录后可以读取。
func TestRead_afterAddRoot(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "doc.md"), "追加根目录\n")
	kb := NewKB(nil)
	kb.AddRoot(root)

	content, err := kb.Read(context.Background(), "doc.md")
	if err != nil {
		t.Fatalf("AddRoot 后 Read 返回错误: %v", err)
	}
	if !strings.Contains(content, "追加根目录") {
		t.Errorf("读取内容不完整: %q", content)
	}
}

// TestSetExtensions 验证扩展名可配置。
func TestSetExtensions(t *testing.T) {
	root := t.TempDir()
	buildTree(t, root)
	kb := NewKB([]string{root})
	kb.SetExtensions([]string{".md"})

	results, err := kb.Search(context.Background(), "登录", 10)
	if err != nil {
		t.Fatalf("Search 返回错误: %v", err)
	}
	for _, r := range results {
		if r.Path == "notes/todo.txt" {
			t.Error("SetExtensions 后 .txt 文件不应被检索")
		}
	}
}

// TestNewKBTools 验证工具列表构造、schema 形态与方法签名。
func TestNewKBTools(t *testing.T) {
	root := t.TempDir()
	buildTree(t, root)
	kb := NewKB([]string{root})
	tools := NewKBTools(kb)

	if len(tools) != 2 {
		t.Fatalf("期望 2 个工具，实际 %d", len(tools))
	}

	var searchTool, readTool *Tool
	for _, tk := range tools {
		if tk.Name() == "search_knowledge" {
			searchTool = tk
		}
		if tk.Name() == "read_document" {
			readTool = tk
		}
	}
	if searchTool == nil || readTool == nil {
		t.Fatalf("应同时包含 search_knowledge 与 read_document，实际 %q / %q",
			tools[0].Name(), tools[1].Name())
	}
	if searchTool.Description() == "" || readTool.Description() == "" {
		t.Error("工具描述不应为空")
	}

	// Schema 必须可序列化且符合 JSON Schema 形态。
	for _, tk := range tools {
		data, err := json.Marshal(tk.Schema())
		if err != nil {
			t.Fatalf("Schema 无法序列化为 JSON: %v", err)
		}
		if len(data) == 0 {
			t.Error("Schema JSON 不应为空")
		}
		schema := tk.Schema()
		if schema["type"] != "object" {
			t.Errorf("Schema.type 应为主对象: %v", schema["type"])
		}
		if _, ok := schema["properties"].(map[string]any); !ok {
			t.Error("Schema 应包含 properties")
		}
		required, ok := schema["required"].([]string)
		if !ok || len(required) == 0 {
			t.Errorf("Schema 应包含非空 required 数组: %+v", schema["required"])
		}
	}
}

// TestSearchTool_Call 验证 search_knowledge 工具的参数解析与调用。
func TestSearchTool_Call(t *testing.T) {
	root := t.TempDir()
	buildTree(t, root)
	kb := NewKB([]string{root})
	tools := NewKBTools(kb)
	searchTool := tools[0]

	// 正常调用：结果文本应包含文件路径与行号。
	out, err := searchTool.Call(context.Background(), map[string]any{"query": "登录", "limit": float64(3)})
	if err != nil {
		t.Fatalf("Call 返回错误: %v", err)
	}
	if !strings.Contains(out, "docs/api.md") || !strings.Contains(out, "行 1") {
		t.Errorf("输出应包含文件路径与行号，实际:\n%s", out)
	}

	// json.Number 形式的 limit 也应支持。
	_, err = searchTool.Call(context.Background(), map[string]any{"query": "登录", "limit": json.Number("2")})
	if err != nil {
		t.Fatalf("json.Number limit 调用失败: %v", err)
	}

	// 缺少 query 应报错。
	if _, err := searchTool.Call(context.Background(), map[string]any{}); err == nil {
		t.Error("缺少 query 应报错")
	}
	// limit 非整数应报错。
	if _, err := searchTool.Call(context.Background(), map[string]any{"query": "x", "limit": "abc"}); err == nil {
		t.Error("limit 非整数应报错")
	}
	// 空 query 应报错。
	if _, err := searchTool.Call(context.Background(), map[string]any{"query": "   "}); err == nil {
		t.Error("空 query 应报错")
	}
}

// TestReadDocumentTool_Call 验证 read_document 工具的参数解析与路径安全。
func TestReadDocumentTool_Call(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "kb")
	writeTestFile(t, filepath.Join(root, "doc.md"), "工具读取内容\n")
	kb := NewKB([]string{root})
	tools := NewKBTools(kb)
	readTool := tools[1]

	out, err := readTool.Call(context.Background(), map[string]any{"path": "doc.md"})
	if err != nil {
		t.Fatalf("Call 返回错误: %v", err)
	}
	if !strings.Contains(out, "工具读取内容") {
		t.Errorf("输出应包含文档内容，实际: %q", out)
	}

	// 缺少 path 应报错。
	if _, err := readTool.Call(context.Background(), map[string]any{}); err == nil {
		t.Error("缺少 path 应报错")
	}
	// 越界路径应报错。
	if _, err := readTool.Call(context.Background(), map[string]any{"path": "../secret.md"}); err == nil {
		t.Error("越界路径应报错")
	}
}
