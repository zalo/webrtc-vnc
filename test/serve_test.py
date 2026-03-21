#!/usr/bin/env python3
"""Serve the WebCodecs test page with an H.264 test file."""
import http.server
import subprocess
import os
import sys

PORT = 9876
DIR = os.path.dirname(os.path.abspath(__file__))

# Generate test H.264 file from nvfbc_nvenc
H264_FILE = os.path.join(DIR, 'test.h264')
NVFBC = os.path.join(DIR, '..', 'nvfbc_nvenc')

if not os.path.exists(H264_FILE) or os.path.getsize(H264_FILE) < 1000:
    print(f"Generating test H.264 with {NVFBC}...")
    env = os.environ.copy()
    env['DISPLAY'] = ':0'
    result = subprocess.run([NVFBC, '-n', '30'], capture_output=True, env=env)
    if result.returncode != 0:
        print(f"nvfbc_nvenc failed: {result.stderr.decode()}")
        # Fallback: use ffmpeg
        print("Falling back to ffmpeg...")
        subprocess.run([
            'ffmpeg', '-hide_banner', '-f', 'x11grab', '-video_size', '320x240',
            '-framerate', '30', '-i', ':0', '-frames:v', '30',
            '-c:v', 'libx264', '-preset', 'ultrafast', '-tune', 'zerolatency',
            '-profile:v', 'baseline', '-pix_fmt', 'yuv420p',
            '-f', 'h264', H264_FILE
        ], env=env)
    else:
        with open(H264_FILE, 'wb') as f:
            f.write(result.stdout)
    print(f"Generated {os.path.getsize(H264_FILE)} bytes")

os.chdir(DIR)
handler = http.server.SimpleHTTPRequestHandler
httpd = http.server.HTTPServer(('127.0.0.1', PORT), handler)
print(f"Serving test at http://127.0.0.1:{PORT}/webcodecs_test.html")
httpd.serve_forever()
