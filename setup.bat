@echo off
setlocal EnableExtensions
chcp 65001 >nul
set "PYTHONUTF8=1"
cd /d "%~dp0"
echo.
echo   CitationLive - installation and settings
echo   [1] Russian / Русский
echo   [2] English
choice /c 12 /n /m "Language / Язык [1/2]: "
set "CITATION_LANG=ru"
if errorlevel 2 set "CITATION_LANG=en"
set "CITATION_PYTHON=python"
python -c "import sys; sys.exit(0 if sys.version_info >= (3, 10) else 1)" >nul 2>nul
if not errorlevel 1 goto ready
set "CITATION_PYTHON=py -3"
py -3 -c "import sys; sys.exit(0 if sys.version_info >= (3, 10) else 1)" >nul 2>nul
if not errorlevel 1 goto ready
echo.
echo Python 3.10+ is required / Требуется Python 3.10+.
echo https://www.python.org/downloads/
echo Install Python, then open setup.bat again.
echo Установите Python и снова откройте setup.bat.
pause
exit /b 1
:ready
%CITATION_PYTHON% -B CitationLive\citation_tool.py run --lang %CITATION_LANG%
set "RC=%ERRORLEVEL%"
if "%RC%"=="0" exit /b 0
echo.
echo Setup stopped. Read the message above / Настройка прервана. См. сообщение выше.
pause
exit /b %RC%
