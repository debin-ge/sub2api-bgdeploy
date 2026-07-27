package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type lifecycleTarget struct {
	slot string
	port int
	tag  string
}

func (a *app) stop(ctx context.Context, slug string) error {
	site, err := a.prepareLifecycle(ctx, slug, false)
	if err != nil {
		return err
	}
	release, err := a.acquireStackLock(slug)
	if err != nil {
		return err
	}
	defer release()

	return a.stopLocked(ctx, site, true)
}

func (a *app) start(ctx context.Context, slug string) error {
	site, err := a.prepareLifecycle(ctx, slug, true)
	if err != nil {
		return err
	}
	release, err := a.acquireStackLock(slug)
	if err != nil {
		return err
	}
	defer release()

	return a.startLocked(ctx, site)
}

func (a *app) restart(ctx context.Context, slug string) error {
	site, err := a.prepareLifecycle(ctx, slug, true)
	if err != nil {
		return err
	}
	release, err := a.acquireStackLock(slug)
	if err != nil {
		return err
	}
	defer release()

	if err := a.stopLocked(ctx, site, false); err != nil {
		return fmt.Errorf("站点停止未完成，取消重新启动: %w", err)
	}
	return a.startLocked(ctx, site)
}

func (a *app) prepareLifecycle(ctx context.Context, slug string, requireEnvironment bool) (resolvedSite, error) {
	if err := a.checkRoot(); err != nil {
		return resolvedSite{}, err
	}
	if err := a.checkDocker(ctx); err != nil {
		return resolvedSite{}, err
	}
	sites, err := a.loadSites()
	if err != nil {
		return resolvedSite{}, err
	}
	site, err := findSite(sites, slug)
	if err != nil {
		return resolvedSite{}, err
	}
	for _, name := range []string{"compose.data.yml", "compose.app.yml"} {
		path := filepath.Join(a.stackDir(slug), name)
		if _, err := os.Stat(path); err != nil {
			return resolvedSite{}, fmt.Errorf("缺少渲染产物 %s；先执行 sudo ./bgdeploy render", path)
		}
	}
	if requireEnvironment {
		if err := a.validateSiteEnvironment(slug); err != nil {
			return resolvedSite{}, err
		}
		if err := a.validateStackEnvironmentLink(slug); err != nil {
			return resolvedSite{}, err
		}
	}
	return site, nil
}

func (a *app) stopLocked(ctx context.Context, site resolvedSite, showResumeHint bool) error {
	targets, activeSlot, _ := a.lifecycleTargets(site)

	for _, slot := range []string{slotBlue, slotGreen} {
		if _, err := a.cancelTeardown(ctx, site.Slug, slot); err != nil {
			return fmt.Errorf("取消 %s 排空任务失败，站点尚未停止: %w", slot, err)
		}
	}

	// Stop the inactive slot first so the active slot remains available for as
	// long as possible. If the upstream cannot be read, use the stable
	// blue/green order and still make a best effort to stop the whole site.
	if activeSlot != "" && len(targets) == 2 && targets[0].slot == activeSlot {
		targets[0], targets[1] = targets[1], targets[0]
	}

	a.log("停止站点 %s（应用层 → 数据层）...", site.Slug)
	var stopErrors []error
	for _, target := range targets {
		a.log("停止应用 slot %s:%d（遵守 stop_grace_period）...", target.slot, target.port)
		if _, err := a.appCompose(ctx, true, site.Slug, target.slot, target.port, target.tag, "stop"); err != nil {
			stopErrors = append(stopErrors, fmt.Errorf("停止应用 slot %s: %w", target.slot, err))
		}
	}
	a.log("停止数据层 PostgreSQL/Redis...")
	if _, err := a.dataCompose(ctx, true, site.Slug, "stop"); err != nil {
		stopErrors = append(stopErrors, fmt.Errorf("停止数据层: %w", err))
	}
	if err := errors.Join(stopErrors...); err != nil {
		return fmt.Errorf("站点 %s 未能完全停止: %w", site.Slug, err)
	}

	a.log("站点 %s 已停止；容器、数据、STATE 与 Nginx upstream 均已保留", site.Slug)
	if showResumeHint {
		a.log("恢复服务: sudo ./bgdeploy start %s", site.Slug)
	}
	return nil
}

