# webrtc-vnc

Stream your desktop to any browser over WebRTC — keyboard, mouse, and gamepad input included. Works on NVIDIA, Intel/AMD, Raspberry Pi 4b, and any Linux machine with FFmpeg.

## Features

- H.264 video via WebRTC media tracks (Safari, iOS, Firefox) and WebCodecs data channel (Chrome, Edge)
- Opus audio
- Keyboard, mouse, and gamepad input via `/dev/uinput`
- Multi-peer: host + up to 3 guest players, spectators
- Auto-detects the best capture and encoder: NvFBC → kmsgrab → PipeWire → x11grab
- Hardware encoders: NVENC, VAAPI, V4L2 M2M (Raspberry Pi), libx264 fallback

## Requirements

- Linux with X11 or Wayland
- Go 1.22+
- FFmpeg (with libx264, and optionally h264_nvenc / h264_vaapi / h264_v4l2m2m)

## Quick Start

```bash
go build -o webrtc-vnc .
sudo ./setup.sh          # one-time: groups, udev, ffmpeg capabilities
./webrtc-vnc
```

Open `http://localhost:8080` in a browser.

## Raspberry Pi 4b

The Pi 4b VideoCore VI GPU supports hardware H.264 encoding via `h264_v4l2m2m`. The server detects and uses it automatically — no configuration needed.

Install prerequisites:

```bash
sudo apt update
sudo apt install ffmpeg golang-go
```

Build and run:

```bash
go build -o webrtc-vnc .
sudo ./setup.sh
./webrtc-vnc --width 1280 --height 720 --fps 30 --bitrate 2000
```

Lower resolution and framerate are recommended on the Pi to keep CPU usage manageable when software encoding is used as a fallback.

To confirm the V4L2 hardware encoder is active, check the `/api/encoder` endpoint:

```bash
curl http://localhost:8080/api/encoder
# {"status":true,"encoder":"h264_v4l2m2m (x11grab)"}
```

## Options

```
  -port int        HTTP server port (default 8080)
  -display string  X11 display (default $DISPLAY or :0)
  -width int       Capture width (default 1920)
  -height int      Capture height (default 1080)
  -fps int         Capture framerate (default 60)
  -bitrate int     Video bitrate in kbps (default 3000)
  -encoder string  Video encoder: auto, nvenc, vaapi, software (default auto)
  -no-audio        Disable audio capture
```

## Setup Details

`setup.sh` makes the following system-wide changes (reverted by `unsetup.sh`):

- Creates `/etc/udev/rules.d/99-webrtc-vnc-uinput.rules` for `/dev/uinput` access
- Adds your user to the `input`, `render`, and `video` groups
- Copies `ffmpeg` to `./ffmpeg-cap` and grants it `cap_sys_admin` (required for kmsgrab)
- On NVIDIA systems: adds `nvidia-drm.modeset=1` to GRUB for kmsgrab support

Input injection (keyboard, mouse, gamepad) requires `/dev/uinput` access. Without it, the server runs in view-only mode.

## Architecture

| File | Purpose |
|------|---------|
| `main.go` | HTTP server, flags, graceful shutdown |
| `capture.go` | Screen capture + encoder detection, FFmpeg process management |
| `stream.go` | WebRTC room, signaling, peer/input/quality management |
| `rtp_sender.go` | H.264 → RTP packetization (RFC 6184 FU-A) |
| `input.go` | `/dev/uinput` keyboard, mouse, gamepad injection |
| `cmd/nvfbc/` | NVIDIA NvFBC capture helpers (optional, NVIDIA only) |
| `web/` | Browser frontend |
