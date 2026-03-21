#!/usr/bin/env python3
"""Run the WebCodecs test with Playwright and capture results."""
import subprocess
import sys
import time
import os
import signal
import json

DIR = os.path.dirname(os.path.abspath(__file__))
PORT = 9876

# Start the test server
server = subprocess.Popen([sys.executable, os.path.join(DIR, 'serve_test.py')],
                          stdout=subprocess.PIPE, stderr=subprocess.STDOUT)

time.sleep(2)  # Wait for server + h264 generation

try:
    # Run Playwright test
    from playwright.sync_api import sync_playwright

    with sync_playwright() as p:
        browser = p.chromium.launch(
            headless=False,
            args=['--enable-features=WebCodecs', '--autoplay-policy=no-user-gesture-required']
        )
        page = browser.new_page()

        # Collect console messages
        logs = []
        page.on('console', lambda msg: logs.append(msg.text))

        page.goto(f'http://127.0.0.1:{PORT}/webcodecs_test.html')

        # Wait for test to complete (max 15 seconds)
        for i in range(30):
            time.sleep(0.5)
            done = page.evaluate('window._testDone')
            if done:
                break

        # Screenshot the final result
        screenshot_path = os.path.join(DIR, 'result.png')
        page.screenshot(path=screenshot_path)
        print(f"\nScreenshot saved to {screenshot_path}")

        # Print all console logs
        print("\n=== CONSOLE LOGS ===")
        for l in logs:
            print(l)

        # Check if canvas has non-black content
        pixel = page.evaluate('''() => {
            var c = document.getElementById("canvas");
            var ctx = c.getContext("2d");
            var d = ctx.getImageData(c.width/2, c.height/2, 10, 10).data;
            var nonBlack = 0;
            for (var i = 0; i < d.length; i += 4) {
                if (d[i] > 10 || d[i+1] > 10 || d[i+2] > 10) nonBlack++;
            }
            return {width: c.width, height: c.height, nonBlackPixels: nonBlack, totalSampled: d.length/4};
        }''')
        print(f"\nCanvas: {pixel['width']}x{pixel['height']}, non-black pixels: {pixel['nonBlackPixels']}/{pixel['totalSampled']}")

        if pixel['nonBlackPixels'] > 0:
            print("SUCCESS: Canvas has visible content")
        else:
            print("FAILURE: Canvas is all black")

        browser.close()

finally:
    server.send_signal(signal.SIGTERM)
    server.wait()