func (a *app) startLocked(ctx context.Context, site resolvedSite) error {
	targets, activeSlot, upstreamErr := a.lifecycleTargets(site)
	if upstreamErr != nil {
		return fmt.Errorf("无法确定需要恢复的生效 slot: %w（先执行 sudo ./bgdeploy render）", upstreamErr)
	}
	var active lifecycleTarget
	for _, target := range targets {
		if target.slot == activeSlot {
			active = target
			break
		}
	}
	if active.slot == "" {
		return errors.New("无法确定需要恢复的生效 slot")
	}
	if active.tag == "" || active.tag == "unknown" {
		return fmt.Errorf("Nginx upstream 与 STATE 均未记录 %s 的镜像 tag，无法安全恢复；请执行 sudo ./bgdeploy deploy %s <image-tag>",
			active.slot, site.Slug)
	}
	if !a.appExists(ctx, site.Slug, active.slot, active.port, active.tag) {
		return fmt.Errorf("生效 slot %s 没有可恢复的应用容器；请执行 sudo ./bgdeploy deploy %s %s",
			active.slot, site.Slug, active.tag)
	}
	if cancelled, err := a.cancelTeardown(ctx, site.Slug, active.slot); err != nil {
		return fmt.Errorf("取消生效 slot %s 的排空任务失败，拒绝启动: %w", active.slot, err)
	} else if cancelled {
		a.log("已取消生效 slot %s 的排空任务", active.slot)
	}

	a.log("启动站点 %s（数据层 → 生效应用 slot）...", site.Slug)
	a.log("启动数据层 PostgreSQL/Redis 并等待健康...")
	if _, err := a.dataCompose(ctx, true, site.Slug, "up", "-d", "--wait"); err != nil {
		return fmt.Errorf("数据层未能进入 healthy 状态，站点未启动: %w", err)
	}

	a.log("启动生效 slot %s:%d tag=%s...", active.slot, active.port, active.tag)
	if _, err := a.appCompose(ctx, true, site.Slug, active.slot, active.port, active.tag, "start"); err != nil {
		return fmt.Errorf("生效 slot %s 启动失败（数据层仍在运行）: %w", active.slot, err)
	}
	a.log("健康门禁: 轮询 http://127.0.0.1:%d/health（间隔 %ds，总超时 %ds）...",
		active.port, site.HealthIntervalSeconds, site.HealthTimeoutSeconds)
	health, err := a.waitForHealth(ctx, site, active.slot, active.port)
	if err == nil {
		err = a.validateHealthIdentity(ctx, site, active.slot, active.port, active.tag, health)
	}
	if err != nil {
		a.warn("恢复后的生效 slot 未通过健康门禁，将重新停止该 slot；数据层保持运行以便排查")
		if _, stopErr := a.appCompose(ctx, true, site.Slug, active.slot, active.port, active.tag, "stop"); stopErr != nil {
			return errors.Join(err, fmt.Errorf("补偿停止 slot %s 失败: %w", active.slot, stopErr))
		}
		return fmt.Errorf("站点 %s 启动失败: %w", site.Slug, err)
	}

	a.log("站点 %s 已恢复: %s:%d tag=%s", site.Slug, active.slot, active.port, active.tag)
	return nil
}

func (a *app) lifecycleTargets(site resolvedSite) ([]lifecycleTarget, string, error) {
	activeSlot := ""
	upstreamTag := ""
	currentPort, tag, upstreamErr := a.readCurrentUpstream(site.Slug)
	if upstreamErr == nil {
		activeSlot, upstreamErr = slotForPort(site.PortBase, currentPort)
		upstreamTag = tag
	}
	state, _ := a.readState(site.Slug)

	targets := make([]lifecycleTarget, 0, 2)
	for _, slot := range []string{slotBlue, slotGreen} {
		port, _ := portForSlot(site.PortBase, slot)
		slotTag := ""
		if state.Slot == slot {
			slotTag = state.Tag
		} else if state.PrevSlot == slot {
			slotTag = state.PrevTag
		}
		if slot == activeSlot {
			if state.Slot == slot {
				slotTag = firstString(slotTag, upstreamTag)
			} else {
				slotTag = firstString(upstreamTag, slotTag)
			}
		}
		slotTag = firstString(slotTag, site.ImageTag, "unknown")
		targets = append(targets, lifecycleTarget{slot: slot, port: port, tag: slotTag})
	}
	return targets, activeSlot, upstreamErr
}
