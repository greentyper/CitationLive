# CitationLive

[Русский](../README.md)

CitationLive adds the Gemini Live voice assistant to a speaker. Spotify, Google Calendar and Tasks, and a personalized Google News feed are optional.

> **Important:** only Spotify is currently supported. Use of the exploit, installer, and CitationLive is entirely at your own risk. The author is not responsible for device damage, data loss, or any other consequences.

Run `setup.bat` from the root of the extracted distribution. The script only opens the CitationLive menu; the exploit and installation are started from menu options.

## Compatibility

Tested on the **Harman Kardon Citation One MK3 (MKIII)**. The researched exploit involves a service in Harman/JBL firmware, so the same vulnerability may affect other devices that use the service. CitationLive compatibility with other models depends on the firmware's architecture and audio components and requires separate testing. If CitationLive works successfully on another speaker model, please report it by opening an issue in the repository.

## Requirements

- An extracted CitationLive distribution with the `CitationLive/` folder, the `citation_tool.py` installer, and the speaker files in `CitationLive/dist/`.
- Windows and Python 3.10 or later. Verify that `python --version` runs in PowerShell. The distribution includes ADB for Windows. The `citation_tool.py` installer uses Python's standard library.
- A speaker and a computer on the same trusted local network. Check your router's device list for the speaker IP.
- A Gemini API key from Google AI Studio. The installer can open the key creation page in your browser.

## CitationLive installation

1. Open `setup.bat` from the root of the extracted distribution.
2. For a complete one-step installation, choose **[1]** and follow the wizard. For separate steps, choose **[3]**, enter the speaker IP, enable network ADB through the exploit, then choose **[4]** to install CitationLive.
4. Enter your Gemini API key and choose a city for weather and local time. Keep the window open while the speaker reboots and the installer checks the Gemini Live connection.
5. After the connection check passes, say “Hey Google” and ask a question. If the installer reports that the files were installed but Gemini Live is unavailable, check your key and use option **[8]** to review DNS or proxy settings.

## Optional settings

After installation, open the settings menu with the command below or run `setup.bat` again from the extracted folder.

```powershell
python .\CitationLive\citation_tool.py run --ip 192.168.1.100 --lang en
```

| Option | Action |
| --- | --- |
| **[2]** | Check the ADB connection to the speaker. |
| **[4]** | Reinstall or update CitationLive. |
| **[5]** | Pair Spotify by confirming the code shown by the wizard on the Spotify pairing page. |
| **[6]** | Connect Google Calendar and Tasks with OAuth. The wizard guides you through Google Cloud setup. |
| **[7]** | Manage the personalized Google News feed, including checking and clearing its saved session. |
| **[8]** | Configure DNS or a proxy for Gemini. |
| **[10]** | Sign in to Google News with Chrome or Edge. The wizard offers to install Playwright on first use. |

[Network ADB security](SECURITY.md) (Russian)

CitationLive code is licensed under MIT. See `docs/THIRD_PARTY.md` in the extracted distribution for bundled component licenses.
