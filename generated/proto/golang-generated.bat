@echo off
chcp 65001 >nul
cls
set PATH=%PATH%;"C:\Program Files\Git\usr\bin"
cd %~dp0
buf format -w
buf generate