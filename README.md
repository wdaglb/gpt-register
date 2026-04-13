# go-register

使用 Go 纯协议完成 OpenAI 账号注册、OAuth 授权与本地邮箱池协同。项目内置 Go 版 `web_mail` 服务，并通过统一的 `accounts.txt` 串联注册线程与授权线程。

## 功能概览

- 纯协议注册 OpenAI 账号
- 纯协议执行 OAuth 授权，并在本地 `auth/` 目录生成授权文件
- 内置 Go 版 `web_mail` 服务，兼容历史 Python 版 HTTP 接口
- 支持 `register`、`authorize`、`pipeline`、`login`、`webmail` 五种模式
- 交互终端默认进入 TUI 首页，支持配置持久化与 worker 卡片视图
- 使用线程锁 + 文件锁保护 `accounts.txt`，避免并发写入互相覆盖

## 目录与运行文件

程序运行过程中主要会读写以下文件：

- `emails.txt`：邮箱池数据源，也是 `web_mail` 的持久化文件
- `accounts.txt`：注册 / 授权状态文件
- `auth/`：OAuth 授权成功后生成的本地授权文件目录
- `user.txt`：`login` 模式单账号调试时的兜底账号文件
- `.config.json`：TUI 配置持久化文件，仅交互模式使用

## 安装方式

### 方式一：通过 GitHub Release 安装

仓库已支持自动发布以下平台的二进制：

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
- `INSTALL_DIR`：二进制安装目录，默认 `~/gpt-register`
- `WORK_DIR`：运行目录，默认 `~/gpt-register`
- `GITHUB_REPO`：Release 仓库地址，默认 `wdaglb/gpt-register`

安装完成后会自动创建：

- `auth/`
- `accounts.txt`
- `emails.txt`
- `user.txt`
- `.config.json`

默认安装后目录结构如下：

```text
~/gpt-register/
├── go-register
├── accounts.txt
├── emails.txt
├── user.txt
└── auth/
```

> `install.sh` 只负责 Linux / macOS 下的安装流程。Windows 请直接从 Release 页面下载对应压缩包。

安装完成后，推荐先进入该目录再运行：

```bash
cd ~/gpt-register
./go-register -mode webmail -web-mail-host 127.0.0.1 -web-mail-port 8030 -web-mail-emails-file ./emails.txt
```

### 方式一补充：安装为 systemd 后台服务

如果你的环境是 Linux + systemd，并且希望把 `webmail` 和 `pipeline` 安装为开机自启后台服务，可以使用仓库内的 `install-system.sh`：

```bash
curl -fsSL https://raw.githubusercontent.com/wdaglb/gpt-register/main/install-system.sh | bash
```

安装内容：

- `go-register-webmail.service`
- `gpt-register.service`

默认行为：

- 二进制与运行文件安装到 `~/gpt-register`
- 自动创建 `accounts.txt`、`emails.txt`、`user.txt`、`auth/`
- 自动生成 `~/gpt-register/.config.json`
- 自动生成 `/etc/default/go-register-webmail`
- 自动生成 `/etc/default/gpt-register`
- 自动执行 `systemctl enable --now`

如需自定义 pipeline 并发或数量，可在执行时覆盖环境变量：

```bash
curl -fsSL https://raw.githubusercontent.com/wdaglb/gpt-register/main/install-system.sh | PIPELINE_COUNT=20 PIPELINE_WORKERS=3 PIPELINE_AUTHORIZE_WORKERS=3 bash
```

安装后常用命令：

```bash
systemctl status go-register-webmail.service
systemctl status gpt-register.service
journalctl -u go-register-webmail.service -f
journalctl -u gpt-register.service -f
```

> 注意：`gpt-register.service` 默认运行 `pipeline` 模式；达到目标数量后会正常退出，这属于当前设计行为。

如需调整 systemd 默认参数，可直接编辑：

```bash
sudo vim /etc/default/go-register-webmail
sudo vim /etc/default/gpt-register
sudo systemctl restart go-register-webmail.service
sudo systemctl restart gpt-register.service
```

### 方式二：本地源码构建

项目当前使用 Go 1.26：

```bash
go build -o go-register .
```

构建完成后，可直接使用当前目录下的 `./go-register` 运行。

## 快速开始

以下运行示例默认你已经通过 `install.sh` 安装完成，并且当前位于 `~/gpt-register` 目录：

```bash
cd ~/gpt-register
```

### 1. 准备邮箱池文件 `emails.txt`

基础格式如下：

```text
email@example.com----password----client_id----refresh_token
```

服务启动后会把该文件当作唯一数据库直接读写，并在需要时于行尾追加托管字段，例如：

```text
email@example.com----password----client_id----refresh_token
email@example.com----password----client_id----refresh_token----lease_token:abc123----leased_at:2026-04-12T12:00:00Z----status:leased
email@example.com----password----client_id----refresh_token----used_at:2026-04-12T12:05:00Z----status:used
```

说明：

