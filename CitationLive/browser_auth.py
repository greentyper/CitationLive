#!/usr/bin/env python3
"""Local browser authorization for Citation ONE.

Opens the user's normal browser (Chrome, Edge or bundled Chromium) through
Playwright, lets the user sign in to Google News and Spotify Web, reads the
required HttpOnly cookies from that browser profile and sends them only to the
speaker's local API (127.0.0.1). Cookie values are never printed.

Examples:
    python browser_auth.py --news
"""
from __future__ import annotations

import argparse
import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

try:
    from playwright.sync_api import sync_playwright
except Exception:
    print("Playwright is not installed. Install it with:")
    print("  python -m pip install --upgrade playwright")
    print("Then run this helper again. Chrome or Edge is used when present.")
    raise SystemExit(2)

NEWS_URL = "https://news.google.com/"
NEWS_AUTH_COOKIES = (
    "SID", "SSID", "HSID", "SAPISID", "APISID",
    "__Secure-1PSID", "__Secure-3PSID",
)


def tr(ru: str, en: str, lang: str) -> str:
    return ru if lang == "ru" else en


def http_json(base: str, method: str, path: str, body: dict | None = None, timeout: int = 40):
    url = base.rstrip("/") + path
    data = None
    headers = {"accept": "application/json"}
    if body is not None:
        data = json.dumps(body, ensure_ascii=False).encode("utf-8")
        headers["content-type"] = "application/json"
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as exc:
        raw = exc.read().decode("utf-8", "replace")
        try:
            parsed = json.loads(raw) if raw.strip() else {}
        except json.JSONDecodeError:
            parsed = {}
        if not isinstance(parsed, dict):
            parsed = {}
        parsed.setdefault("ok", False)
        parsed.setdefault("error", raw.strip() or f"HTTP {exc.code}")
        return parsed
    return json.loads(raw) if raw.strip() else {}


def check_local_base(base: str) -> None:
    host = urllib.parse.urlparse(base).hostname
    if host not in ("127.0.0.1", "localhost", "::1"):
        raise SystemExit("Refusing to send cookies to a non-local address: " + base)


def find_browser() -> str:
    if os.name == "nt":
        candidates = [
            os.path.expandvars(r"%ProgramFiles%\Google\Chrome\Application\chrome.exe"),
            os.path.expandvars(r"%ProgramFiles(x86)%\Google\Chrome\Application\chrome.exe"),
            os.path.expandvars(r"%LocalAppData%\Google\Chrome\Application\chrome.exe"),
            os.path.expandvars(r"%ProgramFiles(x86)%\Microsoft\Edge\Application\msedge.exe"),
            os.path.expandvars(r"%ProgramFiles%\Microsoft\Edge\Application\msedge.exe"),
            os.path.expandvars(r"%LocalAppData%\Microsoft\Edge\Application\msedge.exe"),
        ]
    elif sys.platform == "darwin":
        candidates = [
            "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
            "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
        ]
    else:
        candidates = [
            "/usr/bin/google-chrome",
            "/usr/bin/google-chrome-stable",
            "/usr/bin/chromium",
            "/usr/bin/chromium-browser",
            "/usr/bin/microsoft-edge",
        ]
    for path in candidates:
        if path and os.path.isfile(path):
            return path
    return ""


def free_port() -> int:
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


