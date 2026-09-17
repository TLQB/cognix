@echo off
REM Freeclaude all-in-one launcher (Windows): starts zai-proxy, then runs the TUI.
REM
REM Usage:
REM   start.bat                    (TUI opens in %%USERPROFILE%%)
REM   start.bat C:\path\to\project (TUI opens in that directory)
REM
REM First run: put your Z.AI token in %USERPROFILE%\.config\zai-proxy\token
setlocal EnableDelayedExpansion

set "PORT=3002"
set "AUTH_TOKEN=Waguri"
set "LOG_LEVEL=info"
set "WORKDIR=%~1"
if "%WORKDIR%"=="" set "WORKDIR=%USERPROFILE%"

set "CONFIG_DIR=%USERPROFILE%\.config\zai-proxy"
set "TOKEN_FILE=%CONFIG_DIR%\token"

if not exist "%TOKEN_FILE%" (
  echo Freeclaude needs a Z.AI token ^(first run only^).
  echo Get it from https://chat.z.ai ^(see README^), then save it to:
  echo   %TOKEN_FILE%
  set /p TOKEN="Or paste it here: "
  if "!TOKEN!"=="" (
    echo Error: no token provided.
    exit /b 1
  )
  if not exist "%CONFIG_DIR%" mkdir "%CONFIG_DIR%"
  <nul set /p="!TOKEN!"> "%TOKEN_FILE%"
)

set /p TOKEN=<"%TOKEN_FILE%"
if "%TOKEN%"=="" (
  echo Error: token file %TOKEN_FILE% is empty.
  exit /b 1
)

REM Resolve device-token sources: bundle dir first, then config dir.
set "QBLESS=%~dp0qbless.json"
if not exist "%QBLESS%" set "QBLESS=%CONFIG_DIR%\qbless.json"
set "DBPATH=%~dp0tokens.sqlite"
if not exist "%DBPATH%" set "DBPATH=%CONFIG_DIR%\tokens.sqlite"

set "QBLESS_FILE=%QBLESS%"

REM Auto-bless: if qbless.json missing AND q-bless.exe ships with the bundle,
REM run it once to create qbless.json next to zai-proxy.exe. Skip if the user
REM is offline or q-bless.exe is absent (fall back to the warning).
if not exist "%QBLESS%" (
  set "QBLESS_EXE=%~dp0q-bless.exe"
  if exist "!QBLESS_EXE!" (
    echo [*] qbless.json not found — running q-bless to bless a fresh Q...
    echo     This launches headless Chromium for ~30s, then exits.
    "!QBLESS_EXE!" -out "%QBLESS%"
    if !errorlevel! EQU 0 (
      echo [+] qbless.json created at %QBLESS%
      set "QBLESS_FILE=%QBLESS%"
    ) else if !errorlevel! EQU 2 (
      echo [!] q-bless canary verify FAILED ^(Q not blessed^).
      echo     Network/captcha may be blocked. Continuing without qbless.json.
      echo     Proxy will fail with 'captcha generation returned empty payload'.
    ) else (
      echo [!] q-bless exited with code !errorlevel! — qbless.json NOT created.
      echo     Continuing without it; proxy may fail to mint tokens.
    )
  )
)

REM Auto-rebless: the proxy probes the blessed Q every 10 minutes and
REM re-blesses by itself when the Q dies (needs the shipped q-bless.exe).
REM Without this, a dead Q bricks the proxy until the user re-runs q-bless.
set "QBLESS_AUTO_REBLESS=1"
if exist "%~dp0q-bless.exe" set "QBLESS_BINARY=%~dp0q-bless.exe"

echo [*] Starting zai-proxy on http://localhost:%PORT%
REM Qwen gateway: when qwen-proxy.exe ships with the bundle, start it on 3003
REM and point zai-proxy at it. Requires %USERPROFILE%\.config\qwen-proxy\token.
REM Set QWEN_NO_BUNDLE=1 to skip (pure GLM bundle).
set "QWEN_PORT=3003"
set "QWEN_PROXY_URL="
if exist "%~dp0qwen-proxy.exe" (
  if not defined QWEN_NO_BUNDLE (
    set "QWTOK=%USERPROFILE%\.config\qwen-proxy\token"
    if exist "!QWTOK!" (
      set /p QWEN_TOKEN=<"!QWTOK!"
      set "QWEN_TRANSPORT=apk"
      echo [*] Starting qwen-proxy on http://localhost:!QWEN_PORT!
      start "qwen-proxy" /min "%~dp0qwen-proxy.exe" --agent-mode --port !QWEN_PORT!
      set "QWEN_PROXY_URL=http://localhost:!QWEN_PORT!"
    ) else (
      echo [!] qwen-proxy.exe found but no Qwen token at !QWTOK!.
      echo     Starting WITHOUT Qwen models ^(GLM only^).
    )
  )
)

set "PROXY_ARGS=--agent-mode --verbose --db-path %DBPATH%"
if exist "%QBLESS_FILE%" (
  echo     Using qbless: %QBLESS_FILE%
) else (
  echo [!] Warning: no qbless.json found — requests will fail with
  echo     'captcha generation returned empty payload'. Run q-bless to create one.
)

start "zai-proxy" /min "%~dp0zai-proxy.exe" %PROXY_ARGS%

REM Poll healthz up to 15s
set /a TRIES=0
:waitloop
timeout /t 1 /nobreak >nul
curl -sf "http://localhost:%PORT%/api/healthz" >nul 2>&1
if %errorlevel%==0 goto ready
set /a TRIES+=1
if %TRIES% LSS 15 goto waitloop
echo [!] Proxy not ready after 15s — check the zai-proxy console window.
exit /b 1

:ready
echo [✓] Proxy ready

REM Clear any stale freebuff session so the TUI lands on the model picker.
curl -sf -X DELETE "http://localhost:%PORT%/api/v1/freebuff/session" -H "Authorization: Bearer %AUTH_TOKEN%" >nul 2>&1

echo [*] Starting Freeclaude TUI (workdir: %WORKDIR%)
echo     Quit the TUI, then this window runs the proxy cleanup.
set "CODEBUFF_API_KEY=%AUTH_TOKEN%"
"%~dp0freeclaude.exe" --cwd "%WORKDIR%"

echo [*] Stopping proxy...
taskkill /IM zai-proxy.exe /F >nul 2>&1
taskkill /IM qwen-proxy.exe /F >nul 2>&1
echo [✓] Done.
endlocal
