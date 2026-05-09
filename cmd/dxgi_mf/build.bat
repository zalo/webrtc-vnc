@echo off
REM Build the Windows native capture+encode helper for webrtc-vnc.
REM Run from a "x64 Native Tools Command Prompt for VS 2022" (or newer).
REM
REM   build.bat              -> dxgi_mf.exe
REM   build.bat install      -> also copies to ..\..\dxgi_mf.exe next to webrtc-vnc.exe

setlocal
set OUT=dxgi_mf.exe

cl /nologo /O2 /EHsc /std:c++17 /DUNICODE /D_UNICODE ^
   dxgi_mf.cpp ^
   /link /OUT:%OUT% ^
   d3d11.lib dxgi.lib dxguid.lib mf.lib mfplat.lib mfreadwrite.lib ^
   mfuuid.lib wmcodecdspuuid.lib ole32.lib

if errorlevel 1 (
    echo build failed
    exit /b 1
)

if /I "%~1"=="install" (
    copy /y %OUT% ..\..\%OUT% >nul
    echo installed -> ..\..\%OUT%
)
endlocal
