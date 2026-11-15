#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""CitationLive: DNS и необязательный HTTPS-прокси для Gemini.

Проверяет именно те endpoint'ы, которые использует citationvoice:
  * REST  https://generativelanguage.googleapis.com/v1beta/models
  * WS    wss://generativelanguage.googleapis.com/ws/...BidiGenerateContent

DNS меняется в /run/resolv.conf (rootfs read-only), а runtime-хук
ensure_dns.sh применяет его при старте citationvoice и сторожит dhcpcd.

Использование:
    python citation_net.py --ip 192.168.1.100 --status
    python citation_net.py --ip 192.168.1.100 --auto
    python citation_net.py --ip 192.168.1.100 --dns-preset comss
    python citation_net.py --ip 192.168.1.100 --dns-servers "212.109.195.93 83.220.169.155"
    python citation_net.py --ip 192.168.1.100 --dns-mode system
    python citation_net.py --ip 192.168.1.100 --uninstall
    python citation_net.py --ip 192.168.1.100          # интерактивно
"""
from __future__ import annotations

import ipaddress
import argparse
import os
import shutil
import subprocess
import sys
import tempfile
import time
from pathlib import Path

HERE = Path(__file__).resolve().parent
APP = "/data/opt/citation-gemini"
BIN = f"{APP}/bin"
DNS_CONF = f"{APP}/dns.conf"
PROXY_CONF = f"{APP}/proxy.conf"
PROBE_REMOTE = "/data/local/tmp/citation_probe_dns.sh"
ENSURE_REMOTE = f"{BIN}/ensure_dns.sh"
VOICE_REMOTE = f"{BIN}/citationvoice"
VOICE_REAL = f"{BIN}/citationvoice.real"
GEMINI_HOST = "generativelanguage.googleapis.com"

PRESETS = {
    "magicdns": {
        "label": "MagicDNS",
        "servers": ["217.144.190.216", "109.120.187.249"],
        "doh": "https://api.magicinfo.art/rpc",
    },
    "malw": {
        "label": "MALW DNS",
        "servers": ["179.254.166.171", "164.5.224.81"],
        "doh": "https://dns.malw.link/dns-query",
    },
    "geohide": {
        "label": "GeoHide EU",
        "servers": ["143.20.64.192", "5.175.161.40", "37.221.93.141", "31.25.239.132"],
        "doh": "https://eu.geohide.ru/dns-query",
    },
    "comss": {
        "label": "Comss.one DNS",
        "servers": ["94.250.253.55", "212.109.195.93", "83.220.169.155"],
        "doh": "https://dns.comss.one/dns-query",
    },
    "dnsai": {
        "label": "DNS-AI",
        "servers": ["194.87.196.13", "45.151.102.219"],
        "doh": "https://dns.dns-ai.ru/dns-query",
    },
}

# --- i18n (RU/EN) --------------------------------------------------------
LANG = (os.environ.get("CITATION_LANG") or "ru").strip().lower()
if LANG not in ("ru", "en"):
    LANG = "ru"

EN = {
    "    [!] API ответил отказом: ключ/регион/квота, DNS тут не поможет.":
        "    [!] The API refused: key/region/quota. DNS will not help here.",
    "    [!] Нет соединения с API через этот DNS.":
        "    [!] No connection to the API through this DNS.",
    "[!] ensure_dns.sh вернул ошибку; проверьте /data/opt/citation-gemini/state/dns.log":
        "[!] ensure_dns.sh returned an error; check /data/opt/citation-gemini/state/dns.log",
    "[+] DNS применён.":
        "[+] DNS applied.",
    "[+] DNS-хук удалён, citationvoice восстановлен.":
        "[+] DNS hook removed, citationvoice restored.",
    "\n[+] Системный DNS уже даёт Gemini API 200 (REST). DNS-обход для этого канала не нужен.":
        "\n[+] The system DNS already returns Gemini API 200 (REST). No DNS bypass is needed for this channel.",
    "\nВсё равно включить DNS-обход? [y/N]: ":
        "\nEnable the DNS bypass anyway? [y/N]: ",
    "Оставлено как есть.":
        "Left unchanged.",
    "\nВыберите DNS:":
        "\nChoose DNS:",
    "Номер: ":
        "Number: ",
    "Неверный выбор.":
        "Invalid choice.",
    "DNS-серверы через пробел: ":
        "DNS servers separated by spaces: ",
    "Пустой список DNS.":
        "Empty DNS list.",
    "Этот DNS не дал API 200. Всё равно применить? [y/N]: ":
        "This DNS did not return API 200. Apply it anyway? [y/N]: ",
    "Не применено.":
        "Not applied.",
    "Настроить HTTPS-прокси для Gemini? [y/N]: ":
        "Configure an HTTPS proxy for Gemini? [y/N]: ",
    "Прокси URL (http://user:pass@host:port): ":
        "Proxy URL (http://user:pass@host:port): ",
    "[+] Системный DNS уже работает; менять не нужно.":
        "[+] The system DNS already works; no change needed.",
}

EN_REPLACE = [
    ("не запущен", "not running"),
    ("Свой DNS", "Custom DNS"),
]

EN_PREFIX = [
    ("    [!] ", "    [!] "),
]


def tr(text):
    if LANG != "en" or not isinstance(text, str):
        return text
    if text in EN:
        return EN[text]
    for ru, en in EN_PREFIX:
        if text.startswith(ru):
            text = en + text[len(ru):]
            break
    for ru, en in EN_REPLACE:
        text = text.replace(ru, en)
    return text


import builtins

_orig_print = builtins.print
_orig_input = builtins.input


def _print(*args, **kwargs):
    args = tuple(tr(a) if isinstance(a, str) else a for a in args)
    _orig_print(*args, **kwargs)


def _input(prompt=""):
    return _orig_input(tr(prompt))


builtins.print = _print
builtins.input = _input
# -------------------------------------------------------------------------

WRAPPER = """#!/bin/sh
APP=/data/opt/citation-gemini
CONF="$APP/proxy.conf"
if [ -f "$CONF" ]; then
    PROXY_URL=$(sed -n 's/^PROXY_URL=//p' "$CONF" | head -n1 | tr -d '\\r')
    NO_PROXY=$(sed -n 's/^NO_PROXY=//p' "$CONF" | head -n1 | tr -d '\\r')
    if [ -n "$PROXY_URL" ]; then
        export HTTPS_PROXY="$PROXY_URL" HTTP_PROXY="$PROXY_URL" ALL_PROXY="$PROXY_URL"
    fi
    [ -n "$NO_PROXY" ] && export NO_PROXY
