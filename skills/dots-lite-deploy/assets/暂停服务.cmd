@echo off
"%~dp0executor.exe" maintenance pause -config "%~dp0..\config\executor.json"
set "dots_exit=%errorlevel%"
echo.
if not "%dots_exit%"=="0" echo Pause failed. Read the error above; shutdown is not confirmed.
pause
exit /b %dots_exit%
