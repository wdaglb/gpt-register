# 最新线上登录链路与防护头分析

更新时间：2026-04-11

本文记录当前 `auth.openai.com` 登录前端的**已确认事实**、**待验证推断**和**代码改造方向**，用于指导 `go-register` 从旧的 Auth0 直表单流切换到最新前端链路。

---

## 1. 结论摘要

### 已确认

1. **当前登录入口不是旧 Auth0 直表单主链路**
   - OAuth authorize URL 最终会落到 `https://auth.openai.com/log-in`
   - 页面由 React Router 驱动，并通过前端 `clientAction` 发起后续请求

2. **登录入口第一页真实接口**
   - `POST https://auth.openai.com/api/accounts/authorize/continue`

3. **密码页真实接口**
   - `POST https://auth.openai.com/api/accounts/password/verify`

4. **邮箱验证码页真实接口**
   - `POST https://auth.openai.com/api/accounts/email-otp/validate`
   - `POST https://auth.openai.com/api/accounts/email-otp/resend`

5. **最新动态防护头**
   - `OpenAI-Sentinel-Token`
   - `OpenAI-Sentinel-SO-Token`

6. **前端当前主链路确实还在使用 Sentinel SDK**
   - `window.SentinelSDK.token(flow)`
   - `window.SentinelSDK.sessionObserverToken(flow)`
   - `window.SentinelSDK.init(flow)`

7. **当前登录页 / 密码页模块显式调用的是单 token 版本**
   - 登录页、密码页模块当前都能对应到：
     - `mu(...)`：5 秒超时包装
     - `xo(flow)`：获取 Sentinel token
     - `Hu(token, soToken?)`：组装 header
   - 已确认它们在模块内显式调用的形态是：

```js
const token = await mu(xo("authorize_continue"), 5000)
headers = {
  "Content-Type": "application/json",
  ...Hu(token)
}
```

以及：

```js
const token = await mu(xo("password_verify"), 5000)
headers = {
  "Content-Type": "application/json",
  ...Hu(token)
}
```

### 待验证

1. 登录主链路是否**必须**同时带 `OpenAI-Sentinel-SO-Token`
   - 从前端公共代码看已支持该头
   - 但登录页/密码页模块当前显式调用看起来仍主要使用 `OpenAI-Sentinel-Token`
   - 仍需继续确认底层公共 fetch helper 是否会自动补 `SO token`

2. `openai-sentinel-go` 是否完整覆盖最新登录流
   - 旧实现大概率仍可覆盖一部分 `OpenAI-Sentinel-Token`
   - 但默认 flow、SDK 路径和是否包含 `SO token` 已明显可能落后

3. 入口页返回的 `auth.openai.com/log-in` HTML 里是否还有额外的前端态切换参数
   - 当前已抓到 `debug_bootstrap.html`
   - 但还需要结合最新路由动作继续确认

---

## 2. 关键证据

### 2.1 授权入口真实现象

真实协议联调时，OAuth 授权入口返回：

- `status=200`
- `final=https://auth.openai.com/log-in`
- body 为完整登录页 HTML，而不是旧 Auth0 `state` 表单页

本地留档：

- `debug_bootstrap.html`

该 HTML 中可见：

- `immutableClientSessionMetadata.session_id`
- `openai_client_id`
- `app_name_enum: "oaicli"`
- Cloudflare challenge 注入脚本

---

### 2.2 登录页真实前端动作

线上模块：

- `https://auth-cdn.oaistatic.com/assets/route-DHfBC5WP.js`

已确认其 `clientAction` 会调用：

```text
POST https://auth.openai.com/api/accounts/authorize/continue
```

并在用户名登录路径下提交：

```json
{
  "username": {
    "kind": "email",
    "value": "<email>"
  }
}
```

对应页面路由：`LOG_IN`

---

### 2.3 密码页真实前端动作

线上模块：

- `LOG_IN_PASSWORD`
- `https://auth-cdn.oaistatic.com/assets/route-BicGBkRw.js`

已确认其 `clientAction` 会调用：

```text
POST /password/verify
```

也就是：

```text
POST https://auth.openai.com/api/accounts/password/verify
```

请求体为：

```json
{
  "password": "<password>"
}
```

注意：**这里不再重复带 username**，说明用户名已由前一步写入 auth session。

---

### 2.4 邮箱验证码页真实前端动作

线上模块：

- `EMAIL_VERIFICATION`
- `https://auth-cdn.oaistatic.com/assets/route-B51y80xB.js`

已确认其 `clientAction` 会调用：

```text
POST https://auth.openai.com/api/accounts/email-otp/validate
POST https://auth.openai.com/api/accounts/email-otp/resend
```

验证码校验体：

```json
{
  "code": "<otp>"
}
```

---

### 2.5 前端统一状态机

线上公共模块：

- `https://auth-cdn.oaistatic.com/assets/nextStepHandler-Bj8e05AW.js`

