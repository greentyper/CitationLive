#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""CitationLive installer (RU/EN)."""

from pathlib import Path
import argparse
import hashlib
import ipaddress
import json
import os
import shutil
import subprocess
import sys
import time
import webbrowser
from datetime import datetime, timezone

ROOT = Path(__file__).resolve().parent
# Bundled ADB on Windows; system ADB on other hosts.
BIN_DIR = ROOT / 'bin' if (ROOT / 'bin').is_dir() else ROOT.parent / 'tools/platform-tools'
ADB = BIN_DIR / ('adb.exe' if os.name == 'nt' else 'adb')
if not ADB.is_file() and shutil.which('adb'):
    ADB = Path(shutil.which('adb'))
PYTHON_EXE = sys.executable

DIST_DIR = ROOT / 'dist' if (ROOT / 'dist').is_dir() else ROOT.parent / 'dist'
SCRIPTS_DIR = ROOT / 'scripts'
CONFIG_DIR = ROOT / 'config'
LOG_DIR = ROOT / 'logs'
LOG_DIR.mkdir(exist_ok=True)

# --- i18n (RU/EN) --------------------------------------------------------
LANG = (os.environ.get("CITATION_LANG") or "ru").strip().lower()
if LANG not in ("ru", "en"):
    LANG = "ru"

EN = {
    "[ЭТАП 3/4] ЗАГРУЗКА В СИСТЕМУ И ПОДКЛЮЧЕНИЕ СЕТЕВОГО ADB":
        "[STEP 3/4] BOOTING INTO THE SYSTEM AND CONNECTING NETWORK ADB",
    "Колонка перезагружается в рабочую систему Linux.":
        "The speaker is booting into the Linux system.",
    "\nВведите IP-адрес колонки в сети Wi-Fi (по умолчанию автоопрос): ":
        "\nEnter the speaker IP address on Wi-Fi (blank = auto-detect): ",
    "Колонка не ответила по сетевому ADB за отведённое время.":
        "The speaker did not answer over network ADB in time.",
    "\nВведите текущий IP-адрес колонки в вашей сети Wi-Fi: ":
        "\nEnter the current speaker IP address on your Wi-Fi network: ",
    "ВНИМАНИЕ: Подключённое устройство не опознано как Harman Kardon Citation ONE MKIII!":
        "WARNING: The connected device is not recognized as Harman Kardon Citation ONE MKIII!",
    "Продолжить экспериментальную установку CitationLive? [y/N]: ":
        "Continue the experimental CitationLive installation? [y/N]: ",
    "Установка отменена пользователем.":
        "Installation cancelled by the user.",
    "Экспериментальная установка разрешена пользователем (--force).":
        "Experimental installation allowed by the user (--force).",
    "Найден локальный сохранённый API-ключ Gemini.":
        "Found a saved local Gemini API key.",
    " НАСТРОЙКА КЛЮЧА GOOGLE GEMINI LIVE API":
        " GOOGLE GEMINI LIVE API KEY SETUP",
    " ЧТО НУЖНО СДЕЛАТЬ ВРУЧНУЮ В БРАУЗЕРЕ:":
        " WHAT TO DO MANUALLY IN THE BROWSER:",
    " 1. Откройте страницу: https://aistudio.google.com/app/apikey":
        " 1. Open: https://aistudio.google.com/app/apikey",
    " 2. Войдите под вашим Google-аккаунтом.":
        " 2. Sign in with your Google account.",
    " 3. Нажмите кнопку 'Create API key' (Создать ключ).":
        " 3. Click 'Create API key'.",
    " 4. Скопируйте сгенерированный ключ (строка вида AIzaSy...).":
        " 4. Copy the generated key (a string like AIzaSy...).",
    "Открыть Google AI Studio в браузере прямо сейчас? [Y/n]: ":
        "Open Google AI Studio in the browser now? [Y/n]: ",
    "\nВставьте ваш Gemini API Key: ":
        "\nPaste your Gemini API key: ",
    " DNS ДЛЯ GEMINI":
        " DNS FOR GEMINI",
    "Настроить DNS для Gemini? [y/N]: ":
        "Configure DNS for Gemini? [y/N]: ",
    "DNS оставлен системным.":
        "DNS left as system default.",
    "DNS не настроен (см. вывод выше).":
        "DNS was not configured (see output above).",
    "УСТАНОВКА АССИСТЕНТА GEMINI":
        "[STEP 4/4] INSTALLING AND CONFIGURING THE GEMINI ASSISTANT IN /DATA",
    "Очистка временных файлов на колонке...":
        "Cleaning temporary files on the speaker...",
    "Найден существующий API-ключ Gemini на колонке.":
        "Found an existing Gemini API key on the speaker.",
    "API-ключ не предоставлен. Установка отменена.":
        "No API key provided. Installation cancelled.",
    " НАСТРОЙКА ГОРОДА ДЛЯ ПОГОДЫ И ТОЧНОГО ВРЕМЕНИ":
        " HOME CITY FOR WEATHER AND LOCAL TIME",
    "Введите ваш город: ":
        "Enter your city: ",
    "Создание структуры каталогов /data/opt/citation-gemini...":
        "Creating the /data/opt/citation-gemini directory structure...",
    "Копирование исполняемых файлов на колонку...":
        "Copying executable files to the speaker...",
    "Нативный сервис Spotify Connect (go-librespot) установлен.":
        "Native Spotify Connect service (go-librespot) installed.",
    "Хук автозапуска /data/alsa_looper.sh установлен.":
        "Autostart hook /data/alsa_looper.sh installed.",
    "Звук запуска загружен в /data/opt/citation-gemini/cues/.":
        "Boot sound cue uploaded to /data/opt/citation-gemini/cues/.",
    "Светодиодные анимации Gemini (listening, thinking, speaking) установлены.":
        "Gemini LED animations (listening, thinking, speaking) installed.",
    "Файлы ассистента успешно установлены в /data/opt/citation-gemini!":
        "Assistant files installed to /data/opt/citation-gemini!",
    "Перезагрузка колонки для активации службы...":
        "Rebooting the speaker to activate the service...",
    "Ожидание перезапуска службы ассистента и аудиосистемы (порт 8766)...":
        "Waiting for the assistant and audio service to restart (port 8766)...",
    "Служба citationvoice активна! Микрофонный поток и аудиосистема в норме.":
        "citationvoice is running! Microphone stream and audio system are healthy.",
    "Проверка готовности облачного шлюза Gemini Live...":
        "Checking the Gemini Live cloud gateway...",
    "Соединение с Gemini Live установлено и проверено!":
        "Gemini Live connection established and verified!",
    "Ассистент установлен. Служба стартует в фоновом режиме.":
        "Assistant installed. The service is starting in the background.",
    "Spotify успешно подключён и авторизован на колонке!":
        "Spotify connected and authorized on the speaker!",
    "Привязка не завершена за 2 минуты. Вы можете повторить попытку позже через меню [5].":
        "Pairing did not complete in 2 minutes. You can retry later via menu [5].",
    " НАСТРОЙКА GOOGLE КАЛЕНДАРЯ И ЗАДАЧ (GOOGLE TASKS)":
        " GOOGLE CALENDAR AND TASKS SETUP",
    "Укажите путь к скачанному файлу credentials.json: ":
        "Path to the downloaded credentials.json: ",
    "Файл credentials.json не найден. Настройка отменена.":
        "credentials.json not found. Setup cancelled.",
    "Запуск скрипта авторизации Google OAuth...":
        "Running the Google OAuth authorization script...",
    "Завершите авторизацию Google в браузере; подключение подтверждается страницей callback.":
        "Google Calendar and Tasks connected!",
    "Ошибка при подключении Google OAuth.":
        "Google OAuth connection failed.",
    " НАСТРОЙКА ПЕРСОНАЛЬНЫХ НОВОСТЕЙ GOOGLE NEWS":
        " PERSONAL GOOGLE NEWS SETUP",
    "Вставьте строку cookies Google News: ":
        "Paste the Google News cookies string: ",
    "Cookies не введены. Настройка отменена.":
        "No cookies entered. Setup cancelled.",
    "Cookies Google News успешно сохранены на колонке!":
        "Google News cookies saved on the speaker!",
    "Колонка отклонила cookies.":
        "The speaker rejected the cookies.",
    "Не найден бинарный файл dist/citationvoice.":
        "Binary dist/citationvoice not found.",
    " НАЧАЛО СКВОЗНОЙ УСТАНОВКИ CITATIONLIVE":
        " STARTING END-TO-END CITATIONLIVE INSTALLATION",
    "ADB не найден. Включите его airplay-эксплойтом:":
        "ADB not found. Enable it with the airplay exploit:",
    "или запустите install_network.py --ip <IP>.":
        "or run install_network.py --ip <IP>.",
    "Ошибка при установке ассистента Gemini.":
        "Gemini assistant installation failed.",
    " НАСТРОЙКА ДОПОЛНИТЕЛЬНЫХ СЕРВИСОВ":
        " ADDITIONAL SERVICES SETUP",
    "Настроить Spotify Connect прямо сейчас? [y/N]: ":
        "Set up Spotify Connect now? [y/N]: ",
    "Настроить Google Календарь и Задачи прямо сейчас? [y/N]: ":
        "Set up Google Calendar and Tasks now? [y/N]: ",
    "Настроить персональные Google News прямо сейчас? [y/N]: ":
        "Set up personal Google News now? [y/N]: ",
    "             УСТАНОВКА CITATIONLIVE ЗАВЕРШЕНА!":
        "             CITATIONLIVE INSTALLATION COMPLETE!",
    "  Все основные компоненты и службы успешно настроены.":
        "  All main components and services are configured.",
    "  Если вы проверили работу на другом устройстве — пожалуйста,":
        "  If you tested it on another device, please",
    "  отправьте Pull Request в репозиторий проекта на GitHub!":
        "  send a Pull Request to the project repository on GitHub!",
    "\n--- ГЛАВНОЕ МЕНЮ CITATIONLIVE ---":
        "\n--- CITATIONLIVE MAIN MENU ---",
    " [1] Полная автоматическая установка «с нуля» (Рекомендуется)":
        " [1] Full automatic installation from scratch (Recommended)",
    " [2] Проверить текущий статус устройства":
        " [2] Check current device status",
    " [3] Включить ADB сетевым эксплойтом airplay":
        " [3] Enable ADB with the network airplay exploit",
    " [4] Установить / обновить CitationLive (на устройство с ADB)":
        " [4] Install / update CitationLive (on a device with ADB)",
    " [5] Подключить Spotify через код":
        " [5] Pair Spotify using a code",
    " [6] Подключить Google Календарь и Задачи (Google Cloud OAuth)":
        " [6] Connect Google Calendar and Tasks (Google Cloud OAuth)",
    " [7] Подключить персонализацию Google News (сессионные cookies)":
        " [7] Connect Google News personalization (session cookies)",
    " [8] Настроить DNS для Gemini":
        " [8] Configure DNS for Gemini",
    " [0] Выход":
        " [0] Exit",
    "\nВыберите действие [0-8]: ":
        "\nChoose an action [0-8]: ",
    "Выход.":
        "Exit.",
    "IP колонки: ":
        "Speaker IP: ",
    "Сначала подключите колонку по ADB.":
        "Connect the speaker over ADB first.",
    "Введите IP адрес колонки: ":
        "Enter the speaker IP address: ",
    "Неверный ввод, попробуйте снова.":
        "Invalid input, try again.",
    "Укажите --ip адрес устройства.":
        "Specify the device address with --ip.",
    "Устройство не обнаружено по ADB.":
        "Device not found over ADB.",
    "Проверка запуска citationvoice, микрофона и микшера...":
        "Checking citationvoice, microphone and mixer startup...",
    " — УСПЕШНО!":
        " — done!",
    " — ТАЙМАУТ":
        " — timeout",
    " сек).":
        " s).",
}

