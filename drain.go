package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

func drainUnit(slug, slot string) string {
	return "bgdeploy-drain-" + slug + "-" + slot
}

func (a *app) scheduleTeardown(ctx context.Context, slug, slot string, seconds int) error {
	_, _ = a.cancelTeardown(ctx, slug, slot)
	commandArgs := []string{
		"--root", a.root,
		"--nginx-dir", a.nginxDir,
		"--nginx-snippet-dir", a.nginxSnippetDir,
		"teardown", slug, slot,
	}
	if commandAvailable("systemd-run") {
		args := []string{
			"--unit=" + drainUnit(slug, slot),
			"--collect",
			fmt.Sprintf("--on-active=%ds", seconds),
			a.executable,
		}
		args = append(args, commandArgs...)
		if _, err := a.runCapture(ctx, nil, "systemd-run", args...); err != nil {
			return fmt.Errorf("创建 systemd 排空定时器: %w", err)
		}
		return nil
	}

	args := []string{
		"--root", a.root,
		"--nginx-dir", a.nginxDir,
		"--nginx-snippet-dir", a.nginxSnippetDir,
		"__drain", strconv.Itoa(seconds), slug, slot,
	}
	command := exec.Command(a.executable, args...)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("打开空设备: %w", err)
	}
	defer null.Close()
	command.Stdin = null
	command.Stdout = null
	command.Stderr = null
	if err := command.Start(); err != nil {
		return fmt.Errorf("启动后台排空进程: %w", err)
	}
	pid := command.Process.Pid
	if err := command.Process.Release(); err != nil {
		return fmt.Errorf("释放后台排空进程: %w", err)
	}
	if err := atomicWrite(a.drainPIDFile(slug, slot), []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		_ = syscall.Kill(pid, syscall.SIGTERM)
		return err
	}
	return nil
}

func (a *app) cancelTeardown(ctx context.Context, slug, slot string) (bool, error) {
	cancelled := false
	var collected []error
	if commandAvailable("systemctl") {
		unit := drainUnit(slug, slot)
		for _, suffix := range []string{".timer", ".service"} {
			if _, err := a.runCapture(ctx, nil, "systemctl", "stop", unit+suffix); err == nil {
				cancelled = true
			}
		}
	}

	pidFile := a.drainPIDFile(slug, slot)
	content, err := os.ReadFile(pidFile)
	switch {
	case err == nil:
		pid, parseErr := strconv.Atoi(strings.TrimSpace(string(content)))
		if parseErr != nil || pid < 2 {
			collected = append(collected, fmt.Errorf("非法排空 pid: %q", strings.TrimSpace(string(content))))
		} else if killErr := syscall.Kill(pid, syscall.SIGTERM); killErr == nil || errors.Is(killErr, syscall.ESRCH) {
			cancelled = true
		} else {
			collected = append(collected, fmt.Errorf("终止排空进程 %d: %w", pid, killErr))
		}
		if removeErr := os.Remove(pidFile); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			collected = append(collected, removeErr)
		}
	case !errors.Is(err, os.ErrNotExist):
		collected = append(collected, err)
	}
	return cancelled, errors.Join(collected...)
}

func (a *app) teardownPending(ctx context.Context, slug, slot string) string {
	unit := drainUnit(slug, slot)
	if commandAvailable("systemctl") {
		if output, err := a.runCapture(ctx, nil, "systemctl", "list-timers", "--all"); err == nil {
			for _, line := range strings.Split(output, "\n") {
				if strings.Contains(line, unit+".timer") {
					return strings.TrimSpace(line)
				}
			}
		}
	}
	content, err := os.ReadFile(a.drainPIDFile(slug, slot))
	if err != nil {
		return ""
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(content)))
	if err == nil && pid > 1 && syscall.Kill(pid, 0) == nil {
		return fmt.Sprintf("pid=%d（bgdeploy 后台排空进程）", pid)
	}
	return ""
}
