package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode"
)

type testEnvironment struct {
	root       string
	nginxDir   string
	snippetDir string
	stateDir   string
	flagsDir   string
	logFile    string
	certFile   string
	keyFile    string
	stdout     *bytes.Buffer
	stderr     *bytes.Buffer
	app        *app
}

func newTestEnvironment(t *testing.T) *testEnvironment {
	t.Helper()
	temp := t.TempDir()
	binDir := filepath.Join(temp, "bin")
	stateDir := filepath.Join(temp, "fake-state")
	flagsDir := filepath.Join(temp, "fake-flags")
	for _, dir := range []string{binDir, stateDir, flagsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	logFile := filepath.Join(temp, "commands.log")
	writeExecutable(t, filepath.Join(binDir, "docker"), `#!/bin/bash
set -eu
printf 'docker %s\n' "$*" >> "$BG_TEST_LOG"
if [ "${1:-}" = compose ]; then
  shift
  project=""
  while [ "$#" -gt 0 ]; do
    case "$1" in
      -p) project="$2"; shift 2 ;;
      --project-directory|-f) shift 2 ;;
      *) break ;;
    esac
  done
  command="${1:-}"
  case "$command" in
    version) exit 0 ;;
    up)
      touch "$BG_TEST_STATE/running-$project"
      if [ -n "${SLOT:-}" ]; then
        printf '%s\n' "$SLOT" > "$BG_TEST_STATE/container-slot-$project"
        printf 'registry.example.com/application:%s\n' "${IMAGE_TAG:-}" > "$BG_TEST_STATE/container-image-$project"
      fi
      exit 0
      ;;
    ps)
      [ ! -f "$BG_TEST_STATE/running-$project" ] || printf 'cid-%s\n' "$project"
      exit 0 ;;
    down)
      rm -f "$BG_TEST_STATE/running-$project" \
        "$BG_TEST_STATE/container-slot-$project" \
        "$BG_TEST_STATE/container-image-$project"
      exit 0
      ;;
    logs) printf 'fake logs for %s\n' "$project"; exit 0 ;;
  esac
  exit 0
fi
case "${1:-}" in
  info)
    [ ! -f "$BG_TEST_FLAGS/docker-info-fail" ]
    ;;
  inspect)
    container_id="${!#}"
    project="${container_id#cid-}"
    slot="$(cat "$BG_TEST_STATE/container-slot-$project")"
    image="$(cat "$BG_TEST_STATE/container-image-$project")"
    [ ! -f "$BG_TEST_FLAGS/docker-inspect-slot-mismatch" ] || slot="wrong-slot"
    [ ! -f "$BG_TEST_FLAGS/docker-inspect-image-mismatch" ] || image="registry.example.com/application:wrong-tag"
    printf '{"Image":"%s","Env":["APP_SLOT=%s"]}\n' "$image" "$slot"
    ;;
  network)
    name="${3:-}"
    case "${2:-}" in
      inspect) [ -f "$BG_TEST_STATE/network-$name" ] ;;
      create) touch "$BG_TEST_STATE/network-$name" ;;
    esac
    ;;
  ps)
    project=""
    while [ "$#" -gt 0 ]; do
      if [ "$1" = --filter ]; then project="${2##*=}"; shift 2; else shift; fi
    done
    [ ! -f "$BG_TEST_STATE/running-$project" ] || printf 'Up 1 minute (fake/image:latest)\n'
    ;;
esac
`)
	writeExecutable(t, filepath.Join(binDir, "nginx"), `#!/bin/bash
set -eu
printf 'nginx %s\n' "$*" >> "$BG_TEST_LOG"
if [ "${1:-}" = -v ]; then
  if [ -f "$BG_TEST_FLAGS/nginx-modern-version" ]; then
    printf 'nginx version: nginx/1.25.3\n' >&2
  else
    printf 'nginx version: nginx/1.24.0\n' >&2
  fi
  exit 0
fi
if [ "${1:-}" = -t ] && [ -f "$BG_TEST_FLAGS/nginx-test-fail" ]; then exit 1; fi
if [ "${1:-}" = -t ] && [ -f "$BG_TEST_FLAGS/nginx-managed-http2-fail" ]; then
  for file in "$BG_TEST_NGINX_DIR"/servers/*.conf; do
    [ -f "$file" ] || continue
    if grep -q '^[[:space:]]*http2 on;' "$file"; then
      printf 'nginx: [emerg] unknown directive "http2" in %s:18\n' "$file" >&2
      exit 1
    fi
  done
fi
if [ "${1:-}" = -T ]; then
  [ -f "$BG_TEST_FLAGS/nginx-base-include-missing" ] || printf 'include %s/*.conf;\n' "$BG_TEST_NGINX_DIR"
  [ -f "$BG_TEST_FLAGS/nginx-include-missing" ] || printf '# blue-green-managed-http-config\n'
  [ -f "$BG_TEST_FLAGS/nginx-worker-missing" ] || printf 'worker_shutdown_timeout 1200s;\n'
fi
`)
	writeExecutable(t, filepath.Join(binDir, "systemd-run"), `#!/bin/bash
set -eu
printf 'systemd-run %s\n' "$*" >> "$BG_TEST_LOG"
unit=""
for value in "$@"; do case "$value" in --unit=*) unit="${value#--unit=}" ;; esac; done
touch "$BG_TEST_STATE/timer-$unit"
`)
	writeExecutable(t, filepath.Join(binDir, "systemctl"), `#!/bin/bash
set -eu
printf 'systemctl %s\n' "$*" >> "$BG_TEST_LOG"
case "${1:-}" in
  stop)
    unit="${2%.timer}"; unit="${unit%.service}"
    if [ -f "$BG_TEST_STATE/timer-$unit" ]; then
      rm -f "$BG_TEST_STATE/timer-$unit"
      exit 0
    fi
    exit 1
    ;;
  list-timers)
    for file in "$BG_TEST_STATE"/timer-*; do
      [ ! -f "$file" ] || printf '%s.timer fake-pending\n' "${file##*/timer-}"
    done
    ;;
esac
`)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BG_TEST_STATE", stateDir)
	t.Setenv("BG_TEST_FLAGS", flagsDir)
	t.Setenv("BG_TEST_LOG", logFile)
	t.Setenv("BGDEPLOY_CONFIG", "")
	t.Setenv("BGDEPLOY_ROOT", "")
	t.Setenv("BGDEPLOY_NGINX_DIR", "")
	t.Setenv("BGDEPLOY_NGINX_SNIPPET_DIR", "")

	root := filepath.Join(temp, "srv")
	nginxDir := filepath.Join(temp, "nginx", "blue-green")
	snippetDir := filepath.Join(temp, "nginx", "snippets")
	t.Setenv("BG_TEST_NGINX_DIR", nginxDir)
	certFile := filepath.Join(temp, "tls", "cert.pem")
	keyFile := filepath.Join(temp, "tls", "key.pem")
	if err := os.MkdirAll(filepath.Dir(certFile), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{certFile, keyFile} {
		if err := os.WriteFile(file, []byte("test\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	testApp, err := newApp(root, nginxDir, snippetDir, stdout, stderr)
	if err != nil {
		t.Fatal(err)
	}
	testApp.euid = func() int { return 0 }
	testApp.now = func() time.Time { return time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC) }
	testApp.runtimeConfig = filepath.Join(root, "runtime.yaml")
	return &testEnvironment{
		root: root, nginxDir: nginxDir, snippetDir: snippetDir,
		stateDir: stateDir, flagsDir: flagsDir, logFile: logFile,
		certFile: certFile, keyFile: keyFile,
		stdout: stdout, stderr: stderr, app: testApp,
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func (environment *testEnvironment) writeSites(t *testing.T, portBase int) {
	t.Helper()
	for _, dir := range []string{filepath.Join(environment.root, "registry"), environment.app.envsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	content := fmt.Sprintf(`defaults:
  image_repo: registry.example.com/application
  bind_host: 127.0.0.1
  drain_seconds: 60
  health_timeout_seconds: 2
  health_interval_seconds: 1
  tz: UTC
stacks:
  - slug: api-test
    domain: test.example.com
    port_base: %d
    image_tag: v1.0.0
    tls:
      cert: %s
      key: %s
`, portBase, environment.certFile, environment.keyFile)
	if err := os.WriteFile(filepath.Join(environment.root, "registry", "sites.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (environment *testEnvironment) writeValidEnvironment(t *testing.T, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(environment.app.envsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(environment.app.envsDir, "api-test.env")
	content := `POSTGRES_PASSWORD=postgres-test-secret
REDIS_PASSWORD=redis-test-secret
JWT_SECRET=fixed-jwt-test-secret
TOTP_ENCRYPTION_KEY=fixed-totp-test-key
ADMIN_EMAIL=admin@test.invalid
ADMIN_PASSWORD=admin-test-password
`
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBootstrapDoesNotOverwriteConfiguration(t *testing.T) {
	environment := newTestEnvironment(t)
	if err := environment.app.bootstrap(); err != nil {
		t.Fatal(err)
	}
	sitesPath := filepath.Join(environment.root, "registry", "sites.yaml")
	if err := os.WriteFile(sitesPath, []byte("custom: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := environment.app.bootstrap(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(sitesPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "custom: true\n" {
		t.Fatalf("bootstrap overwrote sites.yaml: %q", content)
	}
	for _, path := range []string{
		filepath.Join(environment.root, "envs"),
		filepath.Join(environment.root, "stacks"),
		filepath.Join(environment.root, "env.example"),
		filepath.Join(environment.root, "runtime.yaml"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("bootstrap did not create %s: %v", path, err)
		}
	}
}

func TestCurrentDirectoryDefaults(t *testing.T) {
	temp := t.TempDir()
	previousDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(temp); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(previousDirectory); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	}()
	t.Setenv("BGDEPLOY_CONFIG", "")
	t.Setenv("BGDEPLOY_ROOT", "")
	t.Setenv("BGDEPLOY_NGINX_DIR", "")
	t.Setenv("BGDEPLOY_NGINX_SNIPPET_DIR", "")
	currentDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	configured, err := newApp("", "", "", io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if configured.root != currentDirectory {
		t.Fatalf("root = %s, want current directory %s", configured.root, currentDirectory)
	}
	if configured.runtimeConfig != filepath.Join(currentDirectory, "runtime.yaml") {
		t.Fatalf("runtimeConfig = %s", configured.runtimeConfig)
	}
	if configured.nginxDir != "/etc/nginx/sites" {
		t.Fatalf("nginxDir = %s", configured.nginxDir)
	}
	if configured.nginxSnippetDir != "/etc/nginx/sites/snippets" {
		t.Fatalf("nginxSnippetDir = %s", configured.nginxSnippetDir)
	}
}

func TestHelpIncludesCompleteOperationsGuide(t *testing.T) {
	for _, args := range [][]string{
		{"--help"},
		{"-h"},
		{"help"},
		{"deploy", "--help"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			stdout := new(bytes.Buffer)
			stderr := new(bytes.Buffer)
			if err := runCLI(context.Background(), args, stdout, stderr); err != nil {
				t.Fatalf("runCLI(%v): %v", args, err)
			}
			if stderr.Len() != 0 {
				t.Fatalf("runCLI(%v) stderr = %q", args, stderr)
			}
			help := stdout.String()
			for _, required := range []string{
				"bgdeploy [global options] <command> [arguments]",
				"bootstrap",
				"deploy <slug> [image-tag]",
				"teardown <slug> <blue|green>",
				"registry/sites.yaml",
				"envs/<slug>.env",
				"worker_shutdown_timeout 1200s;",
				"First-time setup:",
				"Routine blue-green release:",
				"Failure and safety behavior:",
				"command-line options > BGDEPLOY_* environment variables > runtime.yaml >",
			} {
				if !strings.Contains(help, required) {
					t.Errorf("runCLI(%v) help missing %q", args, required)
				}
			}
			for _, character := range help {
				if unicode.Is(unicode.Han, character) {
					t.Fatalf("runCLI(%v) help contains Chinese character %q", args, character)
				}
			}
		})
	}
}

func TestRuntimeConfigurationPrecedence(t *testing.T) {
	temp := t.TempDir()
	configPath := filepath.Join(temp, "runtime.yaml")
	configRoot := filepath.Join(temp, "from-config")
	configNginx := filepath.Join(temp, "nginx-from-config")
	configSnippet := filepath.Join(temp, "snippet-from-config")
	content := fmt.Sprintf("root: %s\nnginx_dir: %s\nnginx_snippet_dir: %s\n", configRoot, configNginx, configSnippet)
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	environmentRoot := filepath.Join(temp, "from-environment")
	flagRoot := filepath.Join(temp, "from-flag")
	environmentSnippet := filepath.Join(temp, "snippet-from-environment")
	t.Setenv("BGDEPLOY_ROOT", environmentRoot)
	t.Setenv("BGDEPLOY_NGINX_SNIPPET_DIR", environmentSnippet)

	configured, err := newAppWithConfig(configPath, flagRoot, "", "", io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if configured.root != flagRoot {
		t.Fatalf("root = %s, want flag value %s", configured.root, flagRoot)
	}
	if configured.nginxDir != configNginx {
		t.Fatalf("nginxDir = %s, want config value %s", configured.nginxDir, configNginx)
	}
	if configured.nginxSnippetDir != environmentSnippet {
		t.Fatalf("nginxSnippetDir = %s, want environment value %s", configured.nginxSnippetDir, environmentSnippet)
	}
}

func TestWriteCommandsRequireRoot(t *testing.T) {
	environment := newTestEnvironment(t)
	environment.app.euid = func() int { return 1000 }
	err := environment.app.bootstrap()
	if err == nil || !strings.Contains(err.Error(), "权限不足") {
		t.Fatalf("bootstrap error = %v, want permission failure", err)
	}
	if _, statErr := os.Stat(filepath.Join(environment.root, "registry")); !os.IsNotExist(statErr) {
		t.Fatalf("bootstrap mutated files without root: %v", statErr)
	}
}

func TestEnvironmentValidationRejectsExamplesAndMissingKeys(t *testing.T) {
	environment := newTestEnvironment(t)
	if err := os.MkdirAll(environment.app.envsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(environment.app.envsDir, "api-test.env")
	example, err := readAsset("env.example")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, example, 0o600); err != nil {
		t.Fatal(err)
	}
	err = environment.app.validateSiteEnvironment("api-test")
	if err == nil || !strings.Contains(err.Error(), "仍是示例值") {
		t.Fatalf("validation error = %v, want unchanged example failure", err)
	}

	if err := os.WriteFile(path, []byte("POSTGRES_PASSWORD=changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = environment.app.validateSiteEnvironment("api-test")
	if err == nil || !strings.Contains(err.Error(), "缺少必要参数: REDIS_PASSWORD") {
		t.Fatalf("validation error = %v, want missing required key failure", err)
	}

	environment.writeValidEnvironment(t, 0o600)
	if err := environment.app.validateSiteEnvironment("api-test"); err != nil {
		t.Fatalf("valid environment rejected: %v", err)
	}
}

func TestRenderAndInitUseEmbeddedAssets(t *testing.T) {
	environment := newTestEnvironment(t)
	environment.writeSites(t, 28080)
	envPath := environment.writeValidEnvironment(t, 0o644)
	if err := environment.app.render(context.Background()); err != nil {
		t.Fatal(err)
	}
	expected := []string{
		filepath.Join(environment.root, "stacks", "api-test", "compose.data.yml"),
		filepath.Join(environment.root, "stacks", "api-test", "compose.app.yml"),
		filepath.Join(environment.nginxDir, "http.conf"),
		filepath.Join(environment.nginxDir, "servers", "api-test.conf"),
		filepath.Join(environment.nginxDir, "upstreams", "api-test.conf"),
		filepath.Join(environment.snippetDir, "blue-green-proxy.conf"),
	}
	for _, path := range expected {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("missing rendered file %s: %v", path, err)
		}
	}

	upstreamPath := environment.app.upstreamPath("api-test")
	if err := os.WriteFile(upstreamPath, []byte("upstream api_test { server 127.0.0.1:28081; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := environment.app.render(context.Background()); err != nil {
		t.Fatal(err)
	}
	upstream, _ := os.ReadFile(upstreamPath)
	if !bytes.Contains(upstream, []byte(":28081")) {
		t.Fatalf("render overwrote active upstream: %s", upstream)
	}

	if err := environment.app.initStack(context.Background(), "api-test"); err == nil || !strings.Contains(err.Error(), "权限必须为 0600") {
		t.Fatalf("init error = %v, want environment permission failure", err)
	}
	if _, err := os.Stat(filepath.Join(environment.stateDir, "network-api-test-net")); !os.IsNotExist(err) {
		t.Fatalf("init created network before environment validation: %v", err)
	}
	if err := os.Chmod(envPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := environment.app.initStack(context.Background(), "api-test"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("env mode = %o, want 600", info.Mode().Perm())
	}
	link := filepath.Join(environment.root, "stacks", "api-test", ".env")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	if target != envPath {
		t.Fatalf(".env target = %s, want %s", target, envPath)
	}
	if _, err := os.Stat(filepath.Join(environment.stateDir, "network-api-test-net")); err != nil {
		t.Fatalf("network was not created: %v", err)
	}
}

func TestRenderSelectsHTTP2SyntaxForNginxVersion(t *testing.T) {
	tests := []struct {
		name      string
		modern    bool
		want      string
		doNotWant string
	}{
		{
			name:      "legacy",
			want:      "listen 443 ssl http2;",
			doNotWant: "http2 on;",
		},
		{
			name:      "modern",
			modern:    true,
			want:      "listen 443 ssl;\n    listen [::]:443 ssl;\n    http2 on;",
			doNotWant: "listen 443 ssl http2;",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			environment := newTestEnvironment(t)
			environment.writeSites(t, 28085)
			if test.modern {
				if err := os.WriteFile(filepath.Join(environment.flagsDir, "nginx-modern-version"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := environment.app.render(context.Background()); err != nil {
				t.Fatal(err)
			}
			content, err := os.ReadFile(filepath.Join(environment.nginxDir, "servers", "api-test.conf"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(content), test.want) {
				t.Fatalf("rendered site does not contain %q:\n%s", test.want, content)
			}
			if strings.Contains(string(content), test.doNotWant) {
				t.Fatalf("rendered site unexpectedly contains %q:\n%s", test.doNotWant, content)
			}
		})
	}
}

func TestRenderRepairsManagedHTTP2SyntaxBeforeNginxTest(t *testing.T) {
	environment := newTestEnvironment(t)
	environment.writeSites(t, 28086)
	if err := os.MkdirAll(environment.app.nginxSites, 0o755); err != nil {
		t.Fatal(err)
	}
	sitePath := filepath.Join(environment.app.nginxSites, "api-test.conf")
	if err := os.WriteFile(sitePath, []byte("server {\n    http2 on;\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(environment.flagsDir, "nginx-managed-http2-fail"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := environment.app.render(context.Background()); err != nil {
		t.Fatalf("render did not repair managed config: %v", err)
	}
	content, err := os.ReadFile(sitePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "listen 443 ssl http2;") || strings.Contains(string(content), "http2 on;") {
		t.Fatalf("managed config was not converted to legacy HTTP/2 syntax:\n%s", content)
	}
	if !strings.Contains(environment.stderr.String(), "将尝试重新渲染修复") {
		t.Fatalf("repair warning missing: %s", environment.stderr)
	}
}

func TestParseNginxVersion(t *testing.T) {
	for _, test := range []struct {
		output string
		want   [3]int
		ok     bool
	}{
		{output: "nginx version: nginx/1.18.0", want: [3]int{1, 18, 0}, ok: true},
		{output: "nginx version: openresty/1.21.4.1", want: [3]int{1, 21, 4}, ok: true},
		{output: "unknown", ok: false},
	} {
		actual, ok := parseNginxVersion(test.output)
		if ok != test.ok || actual != test.want {
			t.Errorf("parseNginxVersion(%q) = %v, %v; want %v, %v", test.output, actual, ok, test.want, test.ok)
		}
	}
	if versionAtLeast([3]int{1, 25, 0}, [3]int{1, 25, 1}) {
		t.Error("Nginx 1.25.0 must use legacy HTTP/2 syntax")
	}
	if !versionAtLeast([3]int{1, 25, 1}, [3]int{1, 25, 1}) {
		t.Error("Nginx 1.25.1 must support the standalone HTTP/2 directive")
	}
}

func TestDeployAcceptsLegacyHealthUsingContainerMetadata(t *testing.T) {
	blueListener, greenListener, portBase := listenOnConsecutivePorts(t)
	var blueBody atomic.Value
	blueBody.Store(`{"status":"ok"}`)
	serveHealth(t, blueListener, &blueBody)
	_ = greenListener

	environment := newTestEnvironment(t)
	environment.writeSites(t, portBase)
	environment.writeValidEnvironment(t, 0o600)
	if err := environment.app.render(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := environment.app.initStack(context.Background(), "api-test"); err != nil {
		t.Fatal(err)
	}

	if err := environment.app.deploy(context.Background(), "api-test", "1.6.7"); err != nil {
		t.Fatalf("legacy image deploy: %v\nstdout=%s\nstderr=%s", err, environment.stdout, environment.stderr)
	}
	assertUpstreamPort(t, environment.app.upstreamPath("api-test"), portBase)
	if !strings.Contains(environment.stderr.String(), "兼容旧版健康接口") ||
		!strings.Contains(environment.stderr.String(), "已通过 Docker 元数据确认") {
		t.Fatalf("legacy compatibility warning missing: %s", environment.stderr)
	}
}

func TestRenderRejectsMissingNginxOneTimeConfiguration(t *testing.T) {
	environment := newTestEnvironment(t)
	environment.writeSites(t, 28090)
	if err := os.WriteFile(filepath.Join(environment.flagsDir, "nginx-base-include-missing"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := environment.app.render(context.Background())
	if err == nil || !strings.Contains(err.Error(), "nginx 一次性配置不完整") {
		t.Fatalf("render error = %v, want nginx one-time configuration failure", err)
	}
	requiredInclude := "include " + filepath.Join(environment.nginxDir, "*.conf") + ";"
	if !strings.Contains(err.Error(), requiredInclude) {
		t.Fatalf("render error does not contain %q: %v", requiredInclude, err)
	}
	if _, statErr := os.Stat(environment.app.stacksDir); !os.IsNotExist(statErr) {
		t.Fatalf("render mutated files before nginx integration check: %v", statErr)
	}
}

func TestRenderRejectsMissingWorkerShutdownTimeout(t *testing.T) {
	environment := newTestEnvironment(t)
	environment.writeSites(t, 28100)
	if err := os.WriteFile(filepath.Join(environment.flagsDir, "nginx-worker-missing"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := environment.app.render(context.Background())
	if err == nil || !strings.Contains(err.Error(), "worker_shutdown_timeout 1200s;") {
		t.Fatalf("render error = %v, want worker shutdown configuration prompt", err)
	}
	if _, statErr := os.Stat(environment.app.stacksDir); !os.IsNotExist(statErr) {
		t.Fatalf("render mutated files before nginx integration check: %v", statErr)
	}
}

func TestInitChecksDependenciesBeforeMutation(t *testing.T) {
	environment := newTestEnvironment(t)
	environment.writeSites(t, 28110)
	if err := environment.app.render(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(environment.flagsDir, "nginx-include-missing"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := environment.app.initStack(context.Background(), "api-test")
	if err == nil || !strings.Contains(err.Error(), "nginx 未加载") {
		t.Fatalf("init error = %v, want nginx include failure", err)
	}
	if _, statErr := os.Stat(filepath.Join(environment.root, "stacks", "api-test", "data")); !os.IsNotExist(statErr) {
		t.Fatalf("init mutated stack before dependency check: %v", statErr)
	}
}

func TestDeployRollbackAndSafetyGates(t *testing.T) {
	blueListener, greenListener, portBase := listenOnConsecutivePorts(t)
	var blueBody atomic.Value
	var greenBody atomic.Value
	blueBody.Store(`{"status":"ok","version":"1.0.0","slot":"blue"}`)
	greenBody.Store(`{"status":"ok","version":"1.1.0","slot":"green"}`)
	serveHealth(t, blueListener, &blueBody)
	serveHealth(t, greenListener, &greenBody)

	environment := newTestEnvironment(t)
	environment.writeSites(t, portBase)
	environment.writeValidEnvironment(t, 0o600)
	if err := environment.app.render(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := environment.app.initStack(context.Background(), "api-test"); err != nil {
		t.Fatal(err)
	}

	if err := environment.app.deploy(context.Background(), "api-test", "v1.0.0"); err != nil {
		t.Fatalf("first deploy: %v\nstdout=%s\nstderr=%s", err, environment.stdout, environment.stderr)
	}
	assertUpstreamPort(t, environment.app.upstreamPath("api-test"), portBase)
	state, err := environment.app.readState("api-test")
	if err != nil || state.Slot != slotBlue || state.PrevSlot != "" {
		t.Fatalf("first state = %+v, err=%v", state, err)
	}

	if err := environment.app.deploy(context.Background(), "api-test", "v1.1.0"); err != nil {
		t.Fatalf("blue-green deploy: %v", err)
	}
	assertUpstreamPort(t, environment.app.upstreamPath("api-test"), portBase+1)
	state, _ = environment.app.readState("api-test")
	if state.Slot != slotGreen || state.PrevSlot != slotBlue || state.PrevTag != "v1.0.0" {
		t.Fatalf("second state = %+v", state)
	}

	if err := environment.app.rollback(context.Background(), "api-test"); err != nil {
		t.Fatalf("fast rollback: %v", err)
	}
	assertUpstreamPort(t, environment.app.upstreamPath("api-test"), portBase)

	greenBody.Store(`{"status":"ok","version":"1.2.0"}`)
	if err := os.WriteFile(filepath.Join(environment.flagsDir, "docker-inspect-slot-mismatch"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err = environment.app.deploy(context.Background(), "api-test", "v1.2.0")
	if err == nil || !strings.Contains(err.Error(), "未返回 slot") {
		t.Fatalf("deploy error = %v, want missing slot failure", err)
	}
	assertUpstreamPort(t, environment.app.upstreamPath("api-test"), portBase)
	if err := os.Remove(filepath.Join(environment.flagsDir, "docker-inspect-slot-mismatch")); err != nil {
		t.Fatal(err)
	}

	greenBody.Store(`{"status":"ok","version":"1.2.0","slot":"green"}`)
	if err := os.WriteFile(filepath.Join(environment.flagsDir, "nginx-test-fail"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err = environment.app.deploy(context.Background(), "api-test", "v1.2.0")
	if err == nil || !strings.Contains(err.Error(), "nginx -t 校验失败") {
		t.Fatalf("deploy error = %v, want nginx test failure", err)
	}
	assertUpstreamPort(t, environment.app.upstreamPath("api-test"), portBase)
	if _, err := os.Stat(environment.app.upstreamPath("api-test") + ".bak"); !os.IsNotExist(err) {
		t.Fatalf("upstream backup was not cleaned up: %v", err)
	}
	if err := os.Remove(filepath.Join(environment.flagsDir, "nginx-test-fail")); err != nil {
		t.Fatal(err)
	}

	err = environment.app.teardown(context.Background(), "api-test", slotBlue)
	if err == nil || !strings.Contains(err.Error(), "拒绝回收") {
		t.Fatalf("teardown current slot error = %v", err)
	}
	if err := environment.app.teardown(context.Background(), "api-test", slotGreen); err != nil {
		t.Fatalf("teardown inactive slot: %v", err)
	}
	greenBody.Store(`{"status":"ok","version":"1.1.0","slot":"green"}`)
	if err := environment.app.rollback(context.Background(), "api-test"); err != nil {
		t.Fatalf("fallback rollback: %v", err)
	}
	assertUpstreamPort(t, environment.app.upstreamPath("api-test"), portBase+1)
}

func listenOnConsecutivePorts(t *testing.T) (net.Listener, net.Listener, int) {
	t.Helper()
	for port := 30000; port < 60000; port++ {
		first, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue
		}
		second, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port+1))
		if err == nil {
			t.Cleanup(func() {
				_ = first.Close()
				_ = second.Close()
			})
			return first, second, port
		}
		_ = first.Close()
	}
	t.Fatal("could not reserve consecutive ports")
	return nil, nil, 0
}

func serveHealth(t *testing.T, listener net.Listener, body *atomic.Value) {
	t.Helper()
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(writer, body.Load().(string))
	})}
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		_ = server.Close()
	})
}

func assertUpstreamPort(t *testing.T, path string, port int) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), fmt.Sprintf("server 127.0.0.1:%d", port)) {
		t.Fatalf("upstream = %s, want port %d", content, port)
	}
}