EN_PREFIX = [
    ("Сетевой ADB подключён! Права root подтверждены: ",
     "Network ADB connected! Root confirmed: "),
    ("Архитектура процессора: ", "CPU architecture: "),
    ("Установлен домашний город: ", "Home city set: "),
    ("Статус проверки Gemini Live: ", "Gemini Live check status: "),
    ("Открываю страницу настройки Spotify в браузере: ",
     "Opening the Spotify setup page in the browser: "),
    ("Проброс порта ADB tcp:28766 -> tcp:8766 на ",
     "Forwarding ADB port tcp:28766 -> tcp:8766 on "),
    ("Обнаружено устройство с активным ADB: ",
     "Device with active ADB found: "),
    ("Статус устройства: ", "Device status: "),
    ("Текущий статус устройства: ", "Current device status: "),
    ("Не найден эксплойт: ", "Exploit not found: "),
    ("ADB уже работает: ", "ADB already works: "),
    ("Ошибка передачи cookies: ", "Failed to send cookies: "),
    ("Откройте ссылку в браузере: ", "Open the link in the browser: "),
]

EN_REPLACE = [("не подключено", "not connected")]

EN_EXPERIMENTAL_WARNING = """
  -----------------------------------------------------------------
  On devices other than Citation ONE MKIII the project has
  NOT BEEN TESTED. Installation is AT YOUR OWN RISK.
  If it works, please send a Pull Request on GitHub!
  -----------------------------------------------------------------
        """

EN_GOOGLE_GUIDE = """
  WHAT TO DO MANUALLY IN GOOGLE CLOUD CONSOLE:
  -----------------------------------------------------------------
  1. Open Google Cloud Console: https://console.cloud.google.com/
  2. Create a project and enable in 'APIs & Services -> Library':
     - Google Calendar API
     - Google Tasks API
  3. In 'OAuth consent screen' set the Redirect URI:
     http://127.0.0.1:28766/oauth/callback
  4. In 'Credentials' create an OAuth client ID (Web application).
  5. Download the credentials file (credentials.json).
  -----------------------------------------------------------------
    """

EN_NEWS_GUIDE = """
  WHAT TO DO IN THE BROWSER:
  -----------------------------------------------------------------
  1. Open: https://news.google.com/
  2. Sign in to your Google account.
  3. Open developer tools (F12) -> Network,
     or copy the page cookies (a string like SID=...; HSID=...).
  -----------------------------------------------------------------
    """


def tr(text):
    if LANG != "en" or not isinstance(text, str):
        return text
    if text in EN:
        return EN[text]
    if text.startswith("\n  -----------------------------------------------------------------\n  На устройствах"):
        return EN_EXPERIMENTAL_WARNING
    if text.startswith("\n  ЧТО НУЖНО СДЕЛАТЬ ВРУЧНУЮ В GOOGLE CLOUD"):
        return EN_GOOGLE_GUIDE
    if text.startswith("\n  ЧТО НУЖНО СДЕЛАТЬ В БРАУЗЕРЕ:\n  -----------------------------------------------------------------\n  1. Откройте в браузере: https://news.google.com/"):
        return EN_NEWS_GUIDE
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



def ui(ru, en):
    return en if LANG == 'en' else ru


def open_page(url):
    """Always leave a usable URL when the system browser cannot be opened."""
    print(ui('Ссылка: ', 'Link: ') + url)
    try:
        opened = webbrowser.open(url)
    except Exception:
        opened = False
    if not opened:
        print(ui('Браузер не открылся. Скопируйте ссылку выше в браузер на этом компьютере.',
                 'Browser did not open. Paste the link above into a browser on this computer.'))
    return opened


def ask_speaker_ip():
    print(ui('IP колонки можно найти в приложении роутера, в списке подключённых устройств.',
             'Find the speaker IP in your router app, in the connected devices list.'))
    while True:
        value = input(ui('IP колонки (например 192.168.1.100; Enter — назад): ',
                         'Speaker IP (for example 192.168.1.100; Enter to go back): ')).strip()
        if not value:
            return None
        try:
            return str(ipaddress.IPv4Address(value))
        except ValueError:
            print(ui('Введите только IPv4-адрес, без http:// и номера порта.',
                     'Enter an IPv4 address without http:// or a port number.'))


def banner():
    print(r"""
===================================================================
             CitationLive — Voice Assistant Setup Tool
===================================================================
    """)


def log(msg, level='INFO'):
    prefixes = {
        'INFO': '[*]',
        'OK':   '[+]',
        'WARN': '[!]',
        'ERR':  '[-]',
        'STEP': '[>]'
    }
    pfx = prefixes.get(level, '[*]')
    print(f"{pfx} {tr(msg)}", flush=True)


def spinner_wait(message, check_fn, timeout=60, interval=0.8):
    """Wait for check_fn() to return truthy value while displaying an animated indicator."""
    message = tr(message)
    frames = ['[—]', '[\\]', '[|]', '[/]']
    frame_idx = 0
    start = time.time()
    deadline = start + timeout

    sys.stdout.write(f"[*] {message} ")
    sys.stdout.flush()

    while time.time() < deadline:
        res = check_fn()
        if res:
            sys.stdout.write(f"\r[+] {message}{tr(' — УСПЕШНО!')}               \n")
            sys.stdout.flush()
            return res
        sys.stdout.write(f"\r[*] {message} {frames[frame_idx % len(frames)]} ")
        sys.stdout.flush()
        frame_idx += 1
        time.sleep(interval)

    sys.stdout.write(f"\r[-] {message}{tr(' — ТАЙМАУТ')} ({timeout}{tr(' сек).')}   \n")
    sys.stdout.flush()
    return None


def check_prerequisites():
    """Verify network-install assets exist."""
    errors = []
    if not ADB.is_file():
        errors.append(f"Не найден ADB: {ADB}")
    if not (DIST_DIR / 'citationvoice').is_file():
        errors.append("Не найден dist/citationvoice")

    for relative in ('dist/iot_audio_capture48k', 'dist/libassistant.hotword-observe-v3.so',
                     'runtime/supervise.sh', 'runtime/alsa_looper.sh',
                     'runtime/ensure_native_capture.sh', 'runtime/ensure_early_hotword.sh',
                     'config.example.json'):
        if not (ROOT / relative).is_file():
            errors.append(f'Missing release asset: {relative}')

    if errors:
        for err in errors:
            log(err, 'ERR')
        return False
    return True


def detect_mode(target_ip=None):
    """Detect network or USB ADB."""
    if target_ip:
        try:
            subprocess.run([str(ADB), 'connect', f'{target_ip}:5555'],
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                           timeout=3)
            p = subprocess.run([str(ADB), '-s', f'{target_ip}:5555', 'shell', 'id'],
                               capture_output=True, text=True, timeout=3,
                               errors='replace')
            if p.returncode == 0 and 'uid=0(root)' in p.stdout:
                return 'ADB_NETWORK', f'{target_ip}:5555'
        except Exception:
            pass
        return 'NONE', None
    try:
        p = subprocess.run([str(ADB), 'devices'], capture_output=True, text=True,
                           timeout=2, errors='replace')
        for line in p.stdout.splitlines()[1:]:
            parts = line.split()
            if len(parts) >= 2 and parts[1] == 'device':
                if parts[0].startswith('192.168.') or parts[0].endswith(':5555'):
                    return 'ADB_NETWORK', parts[0]
                return 'ADB_USB', parts[0]
    except Exception:
        pass
    return 'NONE', None


