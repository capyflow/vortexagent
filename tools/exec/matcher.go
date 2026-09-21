// 本文件实现 exec_command 工具的权限参数匹配器：把权限规则里的参数模式
// 解释为 shell 命令语义，供 agent.Checker 在判定 allow / deny 规则时调用。
//
// 两套语义，宽严刻意不同：
//   - CommandMatcher（allow 语义，从严）：复合命令按 ; && || | 换行 拆段，
//     所有段都命中模式才放行——`git status` 放行不了 `git status; rm -rf /`。
//   - CommandDenyMatcher（deny 语义，从宽）：对整条命令做词边界扫描，
//     连藏在命令替换里的片段也拦得住——deny 规则 rm -rf* 能拦下
//     echo $(rm -rf /)，而 format 不会被模式 rm 误伤。
//
// 注意这是字符串级过滤，不是沙箱：它防模型误用，防不了蓄意构造的绕过
// （如 base64 解码后执行）。高危环境请配合操作系统级沙箱使用。
package exec

import "strings"

// CommandMatcher 判断 args 中的 command 是否命中 allow 模式（从严语义）。
// 模式尾缀 * 表示前缀匹配（git status* 命中 git status --short）；不带 * 时
// 仍按词边界前缀理解（git status 命中 git status 与 git status -s，不命中
// git statusx）。复合命令要求每一段都命中；模式为 *（剥掉后为空）时全放行。
// 签名与 agent.ArgMatcher 一致，本包不 import agent，由使用方适配。
func CommandMatcher(pattern string, args map[string]any) bool {
	command, _ := args["command"].(string)
	if command == "" {
		return false
	}
	base := patternBase(pattern)
	if base == "" {
		return true
	}
	for _, seg := range splitCommand(command) {
		if seg != base && !strings.HasPrefix(seg, base+" ") {
			return false
		}
	}
	return true
}

// CommandDenyMatcher 判断 args 中的 command 是否命中 deny 模式（从宽语义）：
// 规范化后的整条命令中，模式以完整"词"的形式出现即命中（前后不能紧邻
// 字母/数字/下划线）。签名与 agent.ArgMatcher 一致。
func CommandDenyMatcher(pattern string, args map[string]any) bool {
	command, _ := args["command"].(string)
	if command == "" {
		return false
	}
	base := patternBase(pattern)
	if base == "" {
		return true
	}
	return containsWord(normalizeCommand(command), base)
}

// patternBase 规范化模式并剥离尾缀通配 *（git status* → git status）。
// 返回空串表示模式匹配一切。
func patternBase(pattern string) string {
	base := strings.TrimSuffix(normalizeCommand(pattern), "*")
	return strings.TrimRight(base, " ")
}

// normalizeCommand 折叠连续空白为单个空格并去首尾（git  status → git status）。
func normalizeCommand(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// splitCommand 按复合命令操作符（; && || | 及换行）拆段，感知单双引号——
// 引号内的分隔符不拆。单个 &（后台运行）不是分隔符，保留在段内。
// 返回的段已规范化空白，空段被丢弃。
func splitCommand(cmd string) []string {
	var (
		segs  []string
		cur   strings.Builder
		quote byte
	)
	for i := 0; i < len(cmd); i++ {
		ch := cmd[i]
		if quote != 0 {
			cur.WriteByte(ch)
			if ch == quote {
				quote = 0
			}
			continue
		}
		switch ch {
		case '\'', '"':
			quote = ch
			cur.WriteByte(ch)
		case ';', '\n':
			segs = append(segs, cur.String())
			cur.Reset()
		case '|':
			if i+1 < len(cmd) && cmd[i+1] == '|' {
				i++
			}
			segs = append(segs, cur.String())
			cur.Reset()
		case '&':
			if i+1 < len(cmd) && cmd[i+1] == '&' {
				i++
				segs = append(segs, cur.String())
				cur.Reset()
			} else {
				cur.WriteByte(ch)
			}
		default:
			cur.WriteByte(ch)
		}
	}
	segs = append(segs, cur.String())

	out := make([]string, 0, len(segs))
	for _, s := range segs {
		if s = normalizeCommand(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// containsWord 判断 s 中是否以完整词的形式包含 word（匹配位置的紧邻前后
// 不能是字母/数字/下划线）。非 ASCII 字节视为边界：deny 匹配从宽。
func containsWord(s, word string) bool {
	if word == "" {
		return false
	}
	for i := 0; i+len(word) <= len(s); i++ {
		if i > 0 && isWordByte(s[i-1]) {
			continue
		}
		if !strings.HasPrefix(s[i:], word) {
			continue
		}
		if end := i + len(word); end < len(s) && isWordByte(s[end]) {
			continue
		}
		return true
	}
	return false
}

func isWordByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}
