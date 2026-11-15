# Сторонние компоненты и лицензии (Third-Party Components & Licenses)

Проект **CitationLive** распространяется под лицензией **MIT** (см. файл
`LICENSE`). MIT покрывает исходный код и собранный из него `citationvoice`.

## Go-библиотеки (компилируются в citationvoice)
- **`github.com/gorilla/websocket`** (v1.5.3) — [BSD-2-Clause](https://github.com/gorilla/websocket/blob/master/LICENSE).
- **`golang.org/x/net`** (v0.62.0) — [BSD-3-Clause](https://go.dev/LICENSE).
- **`golang.org/x/text`** (v0.43.0) — [BSD-3-Clause](https://go.dev/LICENSE).

## Runtime и вендорские компоненты
- **`go-librespot`** — GPL-3.0 (devgianlu/go-librespot). Бинарник:
  `spotify-connect/client.tar.gz`; текст лицензии:
  `spotify-connect/LICENSE-GPL-3.0.txt`; предложение исходников:
  `spotify-connect/SOURCE_OFFER.md`.
- **Android Platform Tools (ADB)** — Apache-2.0 (Google LLC). Текст:
  `../ADB-Exploit/NOTICE-APACHE-2.0.txt`; атрибуция:
  `../ADB-Exploit/NOTICE.md`.
- **`dist/iot_audio_capture48k`** — вендорский capture-бинарник, не MIT.
  Происхождение и ограничения: `PROVENANCE.md`.
- **`dist/libassistant.hotword-observe-v3.so`** — пропатченный вендорский
  `libassistant`, не MIT. Происхождение и ограничения: `PROVENANCE.md`.
- **`runtime/cast_receiver_framework.js`** — Google Cast Receiver Framework
  (CAF), проприетарный компонент Google, не MIT. Происхождение:
  `PROVENANCE.md`.

## Python
- **Playwright** (опционально, для входа через браузер) — Apache-2.0.
- `msgpack-python`, `websocket-client` относятся к ADB-Exploit, см.
  `../ADB-Exploit/THIRD_PARTY.md`.

Репозиторий не содержит закрытых исходных кодов Google/Harman, ключей,
персональных токенов или дампов чужих устройств. Вендорские бинарники
включены только для совместимости с собственной колонкой; подробности и
ограничения — в `PROVENANCE.md`.
