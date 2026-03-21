#!/bin/bash
# Setup script for webrtc-vnc — run with sudo.
# Idempotent: safe to re-run, skips steps that are already done.
#
# Global OS changes made (reverted by unsetup.sh):
#   - /etc/udev/rules.d/99-webrtc-vnc-uinput.rules
#   - nvidia-drm.modeset=1 in /etc/default/grub
#   - User added to input/render/video groups
#
# Local changes (in project directory):
#   - ffmpeg-cap: copy of ffmpeg with cap_sys_admin

set -euo pipefail

DIR="$(dirname "$(realpath "$0")")"
FFMPEG_CAP="${DIR}/ffmpeg-cap"
USER="${SUDO_USER:-$(whoami)}"
UDEV_RULE="/etc/udev/rules.d/99-webrtc-vnc-uinput.rules"
GRUB_FILE="/etc/default/grub"

if [ "$(id -u)" -ne 0 ]; then
    echo "Usage: sudo $0"
    exit 1
fi

echo "=== webrtc-vnc setup ==="
CHANGES=0

# 0. Build NvFBC capture helper (if NVIDIA GPU + source available)
NVFBC_HDR_DIR="/home/selstad/Desktop/Sunshine/third-party/nvfbc"
NVENC_HDR_DIR="/home/selstad/Desktop/Sunshine/third-party/nv-codec-headers/include"
echo "[0/5] NvFBC capture helpers..."

# Build nvfbc_nvenc (zero-copy: NvFBC→CUDA→NVENC, no CPU roundtrip)
NVFBC_NVENC_SRC="${DIR}/cmd/nvfbc/nvfbc_nvenc.c"
NVFBC_NVENC_BIN="${DIR}/nvfbc_nvenc"
if [ -f "$NVFBC_NVENC_SRC" ] && [ -d "$NVFBC_HDR_DIR" ] && [ -d "$NVENC_HDR_DIR" ] && which gcc >/dev/null 2>&1; then
    if [ ! -f "$NVFBC_NVENC_BIN" ] || [ "$NVFBC_NVENC_SRC" -nt "$NVFBC_NVENC_BIN" ]; then
        gcc -O2 -o "$NVFBC_NVENC_BIN" "$NVFBC_NVENC_SRC" \
            -I"$NVFBC_HDR_DIR" -I"$NVENC_HDR_DIR" -I/usr/include -ldl -lrt -lcuda
        echo "  Built nvfbc_nvenc (zero-copy GPU pipeline)"
        CHANGES=$((CHANGES + 1))
    else
        echo "  nvfbc_nvenc up to date"
    fi
else
    echo "  nvfbc_nvenc skipped (headers not found)"
fi

# Build nvfbc_capture (fallback: NvFBC→NV12 pipe→FFmpeg)
NVFBC_SRC="${DIR}/cmd/nvfbc/nvfbc_capture.c"
NVFBC_BIN="${DIR}/nvfbc_capture"
if [ -f "$NVFBC_SRC" ] && [ -d "$NVFBC_HDR_DIR" ] && which gcc >/dev/null 2>&1; then
    if [ ! -f "$NVFBC_BIN" ] || [ "$NVFBC_SRC" -nt "$NVFBC_BIN" ]; then
        gcc -O2 -o "$NVFBC_BIN" "$NVFBC_SRC" -I"$NVFBC_HDR_DIR" -ldl -lrt
        echo "  Built nvfbc_capture (NV12 pipe fallback)"
        CHANGES=$((CHANGES + 1))
    else
        echo "  nvfbc_capture up to date"
    fi
fi

# 1. ffmpeg with cap_sys_admin for kmsgrab
echo "[1/5] ffmpeg-cap (cap_sys_admin for kmsgrab)..."
SYSTEM_FFMPEG="$(which ffmpeg 2>/dev/null || true)"
if [ -z "$SYSTEM_FFMPEG" ]; then
    echo "  ERROR: ffmpeg not found in PATH"
    exit 1
fi

NEED_COPY=false
if [ ! -f "$FFMPEG_CAP" ]; then
    NEED_COPY=true
