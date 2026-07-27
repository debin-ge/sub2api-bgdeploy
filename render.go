package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var nginxVersionPattern = regexp.MustCompile(`/([0-9]+)\.([0-9]+)\.([0-9]+)`)

type http2Syntax struct {
	listenSuffix string
	directive    string
}

func slugUnderscore(slug string) string {
	return strings.ReplaceAll(slug, "-", "_")
}

func (a *app) render(ctx context.Context) error {
	if err := a.checkRoot(); err != nil {
		return err
	}
	if err := a.requireCommands("nginx"); err != nil {
		return err
	}
	if err := a.checkNginxIntegration(ctx, false); err != nil {
		if !a.isManagedNginxConfigError(err) {
			return err
		}
		a.warn("检测到部署工具管理的 Nginx 配置无效，将尝试重新渲染修复: %v", err)
	}

	sites, err := a.loadSites()
	if err != nil {
		return err
	}
	a.log("清单校验通过: %s", a.registryFile)
	http2 := a.detectHTTP2Syntax(ctx)

	for _, dir := range []string{a.stacksDir, a.nginxDir, a.nginxUpstreams, a.nginxSites, a.nginxSnippetDir} {
		if err := ensureDir(dir, 0o755); err != nil {
			return err
		}
	}

	httpConfig, err := renderAsset("templates/nginx-http.conf.tmpl", map[string]string{
		"NGINX_BG_DIR": a.nginxDir,
	})
	if err != nil {
		return err
	}
	if err := atomicWrite(filepath.Join(a.nginxDir, "http.conf"), httpConfig, 0o644); err != nil {
		return err
	}

	for _, site := range sortedSites(sites) {
		if err := a.renderSite(site, http2); err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(a.envsDir, site.Slug+".env")); err != nil {
			a.warn("%s 的密钥文件缺失: %s（init 前需创建）", site.Slug, filepath.Join(a.envsDir, site.Slug+".env"))
		}
	}

	snippet, err := readAsset("snippets/blue-green-proxy.conf")
	if err != nil {
		return err
	}
	snippetPath := filepath.Join(a.nginxSnippetDir, "blue-green-proxy.conf")
	if existing, readErr := os.ReadFile(snippetPath); readErr == nil && !bytes.Equal(existing, snippet) {
		a.warn("覆盖已漂移的 %s（以内置版本为准）", snippetPath)
	}
	if err := atomicWrite(snippetPath, snippet, 0o644); err != nil {
		return err
	}

	if err := a.checkNginxIntegration(ctx, false); err != nil {
		return errorsWithMessage(err, "nginx -t 校验失败，未执行 reload；请检查 nginx.conf include 与渲染产物")
	}
	if err := a.runAttached(ctx, nil, "nginx", "-s", "reload"); err != nil {
		return errorsWithMessage(err, "nginx reload 失败")
	}
	a.log("全部产物渲染完成，nginx 已 reload")
	return nil
}

func (a *app) renderSite(site resolvedSite, http2 http2Syntax) error {
	stackDir := a.stackDir(site.Slug)
	if err := ensureDir(stackDir, 0o755); err != nil {
		return err
	}

	dataCompose, err := renderAsset("templates/compose.data.yml.tmpl", map[string]string{
		"SLUG": site.Slug,
		"TZ":   site.TZ,
	})
	if err != nil {
		return err
	}
	appCompose, err := renderAsset("templates/compose.app.yml.tmpl", map[string]string{
		"SLUG":          site.Slug,
		"IMAGE_REPO":    site.ImageRepo,
		"BIND_HOST":     site.BindHost,
		"DRAIN_SECONDS": fmt.Sprintf("%d", site.DrainSeconds),
		"TZ":            site.TZ,
	})
	if err != nil {
		return err
	}
	siteConfig, err := renderAsset("templates/nginx-site.conf.tmpl", map[string]string{
		"DOMAIN":                site.Domain,
		"SLUG":                  site.Slug,
		"SLUG_US":               slugUnderscore(site.Slug),
		"TLS_CERT":              site.TLSCert,
		"TLS_KEY":               site.TLSKey,
		"NGINX_SNIPPET_DIR":     a.nginxSnippetDir,
		"CLIENT_MAX_BODY_SIZE":  site.ClientMaxBodySize,
		"PROXY_CONNECT_TIMEOUT": site.ProxyConnectTimeout,
		"PROXY_SEND_TIMEOUT":    site.ProxySendTimeout,
		"PROXY_READ_TIMEOUT":    site.ProxyReadTimeout,
		"HTTP2_LISTEN_SUFFIX":   http2.listenSuffix,
		"HTTP2_DIRECTIVE":       http2.directive,
	})
	if err != nil {
		return err
	}

	for path, content := range map[string][]byte{
		filepath.Join(stackDir, "compose.data.yml"):    dataCompose,
		filepath.Join(stackDir, "compose.app.yml"):     appCompose,
		filepath.Join(a.nginxSites, site.Slug+".conf"): siteConfig,
	} {
		if err := atomicWrite(path, content, 0o644); err != nil {
			return err
		}
	}

	upstream, err := a.renderUpstream(site, slotBlue, site.ImageTag)
	if err != nil {
		return err
	}
	created, err := writeIfMissing(a.upstreamPath(site.Slug), upstream, 0o644)
	if err != nil {
		return err
	}
	if created {
		a.log("%s: 初始化 upstream → blue:%d", site.Slug, site.PortBase)
	}
	a.log("%s: compose + nginx site 渲染完成", site.Slug)
	return nil
}

func (a *app) detectHTTP2Syntax(ctx context.Context) http2Syntax {
	legacy := http2Syntax{listenSuffix: " http2"}
	output, err := a.runCombinedCapture(ctx, nil, "nginx", "-v")
	if err != nil {
		a.warn("无法检测 Nginx 版本，将使用兼容语法 listen ... ssl http2: %v", err)
		return legacy
	}
	version, ok := parseNginxVersion(output)
	if !ok {
		a.warn("无法从 %q 识别 Nginx 版本，将使用兼容语法 listen ... ssl http2", strings.TrimSpace(output))
		return legacy
	}
	if versionAtLeast(version, [3]int{1, 25, 1}) {
		a.log("检测到 Nginx %d.%d.%d，使用 http2 on 语法", version[0], version[1], version[2])
		return http2Syntax{directive: "    http2 on;\n"}
	}
	a.log("检测到 Nginx %d.%d.%d，使用 listen ... ssl http2 兼容语法", version[0], version[1], version[2])
	return legacy
}

func parseNginxVersion(output string) ([3]int, bool) {
	match := nginxVersionPattern.FindStringSubmatch(output)
	if match == nil {
		return [3]int{}, false
	}
	var version [3]int
	for index := range version {
		value, err := strconv.Atoi(match[index+1])
		if err != nil {
			return [3]int{}, false
		}
		version[index] = value
	}
	return version, true
}

func versionAtLeast(actual, minimum [3]int) bool {
	for index := range actual {
		if actual[index] != minimum[index] {
			return actual[index] > minimum[index]
		}
	}
	return true
}

func errorsWithMessage(err error, message string) error {
	return fmt.Errorf("%s: %w", message, err)
}
