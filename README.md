# webrtc-vnc

Stream your desktop to any browser over WebRTC — keyboard, mouse, and gamepad input included. Cross-platform: Linux, macOS, Windows.

## Features

- H.264 video via WebRTC media tracks (Safari, iOS, Firefox) and WebCodecs data channel (Chrome, Edge)
- Opus audio (Linux only for now)
- Keyboard, mouse, and gamepad input
- Multi-peer: host + up to 3 guest players, spectators
- Per-OS hardware-accelerated capture+encode pipelines:
  - **Linux**: `nvfbc_nvenc` (NVIDIA, GPU-only) → `kmsgrab` → PipeWire → `x11grab`. Encoders: NVENC / VAAPI / V4L2 M2M (Raspberry Pi) / libx264.
  - **macOS**: `screenkit_vt` (ScreenCaptureKit + VideoToolbox, ANE on Apple Silicon) → ffmpeg avfoundation + h264_videotoolbox.
  - **Windows**: `dxgi_mf.exe` (Desktop Duplication + MediaFoundation auto-routing to NVENC/QSV/AMF) → ffmpeg ddagrab + h264_nvenc/qsv/amf/mf.
- Optional cloudflared quick tunnel: server exposes itself to the public internet on startup (default on, `-tunnel=false` to disable).

## Requirements

- Go 1.25+ (1.26 used via the `GOTOOLCHAIN=auto` mechanism — `go build` will fetch it automatically)
- ffmpeg in PATH (Linux primary path skips ffmpeg on the hot path; ffmpeg is needed for fallbacks and audio)

## Linux

```bash
go build -o webrtc-vnc .
sudo ./setup.sh          # one-time: groups, udev, ffmpeg capabilities
./webrtc-vnc
```

Open `http://localhost:8080` in a browser.

The startup probe picks the best capture method automatically. To confirm:

```bash
curl http://localhost:8080/api/encoder
# {"status":true,"encoder":"h264_nvenc (nvfbc)"}
```

### Raspberry Pi 4b

The VideoCore VI GPU supports hardware H.264 via `h264_v4l2m2m`. The server detects and uses it automatically.

```bash
sudo apt install ffmpeg
go build -o webrtc-vnc .
sudo ./setup.sh
./webrtc-vnc -width 1280 -height 720 -fps 30 -bitrate 2000
```

## macOS

Build the Go server (cgo on, for Quartz CoreGraphics input):

```bash
CGO_ENABLED=1 go build -o webrtc-vnc .
```

Build the native capture helper (universal arm64 + x86_64):

```bash
cd cmd/screenkit_vt
make install        # produces ./screenkit_vt and copies it next to webrtc-vnc
cd ../..
./webrtc-vnc
```

The first run will prompt for **Screen Recording** and **Accessibility** permissions in *System Settings → Privacy & Security*. Grant both.

If the helper isn't present, the server falls back to ffmpeg's `-f avfoundation -c:v h264_videotoolbox` path automatically.

## Windows

Install `ffmpeg.exe` somewhere on `PATH` (or copy alongside `webrtc-vnc.exe`).

Build the Go server:

```powershell
go build -o webrtc-vnc.exe .
```

Build the native capture helper (from a Visual Studio "x64 Native Tools Command Prompt"):

```cmd
cd cmd\dxgi_mf
build.bat install   :: produces dxgi_mf.exe and copies it next to webrtc-vnc.exe
cd ..\..
webrtc-vnc.exe
```

If the helper isn't present, the server falls back to ffmpeg's `-f ddagrab` + best detected encoder (`h264_nvenc` / `h264_qsv` / `h264_amf` / `h264_mf`).

## Options

```
  -port int        HTTP server port (default 8080)
  -display string  X11 display id (Linux only; default $DISPLAY or :0)
  -width int       Capture width (default 854)
  -height int      Capture height (default 480)
  -fps int         Capture framerate (default 144)
  -bitrate int     Video bitrate in kbps (default 1000)
  -encoder string  Video encoder: auto, nvenc, vaapi, qsv, amf, software (default auto)
  -no-audio        Disable audio capture
  -tunnel          Expose this server via a cloudflared quick tunnel (default true)
```

## Tunnel (public internet exposure)

The server can launch a [cloudflared](https://github.com/cloudflare/cloudflared) quick tunnel on startup so you can hand the URL to a friend without configuring port-forwarding:

```bash
git submodule update --init third_party/cloudflared
(cd third_party/cloudflared && GOTOOLCHAIN=auto go build -o ../../cloudflared-bin ./cmd/cloudflared)
./webrtc-vnc                 # tunnel URL is printed to the log
./webrtc-vnc -tunnel=false   # local-only
```

⚠️ **Anyone with the trycloudflare URL can view and control this computer.** Treat it like a password.

## Setup details (Linux)

`setup.sh` makes the following system changes (revert with `unsetup.sh`):

- Creates `/etc/udev/rules.d/99-webrtc-vnc-uinput.rules` for `/dev/uinput` access
- Adds your user to the `input`, `render`, and `video` groups
- Copies `ffmpeg` to `./ffmpeg-cap` and grants it `cap_sys_admin` (required for kmsgrab)
- On NVIDIA systems: adds `nvidia-drm.modeset=1` to GRUB for kmsgrab support

Input injection requires `/dev/uinput` access. Without it the server runs view-only.

## Architecture

| File | Purpose | OS |
|------|---------|----|
| `main.go` | HTTP server, flags, graceful shutdown, embed `web/` | all |
| `tunnel.go` | Supervised cloudflared quick tunnel launcher | all |
| `stream.go` | WebRTC room, signaling, peer + quality management | all |
| `capture.go` | Capture struct + generic process orchestration + Annex-B reader | all |
| `capture_linux.go` | Linux probe (NvFBC/kmsgrab/pipewire/x11grab) + ffmpeg pipeline | linux |
| `capture_darwin.go` | macOS probe (screenkit_vt → avfoundation+videotoolbox) | darwin |
| `capture_windows.go` | Windows probe (dxgi_mf → ddagrab+nvenc/qsv/amf) | windows |
| `input_linux.go` | `/dev/uinput` keyboard / mouse / scroll / gamepad | linux |
| `input_darwin.go` | Quartz CGEvent input (cgo) | darwin |
| `input_windows.go` | `user32.SendInput` mouse + keyboard (no cgo) | windows |
| `cmd/nvfbc/` | NVIDIA NvFBC + NVENC GPU-only helper (C) | linux |
| `cmd/screenkit_vt/` | macOS ScreenCaptureKit + VideoToolbox helper (Swift) | darwin |
| `cmd/dxgi_mf/` | Windows DXGI Duplication + MediaFoundation helper (C++) | windows |
| `web/` | Browser frontend (single-page) | all |

The native helpers all speak the same protocol the Go server already consumes: H.264 Annex B with SPS/PPS prepended to every IDR, written to stdout.
