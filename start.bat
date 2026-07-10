@echo off
chcp 65001 >nul
cd /d "%~dp0"
title grokcli-2api

echo.
echo  === grokcli-2api ===
echo  Working dir: %CD%
echo.

where python >nul 2>nul
if errorlevel 1 (
  echo [ERROR] 未找到 python，请先安装 Python 3.10+ 并加入 PATH
  pause
  exit /b 1
)

python -c "import fastapi,uvicorn,httpx" 2>nul
if errorlevel 1 (
  echo Installing dependencies...
  python -m pip install -r requirements.txt
  if errorlevel 1 (
    echo [ERROR] 依赖安装失败
    pause
    exit /b 1
  )
)

REM Registration deps (HTTP protocol)
python -c "import curl_cffi,requests" 2>nul
if errorlevel 1 (
  echo Installing remaining dependencies...
  python -m pip install -r requirements.txt
)

REM Vendored grok-build-auth package path
set "PYTHONPATH=%CD%\grok-build-auth;%PYTHONPATH%"

if not defined GROK2API_OPEN_BROWSER set GROK2API_OPEN_BROWSER=1
if not defined GROK2API_HOST set GROK2API_HOST=127.0.0.1
if not defined GROK2API_PORT set GROK2API_PORT=3000

echo Starting grokcli-2api on http://%GROK2API_HOST%:%GROK2API_PORT% ...
echo Admin: http://127.0.0.1:%GROK2API_PORT%/admin
echo Registration: grok-build-auth (HTTP protocol)
echo.

python app.py
set EXITCODE=%ERRORLEVEL%
if not %EXITCODE%==0 (
  echo.
  echo [ERROR] 服务退出，代码 %EXITCODE%
  echo 常见修复:
  echo   1^) python -m pip install -r requirements.txt
  echo   2^) 确认 grok-build-auth 目录存在
  echo   3^) 协议注册需要 YesCaptcha + MoeMail 配置
  pause
)
exit /b %EXITCODE%
