# go-register

使用 Go 纯协议实现 OpenAI 账号注册与 OAuth 授权，并通过统一的 `accounts.txt` 协调注册线程与授权线程。

## 当前能力

- 纯协议注册 OpenAI 账号
- 纯协议执行 OAuth 授权，生成本地 `auth/` 授权文件
- 对接本地 `web_mail`，注册阶段按 `account_id` 取码，授权阶段按邮箱取码
- 内置 Go 版 `web_mail` 服务，兼容历史 Python 版 HTTP 接口
- 交互终端下默认进入 TUI 首页（worker 卡片列表）
- TUI 底部固定展示注册/授权统计与平均注册速度
- TUI 配置会持久化到项目根目录 `.config.json`
- 支持五种执行模式：
  - `webmail`：只启动邮箱池 HTTP 服务
  - `register`：只注册
  - `authorize`：只授权
  - `pipeline`：注册线程与授权线程独立运行，共享 `accounts.txt`，注册达标后仍会等待授权线程收尾
  - `login`：单账号登录调试
- 使用线程锁 + 文件锁保护 `accounts.txt`，避免并发写乱

## 二进制安装（GitHub Release）

当前仓库支持两种自动发布：

- `main` 分支 push：自动更新 `latest` Release
- 推送 `v*` tag：自动发布同名版本 Release，例如 `v1.0.0`

自动构建以下平台二进制：

- Linux amd64
- Linux arm64
- macOS amd64
- macOS arm64
- Windows amd64

Unix 环境可直接使用仓库内的 `install.sh` 下载最新 Release 并初始化运行目录：

```bash
curl -fsSL https://raw.githubusercontent.com/wdaglb/gpt-register/main/install.sh | bash
```

安装指定 tag 版本：

```bash
curl -fsSL https://raw.githubusercontent.com/wdaglb/gpt-register/main/install.sh | GO_REGISTER_TAG=v1.0.0 bash
```

可选环境变量：

- `GO_REGISTER_TAG`：指定下载的 Release tag，默认 `latest`
- `INSTALL_DIR`：二进制安装目录，默认 `$HOME/.local/bin`
- `WORK_DIR`：运行目录，默认执行脚本时的当前目录

安装完成后，会自动创建：

- `auth/`
- `accounts.txt`
- `emails.txt`
- `user.txt`

> 下面的二进制运行示例默认你已经执行过 `install.sh`，并且 `go-register` 已在 `PATH` 中。  
> 如果 `~/.local/bin` 没加入 `PATH`，请把命令里的 `go-register` 替换成 `~/.local/bin/go-register`。

## 运行前准备

1. 准备邮箱池 txt 数据库 `emails.txt`，基础格式如下：

```text
email@example.com----password----client_id----refresh_token
```

服务启动后会把该文件当作**唯一数据库**直接读写，并在需要时于行尾追加托管字段，例如：

```text
email@example.com----password----client_id----refresh_token
email@example.com----password----client_id----refresh_token----lease_token:abc123----leased_at:2026-04-12T12:00:00Z----status:leased
email@example.com----password----client_id----refresh_token----used_at:2026-04-12T12:05:00Z----status:used
```

默认 `available` 状态不会单独写出，只有记录处于租出或已使用状态时，服务才会追加 `status`、`lease_token`、`leased_at`、`used_at` 等后缀字段。  
只有基础四列的旧格式也可以直接使用，缺少的托管字段会在后续状态变更时自动补齐。  
如果一行里还有额外列，服务会继续保留，并在接口响应的 `extra_fields` 中返回。

2. 启动当前仓库内置的 `web_mail` 服务，例如：

```bash
go-register \
  -mode webmail \
  -web-mail-host 127.0.0.1 \
  -web-mail-port 8030 \
  -web-mail-emails-file emails.txt
```

3. 确认本机代理可访问：

```text
chatgpt.com
auth.openai.com
sentinel.openai.com
auth-cdn.oaistatic.com
```

## TUI 运行方式（推荐）

在交互终端里直接运行：

```bash
go-register \
  -accounts-file accounts.txt \
  -proxy http://127.0.0.1:7890 \
  -web-mail-url http://127.0.0.1:8030
```

启动后默认先进入 **首页（worker 卡片列表）**。首页只展示：

- 系统日志卡片
- 按当前配置推导出的注册 / 授权 worker 占位卡片
- 底部统计栏与快捷键提示

按 `c` 进入独立的 **配置页**，在该页内配置：

- `mode`
- `web-mail-url`
- `email` / `password`（**仅 `login` 模式使用**）
- `user-file`（**仅 `login` 模式在未填写 `email/password` 时兜底**）
- `auth-dir`
- `accounts-file`
- `proxy`
- `mailbox`
- `count`
- `workers`
- `authorize-workers`
- `timeout`
- `otp-timeout`
- `poll-interval`
- `request-timeout`

配置页可以先点“保存配置”落盘；回到首页后按 `Enter` 或 `Ctrl+S` 开始运行。首页包含：