def launch_cdp_browser(profile: Path):
    """Launch Chrome/Edge as a normal browser, without automation flags."""
    exe = find_browser()
    if not exe:
        return None, 0
    port = free_port()
    proc = subprocess.Popen([
        exe,
        f"--remote-debugging-port={port}",
        f"--user-data-dir={profile}",
        "--no-first-run",
        "--no-default-browser-check",
        "--disable-sync",
        "--remote-allow-origins=*",
        "--start-maximized",
    ], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    deadline = time.time() + 20
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(f"http://127.0.0.1:{port}/json/version", timeout=1) as resp:
                if resp.status == 200:
                    return proc, port
        except Exception:
            time.sleep(0.5)
    close_browser(proc)
    return None, 0


def close_browser(proc) -> None:
    if proc is None:
        return
    try:
        if os.name == "nt":
            subprocess.run(["taskkill", "/F", "/T", "/PID", str(proc.pid)],
                           capture_output=True, timeout=15)
        else:
            proc.terminate()
            proc.wait(timeout=5)
    except Exception:
        pass


def launch_context(p, profile: Path):
    args = ["--start-maximized"]
    for channel in ("chrome", "msedge"):
        try:
            return p.chromium.launch_persistent_context(
                user_data_dir=str(profile),
                channel=channel,
                headless=False,
                no_viewport=True,
                args=args,
            )
        except Exception:
            continue
    print("Chrome/Edge not found; using Playwright Chromium.")
    print("If it is missing, run: python -m playwright install chromium")
    return p.chromium.launch_persistent_context(
        user_data_dir=str(profile), headless=False, no_viewport=True, args=args
    )


def google_cookies(context) -> dict[str, str]:
    cookies: dict[str, str] = {}
    for cookie in context.cookies([
        "https://news.google.com",
        "https://accounts.google.com",
        "https://www.google.com",
    ]):
        domain = cookie.get("domain", "")
        name = cookie.get("name", "")
        value = cookie.get("value", "")
        if "google.com" in domain and name and value:
            cookies[name] = value
    return cookies


def setup_news(context, base: str, timeout: int, lang: str) -> bool:
    page = context.new_page()
    page.goto(NEWS_URL, wait_until="domcontentloaded", timeout=60000)
    print(tr(
        "\n[Google News] Войдите в Google-аккаунт в открытом окне, если это нужно.",
        "\n[Google News] Sign in to your Google account in the opened window if needed.",
        lang,
    ))
    deadline = time.time() + timeout
    cookies: dict[str, str] = {}
    while time.time() < deadline:
        cookies = google_cookies(context)
        if any(name in cookies for name in NEWS_AUTH_COOKIES):
            break
        time.sleep(2)
    if not any(name in cookies for name in NEWS_AUTH_COOKIES):
        print(tr(
            "[Google News] Cookies входа не найдены. Войдите в Google и запустите снова.",
            "[Google News] Sign-in cookies were not found. Sign in to Google and try again.",
            lang,
        ))
        return False
    print(tr(
        f"[Google News] Найдено cookie-пар: {len(cookies)}. Сохраняю на колонке...",
        f"[Google News] Found {len(cookies)} cookie pairs. Saving on the speaker...",
        lang,
    ))
    result = http_json(base, "POST", "/google/news-cookies", {"cookies": cookies})
    if not result.get("ok"):
        print(tr(
            f"[Google News] Колонка не приняла cookies: {result.get('error', 'unknown error')}",
            f"[Google News] The speaker rejected the cookies: {result.get('error', 'unknown error')}",
            lang,
        ))
        return False
    check = http_json(base, "GET", "/google/news-cookies/check")
    valid = bool(check.get("valid"))
    print(tr(
        f"[Google News] Проверка входа: {'успешно' if valid else 'не удалось'}.",
        f"[Google News] Sign-in check: {'OK' if valid else 'failed'}.",
        lang,
    ))
    return valid


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="http://127.0.0.1:28766")
    ap.add_argument("--lang", choices=("ru", "en"), default="ru")
    ap.add_argument("--news", action="store_true", help="authorize Google News cookies")
    ap.add_argument("--timeout", type=int, default=300)
    ap.add_argument("--profile", default="", help="keep the browser profile in this directory")
    args = ap.parse_args()
    check_local_base(args.base)

    if args.profile:
        profile = Path(args.profile)
        profile.mkdir(parents=True, exist_ok=True)
        cleanup = False
    else:
        profile = Path(tempfile.mkdtemp(prefix="citation-auth-"))
        cleanup = True

    ok_news = True
    proc = None
    print(tr(
        "Открою обычный Chrome/Edge. Войдите в нужный аккаунт. Cookies сохраняются только на колонке; временный профиль браузера будет удалён.",
        "Opening your normal Chrome/Edge. Sign in to the required account. Cookies are stored only on the speaker; the temporary browser profile will be deleted.",
        args.lang,
    ))
    try:
        with sync_playwright() as p:
            proc, port = launch_cdp_browser(profile)
            if port:
                browser = p.chromium.connect_over_cdp(f"http://127.0.0.1:{port}")
                context = browser.contexts[0] if browser.contexts else browser.new_context()
            else:
                context = launch_context(p, profile)
            try:
                ok_news = setup_news(context, args.base, args.timeout, args.lang)
            finally:
                try:
                    context.close()
                except Exception:
                    pass
    finally:
        close_browser(proc)
        if cleanup:
            shutil.rmtree(profile, ignore_errors=True)
    return 0 if ok_news else 1


if __name__ == "__main__":
    raise SystemExit(main())
