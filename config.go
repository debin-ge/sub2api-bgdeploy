package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type sitesConfig struct {
	Defaults siteDefaults `yaml:"defaults"`
	Stacks   []siteConfig `yaml:"stacks"`
}

type siteDefaults struct {
	ImageRepo             string `yaml:"image_repo"`
	BindHost              string `yaml:"bind_host"`
	DrainSeconds          int    `yaml:"drain_seconds"`
	HealthTimeoutSeconds  int    `yaml:"health_timeout_seconds"`
	HealthIntervalSeconds int    `yaml:"health_interval_seconds"`
	ClientMaxBodySize     string `yaml:"client_max_body_size"`
	ProxyConnectTimeout   string `yaml:"proxy_connect_timeout"`
	ProxySendTimeout      string `yaml:"proxy_send_timeout"`
	ProxyReadTimeout      string `yaml:"proxy_read_timeout"`
	TZ                    string `yaml:"tz"`
}

type siteConfig struct {
	Slug                  string    `yaml:"slug"`
	Domain                string    `yaml:"domain"`
	PortBase              int       `yaml:"port_base"`
	ImageTag              string    `yaml:"image_tag"`
	ImageRepo             string    `yaml:"image_repo"`
	BindHost              string    `yaml:"bind_host"`
	DrainSeconds          int       `yaml:"drain_seconds"`
	HealthTimeoutSeconds  int       `yaml:"health_timeout_seconds"`
	HealthIntervalSeconds int       `yaml:"health_interval_seconds"`
	ClientMaxBodySize     string    `yaml:"client_max_body_size"`
	ProxyConnectTimeout   string    `yaml:"proxy_connect_timeout"`
	ProxySendTimeout      string    `yaml:"proxy_send_timeout"`
	ProxyReadTimeout      string    `yaml:"proxy_read_timeout"`
	TZ                    string    `yaml:"tz"`
	TLS                   tlsConfig `yaml:"tls"`
}

type tlsConfig struct {
	Cert string `yaml:"cert"`
	Key  string `yaml:"key"`
}

type resolvedSite struct {
	Slug                  string
	Domain                string
	PortBase              int
	ImageTag              string
	ImageRepo             string
	BindHost              string
	DrainSeconds          int
	HealthTimeoutSeconds  int
	HealthIntervalSeconds int
	ClientMaxBodySize     string
	ProxyConnectTimeout   string
	ProxySendTimeout      string
	ProxyReadTimeout      string
	TZ                    string
	TLSCert               string
	TLSKey                string
}

var (
	slugPattern          = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	domainPattern        = regexp.MustCompile(`^(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]{2,63}$`)
	imageRepoPattern     = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]*$`)
	imageTagValuePattern = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$`)
	bodySizePattern      = regexp.MustCompile(`^[1-9][0-9]*[kKmMgG]?$`)
	timeoutPattern       = regexp.MustCompile(`^[1-9][0-9]*(?:ms|s|m|h|d)?$`)
	tzPattern            = regexp.MustCompile(`^[a-zA-Z0-9_+./-]+$`)
)

func (a *app) loadSites() ([]resolvedSite, error) {
	content, err := os.ReadFile(a.registryFile)
	if err != nil {
		return nil, fmt.Errorf("读取清单 %s: %w", a.registryFile, err)
	}
	var config sitesConfig
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("解析清单 %s: %w", a.registryFile, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("解析清单 %s: 只允许一个 YAML 文档", a.registryFile)
		}
		return nil, fmt.Errorf("解析清单 %s: %w", a.registryFile, err)
	}
	if len(config.Stacks) == 0 {
		return nil, fmt.Errorf("清单中没有任何 stack")
	}

	sites := make([]resolvedSite, 0, len(config.Stacks))
	for _, raw := range config.Stacks {
		site := resolveSite(config.Defaults, raw)
		sites = append(sites, site)
	}
	if err := validateSites(sites); err != nil {
		return nil, err
	}
	return sites, nil
}

func resolveSite(defaults siteDefaults, site siteConfig) resolvedSite {
	return resolvedSite{
		Slug:                  site.Slug,
		Domain:                site.Domain,
		PortBase:              site.PortBase,
		ImageTag:              site.ImageTag,
		ImageRepo:             firstString(site.ImageRepo, defaults.ImageRepo),
		BindHost:              firstString(site.BindHost, defaults.BindHost, "127.0.0.1"),
		DrainSeconds:          firstInt(site.DrainSeconds, defaults.DrainSeconds, 960),
		HealthTimeoutSeconds:  firstInt(site.HealthTimeoutSeconds, defaults.HealthTimeoutSeconds, 300),
		HealthIntervalSeconds: firstInt(site.HealthIntervalSeconds, defaults.HealthIntervalSeconds, 3),
		ClientMaxBodySize:     firstString(site.ClientMaxBodySize, defaults.ClientMaxBodySize, "32m"),
		ProxyConnectTimeout:   firstString(site.ProxyConnectTimeout, defaults.ProxyConnectTimeout, "10s"),
		ProxySendTimeout:      firstString(site.ProxySendTimeout, defaults.ProxySendTimeout, "960s"),
		ProxyReadTimeout:      firstString(site.ProxyReadTimeout, defaults.ProxyReadTimeout, "960s"),
		TZ:                    firstString(site.TZ, defaults.TZ, "Asia/Shanghai"),
		TLSCert:               site.TLS.Cert,
		TLSKey:                site.TLS.Key,
	}
}

