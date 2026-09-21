package exec

import "testing"

func TestSplitCommand(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"git status", []string{"git status"}},
		{"git status && git log", []string{"git status", "git log"}},
		{"ls; rm -rf /", []string{"ls", "rm -rf /"}},
		{"ls || echo x", []string{"ls", "echo x"}},
		{"cat a | grep b", []string{"cat a", "grep b"}},
		{"echo one\necho two", []string{"echo one", "echo two"}},
		// 引号内的分隔符不拆段
		{`echo "a; b" && echo 'c|d'`, []string{`echo "a; b"`, `echo 'c|d'`}},
		// 单个 &（后台运行）不是分隔符
		{"sleep 10 & echo done", []string{"sleep 10 & echo done"}},
		{"  ;;  ", nil}, // 全是分隔符：无段
	}
	for _, tc := range cases {
		got := splitCommand(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("splitCommand(%q) = %q, 期望 %q", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitCommand(%q)[%d] = %q, 期望 %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

func TestCommandMatcher(t *testing.T) {
	cases := []struct {
		pattern string
		command string
		want    bool
	}{
		// 前缀匹配：git status* 命中带子命令参数的调用
		{"git status*", "git status", true},
		{"git status*", "git status --short", true},
		{"git status*", "git status", true},
		// 词边界：前缀不越过单词边界
		{"git status*", "git statusx", false},
		{"git status*", "git stash", false},
		// 不带 * 同样按词边界前缀理解
		{"git status", "git status --short", true},
		{"git status", "git statusx", false},
		// 多余空白不影响
		{"git  status*", "git  status --short", true},
		// 复合命令：每段都要命中
		{"git*", "git status && git log", true},
		{"git*", "git status; rm -rf /", false},
		{"git status*", "git status && git log", false},
		// 引号内的分号不拆段
		{`echo*`, `echo "a; b"`, true},
		// * 通配一切
		{"*", "anything at all", true},
		{"*", "", false}, // 空 command 不放行
	}
	for _, tc := range cases {
		got := CommandMatcher(tc.pattern, map[string]any{"command": tc.command})
		if got != tc.want {
			t.Errorf("CommandMatcher(%q, %q) = %v, 期望 %v", tc.pattern, tc.command, got, tc.want)
		}
	}
	if CommandMatcher("ls", map[string]any{}) {
		t.Error("缺 command 参数时不应命中")
	}
}

func TestCommandDenyMatcher(t *testing.T) {
	cases := []struct {
		pattern string
		command string
		want    bool
	}{
		// 整串词边界扫描：命令替换里的危险片段也拦得住
		{"rm -rf*", "rm -rf /", true},
		{"rm -rf*", "echo $(rm -rf /)", true},
		{"rm -rf*", "ls; rm  -rf tmp", true}, // 多余空白折叠后命中
		{"rm -rf*", "rm file", false},
		// 短模式不误伤包含该字母序列的无关词
		{"rm", "format", false},
		{"rm", "rm -rf /", true},
		{"rm", "echo rm x", true}, // 词边界出现即命中（deny 从宽）
		{"dd", "git add .", false},
		{"shutdown", "shutdown -h now", true},
		{"*", "anything", true},
	}
	for _, tc := range cases {
		got := CommandDenyMatcher(tc.pattern, map[string]any{"command": tc.command})
		if got != tc.want {
			t.Errorf("CommandDenyMatcher(%q, %q) = %v, 期望 %v", tc.pattern, tc.command, got, tc.want)
		}
	}
}

func TestContainsWord(t *testing.T) {
	cases := []struct {
		s, word string
		want    bool
	}{
		{"git status", "git", true},
		{"gitx status", "git", false},    // 前面紧邻字母
		{"git statusx", "status", false}, // 后面紧邻字母
		{"a_git b", "git", false},        // 下划线算词字符
		{"x-git b", "git", true},         // 连字符是边界
		{"", "git", false},
	}
	for _, tc := range cases {
		if got := containsWord(tc.s, tc.word); got != tc.want {
			t.Errorf("containsWord(%q, %q) = %v, 期望 %v", tc.s, tc.word, got, tc.want)
		}
	}
}