- 顶部系统日志卡片：汇总主流程与未绑定 worker 的日志
- 注册 / 授权 worker 卡片：按 worker 展示当前账号、最后状态和最近日志
- 底部固定统计栏：
  - 注册成功数量
  - 注册失败数量
  - 授权成功数
  - 授权失败数
  - 平均注册速度（秒/个）

任务结束后程序会继续停留在首页；此时仍可按 `c` 回到配置页修改参数，并在首页再次开始新任务。

> 注意：`register` / `authorize` / `pipeline` 三种模式都不会读取“登录邮箱 / 登录密码 / 账号文件”。这些字段只服务于 `login` 单账号登录调试。

### TUI 快捷键

| 按键 | 作用 |
| --- | --- |
| `Tab` | 首页切换到下一张卡片；配置页内切换到下一个字段 |
| `Shift+Tab` | 配置页首字段返回首页；其它位置切换到上一个字段 |
| `左右键` | 首页切换卡片；配置页切换 `mode` |
| `上下键` | 首页滚动当前焦点卡片日志；配置页切换字段 |
| `c` | 首页进入配置页 |
| `Enter` | 首页开始运行；配置页内跳到下一项，或在“保存配置”上提交 |
| `Ctrl+S` | 首页开始运行；配置页保存配置 |
| `Ctrl+C` | 退出 |

### TUI 配置持久化

TUI 会在项目根目录读写 `.config.json`，当前持久化字段如下：

```json
{
  "mode": "pipeline",
  "web-mail-url": "http://127.0.0.1:8030",
  "email": "",
  "password": "",
  "user-file": "user.txt",
  "auth-dir": "auth",
  "accounts-file": "accounts.txt",
  "proxy": "http://127.0.0.1:7890",
  "mailbox": "Junk",
  "count": 5,
  "workers": 2,
  "authorize-workers": 2,
  "timeout": "4m0s",
  "otp-timeout": "1m30s",
  "poll-interval": "3s",
  "request-timeout": "20s"
}
```

说明：

- 启动 TUI 时会自动读取 `.config.json`
- 点击“保存配置”或从首页开始运行时，都会自动保存当前页面配置
- `.config.json` 已加入 `.gitignore`
- `webmail` 模式不会进入 TUI，因此不会消费这份持久化配置

## 模式 0：只启动 web_mail 服务

该模式会在当前仓库内启动一个兼容历史 Python 版接口的邮箱池 HTTP 服务：

```bash
go-register \
  -mode webmail \
  -web-mail-host 127.0.0.1 \
  -web-mail-port 8030 \
  -web-mail-emails-file ./emails.txt \
  -mail-api-base https://www.appleemail.top \
  -web-mail-lease-timeout-seconds 600
```

只同步邮箱文件后退出：

```bash
go-register \
  -mode webmail \
  -web-mail-sync-only \
  -web-mail-emails-file ./emails.txt
```

提供的 HTTP 接口：

- `GET /health`
- `GET /api/email-pool/stats`
- `POST /api/email-pool/lease`
- `POST /api/email-pool/accounts/{id}/mark-used`
- `POST /api/email-pool/accounts/{id}/return`
- `GET/POST /api/email-pool/accounts/{id}/latest`
- `GET/POST /api/email-pool/accounts/by-email/latest`
- `POST /api/email-pool/sync`

响应结构保持兼容：

```json
{
  "ok": true,
  "data": {}
}
```

失败时：

```json
{
  "ok": false,
  "error": "错误信息"
}
```

## accounts.txt 格式

当前统一使用一行一条账号记录：

```text
email----password----register_status----register_time----oauth_status----oauth_time----auth_file_path
```

示例：

```text
demo@example.com----Passw0rd!----ok----2026-04-11 22:37:52----oauth=pending--------
demo@example.com----Passw0rd!----ok----2026-04-11 22:37:52----oauth=ok----2026-04-11 22:40:08----auth/codex-demo_example_com.json
demo@example.com----Passw0rd!----ok----2026-04-11 22:37:52----oauth=fail:add_phone----2026-04-11 22:38:38----
```

字段说明：

| 字段 | 说明 |
| --- | --- |
| `register_status` | 注册结果，如 `ok`、`fail:create_account` |
| `register_time` | 注册完成时间 |
| `oauth_status` | 授权结果，如 `oauth=pending`、`oauth=ok`、`oauth=fail:add_phone` |
| `oauth_time` | 最近一次授权完成时间 |
| `auth_file_path` | 授权成功后生成的本地 auth 文件路径 |

## 非交互 / 脚本模式

当输出被重定向、运行在非交互终端，或你明确想走脚本方式时，仍可直接通过 CLI 参数控制模式和并发。

## 模式 1：只注册

只负责注册，并在账号注册成功后立即把账号写入 `accounts.txt`：

```bash
go-register \
  -mode register \
  -accounts-file accounts.txt \
  -proxy http://127.0.0.1:7890 \
  -web-mail-url http://127.0.0.1:8030 \
  -count 3 \
  -workers 2
```

行为：

- 注册成功：写入 `register_status=ok` 和 `register_time`
- 同时初始化 `oauth_status=oauth=pending`
- 注册失败：写入 `fail:<reason>`