func firstString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func firstInt(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func validateSites(sites []resolvedSite) error {
	seenSlugs := make(map[string]struct{}, len(sites))
	seenDomains := make(map[string]string, len(sites))
	type portRange struct {
		slug       string
		start, end int
	}
	ranges := make([]portRange, 0, len(sites))

	for _, site := range sites {
		if !slugPattern.MatchString(site.Slug) {
			return fmt.Errorf("slug 非法: %q（仅允许小写字母/数字/连字符）", site.Slug)
		}
		if _, ok := seenSlugs[site.Slug]; ok {
			return fmt.Errorf("slug 重复: %s", site.Slug)
		}
		seenSlugs[site.Slug] = struct{}{}

		if !domainPattern.MatchString(site.Domain) {
			return fmt.Errorf("%s 的 domain 非法: %q（只允许单个完整域名）", site.Slug, site.Domain)
		}
		domainKey := strings.ToLower(site.Domain)
		if other, ok := seenDomains[domainKey]; ok {
			return fmt.Errorf("域名重复: %s（%s 与 %s）", site.Domain, other, site.Slug)
		}
		seenDomains[domainKey] = site.Slug

		if site.PortBase < 1 || site.PortBase+9 > 65535 {
			return fmt.Errorf("%s 的 port_base 非法: %d", site.Slug, site.PortBase)
		}
		current := portRange{slug: site.Slug, start: site.PortBase, end: site.PortBase + 9}
		for _, existing := range ranges {
			if current.start <= existing.end && existing.start <= current.end {
				return fmt.Errorf("端口区间重叠: %s(%d-%d) 与 %s(%d-%d)",
					current.slug, current.start, current.end,
					existing.slug, existing.start, existing.end)
			}
		}
		ranges = append(ranges, current)

		if site.ImageRepo == "" {
			return fmt.Errorf("%s 缺少 image_repo", site.Slug)
		}
		if !imageRepoPattern.MatchString(site.ImageRepo) {
			return fmt.Errorf("%s 的 image_repo 非法: %q", site.Slug, site.ImageRepo)
		}
		if site.ImageTag != "" && !imageTagValuePattern.MatchString(site.ImageTag) {
			return fmt.Errorf("%s 的 image_tag 非法: %q", site.Slug, site.ImageTag)
		}
		bindIP := net.ParseIP(site.BindHost)
		if bindIP == nil || bindIP.To4() == nil {
			return fmt.Errorf("%s 的 bind_host 必须是 IPv4 地址: %q", site.Slug, site.BindHost)
		}
		if !bodySizePattern.MatchString(site.ClientMaxBodySize) {
			return fmt.Errorf("%s 的 client_max_body_size 非法: %q", site.Slug, site.ClientMaxBodySize)
		}
		for name, value := range map[string]string{
			"proxy_connect_timeout": site.ProxyConnectTimeout,
			"proxy_send_timeout":    site.ProxySendTimeout,
			"proxy_read_timeout":    site.ProxyReadTimeout,
		} {
			if !timeoutPattern.MatchString(value) {
				return fmt.Errorf("%s 的 %s 非法: %q", site.Slug, name, value)
			}
		}
		if !tzPattern.MatchString(site.TZ) {
			return fmt.Errorf("%s 的 tz 非法: %q", site.Slug, site.TZ)
		}
		if strings.TrimSpace(site.TLSCert) == "" || strings.TrimSpace(site.TLSKey) == "" {
			return fmt.Errorf("%s 缺少 tls.cert 或 tls.key", site.Slug)
		}
		if !filepath.IsAbs(site.TLSCert) || !filepath.IsAbs(site.TLSKey) {
			return fmt.Errorf("%s 的 TLS 证书和私钥必须使用绝对路径", site.Slug)
		}
		certInfo, err := os.Stat(site.TLSCert)
		if err != nil {
			return fmt.Errorf("%s TLS 证书不可用 %s: %w", site.Slug, site.TLSCert, err)
		}
		if !certInfo.Mode().IsRegular() {
			return fmt.Errorf("%s TLS 证书不是普通文件: %s", site.Slug, site.TLSCert)
		}
		keyInfo, err := os.Stat(site.TLSKey)
		if err != nil {
			return fmt.Errorf("%s TLS 私钥不可用 %s: %w", site.Slug, site.TLSKey, err)
		}
		if !keyInfo.Mode().IsRegular() {
			return fmt.Errorf("%s TLS 私钥不是普通文件: %s", site.Slug, site.TLSKey)
		}
	}
	return nil
}

func validateImageTag(tag string) error {
	if !imageTagValuePattern.MatchString(tag) {
		return fmt.Errorf("image-tag 非法: %q", tag)
	}
	return nil
}

func findSite(sites []resolvedSite, slug string) (resolvedSite, error) {
	for _, site := range sites {
		if site.Slug == slug {
			return site, nil
		}
	}
	return resolvedSite{}, fmt.Errorf("清单中不存在 stack: %s", slug)
}

func sortedSites(sites []resolvedSite) []resolvedSite {
	result := append([]resolvedSite(nil), sites...)
	sort.Slice(result, func(i, j int) bool { return result[i].Slug < result[j].Slug })
	return result
}
