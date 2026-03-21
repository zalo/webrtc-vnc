#!/bin/bash
# Reverts global OS changes made by setup.sh.
#
# Removes:
#   - /etc/udev/rules.d/99-webrtc-vnc-uinput.rules
#   - nvidia-drm.modeset=1 from /etc/default/grub
#   - ffmpeg-cap local binary
#
# Does NOT remove:
#   - User from input/render/video groups (other software may depend on these)
#   - The webrtc-vnc binary or service file

set -euo pipefail

DIR="$(dirname "$(realpath "$0")")"
FFMPEG_CAP="${DIR}/ffmpeg-cap"
UDEV_RULE="/etc/udev/rules.d/99-webrtc-vnc-uinput.rules"
GRUB_FILE="/etc/default/grub"

if [ "$(id -u)" -ne 0 ]; then
    echo "Usage: sudo $0"
    exit 1
fi

echo "=== webrtc-vnc unsetup ==="
CHANGES=0

# 1. Remove ffmpeg-cap
echo "[1/3] Removing ffmpeg-cap..."
if [ -f "$FFMPEG_CAP" ]; then
    rm -f "$FFMPEG_CAP"
    echo "  Removed $FFMPEG_CAP"
    CHANGES=$((CHANGES + 1))
else
    echo "  Not present"
fi

# 2. Remove udev rule
echo "[2/3] Removing udev rule..."
if [ -f "$UDEV_RULE" ]; then
    rm -f "$UDEV_RULE"
    udevadm control --reload-rules 2>/dev/null || true
    echo "  Removed $UDEV_RULE"
    CHANGES=$((CHANGES + 1))
else
    echo "  Not present"
fi

# 3. Remove nvidia-drm.modeset=1 from GRUB
echo "[3/3] Removing nvidia-drm.modeset=1 from GRUB..."
if [ -f "$GRUB_FILE" ] && grep -q 'nvidia-drm.modeset=1' "$GRUB_FILE"; then
    sed -i 's/ nvidia-drm.modeset=1//g' "$GRUB_FILE"
    update-grub 2>/dev/null || true
    echo "  Removed from GRUB (reboot to take effect)"
    CHANGES=$((CHANGES + 1))
else
    echo "  Not present in GRUB"
fi

echo ""
if [ "$CHANGES" -eq 0 ]; then
    echo "=== Nothing to revert ==="
else
    echo "=== Reverted $CHANGES changes ==="
    echo ""
    echo "To also remove the service:"
    echo "  systemctl --user disable --now webrtc-vnc.service"
    echo "  rm ~/.config/systemd/user/webrtc-vnc.service"
fi
