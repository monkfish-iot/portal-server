# portal-server

> MonkFish 云平台独立 PORTAL 页面服务器：按设备选择认证模板并返回登录页，认证交互不经过云端。

**项目文档网站：<http://monkfish.cn>**

[English](README.en.md) · 中文

`portal-server` 是云侧的单二进制 Go 服务。AP 侧的 `webauthd` 把未认证终端 302 到云端，请求经 AP nginx、云 nginx 两跳反代到达本服务；本服务按设备 ID（或调试参数）从模板库中选出认证模板，返回注入了 `<base>` 的入口页。页面里的静态资源由云 nginx 直接回文件，登录表单 `POST /v1/login` 由 AP nginx 本地截流转给 `webauthd`，云侧不参与认证。

- 语言 / 依赖：Go 1.23，唯一直接依赖 `gopkg.in/yaml.v3`
- 监听：`127.0.0.1:19090`（生产置于云 nginx 之后，不直接对外）
- 部署：`/opt/monkfish/portal-server`，systemd `portal-server.service`，用户 `monkfish`
- 许可：Apache License 2.0（见 [LICENSE](LICENSE)、[NOTICE](NOTICE)）

## 目录 / Contents

1. [为什么需要它 / Why it exists](#1-为什么需要它)
2. [工作原理 / How it works](#2-工作原理)
3. [与其他模块的关系 / Relationship with other modules](#3-与其他模块的关系)
4. [接口 / HTTP interface](#4-接口)
5. [模板约定 / Template conventions](#5-模板约定)
6. [配置 / Configuration](#6-配置)
7. [构建与部署 / Build and deployment](#7-构建与部署)
8. [排障 / Troubleshooting](#8-排障)
9. [相关文档 / Related documentation](#9-相关文档)
10. [许可 / License](#10-许可)

---

## 1. 为什么需要它

`portal-server` 本质上是一个 **Portal 模板选择器**：它不参与认证、不保存会话，只负责在终端触发认证时，按设备与请求参数**按需返回对应的 Portal 认证页面**。

- 设备固件只保留认证本身（`webauthd`）与一个 302 跳转，页面样式、文案、背景图集中放在云端模板库，运营改模板不用刷固件；
- 同一个云平台要给不同设备 / 租户返回不同页面，选模板的规则与页面文件必须集中管理；
- 面向未认证终端的页面不能要求登录、不能依赖管理后台的会话与数据库，所以不能由 WrtHub-UI 直接出页面；
- 页面静态资源由 nginx 直接回文件，本服务只做「选模板 + 回入口页」，保持极低开销。

一句话：**它不是认证服务器，也不是管理后台，而是按需把正确的认证页面送到终端面前的模板选择器。**

## 2. 工作原理

### 2.1 请求链路

```text
STA ──► AP nginx :20001 ──► 云 nginx :9091 ──► portal-server 127.0.0.1:19090
                               │
                               ├─ /p/{code}/…        alias 直接回模板静态文件
                               └─ /_portal_internal/  internal，仅 x_accel 模式使用

登录：页面 JS POST /v1/login ──► AP nginx 同名 location ──► webauthd 127.0.0.1:20002
```

- `webauthd` 的 `portal_type=3`（CLOUD_PORTAL）把终端 302 到 `/{device_id}/`；
- AP nginx 的 `location /` 以 `proxy_pass` 反代到云侧 9091，路径与查询串原样保留；
- 云 nginx 把入口请求交给本服务，`/p/` 前缀的资源自己回；
- 页面里的登录请求命中 AP nginx 的本地 `location /v1/login`，根本到不了云端。

### 2.2 请求形态

```text
GET /{device_id}/?wlanuserip=<STA IP>&wlanusermac=<STA MAC>&wlanacip=<AC IP>&wlanacmac=<AP MAC>
```

- 根路径 `/`（无 `device_id`）同样可用，直接走兜底模板；
- 查询参数原样透传，不参与模板渲染；仅调试参数 `t={code}` 用于强制指定模板；
- 路径第一段作为 `device_id`，后面更长的路径也按该 `device_id` 选模板。

### 2.3 模板选择顺序

1. 查询参数 `t={code}`（`code` 合法且模板存在时生效，用于预览 / 排障）；
2. 配置 `rules`：`device` 精确匹配（按配置顺序）；
3. 配置 `rules`：`prefix` 前缀匹配（按配置顺序）；
4. `default_template` 兜底；
5. 兜底模板也不存在时，取磁盘上第一个可用模板，避免设备首次弹 Portal 就 404。

模板 `code` 白名单为 `^[A-Za-z0-9_-]{1,64}$`，同时用于阻断路径穿越。

### 2.4 入口页返回方式

**base 模式（默认，`x_accel: false`）**

本服务读取 `{template_root}/{code}/index.html`，在 `<head>` 之后注入 `<base href="/p/{code}/">` 后返回 HTML。这样页内相对路径资源（`assets/style.css`、`success.html`）会解析到 `/p/{code}/`，由云 nginx 直接回文件。入口页按参数动态选模板，因此响应带 `Cache-Control: no-store`。

**X-Accel 模式（`x_accel: true`）**

本服务不返回 body，只设置响应头：

```text
X-Accel-Redirect: /_portal_internal/{code}/index.html
X-Portal-Template: {code}
```

由 nginx 的 `internal` location 直接回文件（应用只告诉 nginx「真正的页面路径」）。该模式要求模板使用 `/p/{code}/...` **绝对路径**引用资源，相对路径会因浏览器 URL 停留在 `/` 而失效。

## 3. 与其他模块的关系

| 模块 | 关系 | 交互方式 |
|------|------|----------|
| `webauthd`（设备侧 C 组件） | 上游触发者 | `portal_type=3` 时 302 到 `/{device_id}/?...`；AP nginx `:20001` 反代到本服务；`/v1/login` 留在 AP 本地 |
| 云侧 nginx | 前置代理 | `:9091` 反代到 `127.0.0.1:19090`；`/p/` alias 直回模板文件；`/_portal_internal/` 为 internal |
| `WrtHub-UI`（管理面） | 同机协作，共享模板目录 | 模板上传 / 解压 / 在线编辑落盘到同一个 `template_root`；播种示例模板；概况页探测本服务 `/healthz` |
| `installer` | 构建与部署 | 交叉编译二进制，打包 `config.yaml`、systemd unit、nginx 站点、`LICENSE` / `NOTICE`；升级保留既有配置 |
| `xfrpc_loader` | 间接影响 | 云端控制面把 Portal 参数下发给 `webauthd`（`portal_type`、`portal_url`、`proxy_pass`），决定 302 指向的云端地址 |
| `frps` / `frps_helper` | 同机部署，无直接数据面耦合 | 负责设备接入、管理接口与隧道；Portal 页面流量走 9091 公网站点，与管理隧道相互独立 |

### 3.1 与 webauthd

`webauthd` 是认证执行面，决定「把终端送到哪」。`portal_type=3` 时它按 `portal_port` 组装 `http://<AP 地址>:<portal_port>/{device_id}/?...` 并 302；AP nginx 监听 20001，把 `location /` 反代到云端，同时把 `/v1/get_portal_text`、`/v1/login` 截流到本机 `127.0.0.1:20002`。

因此本服务**永远看不到登录请求**，也不持有任何认证状态；认证结果由 `webauthd` 直接返回给页面。

### 3.2 与 WrtHub-UI

两者**不通过 API 通信**，只共享模板目录：

- WrtHub-UI 负责模板的生命周期（上传 zip、校验解压、在线编辑、复制、启停、删除），落盘到它自己的 `portal.template_root`；
- `portal-server` 只读该目录。生产部署时两个路径必须指向同一位置：安装包把 `portal-server` 的 `template_root` 配为 `/opt/monkfish/wrthub-ui/data/portal-templates`，与 WrtHub-UI 的相对默认值 `./data/portal-templates` 指向同一物理目录；
- 内置示例模板（`example-blue`、`example-dark`、`wind`）由 WrtHub-UI 启动时播种，安装包也显式携带一份，保证 WrtHub-UI 未首次启动时 Portal 仍有兜底页面；
- WrtHub-UI 的概况页把本服务当作被监控服务，定期探测 `/healthz`。

一个已知边界：`window.PORTAL_CONFIG`（可视化配置）目前只在 WrtHub-UI 的预览接口注入，`portal-server` 的入口页只注入 `<base>`。生产页面呈现的是模板内置默认值 + URL query，数据库里的可视化配置不会自动写回静态模板。



### 3.3 与 frps / frps_helper

`frps_helper` 负责设备接入、认证与管理接口，`frps` 提供隧道服务；Portal 页面走的是云侧 nginx 的 9091 公网站点（AP nginx 直接反代），两者没有进程内调用或数据表耦合。设备侧 Portal 参数的变化来自管理面的控制协议，经 `xfrpc_loader` 下发到 `webauthd`。

## 4. 接口

|    方法    |    路径    |    说明 |
|------|------|------|
| GET / HEAD | `/` | 根入口（无 `device_id`），按规则选模板 |
| GET / HEAD | `/{device_id}/...` | 设备入口，按路径第一段选模板 |
| GET | `/p/{code}/...` | 模板静态资源兜底（生产由 nginx alias 直回，本服务仅单机部署时使用） |
| GET | `/healthz` | 健康检查与运行信息（JSON） |

`/healthz` 返回字段：

| 字段 | 说明 |
|------|------|
| `status` | 固定 `ok` |
| `version` | 构建版本，开发构建为 `dev` |
| `started_at` / `uptime_seconds` | 启动时间与运行秒数 |
| `goroutines` | 当前 goroutine 数 |
| `memory_alloc_bytes` / `memory_sys_bytes` | 内存分配 / 占用 |
| `templates` | 磁盘上可用模板 code 列表 |
| `template_root` / `default_template` / `x_accel` | 当前生效配置 |

模板不可用时返回 404 与「认证页面暂不可用」提示页，并带上实际选中的 `code` 便于排查。

## 5. 模板约定

```text
{template_root}/{code}/index.html   入口页
{template_root}/{code}/其它文件      静态资源
```

- 模板 code 只允许字母、数字、`_`、`-`，长度 1–64；
- 入口页固定为 `index.html`，与 WrtHub-UI 的模板约定一致；
- 页面脚本约定（与 `webauthd` 本地模板一致）：拦截表单提交，把原始 URL query（剔除 `t`）加 `username`、`userpasswd` 作为 body `POST /v1/login`；`error_code == 0` 视为成功并跳到模板内 `success.html`，其余错误码在页内展示文案；
- 模板可携带 `schema.json` 描述可视化字段，但**生产链路不注入** `window.PORTAL_CONFIG`，字段默认值需内置在页面里；
- `x_accel` 模式要求资源用 `/p/{code}/...` 绝对路径；base 模式可用相对路径，由注入的 `<base>` 兜住。

## 6. 配置

配置文件：[config.yaml](config.yaml)。

| 键 | 默认值 | 说明 |
|----|--------|------|
| `listen` | `127.0.0.1:19090` | 监听地址；生产置于云 nginx 后 |
| `template_root` | `./data/portal-templates` | 模板根目录；生产指向 WrtHub-UI 模板库 |
| `default_template` | `example-blue` | 兜底模板 code |
| `x_accel` | `false` | `true` 时入口页改用 `X-Accel-Redirect` |
| `rules` | `[]` | `device`（精确）/ `prefix`（前缀）/ `template` 规则列表 |

```yaml
rules:
  - device: "SN-MK0001"
    template: "hotel-blue"
  - prefix: "SN-"
    template: "default"
```

## 7. 构建与部署

```bash
cd portal-server
go build -o portal-server .
./portal-server -c config.yaml
```

交叉编译：

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o portal-server .
```

生产部署（由安装包完成）：

- 路径 `/opt/monkfish/portal-server`，unit `portal-server.service`（`User=monkfish`，`After=wrthub-ui.service`）；
- 云侧 nginx 站点见 [nginx/portal-server.conf](nginx/portal-server.conf)，即安装包的 `/etc/nginx/conf.d/portal-server.conf`；
- 自检：

```bash
curl -s http://127.0.0.1:19090/healthz
curl -s http://127.0.0.1:9091/healthz
systemctl status portal-server
```

## 8. 排障

| 现象 | 排查方向 |
|------|----------|
| 返回「认证页面暂不可用」 | 看 404 页里的 `code`；确认 `template_root/{code}/index.html` 存在、目录权限对 `monkfish` 可读 |
| 页面无样式、`success.html` 404 | base 模式检查 `<base>` 注入；`x_accel` 模式检查资源是否为 `/p/{code}/...` 绝对路径 |
| 云 nginx 502 | `portal-server` 未启动或未监听 19090：`systemctl status portal-server`、`curl 127.0.0.1:19090/healthz` |
| 登录无响应，请求似乎到了云端 | AP nginx 缺少 `/v1/login`、`/v1/get_portal_text` 的本地 location，登录请求被 `location /` 反代上云 |
| 换模板不生效 | 入口页 `no-store` 但静态资源有 10 分钟缓存；确认 `rules` 与 `device_id` 匹配、`t` 参数是否被占用 |
| 想快速确认模板清单 | `curl -s 127.0.0.1:19090/healthz` 的 `templates` 字段 |

## 9. 相关文档

以下文档位于项目文档网站与仓库文档目录，这里给出内容摘要，便于不跳转也能理解上下游模块：

- **WrtHub-UI（云管理界面）**：面向运维的 Web 控制台，负责 Portal 模板的上传与 zip 校验解压、在线编辑、复制、启停与删除，维护 `schema.json` 可视化字段与预览；同时提供设备管理、认证策略、系统设置与概况页服务状态。模板文件落在它配置的 `template_root`，与本服务的模板目录是同一份数据；概况页还会定期探测本服务的 `/healthz`。

- **webauthd（设备侧认证守护进程）**：运行在 AP 上，负责 Portal 认证会话、RADIUS / 本地 / HTTP 认证以及终端上下线控制。`portal_type=3` 时按 `portal_port` 组装 `/{device_id}/?wlanuserip=…` 并 302 到云端，同时生成本机 nginx `:20001` 反代配置，把 `/v1/login`、`/v1/get_portal_text` 截流到 `127.0.0.1:20002`。认证结果由它直接返回页面，云端不保存会话。

- **安装包与部署（installer）**：提供 `build.ps1` 交叉编译与打包、`install.sh` 安装、`update.sh` 增量更新、`uninstall.sh` 卸载。定义了各组件的 systemd unit、部署路径与端口（WrtHub-UI 8080、frps_helper 8089 / 6999 / 7443、portal-server 19090、云 nginx 9091、frps 7000 / 20000 / 20001），并做端口预检；升级时保留既有配置，二进制变化才重启服务。

- **PORTAL 模板结构与工作原理**：描述模板目录布局、`schema.json` 的字段类型（`text` / `color` / `image`）与校验规则、可视化配置的存储方式（数据库 `config_json`）与取值优先级（URL query > `window.PORTAL_CONFIG` > 页面内置默认值），以及模板生命周期（启动播种 → 上传 → 校验落盘 → 入库 → 在线编辑 / 复制 / 启停 / 删除）和预览注入链路。

- **Portal 服务速查文档**：本服务的接口与部署速查，包含请求形态、模板选择顺序、base 与 X-Accel 两种返回方式、`/healthz` 返回字段、配置项说明，以及 systemd 单元、部署路径与云侧 nginx 站点位置。

## 10. 许可

Apache License 2.0。完整许可证文本见 [LICENSE](LICENSE)，第三方组件归属与依赖说明见 [NOTICE](NOTICE)。
