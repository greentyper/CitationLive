# Сторонние компоненты и лицензии / Third-party components

Проект состоит из двух частей:

- **ADB-Exploit** — MIT, см. `../ADB-Exploit/LICENSE`.
- **CitationLive** — MIT, см. `../CitationLive/LICENSE`.

MIT покрывает исходный код и собранный из него `citationvoice`. Он **не**
покрывает перечисленные ниже сторонние и вендорские компоненты.

## Компоненты

| Компонент | Лицензия | Где |
|---|---|---|
| Android SDK Platform-Tools (`adb.exe`, DLL) | Apache-2.0, Google LLC | `../ADB-Exploit/bin/`, `../CitationLive/bin/`; текст: `../ADB-Exploit/NOTICE-APACHE-2.0.txt`, `../ADB-Exploit/NOTICE.md` |
| go-librespot | GPL-3.0 | `../CitationLive/spotify-connect/client.tar.gz`; текст: `../CitationLive/spotify-connect/LICENSE-GPL-3.0.txt`; source offer: `../CitationLive/spotify-connect/SOURCE_OFFER.md` |
| `iot_audio_capture48k` | вендорский бинарник, не MIT | `../CitationLive/dist/`; происхождение: `../CitationLive/PROVENANCE.md` |
| `libassistant.hotword-observe-v3.so` | вендорский бинарник, не MIT | `../CitationLive/dist/`; происхождение: `../CitationLive/PROVENANCE.md` |
| Google Cast Receiver Framework (`cast_receiver_framework.js`) | проприетарный Google, не MIT | `../CitationLive/runtime/`; происхождение: `../CitationLive/PROVENANCE.md` |
| msgpack-python, websocket-client | Apache-2.0 | `../ADB-Exploit/requirements.txt`; см. `../ADB-Exploit/THIRD_PARTY.md` |
| Playwright (опционально) | Apache-2.0 | устанавливается мастером для входа через браузер |
| `github.com/gorilla/websocket` | BSD-2-Clause | Go-зависимость `citationvoice` |
| `golang.org/x/net`, `golang.org/x/text` | BSD-3-Clause | Go-зависимости `citationvoice` |

Подробности по CitationLive: `../CitationLive/THIRD_PARTY.md`.
Подробности по эксплойту: `../ADB-Exploit/THIRD_PARTY.md`.
