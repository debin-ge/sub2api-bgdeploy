package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	slotBlue  = "blue"
	slotGreen = "green"
)

var (
	upstreamPortPattern = regexp.MustCompile(`server\s+127\.0\.0\.1:([0-9]+)`)
	upstreamTagPattern  = regexp.MustCompile(`\btag=([^\s]+)`)
	versionTagPattern   = regexp.MustCompile(`^v?[0-9]+(?:\.[0-9]+)*$`)
)

type deploymentState struct {
	Slot     string
	Tag      string
	PrevSlot string
	PrevTag  string
	At       string
}

func (a *app) readCurrentUpstream(slug string) (int, string, error) {
	content, err := os.ReadFile(a.upstreamPath(slug))
	if err != nil {
		return 0, "", fmt.Errorf("读取 upstream %s: %w", a.upstreamPath(slug), err)
	}
	match := upstreamPortPattern.FindSubmatch(content)
	if len(match) != 2 {
		return 0, "", fmt.Errorf("upstream %s 中没有可识别的 127.0.0.1 端口", a.upstreamPath(slug))
	}
	port, err := strconv.Atoi(string(match[1]))
	if err != nil {
		return 0, "", fmt.Errorf("解析 upstream 端口: %w", err)
	}
	tag := ""
	if tagMatch := upstreamTagPattern.FindSubmatch(content); len(tagMatch) == 2 {
		tag = string(tagMatch[1])
	}
	return port, tag, nil
}

func (a *app) renderUpstream(site resolvedSite, slot, tag string) ([]byte, error) {
	port, err := portForSlot(site.PortBase, slot)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(tag) == "" {
		tag = "unknown"
	}
	return renderAsset("templates/nginx-upstream.conf.tmpl", map[string]string{
		"SLUG":      site.Slug,
		"SLUG_US":   slugUnderscore(site.Slug),
		"APP_PORT":  strconv.Itoa(port),
		"SLOT":      slot,
		"IMAGE_TAG": tag,
		"TIMESTAMP": a.now().UTC().Format(time.RFC3339),
	})
}

func (a *app) readState(slug string) (deploymentState, error) {
	content, err := os.ReadFile(a.statePath(slug))
	if err != nil {
		return deploymentState{}, err
	}
	values := make(map[string]string)
	for _, line := range strings.Split(string(content), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[key] = value
		}
	}
	return deploymentState{
		Slot:     values["slot"],
		Tag:      values["tag"],
		PrevSlot: values["prev_slot"],
		PrevTag:  values["prev_tag"],
		At:       values["at"],
	}, nil
}

func (a *app) writeState(slug string, state deploymentState) error {
	state.At = a.now().UTC().Format(time.RFC3339)
	content := fmt.Sprintf("slot=%s\ntag=%s\nprev_slot=%s\nprev_tag=%s\nat=%s\n",
		state.Slot, state.Tag, state.PrevSlot, state.PrevTag, state.At)
	return atomicWrite(a.statePath(slug), []byte(content), 0o644)
}

func (a *app) acquireStackLock(slug string) (func(), error) {
	lockDir := filepath.Join(a.stackDir(slug), ".op.lock")
	tryCreate := func() error {
		if err := os.Mkdir(lockDir, 0o700); err != nil {
			return err
		}
		return atomicWrite(filepath.Join(lockDir, "pid"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
	}
	if err := tryCreate(); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("获取 %s 操作锁: %w", slug, err)
		}
		holderContent, _ := os.ReadFile(filepath.Join(lockDir, "pid"))
		holder, parseErr := strconv.Atoi(strings.TrimSpace(string(holderContent)))
		stale := parseErr == nil && holder > 1 && errors.Is(syscall.Kill(holder, 0), syscall.ESRCH)
		if !stale {
			return nil, fmt.Errorf("另一个 %s 操作正在进行（pid=%s），拒绝并发执行", slug, firstString(strings.TrimSpace(string(holderContent)), "unknown"))
		}
		a.warn("清理残留锁（持有者 pid=%d 已退出）", holder)
		if err := os.RemoveAll(lockDir); err != nil {
			return nil, fmt.Errorf("清理残留锁: %w", err)
		}
		if err := tryCreate(); err != nil {
			return nil, fmt.Errorf("重新获取 %s 操作锁: %w", slug, err)
		}
	}
	return func() {
		if err := os.RemoveAll(lockDir); err != nil {
			a.warn("释放 %s 操作锁失败: %v", slug, err)
		}
	}, nil
}
