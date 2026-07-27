package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	envKeyPattern      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	placeholderPattern = regexp.MustCompile(`(?i)(change[_-]?this|replace[_-]?with|change[_-]?me|your[_-]?(password|secret|key|email))`)
	requiredEnvKeys    = []string{
		"POSTGRES_PASSWORD",
		"REDIS_PASSWORD",
		"JWT_SECRET",
		"TOTP_ENCRYPTION_KEY",
		"ADMIN_EMAIL",
		"ADMIN_PASSWORD",
	}
)

func parseEnvironment(content []byte) (map[string]string, error) {
	values := make(map[string]string)
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "\ufeff"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("第 %d 行缺少 '='", lineNumber)
		}
		key = strings.TrimSpace(key)
		if !envKeyPattern.MatchString(key) {
			return nil, fmt.Errorf("第 %d 行变量名非法: %q", lineNumber, key)
		}
		if _, exists := values[key]; exists {
			return nil, fmt.Errorf("第 %d 行变量重复: %s", lineNumber, key)
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("读取环境变量: %w", err)
	}
	return values, nil
}

func (a *app) validateSiteEnvironment(slug string) error {
	path := filepath.Join(a.envsDir, slug+".env")
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("环境变量文件不存在: %s（请从 env.example 复制并填写）", path)
		}
		return fmt.Errorf("检查环境变量文件 %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("环境变量文件必须是普通文件，拒绝使用目录或软链接: %s", path)
	}
	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf("环境变量文件权限必须为 0600，当前为 %04o: %s；请执行 chmod 600 %s",
			info.Mode().Perm(), path, path)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取环境变量文件 %s: %w", path, err)
	}
	values, err := parseEnvironment(content)
	if err != nil {
		return fmt.Errorf("解析环境变量文件 %s: %w", path, err)
	}
	exampleContent, err := readAsset("env.example")
	if err != nil {
		return err
	}
	exampleValues, err := parseEnvironment(exampleContent)
	if err != nil {
		return fmt.Errorf("解析内置 env.example: %w", err)
	}
	for _, key := range requiredEnvKeys {
		value, exists := values[key]
		if !exists {
			return fmt.Errorf("环境变量文件 %s 缺少必要参数: %s", path, key)
		}
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("环境变量文件 %s 的必要参数为空: %s", path, key)
		}
		if value == exampleValues[key] || placeholderPattern.MatchString(value) {
			return fmt.Errorf("环境变量文件 %s 的 %s 仍是示例值，必须修改后才能部署", path, key)
		}
	}
	return nil
}

func (a *app) validateStackEnvironmentLink(slug string) error {
	link := filepath.Join(a.stackDir(slug), ".env")
	info, err := os.Lstat(link)
	if err != nil {
		return fmt.Errorf("stack 环境变量链接不存在: %s；请先执行 sudo ./bgdeploy init %s", link, slug)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("stack 环境变量入口不是软链接: %s；请先执行 sudo ./bgdeploy init %s", link, slug)
	}
	target, err := os.Readlink(link)
	if err != nil {
		return fmt.Errorf("读取 stack 环境变量链接 %s: %w", link, err)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(link), target)
	}
	expected := filepath.Join(a.envsDir, slug+".env")
	if filepath.Clean(target) != filepath.Clean(expected) {
		return fmt.Errorf("stack 环境变量链接指向错误: %s -> %s，期望 %s；请重新执行 sudo ./bgdeploy init %s",
			link, target, expected, slug)
	}
	return nil
}
