package main

import (
	"fmt"
	"io"
	"strings"
)

func printUsage(w io.Writer) {
	fmt.Fprintln(w, strings.TrimSpace(`
bgdeploy - single-host, multi-site blue-green deployment CLI

Notes:
  The examples below assume that the executable is named bgdeploy. If it is
  named deploy on your server, replace ./bgdeploy with ./deploy.
  The example site slug is api-staging. Replace it with a slug from sites.yaml.

Usage:
  bgdeploy [global options] <command> [arguments]
  bgdeploy --help
  bgdeploy <command> --help

Global options must appear before the command. For example:
  ./bgdeploy --config /etc/bgdeploy/runtime.yaml status api-staging

Commands:
  bootstrap
      Create the deployment directory structure and example configuration
      without overwriting existing files. Creates runtime.yaml,
      registry/sites.yaml, env.example, envs/, and stacks/.

  check
      Check root privileges, the Docker daemon, Docker Compose v2, Nginx,
      the one-time Nginx integration, and whether generated configuration is
      loaded.

  render
      Validate registry/sites.yaml and generate Compose files, Nginx sites,
      upstreams, and the shared proxy snippet. Run nginx -t and reload Nginx.
      Existing upstream files are preserved. The HTTP/2 syntax is selected
      automatically for the installed Nginx version.

  init <slug>
      Validate envs/<slug>.env, create the stack .env link and external
      network, then start PostgreSQL and Redis and wait for them to become
      healthy. Run this once before the first deployment of each site.

  deploy <slug> [image-tag]
      Perform the initial deployment or a blue-green release. If image-tag is
      omitted, use image_tag from sites.yaml. Explicit immutable image tags are
      recommended for routine releases.

  rollback <slug>
      Roll back to the previous slot. Switch directly to the old container
      during the drain window, or restart the previous image from STATE after
      that container has been removed. Database migrations are not rolled back.

  status [slug]
      Show the Nginx traffic target, STATE, blue and green containers, health
      results, and pending teardown jobs for all sites or one site. Nginx
      upstream is authoritative if states disagree.

  teardown <slug> <blue|green>
      Manually remove a slot. The command reads the Nginx upstream again and
      refuses to remove the active slot.

  version
      Print the bgdeploy version.

  help
      Show this guide.

Global options:
  --config <path>
      Runtime configuration file.
      Default: runtime.yaml in the current working directory.

  --root <path>
      Deployment root.
      Default: the current working directory when the command starts.

  --nginx-dir <path>
      Nginx configuration directory.
      Default: /etc/nginx/sites

  --nginx-snippet-dir <path>
      Nginx snippet directory.
      Default: /etc/nginx/sites/snippets

runtime.yaml:
  root: /srv/blue-green
  nginx_dir: /etc/nginx/sites
  nginx_snippet_dir: /etc/nginx/sites/snippets

Environment variables:
  BGDEPLOY_CONFIG             Path to runtime.yaml
  BGDEPLOY_ROOT               Deployment root
  BGDEPLOY_NGINX_DIR          Nginx configuration directory
  BGDEPLOY_NGINX_SNIPPET_DIR  Nginx snippet directory

Configuration precedence:
  command-line options > BGDEPLOY_* environment variables > runtime.yaml >
  built-in defaults

Deployment directory:
  runtime.yaml                Host-level path settings; normally edited once
  registry/sites.yaml         Sites, images, ports, domains, TLS, and timeouts
  env.example                 Site environment template
  envs/<slug>.env             Site secrets; regular file with mode 0600
  stacks/<slug>/              Generated Compose files, STATE, and runtime data

Files normally edited by an operator:
  registry/sites.yaml
  envs/<slug>.env

Minimal sites.yaml example:
  defaults:
    image_repo: ghcr.io/example/application
    bind_host: 127.0.0.1
    drain_seconds: 960
    health_timeout_seconds: 300
    health_interval_seconds: 3
    tz: Asia/Shanghai

  stacks:
    - slug: api-staging
      domain: api.example.com
      port_base: 18080
      image_tag: 1.6.8
      tls:
        cert: /etc/letsencrypt/live/api.example.com/fullchain.pem
        key: /etc/letsencrypt/live/api.example.com/privkey.pem

Port allocation:
  blue  uses port_base
  green uses port_base + 1
  Example: port_base=18080 gives blue=18080 and green=18081.

Site environment file:
  sudo cp env.example envs/api-staging.env
  sudo chmod 600 envs/api-staging.env
  sudo vi envs/api-staging.env

Required values that must not retain example placeholders:
  POSTGRES_PASSWORD
  REDIS_PASSWORD
  JWT_SECRET
  TOTP_ENCRYPTION_KEY
  ADMIN_EMAIL
  ADMIN_PASSWORD

One-time Nginx integration:
  Add this to the main context of nginx.conf:
    worker_shutdown_timeout 1200s;

  Add this to the http {} context of nginx.conf:
    include /etc/nginx/sites/*.conf;

  Do not include sites/upstreams/*.conf or sites/servers/*.conf directly.
  They are loaded by the generated /etc/nginx/sites/http.conf file.

First-time setup:
  cd /srv/blue-green
  sudo ./bgdeploy bootstrap

  # Edit runtime.yaml, registry/sites.yaml, and envs/<slug>.env. Complete the
  # one-time Nginx integration above, then run:
  sudo ./bgdeploy render
  sudo ./bgdeploy check
  sudo ./bgdeploy init api-staging
  sudo ./bgdeploy deploy api-staging 1.6.8
  ./bgdeploy status api-staging

Routine blue-green release:
  cd /srv/blue-green
  ./bgdeploy status api-staging
  sudo ./bgdeploy deploy api-staging 1.6.9
  ./bgdeploy status api-staging

  If traffic currently targets blue:18080, a release will:
    1. Start green on port 18081.
    2. Wait for /health to return status=ok.
    3. Verify slot and version. For legacy health responses without these
       fields, verify the Docker container metadata instead.
    4. Atomically update the upstream, run nginx -t, and reload Nginx.
    5. Automatically remove blue after drain_seconds.

  The next release automatically performs the reverse green-to-blue switch.

Rollback:
  sudo ./bgdeploy rollback api-staging
  ./bgdeploy status api-staging

Manually remove an inactive slot:
  sudo ./bgdeploy teardown api-staging blue

After configuration changes:
  After changing sites.yaml, a domain, port, TLS setting, image repository, or
  Nginx setting:
    sudo ./bgdeploy render

  After changing envs/<slug>.env:
    sudo chmod 600 envs/api-staging.env
    sudo ./bgdeploy deploy api-staging 1.6.9

Failure and safety behavior:
  - Health or identity validation failure: remove the new slot and keep
    existing production traffic unchanged.
  - nginx -t or reload failure: restore the upstream and do not switch traffic.
  - Initial deployment failure: no usable container exists behind the
    upstream; fix the error and retry.
  - teardown refuses to remove the active slot.
  - Database migrations are not rolled back. Old and new releases must support
    the same database schema.
  - Environment changes must remain compatible with the old slot while it is
    still running during the drain window.

Troubleshooting:
  ./bgdeploy status api-staging
  docker ps --filter name=api-staging-
  nginx -t
  grep -n "server" /etc/nginx/sites/upstreams/api-staging.conf

More help:
  ./bgdeploy --help
  ./bgdeploy help
  ./bgdeploy version
`))
}
