@echo off
"%~dp0executor.exe" maintenance resume -config "%~dp0..\config\executor.json"
set "dots_exit=%errorlevel%"
echo.
if not "%dots_exit%"=="0" echo Resume failed. Read the error above; service readiness is not confirmed.
pause
exit /b %dots_exit%
