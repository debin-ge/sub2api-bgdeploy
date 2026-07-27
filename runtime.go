package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type runtimeSettings struct {
	Root            string `yaml:"root"`
	NginxDir        string `yaml:"nginx_dir"`
	NginxSnippetDir string `yaml:"nginx_snippet_dir"`
}

func resolveRuntimeSettings(requestedConfig, workingDirectory string) (runtimeSettings, string, error) {
	configPath := firstString(requestedConfig, os.Getenv("BGDEPLOY_CONFIG"))
	explicit := configPath != ""
	if configPath == "" {
		configPath = filepath.Join(workingDirectory, "runtime.yaml")
	}
	absoluteConfig, err := filepath.Abs(configPath)
	if err != nil {
		return runtimeSettings{}, "", fmt.Errorf("解析运行配置路径: %w", err)
	}
	content, err := os.ReadFile(absoluteConfig)
	if errors.Is(err, os.ErrNotExist) && !explicit {
		return runtimeSettings{}, absoluteConfig, nil
	}
	if err != nil {
		return runtimeSettings{}, "", fmt.Errorf("读取运行配置 %s: %w", absoluteConfig, err)
	}

	var settings runtimeSettings
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	if err := decoder.Decode(&settings); err != nil {
		return runtimeSettings{}, "", fmt.Errorf("解析运行配置 %s: %w", absoluteConfig, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return runtimeSettings{}, "", fmt.Errorf("解析运行配置 %s: 只允许一个 YAML 文档", absoluteConfig)
		}
		return runtimeSettings{}, "", fmt.Errorf("解析运行配置 %s: %w", absoluteConfig, err)
	}
	for name, value := range map[string]string{
		"root":              settings.Root,
		"nginx_dir":         settings.NginxDir,
		"nginx_snippet_dir": settings.NginxSnippetDir,
	} {
		if value != "" && !filepath.IsAbs(value) {
			return runtimeSettings{}, "", fmt.Errorf("运行配置 %s 必须使用绝对路径: %q", name, value)
		}
	}
	return settings, absoluteConfig, nil
}
