#!/usr/bin/env python3
import argparse
import json
import os
import time
import urllib.parse
import secrets
import hashlib
import base64
from pathlib import Path

from playwright.sync_api import sync_playwright

CLIENT_ID = "app_EMoamEEZ73f0CkXaXp7hrann"
REDIRECT_URI = "http://localhost:1455/auth/callback"
AUTH_URL = "https://auth.openai.com/oauth/authorize"

INTERESTING_HOSTS = (
    "auth.openai.com",
    "auth0.openai.com",
    "chatgpt.com",
    "localhost:1455",
)

def b64url(raw: bytes) -> str:
    return base64.urlsafe_b64encode(raw).decode("ascii").rstrip("=")

def sha256_b64url(value: str) -> str:
    return b64url(hashlib.sha256(value.encode("ascii")).digest())

def generate_oauth_url():
    state = secrets.token_urlsafe(16)
    verifier = secrets.token_urlsafe(64)
    challenge = sha256_b64url(verifier)
    params = {
        "client_id": CLIENT_ID,
        "response_type": "code",
        "redirect_uri": REDIRECT_URI,
        "scope": "openid email profile offline_access",
        "state": state,
        "code_challenge": challenge,
        "code_challenge_method": "S256",
        "prompt": "login",
        "id_token_add_organizations": "true",
        "codex_cli_simplified_flow": "true",
    }
    url = f"{AUTH_URL}?{urllib.parse.urlencode(params)}"
    return url, state, verifier

def now_ts():
    return time.strftime("%Y-%m-%d %H:%M:%S")

def is_interesting(url: str) -> bool:
    return any(host in url for host in INTERESTING_HOSTS)

def extract_callback(url: str) -> bool:
    try:
        parsed = urllib.parse.urlparse(url)
        q = urllib.parse.parse_qs(parsed.query)
        return bool(q.get("code")) and bool(q.get("state"))
    except Exception:
        return False

def write_jsonl(path: Path, payload: dict):
    with path.open("a", encoding="utf-8") as f:
        f.write(json.dumps(payload, ensure_ascii=False) + "\n")

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--output-dir", required=True)
    parser.add_argument("--headed", action="store_true", default=True)
    parser.add_argument("--proxy", default="")
    args = parser.parse_args()

    output_dir = Path(args.output_dir).resolve()
    output_dir.mkdir(parents=True, exist_ok=True)

    auth_url, state, verifier = generate_oauth_url()

    meta = {
        "generated_at": now_ts(),
        "auth_url": auth_url,
        "state": state,
        "verifier": verifier,
    }
    (output_dir / "meta.json").write_text(
        json.dumps(meta, ensure_ascii=False, indent=2),
        encoding="utf-8",
    )

    events_path = output_dir / "events.jsonl"
    callback_path = output_dir / "callback.txt"

    captured = {"url": ""}

    with sync_playwright() as p:
        launch_kwargs = {"headless": not args.headed}
        if args.proxy:
            launch_kwargs["proxy"] = {"server": args.proxy}

        browser = p.firefox.launch(**launch_kwargs)
        context = browser.new_context(
            viewport={"width": 1400, "height": 900},
            locale="en-US",
        )
        page = context.new_page()

        def mark_callback(source: str, url: str):
            if url and extract_callback(url) and not captured["url"]:
                captured["url"] = url
                callback_path.write_text(
                    json.dumps({"source": source, "url": url}, ensure_ascii=False, indent=2),
                    encoding="utf-8",
                )

        def on_request(request):
            url = request.url
            if not is_interesting(url):
                return
            payload = {
                "ts": now_ts(),
                "type": "request",
                "method": request.method,
                "url": url,
                "resource_type": request.resource_type,
                "headers": request.headers,
                "post_data": request.post_data,
            }
            write_jsonl(events_path, payload)
            mark_callback("request", url)

        def on_response(response):
            url = response.url
            if not is_interesting(url):
                return
            headers = response.headers
            payload = {
                "ts": now_ts(),
                "type": "response",
                "status": response.status,
                "url": url,
                "headers": headers,
                "location": headers.get("location", ""),
            }
            try:
                ctype = headers.get("content-type", "")
                if "json" in ctype or "text/html" in ctype or "javascript" in ctype:
                    text = response.text()
                    payload["body_preview"] = text[:3000]
            except Exception as e:
                payload["body_preview_error"] = str(e)
            write_jsonl(events_path, payload)
            mark_callback("response_url", url)
            if headers.get("location"):
                mark_callback("response_location", headers["location"])

        def on_navigate(frame):
            if frame != page.main_frame:
                return
            url = frame.url
            payload = {
                "ts": now_ts(),
                "type": "navigate",
                "url": url,
            }
            write_jsonl(events_path, payload)
            mark_callback("navigate", url)

        page.on("request", on_request)
        page.on("response", on_response)
        page.on("framenavigated", on_navigate)

        print(f"[+] OAuth URL 已生成")
        print(f"[+] state={state}")
        print(f"[+] 输出目录: {output_dir}")
        print(f"[+] 正在打开授权页...")
        page.goto(auth_url, wait_until="domcontentloaded", timeout=60000)

        print()
        print("[下一步]")
        print("1. 手动完成 邮箱 -> 密码 -> OTP")
        print("2. 到 consent 页面停住后，回到终端按回车")
        input(">>> 到 consent 页面后按回车继续... ")

        (output_dir / "consent_before_click.html").write_text(page.content(), encoding="utf-8")
        page.screenshot(path=str(output_dir / "consent_before_click.png"), full_page=True)

        print(f"[+] 当前 URL: {page.url}")

        buttons = page.query_selector_all("button")
        print("[+] 页面按钮预览：")
        for index, button in enumerate(buttons[:10]):
            try:
                text = button.inner_text().strip().replace("\n", " ")
            except Exception:
                text = "<read-failed>"
            print(f"    [{index}] {text[:80]}")

        selectors = [
            'button:has-text("Continue")',
            'button:has-text("Sign in")',
            'button:has-text("允许")',
            'button:has-text("同意")',
            'button[type="submit"]',
        ]

        clicked = False
        for selector in selectors:
            btn = page.query_selector(selector)
            if btn:
                print(f"[+] 点击按钮: {selector}")
                btn.click(timeout=5000)
                clicked = True
                break

        if not clicked:
            print("[!] 没找到可点击按钮，脚本只保留当前页面和网络日志")
            context.storage_state(path=str(output_dir / "storage_state.json"))
            browser.close()
            return

        print("[+] 已点击 consent，开始等待后续跳转/请求...")
        for _ in range(30):
            if captured["url"]:
                break
            time.sleep(1)

        page.screenshot(path=str(output_dir / "consent_after_click.png"), full_page=True)
        (output_dir / "consent_after_click.html").write_text(page.content(), encoding="utf-8")
        context.storage_state(path=str(output_dir / "storage_state.json"))

        if captured["url"]:
            print(f"[+] 已捕获 callback: {captured['url']}")
        else:
            print("[!] 30 秒内未捕获 callback，请检查 events.jsonl")

        browser.close()

if __name__ == "__main__":
    main()
