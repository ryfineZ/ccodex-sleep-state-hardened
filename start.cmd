@echo off
cd /d "%~dp0"
if exist "bin\ccodex-request-recorder.exe" goto run
where go >nul 2>nul
if errorlevel 1 (
  echo Source checkout: Go or a prebuilt recorder is required.
  pause
  exit /b 1
)
if not exist bin mkdir bin
go build -trimpath -o bin\ccodex-request-recorder.exe ./cmd/ccodex-request-recorder
if errorlevel 1 (pause & exit /b 1)
:run
"bin\ccodex-request-recorder.exe" setup %*
if errorlevel 1 pause
