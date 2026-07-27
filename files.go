package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func ensureDir(path string, mode fs.FileMode) error {
	if err := os.MkdirAll(path, mode); err != nil {
		return fmt.Errorf("创建目录 %s: %w", path, err)
	}
	return nil
}

func atomicWrite(path string, content []byte, mode fs.FileMode) error {
	if err := ensureDir(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("创建临时文件 %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("写入临时文件 %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("设置文件权限 %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("同步临时文件 %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时文件 %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("原子替换 %s: %w", path, err)
	}
	return nil
}

func writeIfMissing(path string, content []byte, mode fs.FileMode) (bool, error) {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return false, nil
	case !errors.Is(err, os.ErrNotExist):
		return false, fmt.Errorf("检查文件 %s: %w", path, err)
	}
	if err := atomicWrite(path, content, mode); err != nil {
		return false, err
	}
	return true, nil
}

func readAsset(name string) ([]byte, error) {
	content, err := runtimeAssets.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("读取内置资源 %s: %w", name, err)
	}
	return content, nil
}

func renderAsset(name string, replacements map[string]string) ([]byte, error) {
	content, err := readAsset(name)
	if err != nil {
		return nil, err
	}
	result := string(content)
	for key, value := range replacements {
		result = strings.ReplaceAll(result, "${"+key+"}", value)
	}
	if !strings.HasSuffix(result, "\n") {
		result += "\n"
	}
	return []byte(result), nil
}

func (a *app) bootstrap() error {
	if err := a.checkRoot(); err != nil {
		return err
	}
	for _, dir := range []string{
		filepath.Dir(a.registryFile),
		a.envsDir,
		a.stacksDir,
	} {
		if err := ensureDir(dir, 0o755); err != nil {
			return err
		}
	}

	sites, err := readAsset("sites.example.yaml")
	if err != nil {
		return err
	}
	env, err := readAsset("env.example")
	if err != nil {
		return err
	}
	runtimeConfig, err := renderAsset("runtime.example.yaml", map[string]string{
		"DEPLOY_ROOT":       a.root,
		"NGINX_DIR":         a.nginxDir,
		"NGINX_SNIPPET_DIR": a.nginxSnippetDir,
	})
	if err != nil {
		return err
	}

	createdSites, err := writeIfMissing(a.registryFile, sites, 0o644)
	if err != nil {
		return err
	}
	createdRuntime, err := writeIfMissing(a.runtimeConfig, runtimeConfig, 0o644)
	if err != nil {
		return err
	}
	envExample := filepath.Join(a.root, "env.example")
	createdEnv, err := writeIfMissing(envExample, env, 0o600)
	if err != nil {
		return err
	}

	if createdSites {
		a.log("已创建 %s", a.registryFile)
	} else {
		a.log("保留已有 %s", a.registryFile)
	}
	if createdEnv {
		a.log("已创建 %s", envExample)
	} else {
		a.log("保留已有 %s", envExample)
	}
	if createdRuntime {
		a.log("已创建 %s", a.runtimeConfig)
	} else {
		a.log("保留已有 %s", a.runtimeConfig)
	}
	a.log("初始化目录完成；下一步只需编辑 registry/sites.yaml 和 envs/<slug>.env")
	return nil
}