elif ! getcap "$FFMPEG_CAP" 2>/dev/null | grep -q 'cap_sys_admin'; then
    NEED_COPY=true
elif [ "$SYSTEM_FFMPEG" -nt "$FFMPEG_CAP" ]; then
    # System ffmpeg is newer than our copy
    NEED_COPY=true
fi

if [ "$NEED_COPY" = true ]; then
    # Stop the service first so the file isn't busy
    if systemctl --user -M "${USER}@" is-active webrtc-vnc.service &>/dev/null; then
        echo "  Stopping webrtc-vnc service (file in use)..."
        systemctl --user -M "${USER}@" stop webrtc-vnc.service 2>/dev/null || \
            su - "$USER" -c "systemctl --user stop webrtc-vnc.service" 2>/dev/null || \
            true
        sleep 1
    fi
    # Remove old file first to avoid "text file busy"
    rm -f "$FFMPEG_CAP"
    cp "$SYSTEM_FFMPEG" "$FFMPEG_CAP"
    chown root:root "$FFMPEG_CAP"
    setcap 'cap_sys_admin=ep' "$FFMPEG_CAP"
    echo "  Created $(getcap "$FFMPEG_CAP")"
    CHANGES=$((CHANGES + 1))
else
    echo "  Already set up: $(getcap "$FFMPEG_CAP")"
fi

# 2. Group membership
echo "[2/5] Group membership for ${USER}..."
for grp in input render video; do
    if ! getent group "$grp" &>/dev/null; then
        continue
    fi
    if id -nG "$USER" | grep -qw "$grp"; then
        echo "  $USER already in '$grp'"
    else
        usermod -aG "$grp" "$USER"
        echo "  Added $USER to '$grp' (log out/in to activate)"
        CHANGES=$((CHANGES + 1))
    fi
done

# 3. udev rule for /dev/uinput
echo "[3/5] udev rule for /dev/uinput..."
if [ -f "$UDEV_RULE" ]; then
    echo "  Already exists: $UDEV_RULE"
else
    echo 'KERNEL=="uinput", GROUP="input", MODE="0660"' > "$UDEV_RULE"
    udevadm control --reload-rules 2>/dev/null || true
    # Apply immediately
    if [ -c /dev/uinput ]; then
        chown root:input /dev/uinput
        chmod 660 /dev/uinput
    fi
    echo "  Created $UDEV_RULE"
    CHANGES=$((CHANGES + 1))
fi

# 4. nvidia-drm.modeset=1 for kmsgrab
echo "[4/5] nvidia-drm.modeset kernel parameter..."
NEEDS_REBOOT=false
if lspci | grep -qi nvidia && [ -f "$GRUB_FILE" ]; then
    if grep -q 'nvidia-drm.modeset=1' "$GRUB_FILE"; then
        if grep -q 'nvidia-drm.modeset=1' /proc/cmdline; then
            echo "  Active in GRUB and running kernel"
        else
            echo "  In GRUB but not active — reboot required"
            NEEDS_REBOOT=true
        fi
    else
        sed -i 's/^GRUB_CMDLINE_LINUX_DEFAULT="\(.*\)"/GRUB_CMDLINE_LINUX_DEFAULT="\1 nvidia-drm.modeset=1"/' "$GRUB_FILE"
        update-grub 2>/dev/null || true
        echo "  Added to GRUB — reboot required"
        NEEDS_REBOOT=true
        CHANGES=$((CHANGES + 1))
    fi
else
    echo "  No NVIDIA GPU or no GRUB — skipped"
fi

echo ""
if [ "$CHANGES" -eq 0 ]; then
    echo "=== Everything already configured ==="
else
    echo "=== Setup complete ($CHANGES changes) ==="
fi
echo ""

if [ "$NEEDS_REBOOT" = true ]; then
    echo "REBOOT REQUIRED for nvidia-drm.modeset=1 (enables kmsgrab zero-copy capture)."
    echo "Without reboot, x11grab+NVENC is used (still fast)."
    echo ""
fi

echo "Restart the service:"
echo "  systemctl --user restart webrtc-vnc"
