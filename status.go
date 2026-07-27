package main

import (
	"context"
	"fmt"
	"os"
	"strings"
)

func (a *app) status(ctx context.Context, requestedSlug string) error {
	if err := a.checkDocker(ctx); err != nil {
		return err
	}
	sites, err := a.loadSites()
	if err != nil {
		return err
	}
	if requestedSlug != "" {
		site, err := findSite(sites, requestedSlug)
		if err != nil {
			return err
		}
		return a.showStatus(ctx, site)
	}
	for _, site := range sortedSites(sites) {
		if err := a.showStatus(ctx, site); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) showStatus(ctx context.Context, site resolvedSite) error {
	fmt.Fprintf(a.stdout, "━━━ %s (%s) ━━━\n", site.Slug, site.Domain)
	currentSlot := ""
	currentPort, _, upstreamErr := a.readCurrentUpstream(site.Slug)
	if upstreamErr == nil {
		currentSlot, _ = slotForPort(site.PortBase, currentPort)
		if currentSlot == "" {
			currentSlot = "unknown"
		}
		fmt.Fprintf(a.stdout, "  生效(nginx upstream): %s:%d\n", currentSlot, currentPort)
	} else {
		fmt.Fprintln(a.stdout, "  生效(nginx upstream): 未初始化（未执行 sudo ./bgdeploy render？）")
	}

	state, stateErr := a.readState(site.Slug)
	if stateErr == nil && state.Slot != "" {
		fmt.Fprintf(a.stdout, "  STATE 记录: slot=%s tag=%s\n", state.Slot, firstString(state.Tag, "?"))
		if currentSlot != "" && state.Slot != currentSlot {
			fmt.Fprintf(a.stdout, "  ⚠ 不一致: STATE(%s) ≠ nginx(%s)——以 nginx 为准\n", state.Slot, currentSlot)
		}
	} else {
		fmt.Fprintln(a.stdout, "  STATE 记录: 无")
	}

	for _, slot := range []string{slotBlue, slotGreen} {
		port, _ := portForSlot(site.PortBase, slot)
		marker := " "
		if slot == currentSlot {
			marker = "*"
		}
		containerStatus := a.containerStatus(ctx, site.Slug, slot)
		healthText := "无响应"
		if health, err := a.healthProbe(ctx, port); err == nil {
			healthText = fmt.Sprintf("OK version=%s slot=%s", firstString(health.Version, "?"), firstString(health.Slot, "?"))
		}
		fmt.Fprintf(a.stdout, " %s%s :%d 容器[%s] 健康[%s]\n", marker, slot, port, containerStatus, healthText)
		if pending := a.teardownPending(ctx, site.Slug, slot); pending != "" {
			fmt.Fprintf(a.stdout, "    ⏳ 待回收: %s\n", pending)
		}
	}
	return nil
}

func (a *app) containerStatus(ctx context.Context, slug, slot string) string {
	output, err := a.runCapture(ctx, nil, "docker", "ps",
		"--filter", "label=com.docker.compose.project="+slug+"-"+slot,
		"--format", "{{.Status}} ({{.Image}})")
	if err != nil {
		return "查询失败"
	}
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) != "" {
			return strings.TrimSpace(line)
		}
	}
	return "未运行"
}

func (a *app) teardown(ctx context.Context, slug, slot string) error {
	if err := a.checkRoot(); err != nil {
		return err
	}
	if err := a.checkDocker(ctx); err != nil {
		return err
	}
	sites, err := a.loadSites()
	if err != nil {
		return err
	}
	site, err := findSite(sites, slug)
	if err != nil {
		return err
	}
	release, err := a.acquireStackLock(slug)
	if err != nil {
		return err
	}
	defer release()

	slotPort, err := portForSlot(site.PortBase, slot)
	if err != nil {
		return err
	}
	currentPort, _, err := a.readCurrentUpstream(slug)
	if err != nil {
		return fmt.Errorf("upstream 文件缺失或异常，拒绝回收: %w", err)
	}
	if currentPort == slotPort {
		return fmt.Errorf("拒绝回收: %s 的 %s(%d) 是当前生效 slot", slug, slot, slotPort)
	}
	tag := "unknown"
	if state, stateErr := a.readState(slug); stateErr == nil {
		if state.PrevSlot == slot {
			tag = firstString(state.PrevTag, tag)
		} else if state.Slot == slot {
			tag = firstString(state.Tag, tag)
		}
	}
	a.log("回收 %s 的旧 slot %s:%d（停止会遵守 stop_grace_period）...", slug, slot, slotPort)
	if _, err := a.appCompose(ctx, true, slug, slot, slotPort, tag, "down", "--remove-orphans"); err != nil {
		return err
	}
	if err := os.Remove(a.drainPIDFile(slug, slot)); err != nil && !os.IsNotExist(err) {
		a.warn("删除排空 pid 文件失败: %v", err)
	}
	a.log("旧 slot %s 已回收", slot)
	return nil
}