- 默认 `available` 状态不会单独写出
- 只有记录进入租出或已使用状态时，才会追加 `status`、`lease_token`、`leased_at`、`used_at` 等后缀字段
- 仅包含基础四列的旧格式也可以直接使用
- 如果一行里还有额外列，服务会保留这些字段，并在接口响应的 `extra_fields` 中返回

### 2. 启动内置 `web_mail` 服务

```bash
./go-register \
  -mode webmail \
  -web-mail-host 127.0.0.1 \
  -web-mail-port 8030 \
  -web-mail-emails-file ./emails.txt
```

如果只想先把 `emails.txt` 同步进内存并立即退出：

```bash
./go-register \
  -mode webmail \
  -web-mail-sync-only \
  -web-mail-emails-file ./emails.txt
```

后台运行示例：

```bash
nohup ./go-register -mode webmail -web-mail-host 127.0.0.1 -web-mail-port 8030 -web-mail-emails-file ./emails.txt > webmail.log 2>&1 &
```

常用排查命令：

```bash
tail -f webmail.log
```

```bash
ps aux | grep './go-register -mode webmail'
```

后台停止示例：

```bash
pkill -f './go-register -mode webmail'
```

### 3. 准备代理

运行前请确认本机代理可访问：

```text
chatgpt.com
auth.openai.com
sentinel.openai.com
auth-cdn.oaistatic.com
```

### 4. 启动主程序

推荐直接进入 TUI：

```bash
./go-register \
  -accounts-file ./accounts.txt \
  -proxy http://127.0.0.1:7890 \
  -web-mail-url http://127.0.0.1:8030
```

## TUI 使用说明

在交互终端里运行时，程序默认进入 TUI 首页。首页主要包含：

- 系统日志卡片
- 注册 / 授权 worker 卡片
- 底部统计栏
- 快捷键提示

配置页可配置：

- `mode`
- `web-mail-url`
- `email` / `password`（仅 `login` 模式使用）
- `user-file`（仅 `login` 模式在未填写 `email/password` 时兜底）
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

配置页可先保存配置，再回到首页按 `Enter` 或 `Ctrl+S` 启动任务。

### TUI 首页显示内容

- 系统日志卡片：汇总主流程日志
- 底部统计栏：
  - 注册成功数量
  - 注册失败数量
  - 授权成功数量
  - 授权失败数量
  - 平均注册速度（秒 / 个）

任务结束后程序会继续停留在首页，便于修改配置后再次启动下一轮任务。

### TUI 快捷键

| 按键 | 作用 |
| --- | --- |
| `Tab` | 首页切换到下一张卡片；配置页切换到下一个字段 |
| `Shift+Tab` | 配置页首字段返回首页；其他位置切换到上一个字段 |
| `←` / `→` | 首页切换卡片；配置页切换 `mode` |
| `↑` / `↓` | 首页滚动当前卡片日志；配置页切换字段 |
| `c` | 首页进入配置页 |
| `Enter` | 首页开始运行；配置页跳到下一项，或在“保存配置”上提交 |
| `Ctrl+S` | 首页开始运行；配置页保存配置 |
| `Ctrl+C` | 退出 |

### TUI 配置持久化

TUI 会在项目根目录读写 `.config.json`，典型内容如下：

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
- 点击“保存配置”或从首页开始运行时，都会自动保存当前配置
- `.config.json` 已加入 `.gitignore`
- `webmail` 模式固定走 CLI，不进入 TUI

## 非交互 / 脚本模式

当输出被重定向、运行在非交互终端，或你明确希望通过脚本运行时，可以直接使用 CLI 参数控制。

### 模式 1：只注册

```bash
./go-register \
  -mode register \
  -accounts-file ./accounts.txt \
  -proxy http://127.0.0.1:7890 \
  -web-mail-url http://127.0.0.1:8030 \
  -count 3 \
  -workers 2
```

行为：

- 注册成功：写入 `register_status=ok` 与 `register_time`
- 同时初始化 `oauth_status=oauth=pending`
- 注册失败：写入 `fail:<reason>`

### 模式 2：只授权

```bash
./go-register \
  -mode authorize \
  -accounts-file ./accounts.txt \
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

### 模式 3：注册 + 授权流水线

```bash
./go-register \
  -mode pipeline \
  -accounts-file ./accounts.txt \
  -proxy http://127.0.0.1:7890 \
  -web-mail-url http://127.0.0.1:8030 \
  -count 5 \
  -workers 2 \
  -authorize-workers 2
```

行为：

- 注册线程持续产出新账号
- 账号注册成功后会立刻写入 `accounts.txt`
- 授权线程继续消费这些新账号完成 OAuth 授权
- `count` 表示目标注册成功数；注册达标后，程序仍会等待已入队账号授权结束

### 模式 4：单账号登录调试

显式传入邮箱和密码：

```bash
./go-register \
  -mode login \
  -email your_openai_account@example.com \
  -password 'YourOpenAIPassword!' \
  -accounts-file ./accounts.txt \
  -proxy http://127.0.0.1:7890 \
  -web-mail-url http://127.0.0.1:8030
