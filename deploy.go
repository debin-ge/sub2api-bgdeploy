package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func (a *app) deploy(ctx context.Context, slug, requestedTag string) error {
	if err := a.checkRuntimeDependencies(ctx); err != nil {
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
	if _, err := os.Stat(filepath.Join(a.stackDir(slug), "compose.app.yml")); err != nil {
		return fmt.Errorf("缺少渲染产物；先执行 sudo ./bgdeploy render && sudo ./bgdeploy init %s", slug)
	}
	if err := a.validateSiteEnvironment(slug); err != nil {
		return err
	}
	if err := a.validateStackEnvironmentLink(slug); err != nil {
		return err
	}
	a.log("环境变量检查通过: %s", filepath.Join(a.envsDir, slug+".env"))
	tag := firstString(requestedTag, site.ImageTag)
	if tag == "" {
		return errors.New("未指定 image-tag 且 sites.yaml 中无默认 image_tag")
	}
	if err := validateImageTag(tag); err != nil {
		return err
	}

	release, err := a.acquireStackLock(slug)
	if err != nil {
		return err
	}
	defer release()

	currentPort, upstreamTag, err := a.readCurrentUpstream(slug)
	if err != nil {
		return fmt.Errorf("%w（先执行 sudo ./bgdeploy render）", err)
	}
	currentSlot, err := slotForPort(site.PortBase, currentPort)
	if err != nil {
		return fmt.Errorf("upstream 端口异常，请人工核查: %w", err)
	}
	currentTag := upstreamTag
	if state, stateErr := a.readState(slug); stateErr == nil && state.Slot == currentSlot && state.Tag != "" {
		currentTag = state.Tag
	}
	currentTag = firstString(currentTag, "unknown")

	firstDeploy := !a.appRunning(ctx, slug, currentSlot, currentPort, currentTag)
	newSlot := currentSlot
	if !firstDeploy {
		newSlot, err = otherSlot(currentSlot)
		if err != nil {
			return err
		}
	}
	newPort, _ := portForSlot(site.PortBase, newSlot)
	if firstDeploy {
		a.log("deploy %s: 首次部署 → %s:%d tag=%s", slug, newSlot, newPort, tag)
	} else {
		a.log("deploy %s: %s(%s) → %s(%s)", slug, currentSlot, currentTag, newSlot, tag)
	}

	if cancelled, cancelErr := a.cancelTeardown(ctx, slug, newSlot); cancelErr != nil {
		a.warn("取消 %s 排空任务时出现问题: %v", newSlot, cancelErr)
	} else if cancelled {
		a.warn("目标 slot %s 的待回收任务已取消，由本次发布接管", newSlot)
	}
	if !firstDeploy && a.appRunning(ctx, slug, newSlot, newPort, tag) {
		a.warn("目标 slot %s 仍在运行；重建前 Docker 将等待其优雅停止（最长 %ds）", newSlot, site.DrainSeconds)
	}

	a.log("确认数据层（PostgreSQL/Redis）健康...")
	if _, err := a.dataCompose(ctx, true, slug, "up", "-d", "--wait"); err != nil {
		return fmt.Errorf("数据层未能进入 healthy，发布中止（线上不受影响）: %w", err)
	}

	recoverNewSlot := func() {
		if _, recoverErr := a.appCompose(ctx, false, slug, newSlot, newPort, tag, "down", "--remove-orphans"); recoverErr != nil {
			a.warn("回收新 slot %s 失败: %v", newSlot, recoverErr)
		}
	}
	failBeforeSwitch := func(reason error) error {
		a.warn("发布失败: %v", reason)
		a.warn("补偿动作: 已尝试回收新 slot %s", newSlot)
		if firstDeploy {
			a.warn("首次部署未完成: upstream 仍指向 %s:%d，但目标容器已回收，站点尚不可用", currentSlot, currentPort)
		} else {
			a.warn("线上影响: 无——流量仍在 %s:%d", currentSlot, currentPort)
		}
		return reason
	}

	a.log("拉起新 slot %s:%d（image tag: %s）...", newSlot, newPort, tag)
	if _, err := a.appCompose(ctx, true, slug, newSlot, newPort, tag, "up", "-d", "--pull", "always", "--force-recreate"); err != nil {
		recoverNewSlot()
		return failBeforeSwitch(errors.New("新 slot 容器启动命令失败"))
	}

	a.log("健康门禁: 轮询 http://127.0.0.1:%d/health（间隔 %ds，总超时 %ds）...",
		newPort, site.HealthIntervalSeconds, site.HealthTimeoutSeconds)
	health, err := a.waitForHealth(ctx, site, newSlot, newPort)
	if err != nil {
		a.warn("健康检查未通过，新 slot 最近 200 行日志如下:")
		_, _ = a.appCompose(ctx, true, slug, newSlot, newPort, tag, "logs", "--tail", "200")
		recoverNewSlot()
		return failBeforeSwitch(err)
	}
	if err := a.validateHealthIdentity(ctx, site, newSlot, newPort, tag, health); err != nil {
		recoverNewSlot()
		return failBeforeSwitch(err)
	}
	if health.Version != "" && !versionTagPattern.MatchString(tag) {
		a.log("运行版本: %s（tag %q 非版本号格式，跳过等值校验）", health.Version, tag)
	}
	a.log("健康门禁通过")

	if err := a.switchUpstream(ctx, site, newSlot, tag); err != nil {
		recoverNewSlot()
		return failBeforeSwitch(err)
	}
	a.log("流量已切至 %s:%d", newSlot, newPort)

	newState := deploymentState{Slot: newSlot, Tag: tag}
	if !firstDeploy {
		newState.PrevSlot = currentSlot
		newState.PrevTag = currentTag
	}
	if err := a.writeState(slug, newState); err != nil {
		return fmt.Errorf("流量已切至 %s:%d，但写入 STATE 失败，请立即检查: %w", newSlot, newPort, err)
	}

	if firstDeploy {
		a.log("首次部署完成，无旧 slot 需要排空")
	} else {
		if err := a.scheduleTeardown(ctx, slug, currentSlot, site.DrainSeconds); err != nil {
			return fmt.Errorf("发布已成功且流量在 %s:%d，但旧 slot %s 的自动回收任务创建失败，请稍后执行 sudo ./bgdeploy teardown %s %s: %w",
				newSlot, newPort, currentSlot, slug, currentSlot, err)
		}
		a.log("旧 slot %s 停止接收新请求，将在 %ds 后自动回收", currentSlot, site.DrainSeconds)
	}
	a.log("发布成功: %s 当前生效 %s:%d tag=%s", slug, newSlot, newPort, tag)
	if !firstDeploy {
		a.log("如需回退: sudo ./bgdeploy rollback %s（回滚不撤销数据库迁移）", slug)
	}
	return nil
}

func (a *app) switchUpstream(ctx context.Context, site resolvedSite, slot, tag string) error {
	path := a.upstreamPath(site.Slug)
	backupPath := path + ".bak"
	oldContent, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("备份 upstream: %w", err)
	}
	newContent, err := a.renderUpstream(site, slot, tag)
	if err != nil {
		return err
	}
	if err := atomicWrite(backupPath, oldContent, 0o600); err != nil {
		return fmt.Errorf("创建 upstream 备份: %w", err)
	}
	restore := func() error {
		restoreErr := atomicWrite(path, oldContent, 0o644)
		removeErr := os.Remove(backupPath)
		if errors.Is(removeErr, os.ErrNotExist) {
			removeErr = nil
		}
		return errors.Join(restoreErr, removeErr)
	}
	if err := atomicWrite(path, newContent, 0o644); err != nil {
		_ = os.Remove(backupPath)
		return err
	}
	if err := a.runAttached(ctx, nil, "nginx", "-t"); err != nil {
		if restoreErr := restore(); restoreErr != nil {
			return fmt.Errorf("nginx -t 校验失败，且 upstream 还原失败（需人工处理）: %v / %w", restoreErr, err)
		}
		return fmt.Errorf("nginx -t 校验失败（upstream 已还原，未 reload）: %w", err)
	}
	if err := a.runAttached(ctx, nil, "nginx", "-s", "reload"); err != nil {
		if restoreErr := restore(); restoreErr != nil {
			return fmt.Errorf("nginx reload 失败，且 upstream 还原失败（需人工处理）: %v / %w", restoreErr, err)
		}
		return fmt.Errorf("nginx reload 失败（upstream 文件已还原）: %w", err)
	}
	if err := os.Remove(backupPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		a.warn("删除 upstream 备份 %s 失败: %v", backupPath, err)
	}
	return nil
}
