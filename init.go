package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func (a *app) initStack(ctx context.Context, slug string) error {
	if err := a.checkDependencies(ctx); err != nil {
		return err
	}
	sites, err := a.loadSites()
	if err != nil {
		return err
	}
	if _, err := findSite(sites, slug); err != nil {
		return err
	}

	stackDir := a.stackDir(slug)
	for _, name := range []string{"compose.data.yml", "compose.app.yml"} {
		if _, err := os.Stat(filepath.Join(stackDir, name)); err != nil {
			return fmt.Errorf("缺少渲染产物 %s；先执行 sudo ./bgdeploy render", filepath.Join(stackDir, name))
		}
	}

	envFile := filepath.Join(a.envsDir, slug+".env")
	if err := a.validateSiteEnvironment(slug); err != nil {
		return err
	}
	a.log("环境变量检查通过: %s", envFile)

	envLink := filepath.Join(stackDir, ".env")
	if linkInfo, linkErr := os.Lstat(envLink); linkErr == nil {
		if linkInfo.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("%s 已存在且不是软链接，拒绝覆盖", envLink)
		}
		target, readErr := os.Readlink(envLink)
		if readErr != nil {
			return fmt.Errorf("读取 %s: %w", envLink, readErr)
		}
		if target != envFile {
			if err := os.Remove(envLink); err != nil {
				return fmt.Errorf("更新 env 软链接: %w", err)
			}
		}
	} else if !errors.Is(linkErr, os.ErrNotExist) {
		return fmt.Errorf("检查 env 软链接: %w", linkErr)
	}
	if _, err := os.Lstat(envLink); errors.Is(err, os.ErrNotExist) {
		if err := os.Symlink(envFile, envLink); err != nil {
			return fmt.Errorf("创建 env 软链接: %w", err)
		}
	}

	for _, name := range []string{"data", "postgres_data", "redis_data"} {
		if err := ensureDir(filepath.Join(stackDir, name), 0o755); err != nil {
			return err
		}
	}

	network := slug + "-net"
	if _, err := a.runCapture(ctx, nil, "docker", "network", "inspect", network); err != nil {
		if _, err := a.runCapture(ctx, nil, "docker", "network", "create", network); err != nil {
			return fmt.Errorf("创建 Docker network %s: %w", network, err)
		}
		a.log("创建 network %s", network)
	}

	a.log("启动数据层（PostgreSQL/Redis）...")
	if _, err := a.dataCompose(ctx, true, slug, "up", "-d", "--wait"); err != nil {
		return fmt.Errorf("数据层未能进入 healthy 状态: %w", err)
	}
	a.log("stack %s 初始化完成；首次部署: sudo ./bgdeploy deploy %s [tag]", slug, slug)
	return nil
}