```

如果不想在命令行中直接写账号密码，也可以准备 `user.txt`：

```text
your_openai_account@example.com
YourOpenAIPassword!
```

或者使用单行兼容格式：

```text
your_openai_account@example.com----YourOpenAIPassword!
```

随后执行：

```bash
./go-register \
  -mode login \
  -user-file ./user.txt \
  -accounts-file ./accounts.txt \
  -proxy http://127.0.0.1:7890 \
  -web-mail-url http://127.0.0.1:8030
```

### 模式 5：只启动 `web_mail`

```bash
./go-register \
  -mode webmail \
  -web-mail-host 127.0.0.1 \
  -web-mail-port 8030 \
  -web-mail-emails-file ./emails.txt \
  -mail-api-base https://www.appleemail.top \
  -web-mail-lease-timeout-seconds 600
```

## 数据文件格式

### `accounts.txt`

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
| `register_status` | 注册结果，例如 `ok`、`fail:create_account` |
| `register_time` | 注册完成时间 |
| `oauth_status` | 授权结果，例如 `oauth=pending`、`oauth=ok`、`oauth=fail:add_phone` |
| `oauth_time` | 最近一次授权完成时间 |
| `auth_file_path` | 授权成功后生成的本地 auth 文件路径 |

## `web_mail` HTTP 接口

当前内置服务兼容以下接口：

- `GET /health`
- `GET /api/email-pool/stats`
- `POST /api/email-pool/lease`
- `POST /api/email-pool/accounts/{id}/mark-used`
- `POST /api/email-pool/accounts/{id}/return`
- `GET /api/email-pool/accounts/{id}/latest`
- `POST /api/email-pool/accounts/{id}/latest`
- `GET /api/email-pool/accounts/by-email/latest`
- `POST /api/email-pool/accounts/by-email/latest`
- `POST /api/email-pool/sync`

响应结构：

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

## 常用参数

| 参数 | 说明 |
| --- | --- |
| `-mode` | `register` / `authorize` / `pipeline` / `login` / `webmail` |
| `-accounts-file` | 账号状态文件路径，默认 `accounts.txt` |
| `-web-mail-url` | `web_mail` 服务地址，默认 `http://127.0.0.1:8030` |
| `-web-mail-host` | `webmail` 模式监听地址，默认 `127.0.0.1` |
| `-web-mail-port` | `webmail` 模式监听端口，默认 `8030` |
| `-web-mail-emails-file` | `webmail` 模式邮箱池 txt 文件路径，默认当前项目根目录 `emails.txt` |
| `-mail-api-base` | `webmail` 模式上游邮件接口基础地址 |
| `-web-mail-sync-only` | `webmail` 模式只同步一次邮箱文件后退出 |
| `-web-mail-lease-timeout-seconds` | `webmail` 模式邮箱租约超时秒数，默认 `600` |
| `-email` | `login` 模式账号邮箱；为空时从 `user-file` 读取 |
| `-password` | `login` 模式账号密码；为空时从 `user-file` 读取 |
| `-user-file` | `login` 模式账号文件，支持两行 `email/password` 或单行 `email----password` |
| `-auth-dir` | 授权文件输出目录，默认 `auth` |
| `-proxy` | HTTP / HTTPS 代理地址 |
| `-mailbox` | 验证码轮询优先邮箱目录，默认 `Junk` |
| `-count` | `register` / `pipeline` 模式的注册数量 |
| `-workers` | `register` 模式注册并发数，或 `authorize` 模式授权并发数 |
| `-authorize-workers` | `pipeline` 模式的授权并发数 |
| `-timeout` | 单个账号整条流程超时，默认 `4m` |
| `-otp-timeout` | 单次验证码等待超时，默认 `90s` |
| `-poll-interval` | 收码轮询间隔，默认 `3s` |
| `-request-timeout` | 单次 HTTP 请求超时，默认 `20s` |

## 运行输出

交互终端下默认使用 TUI：

- 系统日志与 worker 日志按卡片拆分展示
- 失败原因会直接显示在对应卡片的最近日志里
- 底部统计栏会持续刷新注册 / 授权结果

非交互模式下会直接输出普通日志，例如：

```text
注册失败账号=demo@example.com reason=create_account err=...
授权失败账号=demo@example.com status=oauth=fail:add_phone err=...
pipeline 授权失败账号=demo@example.com status=oauth=fail:add_phone err=...
```

## 说明与限制

- 当前仓库已经内置 Go 版 `web_mail` 服务
- `register` 模式会按“拿注册会话 → 提交邮箱 → 设置密码 → 发验证码 → 收码 → 验证 → create_account”推进
- OAuth 链路会继续完成“邮箱 → 密码 → 邮箱 OTP → consent / callback → oauth/token”
- 命中 `add_phone` 的账号当前会明确标记失败，不尝试绕过手机验证
- 如果 OpenAI 再次调整页面接口、consent 表单结构或 Sentinel 流程名称，需要同步更新协议实现
