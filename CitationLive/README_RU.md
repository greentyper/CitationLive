# Компоненты CitationLive

[Требования и установка](../README.md)

Папка содержит программу для Harman Kardon Citation One MK3, установщик и файлы для работы колонки. В полном комплекте отдельный компонент включает сетевой ADB. При прямом запуске `citation_tool.py install` сетевой ADB с правами root должен уже работать.

| Путь | Назначение |
| --- | --- |
| `citation_tool.py` | Установка и настройка Gemini, Spotify и сервисов Google. |
| `citation_net.py` | Настройка DNS и прокси для Gemini. |
| `dist/` | Исполняемый файл `citationvoice` и компоненты аудиозахвата. |
| `runtime/` | Скрипты запуска и поддержки аудио, Cast и Spotify. |
| `led/`, `cues/` | Световые и звуковые сигналы. |
| `spotify-connect/` | Клиент Spotify Connect и его настройки. |
| `bin/` | ADB для Windows. |

[Сторонние компоненты и лицензии](THIRD_PARTY.md) · [Происхождение бинарников](PROVENANCE.md)
