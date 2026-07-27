package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

func (a *app) dataComposeArgs(slug string, args ...string) []string {
	prefix := []string{
		"compose",
		"-p", slug + "-data",
		"--project-directory", a.stackDir(slug),
		"-f", filepath.Join(a.stackDir(slug), "compose.data.yml"),
	}
	return append(prefix, args...)
}

func (a *app) appComposeArgs(slug, slot string, args ...string) []string {
	prefix := []string{
		"compose",
		"-p", slug + "-" + slot,
		"--project-directory", a.stackDir(slug),
		"-f", filepath.Join(a.stackDir(slug), "compose.app.yml"),
	}
	return append(prefix, args...)
}

func appComposeEnv(slot string, port int, tag string) map[string]string {
	return map[string]string{
		"SLOT":      slot,
		"APP_PORT":  strconv.Itoa(port),
		"IMAGE_TAG": tag,
	}
}

func (a *app) dataCompose(ctx context.Context, attached bool, slug string, args ...string) (string, error) {
	commandArgs := a.dataComposeArgs(slug, args...)
	if attached {
		return "", a.runAttached(ctx, nil, "docker", commandArgs...)
	}
	return a.runCapture(ctx, nil, "docker", commandArgs...)
}

func (a *app) appCompose(ctx context.Context, attached bool, slug, slot string, port int, tag string, args ...string) (string, error) {
	commandArgs := a.appComposeArgs(slug, slot, args...)
	env := appComposeEnv(slot, port, tag)
	if attached {
		return "", a.runAttached(ctx, env, "docker", commandArgs...)
	}
	return a.runCapture(ctx, env, "docker", commandArgs...)
}

func (a *app) appRunning(ctx context.Context, slug, slot string, port int, tag string) bool {
	output, err := a.appCompose(ctx, false, slug, slot, port, tag, "ps", "--status", "running", "-q")
	return err == nil && strings.TrimSpace(output) != ""
}

func portForSlot(portBase int, slot string) (int, error) {
	switch slot {
	case slotBlue:
		return portBase, nil
	case slotGreen:
		return portBase + 1, nil
	default:
		return 0, fmt.Errorf("非法 slot: %s（仅允许 blue/green）", slot)
	}
}

func slotForPort(portBase, port int) (string, error) {
	switch port {
	case portBase:
		return slotBlue, nil
	case portBase + 1:
		return slotGreen, nil
	default:
		return "", fmt.Errorf("端口 %d 不属于 blue/green（port_base=%d）", port, portBase)
	}
}

func otherSlot(slot string) (string, error) {
	switch slot {
	case slotBlue:
		return slotGreen, nil
	case slotGreen:
		return slotBlue, nil
	default:
		return "", fmt.Errorf("非法 slot: %s", slot)
	}
}
