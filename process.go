package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var workerShutdownPattern = regexp.MustCompile(`(?m)^\s*worker_shutdown_timeout\s+[0-9]+[smh]?;`)

func commandAvailable(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func envList(extra map[string]string) []string {
	result := os.Environ()
	keys := make([]string, 0, len(extra))
	for key := range extra {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		result = append(result, key+"="+extra[key])
	}
	return result
}

func (a *app) runAttached(ctx context.Context, extraEnv map[string]string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = envList(extraEnv)
	cmd.Stdout = a.stdout
	cmd.Stderr = a.stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("执行 %s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func (a *app) runCapture(ctx context.Context, extraEnv map[string]string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = envList(extraEnv)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = strings.TrimSpace(stdout.String())
		}
		if message != "" {
			return stdout.String(), fmt.Errorf("执行 %s %s: %w: %s", name, strings.Join(args, " "), err, message)
		}
		return stdout.String(), fmt.Errorf("执行 %s %s: %w", name, strings.Join(args, " "), err)
	}
	return stdout.String(), nil
}

func (a *app) runCombinedCapture(ctx context.Context, extraEnv map[string]string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = envList(extraEnv)
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message != "" {
			return string(output), fmt.Errorf("执行 %s %s: %w: %s", name, strings.Join(args, " "), err, message)
		}
		return string(output), fmt.Errorf("执行 %s %s: %w", name, strings.Join(args, " "), err)
	}
	return string(output), nil
}

func (a *app) requireCommands(names ...string) error {
	var missing []string
	for _, name := range names {
		if !commandAvailable(name) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("缺少依赖: %s（安装后重试）", strings.Join(missing, ", "))
	}
	return nil
}

func (a *app) checkDocker(ctx context.Context) error {
	if err := a.requireCommands("docker"); err != nil {
		return err
	}
	if _, err := a.runCapture(ctx, nil, "docker", "compose", "version"); err != nil {
		return errors.New("docker compose v2 不可用（请安装 Docker Compose v2）")
	}
	if _, err := a.runCapture(ctx, nil, "docker", "info"); err != nil {
		return errors.New("无法访问 Docker daemon（请确认 Docker 已启动且当前用户有权限）")
	}
	return nil
}

func (a *app) checkRuntimeDependencies(ctx context.Context) error {
	if err := a.checkRoot(); err != nil {
		return err
	}
	if err := a.requireCommands("nginx"); err != nil {
		return err
	}
	return a.checkDocker(ctx)
}

func (a *app) checkDependencies(ctx context.Context) error {
	if err := a.checkRuntimeDependencies(ctx); err != nil {
		return err
	}
	a.log("检查 nginx 配置接入...")
	if err := a.checkNginxIntegration(ctx, true); err != nil {
		return err
	}
	if !commandAvailable("systemd-run") {
		a.warn("systemd-run 不可用，排空回收将使用 bgdeploy 后台子进程")
	}
	a.log("依赖检查通过: Docker/Compose、nginx 与蓝绿配置均可用")
	return nil
}

func (a *app) checkNginxIntegration(ctx context.Context, requireManagedConfig bool) error {
	if _, err := a.runCapture(ctx, nil, "nginx", "-t"); err != nil {
		return &nginxConfigTestError{err: err}
	}
	dump, err := a.runCapture(ctx, nil, "nginx", "-T")
	if err != nil {
		return errors.New("无法读取 nginx 完整配置（nginx -T 失败）")
	}
	includeTarget := filepath.Join(a.nginxDir, "*.conf")
	includePattern := regexp.MustCompile(`(?m)^\s*include\s+["']?` + regexp.QuoteMeta(includeTarget) + `["']?\s*;`)
	if !workerShutdownPattern.MatchString(dump) || !includePattern.MatchString(dump) {
		return fmt.Errorf(`nginx 一次性配置不完整，操作已中断。
请在 nginx.conf 的 main 上下文加入:
  worker_shutdown_timeout 1200s;
并在 http {} 上下文加入:
  include %s;`, includeTarget)
	}
	if requireManagedConfig && !strings.Contains(dump, "blue-green-managed-http-config") {
		return fmt.Errorf("nginx 未加载 %s；一次性 include 已存在，请先执行 sudo ./bgdeploy render", filepath.Join(a.nginxDir, "http.conf"))
	}
	return nil
}

type nginxConfigTestError struct {
	err error
}

func (e *nginxConfigTestError) Error() string {
	return fmt.Sprintf("nginx -t 未通过（请先修复 nginx 配置）: %v", e.err)
}

func (e *nginxConfigTestError) Unwrap() error {
	return e.err
}

func (a *app) isManagedNginxConfigError(err error) bool {
	var configErr *nginxConfigTestError
	if !errors.As(err, &configErr) {
		return false
	}
	message := configErr.Error()
	for _, dir := range []string{a.nginxDir, a.nginxSnippetDir} {
		prefix := filepath.Clean(dir) + string(os.PathSeparator)
		if strings.Contains(message, prefix) {
			return true
		}
	}
	return false
}
