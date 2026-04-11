# go-register

使用 Go 纯协议实现 OpenAI 账号注册与 OAuth 授权，并通过统一的 `accounts.txt` 协调注册线程与授权线程。

## 当前能力

- 纯协议注册 OpenAI 账号
- 纯协议执行 OAuth 授权，生成本地 `auth/` 授权文件
- 对接本地 `web_mail`，注册阶段按 `account_id` 取码，授权阶段按邮箱取码
- 支持三种执行模式：
  - `register`：只注册
  - `authorize`：只授权
  - `pipeline`：注册线程与授权线程独立运行，共享 `accounts.txt`
- 使用线程锁 + 文件锁保护 `accounts.txt`，避免并发写乱

## 运行前准备

1. 启动 `web_mail` 服务，例如：

```bash
/Users/wanz/PycharmProjects/reg2/.venv/bin/python \
  /Users/wanz/PycharmProjects/reg2/web_mail/email_pool_service.py \
  --host 127.0.0.1 --port 8030
```

2. 确认本机代理可访问：

```text
chatgpt.com
auth.openai.com
sentinel.openai.com
auth-cdn.oaistatic.com
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

## 模式 1：只注册

只负责注册，并在账号注册成功后立即把账号写入 `accounts.txt`：

```bash
/Users/wanz/sdk/go1.26.1/bin/go run . \
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
/Users/wanz/sdk/go1.26.1/bin/go run . \
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
/Users/wanz/sdk/go1.26.1/bin/go run . \
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
/Users/wanz/sdk/go1.26.1/bin/go run . \
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
| `-mode` | `register` / `authorize` / `pipeline` / `login` |
| `-accounts-file` | 统一账号状态文件，默认 `accounts.txt` |
| `-web-mail-url` | `web_mail` 服务地址，默认 `http://127.0.0.1:8030` |
| `-proxy` | HTTP/HTTPS 代理地址 |
| `-count` | 注册数量，仅 `register` / `pipeline` 生效 |
| `-workers` | 注册并发数，或 `authorize` 模式的授权并发数 |
| `-authorize-workers` | `pipeline` 模式下的授权并发数 |
| `-mailbox` | 优先轮询的邮箱目录，默认 `Junk` |
| `-otp-timeout` | 单次验证码等待时间 |
| `-poll-interval` | 收码轮询间隔 |
| `-timeout` | 单个账号整条流程超时 |
| `-request-timeout` | 单次 HTTP 请求超时 |
| `-auth-dir` | 授权文件输出目录，默认 `auth` |

## 控制台输出

现在批量模式会在控制台直接打印失败原因，例如：

```text
注册失败账号=demo@example.com reason=create_account err=...
授权失败账号=demo@example.com status=oauth=fail:add_phone err=...
pipeline 授权失败账号=demo@example.com status=oauth=fail:add_phone err=...
```

## 说明

- 注册模式严格按 `http_register.py` 的顺序推进：拿注册会话 → 提交邮箱 → 设置密码 → 发验证码 → 收码 → 验证 → `create_account`
- OAuth 模式会继续推进：邮箱 → 密码 → 邮箱 OTP → consent / callback → `oauth/token`
- 对于落入 `add_phone` 的账号，当前会明确标记失败，不尝试绕过手机验证
- 若 OpenAI 再次调整注册页接口、consent 页表单结构或 Sentinel flow 名称，需要同步更新协议步骤
