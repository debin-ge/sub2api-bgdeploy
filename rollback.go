package main

import (
	"context"
	"errors"
	"fmt"
)

func (a *app) rollback(ctx context.Context, slug string) error {
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
	release, err := a.acquireStackLock(slug)
	if err != nil {
		return err
	}
	locked := true
	defer func() {
		if locked {
			release()
		}
	}()

	state, err := a.readState(slug)
	if err != nil {
		return fmt.Errorf("读取 STATE: %w", err)
	}
	if state.PrevSlot == "" {
		return errors.New("STATE 中没有可回滚的上一个 slot（可能从未发布或仅首次部署）")
	}
	currentPort, _, err := a.readCurrentUpstream(slug)
	if err != nil {
		return err
	}
	currentSlot, err := slotForPort(site.PortBase, currentPort)
	if err != nil {
		return fmt.Errorf("upstream 端口异常，请人工核查: %w", err)
	}
	if currentSlot == state.PrevSlot {
		return fmt.Errorf("当前生效 slot 已是 %s，无需回滚", state.PrevSlot)
	}
	previousPort, err := portForSlot(site.PortBase, state.PrevSlot)
	if err != nil {
		return fmt.Errorf("STATE 中 prev_slot 非法: %w", err)
	}
	currentTag := firstString(state.Tag, "unknown")
	previousTag := firstString(state.PrevTag, "unknown")
	a.log("rollback %s: %s(%s) → %s(%s)", slug, currentSlot, currentTag, state.PrevSlot, previousTag)

	health, healthErr := a.healthProbe(ctx, previousPort)
	fastPath := a.appRunning(ctx, slug, state.PrevSlot, previousPort, previousTag) &&
		healthErr == nil && health.Slot == state.PrevSlot && health.Version != ""
	if fastPath {
		if cancelled, cancelErr := a.cancelTeardown(ctx, slug, state.PrevSlot); cancelErr != nil {
			return fmt.Errorf("取消目标 slot 排空任务失败，拒绝切流: %w", cancelErr)
		} else if cancelled {
			a.log("已取消 %s 的排空任务", state.PrevSlot)
		}
		if err := a.switchUpstream(ctx, site, state.PrevSlot, previousTag); err != nil {
			return fmt.Errorf("快速回滚失败，流量仍在 %s:%d: %w", currentSlot, currentPort, err)
		}
		if err := a.writeState(slug, deploymentState{
			Slot: state.PrevSlot, Tag: previousTag,
			PrevSlot: currentSlot, PrevTag: currentTag,
		}); err != nil {
			return fmt.Errorf("流量已切回 %s:%d，但写入 STATE 失败: %w", state.PrevSlot, previousPort, err)
		}
		if err := a.scheduleTeardown(ctx, slug, currentSlot, site.DrainSeconds); err != nil {
			return fmt.Errorf("快速回滚已生效，但 %s 自动回收任务创建失败，请人工 teardown: %w", currentSlot, err)
		}
		a.log("回滚完成（快速路径）: 流量已切回 %s:%d tag=%s", state.PrevSlot, previousPort, previousTag)
		a.log("被回滚的 %s 将在 %ds 后回收", currentSlot, site.DrainSeconds)
		a.log("注意: 已应用的数据库迁移不会撤销；旧代码必须兼容新 schema")
		return nil
	}

	if state.PrevTag == "" {
		return errors.New("上一 slot 不可用且 STATE 无 prev_tag，无法降级回滚")
	}
	a.warn("上一 slot %s 已回收、无响应或身份校验失败，快速路径不可用", state.PrevSlot)
	a.warn("降级为 tag=%s 的完整发布（包含拉镜像与健康门禁）", state.PrevTag)
	a.log("注意: 已应用的数据库迁移不会撤销；旧代码必须兼容新 schema")
	release()
	locked = false
	return a.deploy(ctx, slug, state.PrevTag)
}
