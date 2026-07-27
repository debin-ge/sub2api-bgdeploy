package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

type healthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	Slot    string `json:"slot"`
}

type containerConfig struct {
	Image string   `json:"Image"`
	Env   []string `json:"Env"`
}

func (a *app) healthProbe(ctx context.Context, port int) (healthResponse, error) {
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: (&net.Dialer{
				Timeout: 3 * time.Second,
			}).DialContext,
		},
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return healthResponse{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return healthResponse{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return healthResponse{}, fmt.Errorf("%s 返回 HTTP %d", url, response.StatusCode)
	}
	var health healthResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&health); err != nil {
		return healthResponse{}, fmt.Errorf("解析 %s 响应: %w", url, err)
	}
	return health, nil
}

func (a *app) waitForHealth(ctx context.Context, site resolvedSite, slot string, port int) (healthResponse, error) {
	deadline := time.NewTimer(time.Duration(site.HealthTimeoutSeconds) * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Duration(site.HealthIntervalSeconds) * time.Second)
	defer ticker.Stop()

	for {
		health, err := a.healthProbe(ctx, port)
		if err == nil {
			return health, nil
		}
		select {
		case <-ctx.Done():
			return healthResponse{}, ctx.Err()
		case <-deadline.C:
			return healthResponse{}, fmt.Errorf("健康门禁超时（%ds）", site.HealthTimeoutSeconds)
		case <-ticker.C:
		}
	}
}

func (a *app) validateHealthIdentity(ctx context.Context, site resolvedSite, slot string, port int, tag string, health healthResponse) error {
	if strings.TrimSpace(health.Status) != "ok" {
		return fmt.Errorf("健康状态校验失败: /health 返回 status=%q，预期 ok", health.Status)
	}
	if health.Slot != "" && health.Slot != slot {
		return fmt.Errorf("slot 校验失败: /health 返回 slot=%s，预期 %s", health.Slot, slot)
	}
	if health.Version != "" && versionTagPattern.MatchString(tag) && strings.TrimPrefix(health.Version, "v") != strings.TrimPrefix(tag, "v") {
		return fmt.Errorf("版本校验失败: /health 返回 version=%s，预期 %s", health.Version, tag)
	}

	var missing []string
	if strings.TrimSpace(health.Slot) == "" {
		missing = append(missing, "slot")
	}
	if strings.TrimSpace(health.Version) == "" {
		missing = append(missing, "version")
	}
	if len(missing) == 0 {
		return nil
	}
	if err := a.validateContainerIdentity(ctx, site, slot, port, tag); err != nil {
		return fmt.Errorf("身份校验失败: /health 未返回 %s，且容器元数据校验失败: %w", strings.Join(missing, "/"), err)
	}
	a.warn("兼容旧版健康接口: /health 未返回 %s，已通过 Docker 元数据确认 slot=%s image=%s:%s",
		strings.Join(missing, "/"), slot, site.ImageRepo, tag)
	return nil
}

func (a *app) validateContainerIdentity(ctx context.Context, site resolvedSite, slot string, port int, tag string) error {
	output, err := a.appCompose(ctx, false, site.Slug, slot, port, tag, "ps", "-q", "app")
	if err != nil {
		return fmt.Errorf("读取 app 容器 ID: %w", err)
	}
	containerIDs := strings.Fields(output)
	if len(containerIDs) != 1 {
		return fmt.Errorf("期望 1 个 app 容器，实际得到 %d 个", len(containerIDs))
	}
	output, err = a.runCapture(ctx, nil, "docker", "inspect", "--format", "{{json .Config}}", containerIDs[0])
	if err != nil {
		return fmt.Errorf("读取 app 容器配置: %w", err)
	}
	var config containerConfig
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &config); err != nil {
		return fmt.Errorf("解析 app 容器配置: %w", err)
	}
	expectedImage := site.ImageRepo + ":" + tag
	if config.Image != expectedImage {
		return fmt.Errorf("容器镜像为 %q，预期 %q", config.Image, expectedImage)
	}
	expectedSlot := "APP_SLOT=" + slot
	for _, value := range config.Env {
		if value == expectedSlot {
			return nil
		}
	}
	return fmt.Errorf("容器环境变量缺少 %s", expectedSlot)
}
