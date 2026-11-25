@echo off
setlocal
set "NETPATROL_NO_AUTH=1"
set "netpatrol_data=%LOCALAPPDATA%\NetPatrol"
if not exist "%netpatrol_data%\" mkdir "%netpatrol_data%"
echo NetPatrol: http://127.0.0.1:8080/devices
"%~dp0netpatrol.exe" -listen 127.0.0.1:8080 -db "%netpatrol_data%\netpatrol.db"
pause
