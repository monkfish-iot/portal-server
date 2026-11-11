# portal-server

> Standalone PORTAL page server for the MonkFish cloud platform: picks an authentication template per device and returns the login page. Authentication traffic never reaches the cloud.

**Project documentation site: <http://monkfish.cn>**

[中文](README.md) · English

`portal-server` is a single-binary Go service running on the cloud side. The AP-side `webauthd` redirects unauthenticated clients to the cloud; the request travels through the AP nginx and the cloud nginx before reaching this service. The service selects a template by device ID (or a debug parameter) and returns the entry page with an injected `<base>` tag. Static assets are served directly by the cloud nginx, and the login form's `POST /v1/login` is intercepted locally by the AP nginx and forwarded to `webauthd` — the cloud is never involved in authentication.

- Language / dependency: Go 1.23, single direct dependency `gopkg.in/yaml.v3`
- Listen: `127.0.0.1:19090` (behind the cloud nginx in production, never exposed directly)
- Deployment: `/opt/monkfish/portal-server`, systemd unit `portal-server.service`, user `monkfish`
- License: Apache License 2.0 (see [LICENSE](LICENSE), [NOTICE](NOTICE))

## Contents / 目录

1. [Why it exists / 为什么需要它](#1-why-it-exists)
2. [How it works / 工作原理](#2-how-it-works)
3. [Relationship with other modules / 与其他模块的关系](#3-relationship-with-other-modules)
4. [HTTP interface / 接口](#4-http-interface)
5. [Template conventions / 模板约定](#5-template-conventions)
6. [Configuration / 配置](#6-configuration)
7. [Build and deployment / 构建与部署](#7-build-and-deployment)
8. [Troubleshooting / 排障](#8-troubleshooting)
9. [Related documentation / 相关文档](#9-related-documentation)
10. [License / 许可](#10-license)

---

## 1. Why it exists

`portal-server` is essentially a **portal template selector**: it does not authenticate or keep sessions. Its only job is, when a client is prompted for authentication, to **return the matching portal page on demand**, based on the device and request parameters.

- The firmware keeps only authentication itself (`webauthd`) plus a 302 redirect. Page layout, copy and background images live in a cloud template library, so operators can change them without reflashing devices.
- One cloud platform must serve different pages to different devices / tenants, so the selection rules and page files have to be managed in one place.
- A page shown to unauthenticated clients cannot require a login or depend on the admin backend's session and database — which is why WrtHub-UI cannot serve it directly.
- Static assets are returned by nginx itself; the service only does "select template + return entry page", keeping overhead minimal.

In one sentence: **it is neither an authentication server nor an admin backend, but a template selector that puts the right authentication page in front of the client on demand.**

## 2. How it works

### 2.1 Request path

```text
STA ──► AP nginx :20001 ──► cloud nginx :9091 ──► portal-server 127.0.0.1:19090
                                 │
                                 ├─ /p/{code}/…        alias serves template assets directly
                                 └─ /_portal_internal/  internal, used only in x_accel mode

Login: page JS POST /v1/login ──► AP nginx same-name location ──► webauthd 127.0.0.1:20002
```

- With `portal_type=3` (CLOUD_PORTAL) `webauthd` redirects the client to `/{device_id}/`;
- The AP nginx `location /` reverse-proxies to the cloud's port 9091, preserving path and query string;
- The cloud nginx forwards entry requests to this service and serves `/p/` assets itself;
- Login requests hit the AP nginx's local `location /v1/login` and never leave the device.

### 2.2 Request format

```text
GET /{device_id}/?wlanuserip=<STA IP>&wlanusermac=<STA MAC>&wlanacip=<AC IP>&wlanacmac=<AP MAC>
```

- The root path `/` (no `device_id`) also works and falls back to the default template;
- Query parameters are passed through untouched and take no part in rendering; only `t={code}` forces a template for debugging/preview;
- The first path segment is the `device_id`; longer paths still select by that `device_id`.

### 2.3 Template selection order

1. Query parameter `t={code}` (when the code is valid and the template exists, for preview/troubleshooting);
2. `rules`: exact `device` match (in configured order);
3. `rules`: `prefix` match (in configured order);
4. `default_template`;
5. If the default template is missing, the first available template on disk — so a freshly installed device never sees a 404 on its first portal popup.

Template `code` is validated against `^[A-Za-z0-9_-]{1,64}$`, which also blocks path traversal.

### 2.4 Entry page delivery modes

**base mode (default, `x_accel: false`)**

The service reads `{template_root}/{code}/index.html`, injects `<base href="/p/{code}/">` after `<head>`, and returns the HTML. Relative assets (`assets/style.css`, `success.html`) then resolve under `/p/{code}/` and are served by the cloud nginx. Because the entry page is selected dynamically, the response carries `Cache-Control: no-store`.

**X-Accel mode (`x_accel: true`)**

The service returns no body, only headers:

```text
X-Accel-Redirect: /_portal_internal/{code}/index.html
X-Portal-Template: {code}
```

The nginx `internal` location serves the file ("the app tells nginx the real page path"). Templates must reference assets with `/p/{code}/...` **absolute paths**; relative paths break because the browser URL stays at `/`.

## 3. Relationship with other modules

| Module | Relationship | Interaction |
|--------|--------------|-------------|
| `webauthd` (device-side, C) | Upstream trigger | With `portal_type=3` it redirects to `/{device_id}/?...`; AP nginx `:20001` proxies to this service; `/v1/login` stays local on the AP |
| Cloud nginx | Front proxy | `:9091` proxies to `127.0.0.1:19090`; `/p/` alias serves template files; `/_portal_internal/` is internal |
| `WrtHub-UI` (management UI) | Same host, shared template directory | Uploads/edits templates into the same `template_root`; seeds example templates; probes this service's `/healthz` for the overview page |
| `installer` | Build and deployment | Cross-compiles the binary and packages `config.yaml`, the systemd unit, the nginx site, `LICENSE` / `NOTICE`; preserves existing config on upgrade |
| `xfrpc_loader` | Indirect | The cloud control plane pushes Portal parameters (`portal_type`, `portal_url`, `proxy_pass`) to `webauthd`, which determines the upstream URL pointing at this service |
| `frps` / `frps_helper` | Same host, no direct data-plane coupling | Device onboarding, management APIs and tunnels; portal traffic uses the public 9091 site and is independent of the management tunnel |

### 3.1 With webauthd

`webauthd` is the authentication execution plane and decides *where to send the client*. With `portal_type=3` it builds `http://<AP address>:<portal_port>/{device_id}/?...` and issues a 302. The AP nginx listens on 20001, proxies `location /` to the cloud, and intercepts `/v1/get_portal_text` and `/v1/login` to the local `127.0.0.1:20002`.

Consequently this service **never sees login requests** and holds no authentication state; results go straight from `webauthd` back to the page.

### 3.2 With WrtHub-UI

The two **do not talk over an API**; they only share the template directory:

- WrtHub-UI owns the template lifecycle (zip upload, validation/extraction, online editing, forking, enable/disable, deletion) and writes to its own `portal.template_root`;
- `portal-server` only reads that directory. In production both paths must resolve to the same place: the installer points `portal-server.template_root` at `/opt/monkfish/wrthub-ui/data/portal-templates`, matching WrtHub-UI's relative default `./data/portal-templates`;
- Built-in examples (`example-blue`, `example-dark`, `wind`) are seeded by WrtHub-UI at startup, and the installer also ships a copy so the portal still works before WrtHub-UI's first boot;
- WrtHub-UI's overview page treats this service as a monitored service and polls `/healthz`.

Known limitation: `window.PORTAL_CONFIG` (visual settings) is currently injected only by WrtHub-UI's preview endpoint. `portal-server` injects only `<base>`, so production pages show template defaults plus URL query values; saved visual settings are not written back into the static templates.

### 3.3 With frps / frps_helper

`frps_helper` handles device onboarding, authentication and management APIs, while `frps` provides tunnelling. Portal pages are served through the cloud nginx site on 9091 (reverse-proxied by the AP nginx), so there is no in-process call or shared table with those components. Device-side portal parameters come from the control plane and are pushed to `webauthd` through `xfrpc_loader`.

## 4. HTTP interface

| Method | Path | Description |
|--------|------|-------------|
| GET / HEAD | `/` | Root entry (no `device_id`), template chosen by rules |
| GET / HEAD | `/{device_id}/...` | Device entry, selected by the first path segment |
| GET | `/p/{code}/...` | Fallback static asset handler (nginx serves these in production; used for standalone runs) |
| GET | `/healthz` | Health check and runtime info (JSON) |

`/healthz` fields:

| Field | Description |
|-------|-------------|
| `status` | Always `ok` |
| `version` | Build version, `dev` for development builds |
| `started_at` / `uptime_seconds` | Start time and uptime |
| `goroutines` | Current goroutine count |
| `memory_alloc_bytes` / `memory_sys_bytes` | Memory allocated / reserved |
| `templates` | Template codes available on disk |
| `template_root` / `default_template` / `x_accel` | Effective configuration |

When no template is usable the service returns 404 with an "authentication page temporarily unavailable" page that includes the selected `code` for diagnosis.

## 5. Template conventions

```text
{template_root}/{code}/index.html   entry page
{template_root}/{code}/other files  static assets
```

- Codes allow only letters, digits, `_` and `-`, length 1–64;
- The entry page is always `index.html`, matching WrtHub-UI's convention;
- Page script contract (same as webauthd's local templates): intercept the form submit and `POST /v1/login` with the original query string (minus `t`) plus `username` and `userpasswd`; `error_code == 0` means success and navigates to the template's `success.html`, other codes are rendered as messages;
- A template may ship a `schema.json` describing visual fields, but **production does not inject** `window.PORTAL_CONFIG` — defaults must be built into the page;
- In `x_accel` mode assets must use `/p/{code}/...` absolute paths; base mode tolerates relative paths thanks to the injected `<base>`.

## 6. Configuration

Config file: [config.yaml](config.yaml).

| Key | Default | Description |
|-----|---------|-------------|
| `listen` | `127.0.0.1:19090` | Listen address; keep it behind nginx in production |
| `template_root` | `./data/portal-templates` | Template root; point it at WrtHub-UI's library in production |
| `default_template` | `example-blue` | Fallback template code |
| `x_accel` | `false` | When `true`, return `X-Accel-Redirect` instead of the body |
| `rules` | `[]` | `device` (exact) / `prefix` / `template` rules |

```yaml
rules:
  - device: "SN-MK0001"
    template: "hotel-blue"
  - prefix: "SN-"
    template: "default"
```

## 7. Build and deployment

```bash
cd portal-server
go build -o portal-server .
./portal-server -c config.yaml
```

Cross-compile:

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o portal-server .
```

Production deployment (handled by the installer):

- Path `/opt/monkfish/portal-server`, unit `portal-server.service` (`User=monkfish`, `After=wrthub-ui.service`);
- Cloud nginx site: [nginx/portal-server.conf](nginx/portal-server.conf), installed as `/etc/nginx/conf.d/portal-server.conf`;
- Self-check:

```bash
curl -s http://127.0.0.1:19090/healthz
curl -s http://127.0.0.1:9091/healthz
systemctl status portal-server
```

## 8. Troubleshooting

| Symptom | What to check |
|---------|---------------|
| "Authentication page temporarily unavailable" | Look at the `code` on the 404 page; verify `template_root/{code}/index.html` exists and is readable by `monkfish` |
| No styling, `success.html` 404 | In base mode check the injected `<base>`; in `x_accel` mode check that assets use `/p/{code}/...` absolute paths |
| Cloud nginx returns 502 | `portal-server` is down or not listening on 19090: `systemctl status portal-server`, `curl 127.0.0.1:19090/healthz` |
| Login does nothing, request seems to reach the cloud | The AP nginx is missing the local `/v1/login` and `/v1/get_portal_text` locations, so `location /` proxies them upstream |
| Template change not visible | Entry page is `no-store` but assets are cached for 10 minutes; verify `rules` match the `device_id` and that `t` is not being used |
| Need the template list quickly | `templates` field of `curl -s 127.0.0.1:19090/healthz` |

## 9. Related documentation

These documents live on the project documentation site and in the repository's documentation tree. Summaries are given here so the surrounding modules can be understood without following a link:

- **WrtHub-UI (cloud management UI)**: the operator-facing web console that handles portal template upload with zip validation and extraction, online editing, forking, enabling/disabling and deletion, and maintains the `schema.json` visual fields and previews. It also provides device management, authentication policies, system settings and the service-status overview. Templates are written to its configured `template_root`, the same directory this service reads, and the overview page periodically probes this service's `/healthz`.

- **webauthd (device-side authentication daemon)**: runs on the AP and handles portal authentication sessions, RADIUS / local / HTTP authentication, and client session control. With `portal_type=3` it builds `/{device_id}/?wlanuserip=…` using `portal_port` and issues a 302 to the cloud, and generates the local nginx `:20001` reverse-proxy config that intercepts `/v1/login` and `/v1/get_portal_text` to `127.0.0.1:20002`. Authentication results are returned to the page by webauthd; the cloud keeps no session.

- **Installer and deployment (installer)**: provides `build.ps1` for cross-compilation and packaging, `install.sh` for installation, `update.sh` for incremental updates and `uninstall.sh` for removal. It defines the systemd units, deployment paths and ports for every component (WrtHub-UI 8080, frps_helper 8089 / 6999 / 7443, portal-server 19090, cloud nginx 9091, frps 7000 / 20000 / 20001) and performs port pre-checks; upgrades keep existing config and restart only when the binary changes.

- **PORTAL template structure and workflow**: describes the template directory layout, `schema.json` field types (`text` / `color` / `image`) and validation rules, how visual settings are stored (the `config_json` database column) and their precedence (URL query > `window.PORTAL_CONFIG` > built-in page defaults), as well as the template lifecycle (startup seeding → upload → validation and extraction → database record → online edit / fork / enable / delete) and the preview injection pipeline.

- **Portal service quick reference**: a condensed interface and deployment guide for this service, covering the request format, template selection order, the base and X-Accel delivery modes, `/healthz` response fields, configuration keys, and the systemd unit, deployment path and cloud nginx site.

## 10. License

Apache License 2.0. The full license text is in [LICENSE](LICENSE); third-party attributions and dependencies are listed in [NOTICE](NOTICE).
