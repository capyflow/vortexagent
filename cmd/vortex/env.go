package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// loadDotEnv 读取 .env 文件并把其中的 KEY=VALUE 写入进程环境。
//
// 行为约定（与主流 dotenv 实现一致）：
//   - 文件不存在时静默返回 nil（.env 是可选的便捷配置）
//   - 不覆盖已存在的环境变量（已有值优先）
//   - 忽略空行与 # 开头的注释行
//   - 不支持引号转义等复杂语法，KEY 与 VALUE 之间按第一个 '=' 切分
//
// 纯标准库实现，避免为这个几十行的功能引入第三方依赖。
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !ok || key == "" {
			return fmt.Errorf("%s:%d: 格式不正确（应为 KEY=VALUE）", path, lineNo)
		}
		if _, exists := os.LookupEnv(key); exists {
			continue // 已有环境变量优先，不覆盖
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("%s:%d: 设置环境变量 %s 失败: %w", path, lineNo, key, err)
		}
	}
	return scanner.Err()
}
