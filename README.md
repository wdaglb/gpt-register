# go-register

使用 Go 纯协议实现 OpenAI 账号注册机，并对接本地 `web_mail` 邮箱池自动提取验证码。

## 当前能力

- 纯协议获取 ChatGPT 注册会话并跳转到 `auth.openai.com`
- 纯协议生成 Sentinel token，覆盖注册邮箱、设置密码、创建资料三个关键步骤
- 从 `web_mail` 租用邮箱、按 `account_id` 轮询验证码邮件、成功后标记 `used`
- 支持批量注册与并发执行
- 保留旧的 `login` 模式，便于继续复用已有协议登录调试链路

## 运行前准备

1. 启动 `web_mail` 服务，例如：

```bash
/Users/wanz/PycharmProjects/reg2/.venv/bin/python \
  /Users/wanz/PycharmProjects/reg2/web_mail/email_pool_service.py \
  --host 127.0.0.1 --port 8030
```

2. 确认本机代理可访问 `chatgpt.com`、`auth.openai.com` 与 `sentinel.openai.com`

## 注册模式

默认模式就是 `register`：

```bash
/Users/wanz/sdk/go1.26.1/bin/go run . \
  -web-mail-url http://127.0.0.1:8030 \
  -proxy http://127.0.0.1:7890 \
  -count 3 \
  -workers 2
```

注册结果默认写入 `registered_accounts.txt`，格式为：

```text
email----password----ok----2026-04-11 21:30:00
email----password----fail:create_account----2026-04-11 21:31:12
```

## 登录模式

如需继续使用已有协议登录调试链路，可显式切到 `login`：

```bash
/Users/wanz/sdk/go1.26.1/bin/go run . \
  -mode login \
  -email your_openai_account@example.com \
  -password 'YourOpenAIPassword!' \
  -web-mail-url http://127.0.0.1:8030
```

## 常用参数

| 参数 | 说明 |
| --- | --- |
| `-mode` | `register` 或 `login`，默认 `register` |
| `-web-mail-url` | `web_mail` 服务地址，默认 `http://127.0.0.1:8030` |
| `-proxy` | HTTP/HTTPS 代理地址 |
| `-count` | 注册数量，仅 `register` 模式生效 |
| `-workers` | 并发数，仅 `register` 模式生效 |
| `-mailbox` | 优先轮询的邮箱目录，默认 `Junk` |
| `-otp-timeout` | 单次验证码等待时间 |
| `-poll-interval` | 收码轮询间隔 |
| `-timeout` | 单个账号整条流程超时 |
| `-request-timeout` | 单次 HTTP 请求超时 |
| `-accounts-file` | 结果输出文件路径 |

## 说明

- 注册模式严格按 `http_register.py` 的顺序推进：拿注册会话 → 提交邮箱 → 设置密码 → 发验证码 → 收码 → 验证 → `create_account`
- `web_mail` 现在优先按 `account_id` 取信，目的是避免并发租约下串信
- 若 OpenAI 再次调整注册页接口或 Sentinel flow 名称，需要同步更新对应请求顺序与 Referer