## 模式 2：只授权

从 `accounts.txt` 中筛出“已注册成功但未授权成功”的账号，批量执行授权：

```bash
go-register \
  -mode authorize \
  -accounts-file accounts.txt \
  -proxy http://127.0.0.1:7890 \
  -web-mail-url http://127.0.0.1:8030 \
  -workers 2
```

筛选规则：

- `register_status == ok`
- `oauth_status != oauth=ok`

行为：

- 授权成功：回写 `oauth=ok`、`oauth_time`、`auth_file_path`
- 授权失败：回写 `oauth=fail:<reason>`

## 模式 3：注册 + 授权流水线

注册线程和授权线程独立运行，但共享同一个 `accounts.txt`：

```bash
go-register \
  -mode pipeline \
  -accounts-file accounts.txt \
  -proxy http://127.0.0.1:7890 \
  -web-mail-url http://127.0.0.1:8030 \
  -count 5 \
  -workers 2 \
  -authorize-workers 2
```

行为：

- 注册线程负责持续产出新账号
- 账号一旦注册成功，会立刻写入 `accounts.txt`
- 授权线程从注册线程投递过来的账号继续跑授权
- `accounts.txt` 由统一状态存储层负责加锁更新，避免并发覆盖

## 模式 4：单账号登录调试

保留历史 `login` 模式，便于单账号调试 OAuth 登录闭环：

```bash
go-register \
  -mode login \
  -email your_openai_account@example.com \
  -password 'YourOpenAIPassword!' \
  -accounts-file accounts.txt \
  -proxy http://127.0.0.1:7890 \
  -web-mail-url http://127.0.0.1:8030
```

## 常用参数

| 参数 | 说明 |
| --- | --- |
| `-mode` | `webmail` / `register` / `authorize` / `pipeline` / `login`。交互终端下会被 TUI 页面中的 mode 覆盖，`webmail` 模式固定走 CLI |
| `-accounts-file` | 统一账号状态文件，默认 `accounts.txt` |
| `-web-mail-url` | `web_mail` 服务地址，默认 `http://127.0.0.1:8030` |
| `-web-mail-host` | `webmail` 模式监听地址，默认 `127.0.0.1` |
| `-web-mail-port` | `webmail` 模式监听端口，默认 `8030` |
| `-web-mail-db` | 已废弃兼容参数，当前 `web_mail` 不再使用 SQLite |
| `-web-mail-emails-file` | `webmail` 模式邮箱池 txt 数据库路径，默认项目根目录 `emails.txt` |
| `-mail-api-base` | `webmail` 模式上游邮件接口基础地址，默认 `https://www.appleemail.top` |
| `-web-mail-sync-only` | `webmail` 模式只同步一次邮箱文件后退出 |
| `-web-mail-lease-timeout-seconds` | `webmail` 模式租约超时秒数，默认 `600` |
| `-proxy` | HTTP/HTTPS 代理地址 |
| `-count` | 注册数量。`register` 模式表示注册尝试数；`pipeline` 模式表示目标注册成功数，注册达标后仍会等待已入队账号授权完成；若邮箱池提示“当前没有可用邮箱账号”，会停止继续补注册 |
| `-workers` | 注册并发数，或 `authorize` 模式的授权并发数。交互终端下会被 TUI 页面中的 workers 覆盖 |
| `-authorize-workers` | `pipeline` 模式下的授权并发数。交互终端下会被 TUI 页面中的 authorize-workers 覆盖 |
| `-mailbox` | 优先轮询的邮箱目录，默认 `Junk` |
| `-otp-timeout` | 单次验证码等待时间 |
| `-poll-interval` | 收码轮询间隔 |
| `-timeout` | 单个账号整条流程超时 |
| `-request-timeout` | 单次 HTTP 请求超时 |
| `-auth-dir` | 授权文件输出目录，默认 `auth` |

## 运行输出

交互终端下默认使用 TUI：

- `logger` 日志会按系统卡片和 worker 卡片拆分展示
- 失败原因会直接显示在对应卡片的最近日志里
- 底部统计栏会持续更新注册/授权结果

非交互 / 脚本模式下，仍会直接输出普通日志，例如：

```text
注册失败账号=demo@example.com reason=create_account err=...
授权失败账号=demo@example.com status=oauth=fail:add_phone err=...
pipeline 授权失败账号=demo@example.com status=oauth=fail:add_phone err=...
```

## 说明

- 当前仓库已经内置 Go 版 `web_mail` 服务，不再依赖外部 Python `email_pool_service.py`
- 注册模式严格按 `http_register.py` 的顺序推进：拿注册会话 → 提交邮箱 → 设置密码 → 发验证码 → 收码 → 验证 → `create_account`
- OAuth 模式会继续推进：邮箱 → 密码 → 邮箱 OTP → consent / callback → `oauth/token`
- 对于落入 `add_phone` 的账号，当前会明确标记失败，不尝试绕过手机验证
- 若 OpenAI 再次调整注册页接口、consent 页表单结构或 Sentinel flow 名称，需要同步更新协议步骤