def verify_target_compatibility(adb_serial, force=False):
    """Verify if the connected device is a known Citation ONE or arbitrary device."""
    p_arch = subprocess.run([str(ADB), '-s', adb_serial, 'shell', 'uname -m'],
                            capture_output=True, text=True, timeout=5, errors='replace')
    arch = p_arch.stdout.strip()

    p_cap = subprocess.run([str(ADB), '-s', adb_serial, 'shell', 'test -f /chrome/bin/iot_audio_capture && echo YES || echo NO'],
                           capture_output=True, text=True, timeout=5, errors='replace')
    is_citation = (p_cap.stdout.strip() == 'YES')

    if arch != 'armv7l':
        log(f'Unsupported architecture: {arch or "unknown"}', 'ERR')
        return False

    if not is_citation:
        log("ВНИМАНИЕ: Подключённое устройство не опознано как Harman Kardon Citation ONE MKIII!", 'WARN')
        log(f"Архитектура процессора: {arch}", 'INFO')
        print("""
  -----------------------------------------------------------------
  На устройствах, отличных от Citation ONE MKIII, работа проекта
  НЕ ТЕСТИРОВАЛАСЬ. Установка выполняется НА ВАШ СТРАХ И РИСК.
  Если всё заработает — пожалуйста, отправьте Pull Request на GitHub!
  -----------------------------------------------------------------
        """)
        if not force:
            ans = input("Продолжить экспериментальную установку CitationLive? [y/N]: ").strip().lower()
            if ans != 'y':
                log("Установка отменена пользователем.", 'ERR')
                return False
        log("Экспериментальная установка разрешена пользователем (--force).", 'WARN')

    return True


def prompt_api_key():
    """Display instructions and prompt for Google Gemini API key."""
    cfg_key_file = CONFIG_DIR / 'gemini.key'
    if cfg_key_file.is_file():
        k = cfg_key_file.read_text(encoding='utf-8').strip()
        if k:
            log("Найден локальный сохранённый API-ключ Gemini.", 'OK')
            return k

    print("\n" + "="*67)
    print(" НАСТРОЙКА КЛЮЧА GOOGLE GEMINI LIVE API")
    print("="*67)
    print(" ЧТО НУЖНО СДЕЛАТЬ ВРУЧНУЮ В БРАУЗЕРЕ:")
    print(" 1. Откройте страницу: https://aistudio.google.com/app/apikey")
    print(" 2. Войдите под вашим Google-аккаунтом.")
    print(" 3. Нажмите кнопку 'Create API key' (Создать ключ).")
    print(" 4. Скопируйте сгенерированный ключ (строка вида AIzaSy...).")
    print("="*67)

    open_now = input("Открыть Google AI Studio в браузере прямо сейчас? [Y/n]: ").strip().lower()
    if open_now != 'n':
        try:
            open_page("https://aistudio.google.com/app/apikey")
        except Exception:
            pass

    key = input("\nВставьте ваш Gemini API Key: ").strip()
    if key:
        CONFIG_DIR.mkdir(parents=True, exist_ok=True)
        cfg_key_file.write_text(key + '\n', encoding='utf-8')
        return key
    return None


def configure_dns(target_host):
    """Offer automatic or manual DNS selection for Gemini."""
    script = ROOT / 'citation_net.py'
    if not script.is_file():
        return
    print("\n" + "="*67)
    print(" DNS ДЛЯ GEMINI")
    print("="*67)
    print(ui(' [1] Автоподбор DNS (рекомендуется) — проверит системный DNS и пресеты,\n'
             '     затем проверит настоящий Gemini Live и оставит рабочий.\n'
             ' [2] Выбрать DNS вручную\n'
             ' [0] Не менять',
             ' [1] Automatic DNS selection (recommended) — checks system DNS and presets,\n'
             '     then verifies the real Gemini Live and keeps the working one.\n'
             ' [2] Choose DNS manually\n'
             ' [0] Do not change'))
    choice = input(ui('Выберите 0, 1 или 2: ', 'Choose 0, 1 or 2: ')).strip()
    if choice in ('0', ''):
        log("DNS оставлен системным.", 'INFO')
        return
    args = ['--setup'] if choice == '2' else ['--auto']
    p = subprocess.run([PYTHON_EXE, '-B', str(script), '--ip', target_host,
                        '--lang', LANG, *args])
    if p.returncode != 0:
        log("DNS не настроен (см. вывод выше).", 'WARN')


def deploy_run(*args, **kwargs):
    kwargs.setdefault('timeout', 120)
    kwargs['check'] = True
    command = list(args[0])
    if 'shell' not in command:
        return subprocess.run(*args, **kwargs)
    # Stock adbd predates shell-v2: adb itself can exit 0 for a failed command.
    index = command.index('shell')
    if len(command[index + 1:]) != 1:
        raise ValueError('Deployment shell command must be a single string')
    marker = '__CITATION_DEPLOY_RC__='
    command[index + 1] = ('( ' + command[index + 1] + ' ); rc=$?; '
                          'printf "\\n' + marker + '%s\\n" "$rc"')
    kwargs['capture_output'] = True
    result = subprocess.run(command, *args[1:], **kwargs)
    output = result.stdout
    token = marker if isinstance(output, str) else marker.encode('ascii')
    body, found, tail = output.rpartition(token)
    try:
        remote_code = int(tail.strip()) if found else -1
    except ValueError:
        remote_code = -1
    if remote_code != 0:
        raise subprocess.CalledProcessError(remote_code, command)
    result.stdout = body.rstrip()
    return result


def create_install_backup(adb_serial):
    """Archive files that the installer will replace before copying anything."""
    script = ROOT / 'runtime' / 'backup_before_install.sh'
    if not script.is_file():
        log("Не найден скрипт резервного копирования.", 'ERR')
        return False

    backup_id = (
        datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ')
        + f'-{os.getpid()}'
    )
    remote_script = f'/data/.citationlive-backup-{backup_id}.sh'
    try:
        deploy_run([str(ADB), '-s', adb_serial, 'push', str(script), remote_script],
                   capture_output=True)
        command = (
            f'chmod 700 {remote_script} && '
            f'/bin/sh {remote_script} {backup_id}; rc=$?; '
            f'rm -f {remote_script}; exit $rc'
        )
        result = deploy_run([str(ADB), '-s', adb_serial, 'shell', command],
                            capture_output=True, text=True, errors='replace')
        backup_path = result.stdout.strip().splitlines()[-1]
        log(f"Резервная копия создана: {backup_path}", 'OK')
        return True
    except (subprocess.SubprocessError, OSError, IndexError):
        log("Не удалось создать резервную копию. Установка отменена.", 'ERR')
        return False


