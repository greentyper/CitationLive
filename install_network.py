#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""Install CitationLive over network ADB.

Order:
  1. Check whether ADB already answers over the network.
  2. If not, try analyticsUpload (WAMP RCE, fj016 method).
  3. If the RCE fails, try the airplay scan (experimental).
  4. Wait for port 5555 and open the CitationLive setup menu.

analyticsUpload does not depend on ASLR or the 68-byte google-wamp buffer, so
it is the primary network method.

Usage:
    python install_network.py --ip 192.168.1.100
    python install_network.py --ip 192.168.1.100 --key AIza...
    python install_network.py --ip 192.168.1.100 --force
"""
from __future__ import annotations

import argparse
import importlib.util
import os
import shutil
import socket
import subprocess
import sys
import time
from pathlib import Path

HERE = Path(__file__).resolve().parent
EXPLOIT = HERE / "ADB-Exploit" / "citation_one_adb.py"
ANALYTICS = HERE / "ADB-Exploit" / "citation_analytics_rce.py"
CL = HERE / "CitationLive"
TOOL = CL / "citation_tool.py"

LANG = (os.environ.get("CITATION_LANG") or "ru").strip().lower()
if LANG not in ("ru", "en"):
    LANG = "ru"


def t(ru: str, en: str) -> str:
    return en if LANG == "en" else ru


def find_adb() -> str:
    name = "adb.exe" if os.name == "nt" else "adb"
    bundled = CL / "bin" / name
    if bundled.is_file():
        return str(bundled)
    found = shutil.which(name)
    if found:
        return found
    sys.exit(t("adb не найден: положите adb.exe в CitationLive/bin",
               "adb not found: put adb.exe into CitationLive/bin"))


def adb_ready(adb: str, ip: str) -> bool:
    serial = f"{ip}:5555"
    try:
        subprocess.run([adb, "connect", serial], capture_output=True,
                       text=True, timeout=15)
        p = subprocess.run([adb, "-s", serial, "shell", "id"],
                           capture_output=True, text=True, timeout=10)
        return p.returncode == 0 and "uid=0" in p.stdout
    except Exception:
        return False


def wait_port(ip: str, timeout: int = 180) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            with socket.create_connection((ip, 5555), timeout=3):
                return True
        except OSError:
            time.sleep(2)
    return False


def main() -> None:
    global LANG
    ap = argparse.ArgumentParser(description="CitationLive over network ADB")
    ap.add_argument("--ip", required=True, help=t("IP колонки", "Speaker IP"))
    ap.add_argument("--key", help="Gemini API key")
    ap.add_argument("--force", action="store_true",
                    help=t("разрешить установку на непроверенном железе",
                           "allow install on untested hardware"))
    ap.add_argument("--lang", choices=["ru", "en"], default=None,
                    help="UI language / язык интерфейса")
    ap.add_argument("--install", action="store_true",
                    help=t("установить сразу, без меню настройки",
                           "install immediately, without the setup menu"))
    ap.add_argument("--all-in-one", action="store_true", help="Install Gemini, then offer Spotify, Google and News")
    ap.add_argument("--connect-only", action="store_true", help="Enable ADB and return without installing")
    args = ap.parse_args()
    if args.lang:
        LANG = args.lang
    os.environ["CITATION_LANG"] = LANG

    if not EXPLOIT.is_file():
        sys.exit(t(f"не найден airplay-эксплойт: {EXPLOIT}",
                   f"airplay exploit not found: {EXPLOIT}"))
    if not ANALYTICS.is_file():
        sys.exit(t(f"не найден analytics-эксплойт: {ANALYTICS}",
                   f"analytics exploit not found: {ANALYTICS}"))
    if not TOOL.is_file():
        sys.exit(t(f"не найден установщик: {TOOL}",
                   f"installer not found: {TOOL}"))

    adb = find_adb()
    ip = args.ip.strip()

    if adb_ready(adb, ip):
        print(t(f"[+] ADB уже работает: {ip}:5555",
                f"[+] ADB already works: {ip}:5555"), flush=True)
    else:
        missing = [name for name in ('msgpack', 'websocket') if importlib.util.find_spec(name) is None]
        if missing:
            print(t('Для подключения нужны Python-зависимости. Выполните команду и повторите установку:',
                    'Connection requires Python dependencies. Run this command, then retry:'))
            print(f'"{sys.executable}" -m pip install -r "{HERE / "ADB-Exploit" / "requirements.txt"}"')
            raise SystemExit(1)
        print(t("[*] ADB не отвечает — пробую analyticsUpload (WAMP RCE)...",
                "[*] ADB is not responding — trying analyticsUpload (WAMP RCE)..."),
              flush=True)
        proc = subprocess.run([sys.executable, "-B", str(ANALYTICS),
                               "--ip", ip])
        if proc.returncode == 0 and wait_port(ip):
            print(t(f"[+] ADB включён analyticsUpload: {ip}:5555",
                    f"[+] ADB enabled by analyticsUpload: {ip}:5555"), flush=True)
        else:
            print(t("[!] analyticsUpload не сработал — пробую airplay-скан...",
                    "[!] analyticsUpload failed — trying the airplay scan..."),
                  flush=True)
            proc = subprocess.run([sys.executable, "-B", str(EXPLOIT), ip])
            if proc.returncode != 0 or not wait_port(ip):
                sys.exit(t(
                    "\n[-] Не удалось включить ADB ни analyticsUpload, ни\n"
                    "    airplay-сканом. Повторите запуск; если analyticsUpload\n"
                    "    не проходит, проверьте /data/ota/logs_enabled.",
                    "\n[-] Could not enable ADB with analyticsUpload or the\n"
                    "    airplay scan. Run it again; if analyticsUpload fails,\n"
                    "    check /data/ota/logs_enabled."))
            print(t(f"[+] ADB включён airplay: {ip}:5555",
                    f"[+] ADB enabled by airplay: {ip}:5555"), flush=True)

    if args.connect_only:
        return

    if args.all_in_one or args.install or args.key or args.force:
        cmd = [sys.executable, "-B", str(TOOL), "pipeline" if args.all_in_one else "install", "--ip", ip,
               "--lang", LANG]
        if args.key:
            cmd += ["--key", args.key]
        if args.force:
            cmd.append("--force")
        print(t("[*] Запускаю установщик CitationLive поверх сетевого ADB...\n",
                "[*] Starting the CitationLive installer over network ADB...\n"),
              flush=True)
    else:
        cmd = [sys.executable, "-B", str(TOOL), "run", "--ip", ip, "--lang", LANG]
        print(t("[*] ADB готов. Открываю меню настройки CitationLive...\n"
                "    Выберите [1] для полной установки или [4] для установки\n"
                "    только Gemini. Остальные пункты — Spotify, Google, News, DNS.\n",
                "[*] ADB is ready. Opening the CitationLive setup menu...\n"
                "    Choose [1] for a full install or [4] to install Gemini only.\n"
                "    Other items cover Spotify, Google, News and DNS.\n"),
              flush=True)
    raise SystemExit(subprocess.run(cmd).returncode)


if __name__ == "__main__":
    main()