fi
/bin/sh "$APP/bin/ensure_dns.sh" >/dev/null 2>&1 || true
exec "$APP/bin/citationvoice.real" "$@"
"""


def find_adb() -> str:
    name = "adb.exe" if os.name == "nt" else "adb"
    for cand in (HERE / "bin" / name, HERE / "bin" / "adb"):
        if cand.is_file():
            return str(cand)
    found = shutil.which("adb")
    if found:
        return found
    sys.exit("adb не найден: положите adb.exe в bin/")


def adb_run(adb: str, ip: str, args: list[str], timeout: int = 60,
            check: bool = False) -> subprocess.CompletedProcess:
    marker = '__CITATION_NET_RC__='
    remote_shell = bool(args and args[0] == 'shell')
    if remote_shell:
        if len(args) != 2:
            raise ValueError('Expected a single remote shell command')
        args = ['shell', '( ' + args[1] + ' ); rc=$?; printf "\\n' + marker + '%s\\n" "$rc"']
    cmd = [adb, "-s", f"{ip}:5555", *args]
    p = subprocess.run(cmd, capture_output=True, text=True,
                       timeout=timeout, errors="replace",
                       stdin=subprocess.DEVNULL)
    if remote_shell and p.returncode == 0:
        body, found, tail = p.stdout.rpartition(marker)
        try:
            p.returncode = int(tail.strip()) if found else -1
        except ValueError:
            p.returncode = -1
        p.stdout = body.rstrip() if found else p.stdout
    if check and p.returncode != 0:
        raise RuntimeError('ADB operation failed; check connection and device storage')
    return p


def connect(adb: str, ip: str) -> None:
    subprocess.run([adb, "connect", f"{ip}:5555"], capture_output=True,
                   text=True, timeout=20, stdin=subprocess.DEVNULL)
    p = adb_run(adb, ip, ["shell", "id"], timeout=15)
    if "uid=0" not in p.stdout:
        sys.exit(f"ADB на {ip}:5555 не отвечает или нет root: {p.stdout}{p.stderr}")


def push_text(adb: str, ip: str, remote: str, text: str, mode: int) -> None:
    with tempfile.NamedTemporaryFile("w", delete=False, encoding="utf-8",
                                     newline="\n") as fh:
        fh.write(text)
        local = fh.name
    try:
        adb_run(adb, ip, ["push", local, remote], timeout=60, check=True)
        adb_run(adb, ip, ["shell", f"chmod {mode:o} {remote}"], timeout=15,
                check=True)
    finally:
        os.unlink(local)


def after(out: str, label: str) -> str:
    lines = out.splitlines()
    for i, line in enumerate(lines):
        if line.strip() == label:
            for nxt in lines[i + 1:]:
                if nxt.strip():
                    return nxt.strip()
    return ""


def parse_code(value: str) -> tuple[str, str]:
    parts = value.split()
    if not parts:
        return "", ""
    return parts[0], (parts[1] if len(parts) > 1 else "")


def probe(adb: str, ip: str, servers: list[str] | None = None) -> dict:
    if servers:
        servers = [str(ipaddress.ip_address(server)) for server in servers]
        mode, arg = "set", " ".join(servers)
    else:
        mode, arg = "system", ""
    push_text(adb, ip, PROBE_REMOTE,
              (HERE / "dns" / "probe_dns.sh").read_text(encoding="utf-8"), 0o755)
    p = adb_run(adb, ip, ["shell", f"sh {PROBE_REMOTE} {mode} '{arg}'"],
                timeout=60)
    out = p.stdout
    rest_code, rest_ip = parse_code(after(out, "REST:"))
    ws_code, ws_ip = parse_code(after(out, "WS:"))
    return {"out": out, "rest": rest_code, "rest_ip": rest_ip,
            "ws": ws_code, "ws_ip": ws_ip}


def show_probe(name: str, result: dict) -> bool:
    print(f"[*] {name}: REST={result['rest'] or '?'} "
          f"({result['rest_ip'] or '-'})  WS={result['ws'] or '?'} "
          f"({result['ws_ip'] or '-'})", flush=True)
    if result["rest"] == "200":
        return True
    if result["rest"] in ("400", "403"):
        print("    [!] API ответил отказом: ключ/регион/квота, DNS тут не поможет.",
              flush=True)
    elif result["rest"] == "000":
        print("    [!] Нет соединения с API через этот DNS.", flush=True)
    return False


def live_ok(adb: str, ip: str, timeout: int = 45) -> tuple[bool, str]:
    """Confirm the real Gemini Live setup, not only the REST catalog."""
    p = adb_run(adb, ip, ["shell", f"{VOICE_REMOTE} check"], timeout=timeout)
    out = (p.stdout + p.stderr).strip()
    return '"live_setup":true' in out, out


def write_dns_conf(adb: str, ip: str, mode: str, servers: list[str],
                   label: str) -> None:
    conf = f"MODE={mode}\nSERVERS={' '.join(servers)}\nLABEL={label}\n"
    push_text(adb, ip, DNS_CONF, conf, 0o644)


def install_runtime(adb: str, ip: str) -> None:
    push_text(adb, ip, ENSURE_REMOTE,
              (HERE / "dns" / "ensure_dns.sh").read_text(encoding="utf-8"), 0o755)
    size = adb_run(adb, ip, ["shell", f"wc -c < {VOICE_REMOTE}"], timeout=15).stdout
    try:
        voice_size = int(size.strip())
    except ValueError:
        voice_size = 0
    if voice_size > 1_000_000:
        adb_run(adb, ip, ["shell", f"mv -f {VOICE_REMOTE} {VOICE_REAL}"],
                timeout=30, check=True)
    push_text(adb, ip, VOICE_REMOTE, WRAPPER, 0o755)


def restart(adb: str, ip: str) -> None:
    adb_run(adb, ip, ["shell",
                      "systemctl restart citation-gemini 2>/dev/null || "
                      "pkill -f citationvoice 2>/dev/null || true"], timeout=30)
    time.sleep(2)
    p = adb_run(adb, ip, ["shell",
                          "pidof citationvoice.real || pidof citationvoice"],
                timeout=15)
    print(f"[*] citationvoice PID: {p.stdout.strip() or 'не запущен'}", flush=True)


def apply(adb: str, ip: str, mode: str, servers: list[str], label: str,
          proxy: str | None, no_restart: bool) -> None:
    if proxy is not None and any(c in proxy for c in "\r\n\x00"):
        raise ValueError("Invalid proxy URL")
    servers = [str(ipaddress.ip_address(server)) for server in servers]
    install_runtime(adb, ip)
    write_dns_conf(adb, ip, mode, servers, label)
    if proxy is not None:
        push_text(adb, ip, PROXY_CONF,
                  f"PROXY_URL={proxy}\nNO_PROXY=localhost,127.0.0.1,192.168.0.0/16\n",
                  0o600)
    else:
        adb_run(adb, ip, ["shell", f"rm -f {PROXY_CONF}"], timeout=15)
    p = adb_run(adb, ip, ["shell", f"sh {ENSURE_REMOTE}"], timeout=40)
    if p.returncode != 0:
        raise RuntimeError("DNS was not applied; check state/dns.log")
    if not no_restart:
        restart(adb, ip)
    print("[+] DNS применён.", flush=True)


def status(adb: str, ip: str) -> None:
    print("=== /run/resolv.conf ===", flush=True)
    print(adb_run(adb, ip, ["shell", "cat /run/resolv.conf"]).stdout.strip())
    print("\n=== dns.conf ===", flush=True)
    print(adb_run(adb, ip, ["shell", f"cat {DNS_CONF} 2>/dev/null || echo none"]).stdout.strip())
    print("\n=== proxy.conf ===", flush=True)
    print(adb_run(adb, ip, ["shell", f"test -s {PROXY_CONF} && echo configured || echo none"]).stdout.strip())
    print("\n=== citationvoice ===", flush=True)
    print(adb_run(adb, ip, ["shell", f"ls -l {VOICE_REMOTE} {VOICE_REAL} 2>/dev/null; echo PIDS:; pidof citationvoice.real || pidof citationvoice"]).stdout.strip())
    print("\n=== probe (system DNS) ===", flush=True)
    show_probe("system", probe(adb, ip))


def uninstall(adb: str, ip: str) -> None:
    write_dns_conf(adb, ip, "system", [], "system")
    adb_run(adb, ip, ["shell", f"sh {ENSURE_REMOTE}"], timeout=30)
    adb_run(adb, ip, ["shell",
                      f"if [ -f {VOICE_REAL} ]; then mv -f {VOICE_REAL} {VOICE_REMOTE}; fi; "
                      f"rm -f {DNS_CONF} {PROXY_CONF}"], timeout=30)
    restart(adb, ip)
    print("[+] DNS-хук удалён, citationvoice восстановлен.", flush=True)


def interactive(adb: str, ip: str, setup: bool = False) -> None:
    if not setup:
        base = probe(adb, ip)
        system_ok = show_probe("system DNS", base)
        if system_ok:
            print("\n[+] Системный DNS уже даёт Gemini API 200 (REST). "
                  "DNS-обход для этого канала не нужен.", flush=True)
        ans = input("\nВсё равно включить DNS-обход? [y/N]: ").strip().lower()
        if ans not in ("y", "yes", "д", "да"):
            print("Оставлено как есть.", flush=True)
            return

    names = list(PRESETS)
    print("\nВыберите DNS:")
    for i, name in enumerate(names, 1):
        p = PRESETS[name]
        print(f"  {i}) {p['label']} — {', '.join(p['servers'])}")
    print(f"  {len(names)+1}) Свой DNS")
    raw = input("Номер: ").strip()
    try:
        idx = int(raw)
    except ValueError:
        print("Неверный выбор.")
        return
    if 1 <= idx <= len(names):
        name = names[idx - 1]
        preset = PRESETS[name]
        servers = preset["servers"]
        label = preset["label"]
    elif idx == len(names) + 1:
        raw_s = input("DNS-серверы через пробел: ").strip()
        servers = [s for s in raw_s.replace(",", " ").split() if s]
        label = "custom"
    else:
        print("Неверный выбор.")
        return
    if not servers:
        print("Пустой список DNS.")
        return

    result = probe(adb, ip, servers)
    if not show_probe(label, result):
        ans = input("Этот DNS не дал API 200. Всё равно применить? [y/N]: ").strip().lower()
        if ans not in ("y", "yes", "д", "да"):
            print("Не применено.")
            return
    proxy = None
    pans = input("Настроить HTTPS-прокси для Gemini? [y/N]: ").strip().lower()
    if pans in ("y", "yes", "д", "да"):
        proxy = input("Прокси URL (http://user:pass@host:port): ").strip() or None
    apply(adb, ip, "custom", servers, label, proxy, no_restart=False)


def main() -> None:
    ap = argparse.ArgumentParser(description="CitationLive DNS / proxy setup")
    ap.add_argument("--ip", required=True, help="IP колонки")
    ap.add_argument("--lang", choices=["ru", "en"], default=None,
                    help="UI language / язык интерфейса")
    ap.add_argument("--status", action="store_true")
    ap.add_argument("--auto", action="store_true",
                    help="проверить систему и пресеты, применить первый рабочий")
    ap.add_argument("--setup", action="store_true",
                    help="мастер установки: сразу выбор DNS-пресета")
    ap.add_argument("--dns-preset", choices=sorted(PRESETS))
    ap.add_argument("--dns-servers", help="свой список DNS через пробел")
    ap.add_argument("--dns-mode", choices=["system", "off"])
    ap.add_argument("--proxy", help="HTTPS-прокси, http://user:pass@host:port")
    ap.add_argument("--force", action="store_true",
                    help="применить DNS даже если API не дал 200")
    ap.add_argument("--no-restart", action="store_true")
    ap.add_argument("--uninstall", action="store_true")
    args = ap.parse_args()

    global LANG
    if args.lang:
        LANG = args.lang

    adb = find_adb()
    connect(adb, args.ip)

    if args.status:
        status(adb, args.ip)
        return
    if args.uninstall:
        uninstall(adb, args.ip)
        return
    if args.dns_mode:
        apply(adb, args.ip, "system", [], "system", args.proxy, args.no_restart)
        return

    if args.dns_preset:
        preset = PRESETS[args.dns_preset]
        servers, label = preset["servers"], preset["label"]
        result = probe(adb, args.ip, servers)
        ok = show_probe(label, result)
        if not ok and not args.force:
            sys.exit("DNS не прошёл проверку API; не применяю.")
        apply(adb, args.ip, "custom", servers, label, args.proxy, args.no_restart)
        return

    if args.dns_servers:
        servers = [s for s in args.dns_servers.replace(",", " ").split() if s]
        if not servers:
            sys.exit("Пустой список DNS.")
        result = probe(adb, args.ip, servers)
        ok = show_probe("custom", result)
        if not ok and not args.force:
            sys.exit("DNS не прошёл проверку API; не применяю.")
        apply(adb, args.ip, "custom", servers, "custom", args.proxy, args.no_restart)
        return

    if args.proxy and not args.auto:
        apply(adb, args.ip, "system", [], "system", args.proxy, args.no_restart)
        return

    if args.setup:
        interactive(adb, args.ip, setup=True)
        return

    if args.auto:
        base = probe(adb, args.ip)
        if show_probe("system DNS", base):
            ok, _ = live_ok(adb, args.ip)
            if ok:
                print("[+] Системный DNS уже работает; менять не нужно.", flush=True)
                return
            print("[!] Системный DNS даёт REST 200, но Gemini Live не подтвердился; "
                  "проверяю пресеты.", flush=True)
        for name, preset in PRESETS.items():
            result = probe(adb, args.ip, preset["servers"])
            if not show_probe(preset["label"], result):
                continue
            apply(adb, args.ip, "custom", preset["servers"],
                  preset["label"], args.proxy, args.no_restart)
            ok, out = live_ok(adb, args.ip)
            if not ok:
                time.sleep(2)
                ok, out = live_ok(adb, args.ip)
            if ok:
                print(f"[+] Gemini Live подтверждён через {preset['label']}: "
                      f"{' '.join(preset['servers'])}", flush=True)
                return
            print(f"[!] {preset['label']}: REST отвечает, но Gemini Live не подтвердился; "
                  f"пробую следующий DNS.", flush=True)
        apply(adb, args.ip, "system", [], "system", None, args.no_restart)
        sys.exit("Ни один DNS-пресет не подтвердил Gemini Live; вернул системный DNS.")

    interactive(adb, args.ip)


if __name__ == "__main__":
    main()