def step4_install_gemini_assistant(adb_serial, api_key=None, model="models/gemini-3.8-live", force=False):
    """Step 4: Deploy Gemini voice assistant to /data over ADB."""
    print("\n" + "="*67)
    log("УСТАНОВКА АССИСТЕНТА GEMINI", 'STEP')
    print("="*67)

    if not check_prerequisites():
        return False
    if not verify_target_compatibility(adb_serial, force=force):
        return False

    bin_voice = ensure_assistant_binary()
    if not bin_voice:
        log("Не найден бинарный файл dist/citationvoice.", 'ERR')
        return False

    if not api_key:
        cfg_key_file = CONFIG_DIR / 'gemini.key'
        if cfg_key_file.is_file():
            api_key = cfg_key_file.read_text(encoding='utf-8').strip()
        if not api_key:
            p_k = subprocess.run([str(ADB), '-s', adb_serial, 'shell', 'test -f /data/opt/citation-gemini/gemini.key && cat /data/opt/citation-gemini/gemini.key || true'],
                                 capture_output=True, text=True, errors='replace')
            k_existing = p_k.stdout.strip()
            if k_existing and len(k_existing) > 20:
                log("Найден существующий API-ключ Gemini на колонке.", 'OK')
                api_key = k_existing
        if not api_key:
            api_key = prompt_api_key()
        if not api_key:
            log("API-ключ не предоставлен. Установка отменена.", 'ERR')
            return False

    print("\n" + "="*67)
    print(" НАСТРОЙКА ГОРОДА ДЛЯ ПОГОДЫ И ТОЧНОГО ВРЕМЕНИ")
    print("="*67)
    print(ui("Город нужен для погоды и точного времени.",
             "A city is needed for weather and local time."))
    city_input = input("Введите ваш город: ").strip()
    while not city_input:
        city_input = input("Введите ваш город: ").strip()
    log(f"Установлен домашний город: {city_input}", 'OK')

    log("Создание резервной копии существующей установки...", 'INFO')
    if not create_install_backup(adb_serial):
        return False

    log("Создание структуры каталогов /data/opt/citation-gemini...", 'INFO')
    deploy_run([str(ADB), '-s', adb_serial, 'shell',
                    'mkdir -p /data/opt/citation-gemini/bin /data/opt/citation-gemini/state /data/opt/citation-gemini/cues /data/opt/citation-gemini/led /data/home/chrome'],
                   capture_output=True)

    log("Копирование исполняемых файлов на колонку...", 'INFO')
    # citation_net.py installs a small /bin/sh wrapper at bin/citationvoice
    # (DNS/proxy) that execs citationvoice.real. Keep the wrapper and update the
    # real binary behind it.
    wrapper_probe = subprocess.run(
        [str(ADB), '-s', adb_serial, 'shell', 'head -c 2 /data/opt/citation-gemini/bin/citationvoice 2>/dev/null'],
        capture_output=True, text=True, errors='replace', timeout=15)
    target_name = 'citationvoice.real' if wrapper_probe.stdout.startswith('#!') else 'citationvoice'
    target = f'/data/opt/citation-gemini/bin/{target_name}'
    deploy_run([str(ADB), '-s', adb_serial, 'push', str(bin_voice), target + '.new'], capture_output=True)
    deploy_run([str(ADB), '-s', adb_serial, 'shell', f'chmod 755 {target}.new && mv -f {target}.new {target}'], capture_output=True)
    deploy_run([str(ADB), '-s', adb_serial, 'shell', f'chmod 755 {target}'], capture_output=True)
    if target_name == 'citationvoice.real':
        deploy_run([str(ADB), '-s', adb_serial, 'shell', 'chmod 755 /data/opt/citation-gemini/bin/citationvoice'], capture_output=True)

    cap_bin = DIST_DIR / 'iot_audio_capture48k'
    if cap_bin.is_file():
        deploy_run([str(ADB), '-s', adb_serial, 'push', str(cap_bin), '/data/opt/citation-gemini/bin/iot_audio_capture48k'], capture_output=True)
        deploy_run([str(ADB), '-s', adb_serial, 'shell', 'chmod 755 /data/opt/citation-gemini/bin/iot_audio_capture48k'], capture_output=True)

    hot_so = DIST_DIR / 'libassistant.hotword-observe-v3.so'
    if hot_so.is_file():
        deploy_run([str(ADB), '-s', adb_serial, 'push', str(hot_so), '/data/opt/citation-gemini/bin/libassistant.hotword-observe-v3.so'], capture_output=True)

    runtime_dir = ROOT / 'runtime'
    for sh in ['supervise.sh', 'ensure_native_capture.sh', 'ensure_early_hotword.sh', 'ensure_spotify_connect.sh', 'ensure_cast_spotify_filter.sh']:
        sh_file = runtime_dir / sh
        if sh_file.is_file():
            deploy_run([str(ADB), '-s', adb_serial, 'push', str(sh_file), f'/data/opt/citation-gemini/bin/{sh}'], capture_output=True)
            deploy_run([str(ADB), '-s', adb_serial, 'shell', f'chmod 755 /data/opt/citation-gemini/bin/{sh}'], capture_output=True)
    for asset in ['cast-apps-without-spotify.json',
                  'spotify_home_receiver.html',
                  'cast_receiver_framework.js']:
        asset_file = runtime_dir / asset
        if asset_file.is_file():
            deploy_run([str(ADB), '-s', adb_serial, 'push', str(asset_file), f'/data/opt/citation-gemini/bin/{asset}'], capture_output=True)
            deploy_run([str(ADB), '-s', adb_serial, 'shell', f'chmod 644 /data/opt/citation-gemini/bin/{asset}'], capture_output=True)

    sc_dir = ROOT / 'spotify-connect'
    if sc_dir.is_dir():
        remote = '/data/opt/citation-gemini/spotify-connect'
        deploy_run([str(ADB), '-s', adb_serial, 'shell', f'mkdir -p {remote} && chmod 700 {remote}'], capture_output=True)
        for name in ('client.tar.gz', 'config.yml'):
            deploy_run([str(ADB), '-s', adb_serial, 'push', str(sc_dir / name), f'{remote}/{name}'], capture_output=True)
        log("Нативный сервис Spotify Connect (go-librespot) установлен.", 'OK')

    hook_file = runtime_dir / 'alsa_looper.sh'
    if hook_file.is_file():
        deploy_run([str(ADB), '-s', adb_serial, 'push', str(hook_file), '/data/alsa_looper.sh'], capture_output=True)
        deploy_run([str(ADB), '-s', adb_serial, 'shell', 'chmod 755 /data/alsa_looper.sh'], capture_output=True)
        log("Хук автозапуска /data/alsa_looper.sh установлен.", 'OK')

    cues_dir = ROOT / 'cues'
    if cues_dir.is_dir():
        for cue_f in cues_dir.glob('*.wav'):
            deploy_run([str(ADB), '-s', adb_serial, 'push', str(cue_f), f'/data/opt/citation-gemini/cues/{cue_f.name}'], capture_output=True)
            deploy_run([str(ADB), '-s', adb_serial, 'shell', f'chmod 644 /data/opt/citation-gemini/cues/{cue_f.name}'], capture_output=True)
        log("Звук запуска загружен в /data/opt/citation-gemini/cues/.", 'OK')

    led_dir = ROOT / 'led'
    if led_dir.is_dir():
        for led_f in led_dir.glob('*.bin'):
            deploy_run([str(ADB), '-s', adb_serial, 'push', str(led_f), f'/data/opt/citation-gemini/led/{led_f.name}'], capture_output=True)
        deploy_run([str(ADB), '-s', adb_serial, 'shell',
                        'cd /data/opt/citation-gemini/led && ln -sf listening.bin listening && ln -sf thinking.bin thinking && ln -sf speaking.bin speaking && chmod 755 /data/opt/citation-gemini/led && chmod 644 /data/opt/citation-gemini/led/*'],
                       capture_output=True)
        log("Светодиодные анимации Gemini (listening, thinking, speaking) установлены.", 'OK')

    cfg_template = ROOT / 'config.example.json'
    if cfg_template.is_file():
        cfg_data = json.loads(cfg_template.read_text(encoding='utf-8'))
    else:
        cfg_data = {"model": model, "playback": "native_cast_mixer", "auto_wake": True}
    cfg_data['model'] = model
    cfg_data['key_file'] = '/data/opt/citation-gemini/gemini.key'

    import tempfile
    with tempfile.NamedTemporaryFile(mode='w', encoding='utf-8', delete=False) as tf:
        json.dump(cfg_data, tf, indent=2)
        cfg_temp = tf.name
    try:
        deploy_run([str(ADB), '-s', adb_serial, 'push', cfg_temp, '/data/opt/citation-gemini/config.json'], capture_output=True)
        deploy_run([str(ADB), '-s', adb_serial, 'shell', 'chmod 644 /data/opt/citation-gemini/config.json'], capture_output=True)
    finally:
        os.unlink(cfg_temp)

    clean_key_bytes = api_key.strip().encode('utf-8')
    with tempfile.NamedTemporaryFile(mode='wb', delete=False) as tf:
        tf.write(clean_key_bytes)
        key_temp = tf.name
    try:
        deploy_run([str(ADB), '-s', adb_serial, 'push', key_temp, '/data/opt/citation-gemini/gemini.key'], capture_output=True)
        deploy_run([str(ADB), '-s', adb_serial, 'shell', 'chmod 600 /data/opt/citation-gemini/gemini.key; chown 1000:3003 /data/opt/citation-gemini/gemini.key /data/opt/citation-gemini/config.json 2>/dev/null || true'], capture_output=True)
    finally:
        os.unlink(key_temp)

    pref_data = {"city": city_input, "timezone": "auto"}
    with tempfile.NamedTemporaryFile(mode='w', encoding='utf-8', delete=False) as tf:
        json.dump(pref_data, tf, ensure_ascii=False, indent=2)
        pref_temp = tf.name
    try:
        deploy_run([str(ADB), '-s', adb_serial, 'push', pref_temp, '/data/opt/citation-gemini/state/preferences.json'], capture_output=True)
        deploy_run([str(ADB), '-s', adb_serial, 'shell', 'chmod 644 /data/opt/citation-gemini/state/preferences.json; chown 1000:3003 /data/opt/citation-gemini/state/preferences.json 2>/dev/null || true'], capture_output=True)
    finally:
        os.unlink(pref_temp)

    # Bind-mount early hotword hook immediately
    deploy_run([str(ADB), '-s', adb_serial, 'shell',
                    'test -f /data/opt/citation-gemini/bin/ensure_early_hotword.sh && /bin/sh /data/opt/citation-gemini/bin/ensure_early_hotword.sh || true'],
                   capture_output=True)

    log("Файлы ассистента успешно установлены в /data/opt/citation-gemini!", 'OK')

    log("Перезагрузка колонки для активации службы...", 'INFO')
    deploy_run([str(ADB), '-s', adb_serial, 'reboot'], capture_output=True)

    target_host = adb_serial.split(':')[0] if ':' in adb_serial else adb_serial

    def check_online_again():
        try:
            subprocess.run([str(ADB), 'connect', f'{target_host}:5555'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=3)
            p = subprocess.run([str(ADB), '-s', adb_serial, 'shell', 'curl -s http://127.0.0.1:8766/status; grep -q mixer /proc/net/unix && echo MIXER_OK || true; curl -s -o /dev/null -w RECEIVER=%{http_code} http://127.0.0.1:18768/receiver'],
                               capture_output=True, text=True, timeout=3, errors='replace')
            if 'capture_health' in p.stdout and '"running":true' in p.stdout and 'MIXER_OK' in p.stdout and 'RECEIVER=200' in p.stdout:
                return True
        except Exception:
            pass
        return None

    log("Ожидание перезапуска службы ассистента и аудиосистемы (порт 8766)...", 'INFO')
    ready = spinner_wait("Проверка запуска citationvoice, микрофона и микшера...", check_online_again, timeout=90, interval=2.0)

    if ready:
        log("Служба citationvoice активна! Микрофонный поток и аудиосистема в норме.", 'OK')
        log("Cast-приёмник CitationLive доступен: Google Home получает название трека, исполнителя и обложку.", 'OK')
        configure_dns(target_host)
        log("Проверка готовности облачного шлюза Gemini Live...", 'INFO')
        t_res = subprocess.run([str(ADB), '-s', adb_serial, 'shell',
                                '/data/opt/citation-gemini/bin/citationvoice check'],
                               capture_output=True, text=True, timeout=15, errors='replace')
        try:
            live_ready = json.loads(t_res.stdout).get('live_setup') is True
        except (ValueError, AttributeError):
            live_ready = False
        if t_res.returncode == 0 and live_ready:
            log("Соединение с Gemini Live установлено и проверено!", 'OK')
        else:
            log(ui('Файлы установлены, но Gemini Live не подтвердил готовность. Проверьте ключ и DNS/прокси.',
                   'Files are installed, but Gemini Live readiness was not confirmed. Check the key and DNS/proxy.'), 'ERR')
            return False
    else:
        log("Служба не подтвердила готовность за 90 секунд. Проверьте service.log и Cast-приёмник (порт 18768).", 'ERR')
        return False
    return True

def setup_spotify(adb_serial):
    """Pair the Spotify player using a one-time code."""
    log(f"ADB: tcp:28766 -> tcp:8766 ({adb_serial})", 'INFO')
    subprocess.run([str(ADB), '-s', adb_serial, 'forward', 'tcp:28766', 'tcp:8766'],
                   capture_output=True, check=True, timeout=15)
    print(ui('Откройте настройки Spotify. Если указан код привязки, подтвердите его в том же аккаунте.',
             'Open Spotify settings. If a pairing code is displayed, confirm it in the same account.'))
    open_page('http://127.0.0.1:28766/spotify/setup')

    def check_spotify_connected():
        p = subprocess.run([str(ADB), '-s', adb_serial, 'shell',
                            'curl -fsS --max-time 3 http://127.0.0.1:8766/spotify/status'],
                           capture_output=True, text=True, errors='replace', timeout=5)
        try:
            status = json.loads(p.stdout)
        except ValueError:
            return None
        return status.get('connected') is True and status.get('receiver_ready') is True

    ready = spinner_wait("Ожидание подтверждения привязки Spotify...", check_spotify_connected, timeout=120, interval=2.0)
    if ready:
        log("Spotify успешно подключён и авторизован на колонке!", 'OK')
    else:
        log("Привязка не завершена за 2 минуты. Вы можете повторить попытку позже через меню [5].", 'WARN')


def guide_google_client(callback):
    steps = [
        (ui('Создайте проект с именем CitationLive. Выберите его в верхней панели Google Cloud.',
            'Create a project named CitationLive and select it in the Google Cloud header.'),
         'https://console.cloud.google.com/projectcreate'),
        (ui('Проверьте выбранный проект. Нажмите Enable / Включить для Google Calendar API.',
            'Check the selected project. Click Enable for Google Calendar API.'),
         'https://console.cloud.google.com/apis/library/calendar-json.googleapis.com'),
        (ui('В том же проекте нажмите Enable / Включить для Google Tasks API.',
            'In the same project, click Enable for Google Tasks API.'),
         'https://console.cloud.google.com/apis/library/tasks.googleapis.com'),
        (ui('Нажмите Get started / Начать. Имя приложения — CitationLive.\n'
            'Укажите свою почту для поддержки и связи. Для личного аккаунта выберите External / Внешние.',
            'Click Get started. Use CitationLive as the app name.\n'
            'Enter your support and contact email. For a personal account, choose External.'),
         'https://console.cloud.google.com/auth/branding'),
        (ui('На странице Audience / Аудитория найдите Test users / Тестовые пользователи.\n'
            'Нажмите Add users и добавьте адрес аккаунта, чей календарь хотите подключить.',
            'In Audience, find Test users and click Add users.\n'
            'Add the Google account whose calendar you want to connect.'),
         'https://console.cloud.google.com/auth/audience'),
        (ui('Откройте Data Access / Доступ к данным → Add or remove scopes.\n'
            'В Manually add scopes добавьте обе строки и нажмите Add to table → Update → Save.',
            'Open Data Access → Add or remove scopes.\n'
            'Under Manually add scopes, enter both lines and click Add to table → Update → Save.')
         + '\nhttps://www.googleapis.com/auth/calendar.events'
           '\nhttps://www.googleapis.com/auth/tasks',
         'https://console.cloud.google.com/auth/scopes'),
        (ui('Нажмите Create client / Создать клиент. Тип — Web application / Веб-приложение.\n'
            'В разделе Authorized redirect URIs / Разрешённые URI перенаправления нажмите Add URI.\n'
            f'Вставьте ровно этот адрес:\n{callback}\n'
            'Нажмите Create и сразу скачайте JSON (Download JSON). Оставьте файл в Загрузках.',
            'Click Create client. Select Web application.\n'
            'Under Authorized redirect URIs, click Add URI and paste this exact address:\n'
            f'{callback}\n'
            'Click Create and download the JSON immediately. Leave the file in Downloads.'),
         'https://console.cloud.google.com/auth/clients'),
    ]
    for number, (message, url) in enumerate(steps, 1):
        print(f'\n[{number}/{len(steps)}] {message}')
        answer = input(ui('Enter — открыть страницу, n — отменить: ',
                          'Enter to open this page, n to cancel: ')).strip().lower()
        if answer in ('n', 'н'):
            return False
        open_page(url)
        if input(ui('Выполните шаг в браузере. Enter — дальше, n — отменить: ',
                    'Finish this step in the browser. Enter to continue, n to cancel: ')).strip().lower() in ('n', 'н'):
            return False
    return True


def setup_google_calendar(adb_serial):
    """Guide the owner through creating or loading a Google OAuth web client."""
    from scripts.connect_google import CALLBACK, load_client

    print(ui('\nGoogle Календарь и Задачи\n'
             'Для входа нужен JSON клиента OAuth из Google Cloud.\n'
             ' [1] Провести меня по настройке Google Cloud\n'
             ' [2] JSON уже скачан\n [0] Пропустить',
             '\nGoogle Calendar and Tasks\n'
             'Sign-in requires an OAuth client JSON from Google Cloud.\n'
             ' [1] Guide me through Google Cloud setup\n'
             ' [2] I already downloaded the JSON\n [0] Skip'))
    while True:
        choice = input(ui('Выберите 1, 2 или 0: ', 'Choose 1, 2 or 0: ')).strip()
        if choice in ('1', '2', '0', ''):
            break
    if choice in ('0', '') or (choice == '1' and not guide_google_client(CALLBACK)):
        return

    print(ui('Перетащите скачанный файл из Загрузок сюда или вставьте его полный путь.\n'
             'Имя файла может начинаться с client_secret_. Храните его вне папки CitationLive.',
             'Drag the downloaded file from Downloads here or paste its full path.\n'
             'The name may start with client_secret_. Keep the file outside the CitationLive folder.'))
    while True:
        json_path = input(ui('Путь к JSON (Enter — пропустить): ',
                             'JSON path (Enter to skip): ')).strip().strip('"')
        if not json_path:
            return
        p = Path(json_path).expanduser()
        try:
            load_client(p)
        except (OSError, ValueError) as exc:
            messages = {
                'Use your own Google OAuth web-client JSON': ui(
                    'Скачайте JSON клиента типа Web application со страницы Clients.',
                    'Download a Web application client JSON from the Clients page.'),
                'Register the localhost callback before continuing': ui(
                    f'В клиенте Google добавьте URI {CALLBACK} и скачайте актуальный JSON.',
                    f'Add {CALLBACK} to the Google client and use the updated JSON.'),
                'Private client file must be outside repository': ui(
                    'Переместите файл за пределы папки релиза, например в Загрузки.',
                    'Move the file outside the release folder, for example to Downloads.'),
            }
            print(messages.get(str(exc), ui(
                'Не удалось прочитать OAuth JSON. Проверьте путь и выберите скачанный файл.',
                'Could not read OAuth JSON. Check the path and select the downloaded file.')))
            continue
        break

    log(f"Проброс порта ADB tcp:28766 -> tcp:8766 на {adb_serial}...", 'INFO')
    subprocess.run([str(ADB), '-s', adb_serial, 'forward', 'tcp:28766', 'tcp:8766'], capture_output=True, check=True, timeout=15)

    cmd = [PYTHON_EXE, '-B', str(SCRIPTS_DIR / 'connect_google.py'), '--client-json', str(p.resolve())]
    log("Запуск скрипта авторизации Google OAuth...", 'INFO')
    res = subprocess.run(cmd)
    if res.returncode == 0:
        print(ui('Выберите в браузере аккаунт, который добавили в Test users, и подтвердите доступ.\n'
                 'Дождитесь страницы об успешном подключении и вернитесь сюда.',
                 'In the browser, select the account you added under Test users and approve access.\n'
                 'Wait for the success page, then return here.'))
        input(ui('Enter — продолжить: ', 'Enter to continue: '))
    else:
        log("Ошибка при подключении Google OAuth.", 'ERR')


NEWS_AUTH_COOKIES = ('SID', 'SSID', 'HSID', 'SAPISID', 'APISID',
                     '__Secure-1PSID', '__Secure-3PSID')


def normalize_news_cookie(value):
    """Turn a pasted DevTools cookie header into the service's cookie string."""
    value = (value or '').strip()
    if value.lower().startswith('cookie:'):
        value = value.split(':', 1)[1].strip()
    # A copied Request Headers block can span several lines.
    value = '; '.join(line.strip() for line in value.splitlines() if line.strip())
    pairs = []
    for part in value.split(';'):
        part = part.strip()
        if not part:
            continue
        name, separator, cookie_value = part.partition('=')
        name, cookie_value = name.strip(), cookie_value.strip()
        if separator and name and cookie_value:
            pairs.append(f'{name}={cookie_value}')
    return '; '.join(pairs)


def news_cookie_names(cookie):
    return {part.split('=', 1)[0].strip() for part in cookie.split(';') if '=' in part}


def validate_news_cookie(value):
    """Return (normalized_cookie, problem). The cookie value is never printed."""
    cookie = normalize_news_cookie(value)
    if not cookie:
        return '', ui(
            'Строка пустая или в ней нет пар Name=Value. Скопируйте всё значение cookie из DevTools.',
            'The string is empty or has no Name=Value pairs. Copy the whole cookie value from DevTools.')
    names = news_cookie_names(cookie)
    if len(names) < 2:
        return cookie, ui(
            'Похоже, скопировано только одно значение. Нужна вся строка cookie с несколькими парами Name=Value через «; ».',
            'It looks like only one value was copied. Copy the whole cookie string with several Name=Value pairs separated by “; ”.')
    if not any(name in names for name in NEWS_AUTH_COOKIES):
        return cookie, ui(
            'В строке нет SID, HSID, SSID (или __Secure-…PSID). Без них Google считает запрос гостевым.\n'
            'Скопируйте всю строку cookie из DevTools → Network → Request Headers, а не отдельные значения.',
            'The string has no SID, HSID, SSID (or __Secure-…PSID). Without them Google treats the request as a guest.\n'
            'Copy the whole cookie string from DevTools → Network → Request Headers, not individual values.')
    return cookie, ''


def news_cookie_warning(cookie):
    if 'NID' not in news_cookie_names(cookie):
        return ui(
            'В строке нет NID. Обычно Google добавляет его; если лента не откроется, скопируйте cookies ещё раз полностью.',
            'The string has no NID. Google usually adds it; if the feed does not load, copy all cookies again.')
    return ''


def news_service(adb_serial, path, data=None, timeout=30):
    """Call the local speaker service through an ADB forward."""
    import urllib.request
    from scripts.connect_google import BASE, NoRedirect

    subprocess.run([str(ADB), '-s', adb_serial, 'forward', 'tcp:28766', 'tcp:8766'],
                   capture_output=True, check=True, timeout=15)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    body = json.dumps(data).encode('utf-8') if data is not None else None
    headers = {'Content-Type': 'application/json'} if body is not None else {}
    request = urllib.request.Request(BASE + path, data=body, headers=headers)
    with opener.open(request, timeout=timeout) as response:
        return json.load(response)


def news_status(adb_serial):
    """Print the saved-session state and return True when Google accepts it."""
    try:
        status = news_service(adb_serial, '/google/news-cookies/status', timeout=15)
    except (subprocess.SubprocessError, OSError, ValueError):
        print(ui('Не удалось связаться со службой на колонке. Проверьте ADB-подключение и повторите пункт [7].',
                 'Could not reach the speaker service. Check the ADB connection and retry option [7].'))
        return False
    if not isinstance(status, dict):
        print(ui('Колонка вернула неожиданный ответ. Повторите пункт [7].',
                 'The speaker returned an unexpected response. Retry option [7].'))
        return False
    names = status.get('names') or []
    if not status.get('configured'):
        print(ui('Сейчас сессия Google News не подключена. Выберите [2] для входа через браузер\n'
                 'или [3], чтобы вставить cookie вручную.',
                 'No Google News session is connected. Choose [2] for browser sign-in\n'
                 'or [3] to paste a cookie manually.'))
        return False
    print(ui(f'На колонке сохранено cookie-пар: {len(names)}.',
             f'Cookie pairs saved on the speaker: {len(names)}.'))
    print(ui('Имена cookie: ' + ', '.join(sorted(names)),
             'Cookie names: ' + ', '.join(sorted(names))))
    try:
        check = news_service(adb_serial, '/google/news-cookies/check', timeout=20)
    except (subprocess.SubprocessError, OSError, ValueError):
        print(ui('Не удалось проверить сессию через Google. Повторите позже.',
                 'Could not verify the session with Google. Retry later.'))
        return False
    if not isinstance(check, dict) or not check.get('valid'):
        print(ui('Google не подтверждает вход: сессия истекла или скопирована не полностью.\n'
                 'Выберите [2] для повторного входа через браузер или [3] для новой строки cookie.',
                 'Google does not confirm the sign-in: the session expired or was copied incompletely.\n'
                 'Choose [2] to sign in through the browser again or [3] for a fresh cookie string.'))
        return False
    print(ui('Google подтверждает вход: сессия действительна.',
             'Google confirms the sign-in: the session is valid.'))
    return True


def news_headlines(adb_serial, limit=3):
    """Load a few personalized headlines as proof that the feed works."""
    try:
        feed = news_service(adb_serial, f'/news/google-home?cookies=1&limit={limit}', timeout=30)
    except (subprocess.SubprocessError, OSError, ValueError):
        print(ui('Не удалось загрузить ленту с колонки. Проверьте подключение и повторите пункт [7].',
                 'Could not load the feed from the speaker. Check the connection and retry option [7].'))
        return False
    if not isinstance(feed, dict):
        print(ui('Колонка вернула неожиданный ответ при загрузке ленты.',
                 'The speaker returned an unexpected feed response.'))
        return False
    items = feed.get('items')
    if not (feed.get('ok') is True and feed.get('personal') is True
            and feed.get('source') == 'google_news_home' and items):
        print(ui('Колонка не получила заголовки из аккаунта. Причина: ' +
                 str(feed.get('error') or 'лента недоступна'),
                 'The speaker did not load account headlines. Reason: ' +
                 str(feed.get('error') or 'feed unavailable')))
        return False
    print(ui('Колонка получила ваши заголовки:', 'The speaker loaded your headlines:'))
    for number, item in enumerate(items[:limit], 1):
        title = item.get('title') or ui('без названия', 'untitled')
        source = item.get('source') or ''
        print(f'  {number}. {title}' + (f' — {source}' if source else ''))
    return True


def print_news_guide():
    print(ui(
        '\nКак получить строку cookie (2–3 минуты)\n'
        ' 1. Откройте https://news.google.com/ и войдите в тот Google-аккаунт,\n'
        '    чью ленту хотите слушать.\n'
        ' 2. Убедитесь, что открыта вкладка «Для вас» и видны ваши темы.\n'
        ' 3. Нажмите F12 (или Ctrl+Shift+I) и выберите вкладку Network (Сеть).\n'
        ' 4. Обновите страницу (F5). В списке выберите первый запрос к news.google.com.\n'
        ' 5. В блоке Headers (Заголовки) → Request Headers найдите строку «cookie:».\n'
        ' 6. Скопируйте ВСЁ значение после «cookie:» — длинную строку вида\n'
        '    SID=...; HSID=...; SSID=...; ...\n'
        '    В Chrome и Edge удобно нажать правой кнопкой на значение → Copy value.\n'
        '    Не копируйте только одно значение: нужна вся строка с разделителями «; ».\n'
        ' 7. Вернитесь сюда и вставьте строку целиком (она может быть очень длинной).\n'
        '\nЕсли строки cookie не видно\n'
        ' • Откройте Application (Приложение) → Cookies → https://news.google.com\n'
        '   и скопируйте все пары Name=Value через «; ».\n'
        ' • В строке обязательно должны быть SID, HSID или SSID (или __Secure-…PSID):\n'
        '   без них Google считает запрос гостевым.\n'
        ' • Не используйте режим инкогнито: в нём cookie не сохраняются.\n'
        '\nЕсли Google не подтвердил вход\n'
        ' • Строка скопирована не полностью → повторите шаг 6.\n'
        ' • Сессия истекла или вы сменили пароль → войдите заново и скопируйте строку.\n'
        ' • Включена двухфакторная защита → это нормально, cookie всё равно работает;\n'
        '   просто скопируйте строку после входа.\n'
        ' • Корпоративный аккаунт может запрещать экспорт сессии — тогда останется\n'
        '   публичная лента.\n'
        '\nБезопасность\n'
        ' Строка хранится только на колонке в файле с правами 0600 и не отправляется\n'
        ' на наши серверы. Не передавайте её другим людям. Удалить сессию можно\n'
        ' пунктом [4] — колонка вернётся к публичной ленте.',
        '\nHow to get the cookie string (2–3 minutes)\n'
        ' 1. Open https://news.google.com/ and sign in to the Google account whose feed you want.\n'
        ' 2. Make sure the “For You” tab is open and your topics are visible.\n'
        ' 3. Press F12 (or Ctrl+Shift+I) and open the Network tab.\n'
        ' 4. Reload the page (F5). Select the first request to news.google.com.\n'
        ' 5. Under Headers → Request Headers find the “cookie:” line.\n'
        ' 6. Copy the WHOLE value after “cookie:” — a long string like\n'
        '    SID=...; HSID=...; SSID=...; ...\n'
        '    In Chrome and Edge, right-click the value → Copy value.\n'
        '    Do not copy a single value: the whole string with “; ” separators is required.\n'
        ' 7. Come back here and paste the string (it can be very long).\n'
        '\nIf the cookie line is not visible\n'
        ' • Open Application → Cookies → https://news.google.com and copy every\n'
        '   Name=Value pair separated by “; ”.\n'
        ' • The string must contain SID, HSID or SSID (or __Secure-…PSID); without them\n'
        '   Google treats the request as a guest.\n'
        ' • Do not use Incognito mode: cookies are not saved there.\n'
        '\nIf Google did not confirm the sign-in\n'
        ' • The string was copied incompletely → repeat step 6.\n'
        ' • The session expired or you changed your password → sign in again and copy the string.\n'
        ' • Two-factor authentication is fine: the cookie still works; just copy the string after signing in.\n'
        ' • A corporate account may block session export; in that case the public feed remains.\n'
        '\nSecurity\n'
        ' The string is stored only on the speaker in a 0600 file and is not sent to our servers.\n'
        ' Do not share it. Remove it with option [4] to return to the public feed.'))


def browser_auth_ready():
    return subprocess.run([PYTHON_EXE, '-c', 'import playwright'], capture_output=True).returncode == 0


def ensure_browser_auth():
    """Make sure the optional Playwright helper is available."""
    if browser_auth_ready():
        return True
    print(ui('Для входа через браузер нужен Python-пакет Playwright.\n'
             'Он откроет обычный Chrome/Edge, вы войдёте в аккаунт, а cookies попадут\n'
             'только на колонку. Установить Playwright сейчас? [Y/n]: ',
             'Browser sign-in needs the Playwright Python package.\n'
             'It opens your normal Chrome/Edge, you sign in, and cookies go only to the\n'
             'speaker. Install Playwright now? [Y/n]: '), end='')
    if input().strip().lower() in ('n', 'н'):
        return False
    log('Устанавливаю Playwright (pip)...', 'INFO')
    try:
        p = subprocess.run([PYTHON_EXE, '-m', 'pip', 'install', '--upgrade', 'playwright'],
                           capture_output=True, text=True, timeout=600)
    except (subprocess.SubprocessError, OSError) as e:
        log('Не удалось установить Playwright: ' + str(e), 'ERR')
        return False
    if p.returncode != 0:
        log('Не удалось установить Playwright: ' + (p.stderr or p.stdout)[-300:], 'ERR')
        return False
    return browser_auth_ready()


def run_browser_auth(adb_serial, flags):
    """Run the local browser helper through the existing ADB forward."""
    if not ensure_browser_auth():
        return False
    script = ROOT / 'browser_auth.py'
    if not script.is_file():
        log('browser_auth.py не найден в релизе.', 'ERR')
        return False
    try:
        subprocess.run([str(ADB), '-s', adb_serial, 'forward', 'tcp:28766', 'tcp:8766'],
                       capture_output=True, timeout=15)
    except (subprocess.SubprocessError, OSError) as e:
        log('Не удалось пробросить порт для браузера: ' + str(e), 'ERR')
        return False
    cmd = [PYTHON_EXE, '-B', str(script), '--base', 'http://127.0.0.1:28766',
           '--lang', LANG, *flags]
    return subprocess.run(cmd).returncode == 0


def setup_browser_auth(adb_serial):
    """Open the browser sign-in for the Google News feed."""
    if not run_browser_auth(adb_serial, ['--news']):
        print(ui('Вход не завершён. Повторите попытку или используйте ручной импорт cookie.',
                 'Sign-in did not finish. Retry or use the manual cookie import.'))


def setup_google_news(adb_serial):
    """Connect the owner's Google News session and prove that the feed works."""
    print(ui('\nGoogle News — персональная лента «Для вас»\n'
             'Без входа колонка читает только публичные новости; после входа — вашу ленту «Для вас».',
             '\nGoogle News — your personal “For You” feed\n'
             'Without sign-in the speaker reads public news only; after sign-in it reads your “For You” feed.'))
    while True:
        print(ui(' [1] Проверить и показать заголовки\n'
                 ' [2] Войти через браузер (рекомендуется)\n'
                 ' [3] Вставить строку cookie вручную (для продвинутых)\n'
                 ' [4] Удалить сохранённую сессию\n'
                 ' [0] Назад',
                 ' [1] Check and show headlines\n'
                 ' [2] Sign in through the browser (recommended)\n'
                 ' [3] Paste the cookie string manually (advanced)\n'
                 ' [4] Remove the saved session\n'
                 ' [0] Back'))
        choice = input(ui('Выберите 0, 1, 2, 3 или 4: ', 'Choose 0, 1, 2, 3 or 4: ')).strip()
        if choice in ('0', ''):
            return
        if choice == '1':
            if news_status(adb_serial):
                news_headlines(adb_serial)
            input(ui('Enter — вернуться в меню Google News: ',
                     'Enter to return to the Google News menu: '))
            continue
        if choice == '2':
            if run_browser_auth(adb_serial, ['--news']):
                news_headlines(adb_serial)
            else:
                print(ui('Вход через браузер не завершён. Можно повторить [2] или вставить cookie вручную [3].',
                         'Browser sign-in did not finish. Retry [2] or paste the cookie manually [3].'))
            input(ui('Enter — вернуться в меню Google News: ',
                     'Enter to return to the Google News menu: '))
            continue
        if choice == '4':
            try:
                cleared = news_service(adb_serial, '/google/news-cookies/clear', {}, timeout=15)
            except (subprocess.SubprocessError, OSError, ValueError):
                print(ui('Не удалось удалить сессию: нет связи со службой на колонке.',
                         'Could not remove the session: the speaker service is unreachable.'))
                continue
            if isinstance(cleared, dict) and cleared.get('ok'):
                print(ui('Сохранённая сессия Google News удалена. Колонка снова использует публичную ленту.',
                         'The saved Google News session was removed. The speaker uses the public feed again.'))
            else:
                print(ui('Колонка не подтвердила удаление сессии.',
                         'The speaker did not confirm the session removal.'))
            continue
        if choice != '3':
            print(ui('Введите 0, 1, 2, 3 или 4.', 'Enter 0, 1, 2, 3 or 4.'))
            continue

        print_news_guide()
        if input(ui('Enter — открыть Google News, n — назад: ',
                    'Enter to open Google News, n to go back: ')).strip():
            return
        print(ui('Сейчас откроется Google News. Войдите в нужный аккаунт, откройте вкладку «Для вас»,\n'
                 'затем вернитесь в это окно и вставьте строку cookie.',
                 'Google News will open. Sign in to the right account, open the “For You” tab,\n'
                 'then return to this window and paste the cookie string.'))
        open_page('https://news.google.com/foryou')

        for attempt in range(1, 4):
            print(ui(f'\nПопытка {attempt} из 3. Вставьте строку cookie целиком.\n'
                     'Enter без строки — назад.',
                     f'\nAttempt {attempt} of 3. Paste the whole cookie string.\n'
                     'Enter with no value to go back.'))
            raw = input(ui('Строка cookie Google News: ', 'Google News cookie string: ')).strip()
            if not raw:
                return
            cookie, problem = validate_news_cookie(raw)
            if problem:
                print(problem)
                continue
            warning = news_cookie_warning(cookie)
            if warning:
                print(warning)
            try:
                saved = news_service(adb_serial, '/google/news-cookies', {'cookie': cookie}, timeout=15)
            except (subprocess.SubprocessError, OSError, ValueError):
                print(ui('Не удалось передать строку на колонку. Проверьте ADB-подключение и повторите пункт [7].',
                         'Could not send the string to the speaker. Check the ADB connection and retry option [7].'))
                return
            if not isinstance(saved, dict) or not saved.get('ok'):
                detail = saved.get('error') if isinstance(saved, dict) else ''
                print(ui('Колонка не приняла строку: ' + str(detail or 'неизвестная ошибка'),
                         'The speaker rejected the string: ' + str(detail or 'unknown error')))
                continue
            print(ui(f'Колонка сохранила {saved.get("count", 0)} cookie-пар. Проверяю вход через Google…',
                     f'The speaker saved {saved.get("count", 0)} cookie pairs. Verifying the Google sign-in…'))
            try:
                check = news_service(adb_serial, '/google/news-cookies/check', timeout=20)
            except (subprocess.SubprocessError, OSError, ValueError):
                print(ui('Строка сохранена, но проверить вход сейчас не удалось. Повторите пункт [1] позже.',
                         'The string was saved, but the sign-in could not be checked now. Retry option [1] later.'))
                return
            if not isinstance(check, dict) or not check.get('valid'):
                print(ui('Google не подтвердил вход. Скорее всего, строка скопирована не полностью\n'
                         'или сессия уже истекла. Скопируйте всю строку cookie ещё раз.',
                         'Google did not confirm the sign-in. Most likely the string was copied incompletely\n'
                         'or the session already expired. Copy the whole cookie string again.'))
                continue
            print(ui('Вход подтверждён. Проверяю, что колонка видит вашу ленту…',
                     'Sign-in confirmed. Checking that the speaker can load your feed…'))
            if news_headlines(adb_serial):
                print(ui('Готово. Скажите «Расскажи мои новости» или «Включи мои новости».',
                         'Done. Say “Read my news” or “Turn on my news”.'))
            else:
                print(ui('Вход подтверждён, но лента не загрузилась. Подождите минуту и повторите пункт [1].\n'
                         'Если ошибка сохраняется, удалите сессию пунктом [4] и подключите её заново.',
                         'Sign-in confirmed, but the feed did not load. Wait a minute and retry option [1].\n'
                         'If the error persists, remove the session with [4] and connect it again.'))
            return
        print(ui('Три попытки не удались. Проверьте, что скопирована вся строка cookie,\n'
                 'и повторите пункт [7] позже.',
                 'Three attempts failed. Check that the whole cookie string was copied,\n'
                 'and retry option [7] later.'))


def ensure_assistant_binary():
    """Ensure assistant binary is available."""
    bin_path = DIST_DIR / 'citationvoice'
    if bin_path.is_file() and bin_path.stat().st_size > 1000000:
        return bin_path
    log("Не найден бинарный файл dist/citationvoice.", 'ERR')
    return None


def run_full_automated_pipeline(target_ip=None, api_key=None, force=False):
    """Execute the complete end-to-end automated setup pipeline."""
    banner()
    if not check_prerequisites():
        sys.exit(1)

    print("\n" + "="*67)
    print(" НАЧАЛО СКВОЗНОЙ УСТАНОВКИ CITATIONLIVE")
    print("="*67)

    mode, ident = detect_mode(target_ip)
    adb_serial = None

    if mode in ['ADB_NETWORK', 'ADB_USB']:
        log(f"Обнаружено устройство с активным ADB: {ident}", 'OK')
        adb_serial = ident
    else:
        log("ADB не найден. Включите его airplay-эксплойтом:", 'ERR')
        log("  python ADB-Exploit\\citation_one_adb.py <IP>", 'INFO')
        log("или запустите install_network.py --ip <IP>.", 'INFO')
        return False

    if not step4_install_gemini_assistant(adb_serial, api_key=api_key, force=force):
        log("Ошибка при установке ассистента Gemini.", 'ERR')
        return False

    print(ui('\nGemini установлен. Теперь подключим сервисы в этом же мастере.\n'
             'Enter — подключить, n — пропустить. Пропущенные шаги доступны в меню.',
             '\nGemini is installed. Continue with services in this wizard.\n'
             'Enter to connect, n to skip. Skipped steps remain available in the menu.'))
    services = [('Spotify', setup_spotify),
                (ui('Google Календарь и Задачи', 'Google Calendar and Tasks'), setup_google_calendar),
                ('Google News', setup_google_news)]
    for number, (name, configure) in enumerate(services, 2):
        print(f'\n[{number}/4] {name}')
        while True:
            answer = input(ui('Подключить сейчас? [Y/n]: ', 'Connect now? [Y/n]: ')).strip().lower()
            if answer in ('', 'y', 'yes', 'д', 'да', 'n', 'no', 'н', 'нет'):
                break
            print(ui('Нажмите Enter для подключения или введите n для пропуска.',
                     'Press Enter to connect or type n to skip.'))
        if answer in ('n', 'no', 'н', 'нет'):
            print(ui('Пропущено — можно подключить позже.', 'Skipped — you can connect later.'))
            continue
        try:
            configure(adb_serial)
        except (subprocess.SubprocessError, OSError):
            print(ui('Этот сервис не настроен. Проверьте подключение и повторите позже через меню.',
                     'This service was not configured. Check the connection and retry from the menu later.'))
    print(ui('\nМастер завершён. Gemini установлен; результат каждого подключения указан выше.\n'
             'Пункты 5–7 — повторная настройка Spotify, Календаря и Google News; [10] — веб-авторизация.',
             '\nWizard finished. Gemini is installed; each service result is shown above.\n'
             'Use options 5–7 to retry Spotify, Calendar and Google News; [10] — web authorization.'))
    return True


def interactive_menu(target_ip=None):
    banner()
    print(ui('Первый запуск: выберите [1]. Нужны колонка в той же сети и ключ Gemini.\n'
             'Далее мастер предложит Spotify, Google и новости — любой шаг можно пропустить.\n'
             'Во время установки колонка перезагрузится. Это окно оставьте открытым.',
             'First time: choose [1]. You need the speaker on the same network and a Gemini key.\n'
             'The wizard then offers Spotify, Google and News — any step can be skipped.\n'
             'Installation reboots the speaker. Keep this window open.'))
    while True:
        print('\n' + '=' * 60)
        print(ui('КОЛОНКА: ', 'SPEAKER: ') + (target_ip or ui('не выбрана', 'not selected')))
        print(ui(' [1] Всё в одном — Gemini, Spotify, Google и новости\n'
                 ' [2] Проверить подключение к колонке\n'
                 ' [3] Подключить колонку / включить сетевой доступ\n'
                 ' [4] Переустановить CitationLive\n'
                 '     Дополнительные настройки:\n'
                 ' [5] Подключить Spotify\n [6] Подключить Google Календарь и Задачи\n'
                 ' [7] Google News — лента из аккаунта\n [8] Настроить DNS / прокси Gemini\n'
                 ' [9] Выбрать другую колонку\n [10] Веб-авторизация Google News\n [0] Выход',
                 ' [1] All in one — Gemini, Spotify, Google and News\n'
                 ' [2] Check speaker connection\n [3] Connect speaker / enable network access\n'
                 ' [4] Reinstall CitationLive\n     Optional services:\n'
                 ' [5] Connect Spotify\n [6] Connect Google Calendar and Tasks\n'
                 ' [7] Google News — account feed\n [8] Configure Gemini DNS / proxy\n'
                 ' [9] Select another speaker\n [10] Google News browser sign-in\n [0] Exit'))
        choice = input(ui('Выберите пункт [0–10]: ', 'Choose [0–10]: ')).strip()
        if choice == '0':
            return
        if not choice.isdigit() or not (0 <= int(choice) <= 10):
            print(ui('Введите номер пункта от 0 до 10.', 'Enter a number from 0 to 10.'))
            continue
        choice = str(int(choice))
        if choice == '9' or not target_ip:
            selected = ask_speaker_ip()
            if not selected:
                continue
            target_ip = selected
            if choice == '9':
                continue
        try:
            if choice == '10':
                setup_browser_auth(target_ip + ':5555')
                continue
            if choice in ('1', '3'):
                command = [PYTHON_EXE, '-B', str(ROOT.parent / 'install_network.py'),
                           '--ip', target_ip, '--lang', LANG,
                           '--all-in-one' if choice == '1' else '--connect-only']
                result = subprocess.run(command)
                if result.returncode:
                    print(ui('Шаг не завершён. Исправьте причину, указанную выше, и повторите этот пункт.',
                             'Step did not complete. Fix the issue shown above and retry this option.'))
                elif choice == '1':
                    print(ui('Мастер завершён. Повторная настройка сервисов: пункты 5–8.',
                             'Wizard complete. Configure services again with options 5–8.'))
                continue
            mode, ident = detect_mode(target_ip)
            if choice == '2':
                print(ui('Колонка доступна по сети: ', 'Speaker is reachable: ') + ident if ident else
                      ui('Нет подключения. Проверьте IP и Wi-Fi, затем выберите [3].',
                         'No connection. Check the IP and Wi-Fi, then choose [3].'))
                continue
            if not ident:
                print(ui('Колонка недоступна. Сначала выберите [3] для подключения.',
                         'Speaker is unavailable. Choose [3] to connect first.'))
                continue
            if choice == '4':
                answer = input(ui('Переустановка заменит настройки Gemini и города. Продолжить? [y/N]: ',
                                  'Reinstall replaces Gemini and city settings. Continue? [y/N]: ')).strip().lower()
                if answer in ('y', 'yes', 'д', 'да'):
                    step4_install_gemini_assistant(ident)
            elif choice == '5':
                setup_spotify(ident)
            elif choice == '6':
                setup_google_calendar(ident)
            elif choice == '7':
                setup_google_news(ident)
            elif choice == '8':
                configure_dns(target_ip)
        except (subprocess.SubprocessError, OSError) as exc:
            print(ui('Операция прервана. Проверьте соединение с колонкой и повторите пункт меню.',
                     'Operation interrupted. Check the speaker connection and retry the menu option.'))
        input(ui('Нажмите Enter, чтобы вернуться в меню…', 'Press Enter to return to the menu…'))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', nargs='?', choices=['run', 'status', 'pipeline', 'install', 'spotify', 'google'], default='run')
    parser.add_argument('--ip', help='Speaker network IP address')
    parser.add_argument('--key', help='Gemini API key')
    parser.add_argument('--force', action='store_true', help='Allow install on non-tested hardware with active ADB')
    parser.add_argument('--lang', choices=['ru', 'en'], default=None, help='UI language / язык интерфейса')
    args = parser.parse_args()

    global LANG
    if args.lang:
        LANG = args.lang

    if args.command == 'run':
        interactive_menu(args.ip)
    elif args.command == 'pipeline':
        return 0 if run_full_automated_pipeline(args.ip, api_key=args.key, force=args.force) else 1
    elif args.command == 'status':
        mode, ident = detect_mode(args.ip)
        print(json.dumps({'mode': mode, 'identifier': ident}))
    elif args.command == 'install':
        mode, ident = detect_mode(args.ip)
        target = ident or args.ip
        if target:
            if not target.endswith(':5555') and ':' not in target:
                target = f"{target}:5555"
            return 0 if step4_install_gemini_assistant(target, api_key=args.key, force=args.force) else 1
        else:
            print("Укажите --ip адрес устройства.")
            return 1
    elif args.command == 'spotify':
        mode, ident = detect_mode(args.ip)
        if ident:
            setup_spotify(ident)
        else:
            print("Устройство не обнаружено по ADB.")
    elif args.command == 'google':
        mode, ident = detect_mode(args.ip)
        if ident:
            setup_google_calendar(ident)
        else:
            print("Устройство не обнаружено по ADB.")


if __name__ == '__main__':
    try:
        sys.exit(main())
    except (subprocess.SubprocessError, OSError):
        sys.exit('Operation failed: check the ADB connection and device storage. No success was confirmed.')
