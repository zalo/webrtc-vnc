#!/usr/bin/env python3
"""Connect to the live VNC server and take screenshots to diagnose rendering."""
import time, os

DIR = os.path.dirname(os.path.abspath(__file__))

from playwright.sync_api import sync_playwright

with sync_playwright() as p:
    browser = p.chromium.launch(
        headless=False,
        executable_path='/usr/bin/google-chrome',
        args=['--no-sandbox', '--autoplay-policy=no-user-gesture-required']
    )
    page = browser.new_page()

    logs = []
    page.on('console', lambda m: logs.append(f"{time.strftime('%H:%M:%S')} {m.text}"))

    page.goto('http://localhost:8085/')
    time.sleep(8)  # Wait for connection + some frames

    # Screenshot 1: initial state
    ss1 = os.path.join(DIR, 'vnc_t0.png')
    page.screenshot(path=ss1)
    print(f"Screenshot t=0: {ss1}")

    # Check what elements exist and their sizes
    info = page.evaluate('''() => {
        var vc = document.getElementById('videoContainer');
        var vid = document.getElementById('videoElement');
        var canvases = document.querySelectorAll('canvas');
        var result = {
            container: vc ? {w: vc.offsetWidth, h: vc.offsetHeight, display: getComputedStyle(vc).display} : null,
            video: vid ? {w: vid.videoWidth, h: vid.videoHeight, display: getComputedStyle(vid).display, src: !!vid.srcObject} : null,
            canvasCount: canvases.length,
            canvases: []
        };
        canvases.forEach(function(c) {
            var ctx = c.getContext('2d');
            var d = ctx.getImageData(c.width/2, c.height/2, 1, 1).data;
            result.canvases.push({
                w: c.width, h: c.height,
                clientW: c.clientWidth, clientH: c.clientHeight,
                display: getComputedStyle(c).display,
                zIndex: getComputedStyle(c).zIndex,
                pixel: [d[0],d[1],d[2],d[3]]
            });
        });
        return result;
    }''')
    print(f"\nDOM state:")
    print(f"  Container: {info['container']}")
    print(f"  Video: {info['video']}")
    print(f"  Canvases: {info['canvasCount']}")
    for i, c in enumerate(info['canvases']):
        print(f"    Canvas {i}: {c['w']}x{c['h']} client={c['clientW']}x{c['clientH']} display={c['display']} z={c['zIndex']} pixel={c['pixel']}")

    # Wait 5 more seconds and screenshot again
    time.sleep(5)
    ss2 = os.path.join(DIR, 'vnc_t5.png')
    page.screenshot(path=ss2)
    print(f"\nScreenshot t=5: {ss2}")

    # Check canvas pixel again
    info2 = page.evaluate('''() => {
        var canvases = document.querySelectorAll('canvas');
        var result = [];
        canvases.forEach(function(c) {
            var ctx = c.getContext('2d');
            var d = ctx.getImageData(c.width/2, c.height/2, 1, 1).data;
            result.push({w: c.width, h: c.height, pixel: [d[0],d[1],d[2],d[3]]});
        });
        return result;
    }''')
    print(f"Canvas pixels at t=5:")
    for i, c in enumerate(info2):
        print(f"  Canvas {i}: {c['w']}x{c['h']} pixel={c['pixel']}")

    # Print logs
    print(f"\n=== CONSOLE LOGS ({len(logs)}) ===")
    for l in logs[-30:]:
        print(l)

    browser.close()