已确认：

- 登录页、密码页、邮箱 OTP 页都不是各自手写 fetch
- 它们统一走 `nextStepHandler`
- 当前链路映射为：

| 页面类型 | 真实接口 |
|---|---|
| `login_start` | `/authorize/continue` |
| `login_password` | `/password/verify` |
| `email_otp_verification` | `/email-otp/validate` / `/email-otp/resend` |

---

### 2.6 最新 Sentinel 头

从线上 `app-core-DvbfI5Yp.js` 反推出的 header helper：

```js
function Hu(e,t){
  const n={}
  n["OpenAI-Sentinel-Token"]=e
  if(t) n["OpenAI-Sentinel-SO-Token"]=t
  return n
}
```

已确认线上存在两个头：

```text
OpenAI-Sentinel-Token
OpenAI-Sentinel-SO-Token
```

并且 Sentinel SDK 加载/调用方式为：

```js
window.SentinelSDK.init(flow)
window.SentinelSDK.token(flow)
window.SentinelSDK.sessionObserverToken(flow)
```

SDK 优先路径：

```text
https://sentinel.openai.com/backend-api/sentinel/sdk.js
```

失败后 fallback：

```text
https://chatgpt.com/backend-api/sentinel/sdk.js
```

### 2.7 当前主链路显式调用方式

从 `route-DHfBC5WP.js`、`route-BicGBkRw.js` 与 `app-core-DvbfI5Yp.js` 的导出映射可确认：

- `aa = mu`：超时包装
- `ab = xo`：`SentinelSDK.token(flow)`
- `ac = Hu`：header builder

因此当前登录页 / 密码页前端不是自己手写复杂 header，而是先取 token，再通过 `Hu()` 组装头。

这说明当前优先级应调整为：

1. 先稳定复现 `xo("authorize_continue")`
2. 再稳定复现 `xo("password_verify")`
3. `SO token` 作为后续增强项继续验证

---

## 3. 对 `openai-sentinel-go` 的当前判断

参考目录：

- `/Users/wanz/web/wwwroot/ai/gpt-register/openai-sentinel-go`

### 可能仍可复用的部分

- Sentinel token 的基础生成逻辑
- PoW / enforcement token
- Turnstile 相关求解思路

### 明显可能落后的部分

| 项 | 旧实现倾向 | 当前线上现象 |
|---|---|---|
| SDK URL | 固定脚本版本 URL | `backend-api/sentinel/sdk.js` loader |
| flow 名 | 以注册流为主，如 `username_password_create` | 登录流当前已确认 `authorize_continue` / `password_verify` |
| 登录入口 | Auth0 直表单 | `auth.openai.com/log-in` + React Router action |
| 防护头 | 可能只覆盖单头 | 最新前端已支持 `OpenAI-Sentinel-SO-Token`，但登录主链路当前显式仍以单 token 为主 |

### 当前判断

`openai-sentinel-go` **不是完全没用**，但不能直接假设它已经覆盖最新线上登录链路。  
后续接入时至少需要验证：

1. flow 名是否要改
2. SDK 路径/调用方式是否要改
3. 是否需要同时输出 `SO token`

---

## 4. 代码改造方向

下一步协议链路应按下面顺序重构：

1. 访问 OAuth authorize URL
2. 落到 `https://auth.openai.com/log-in`
3. 为 flow=`authorize_continue` 生成 `OpenAI-Sentinel-Token`
4. `POST /api/accounts/authorize/continue`
5. 进入 `log-in/password`
6. 为 flow=`password_verify` 再生成一次 `OpenAI-Sentinel-Token`
7. `POST /api/accounts/password/verify`
8. 如进入 `email-verification`
   - `POST /api/accounts/email-otp/resend`
   - 从 `web_mail` 取码
   - `POST /api/accounts/email-otp/validate`
9. 继续跟进下一步页面/回调，最终换 token 并落本地 `auth/`

如实链路仍被拦截，再进一步补：

10. `OpenAI-Sentinel-SO-Token`
11. 更贴近线上 fetch 的 header 顺序 / cookie 行为 / 额外会话头

---

## 5. 当前本地相关文件

- 登录页 HTML 抓包：`debug_bootstrap.html`
- 当前协议实现：`oauth_protocol.go`
- 入口参数解析：`main.go`
- 收码客户端：`webmail.go`

---

## 6. 当前阻塞点

当前不是“代码不会发请求”，而是：

1. 我们还没把最新前端的 `authorize_continue` / `password_verify` / `email_otp` 整条链准确接进代码
2. Sentinel 头的生成逻辑还没和最新线上 flow / SDK / `SO token` 对齐

所以后续改造优先级应是：

1. 先把最新前端动作链改进 Go 代码
2. 再把最新 Sentinel 头接进去
3. 最后再做真实联调与 `auth/` 文件生成
