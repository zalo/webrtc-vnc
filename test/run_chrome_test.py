#!/usr/bin/env python3
"""Run WebCodecs test with actual Google Chrome (has H.264 support)."""
import subprocess, sys, time, os, signal

DIR = os.path.dirname(os.path.abspath(__file__))
PORT = 9876

# Start test server (generates H.264 from nvfbc_nvenc)
server = subprocess.Popen([sys.executable, os.path.join(DIR, 'serve_test.py')],
                          stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
time.sleep(3)

try:
    from playwright.sync_api import sync_playwright

    with sync_playwright() as p:
        browser = p.chromium.launch(
            headless=False,
            executable_path='/usr/bin/google-chrome',
            args=[
                '--enable-features=WebCodecs',
                '--autoplay-policy=no-user-gesture-required',
                '--no-sandbox',
            ]
        )
        page = browser.new_page()

        logs = []
        page.on('console', lambda msg: logs.append(msg.text))

        page.goto(f'http://127.0.0.1:{PORT}/webcodecs_test.html')

        # Wait for test to complete
        for i in range(40):
            time.sleep(0.5)
            try:
                done = page.evaluate('window._testDone')
                if done: break
            except:
                pass

        # Take screenshots at different points
        ss1 = os.path.join(DIR, 'result_chrome.png')
        page.screenshot(path=ss1)
        print(f"Screenshot: {ss1}")

        # Print logs
        print("\n=== CONSOLE LOGS ===")
        for l in logs:
            print(l)

        # Check canvas content
        pixel = page.evaluate('''() => {
            var c = document.getElementById("canvas");
            var ctx = c.getContext("2d");
            var d = ctx.getImageData(0, 0, c.width, c.height).data;
            var nonBlack = 0, total = d.length / 4;
            for (var i = 0; i < d.length; i += 4) {
                if (d[i] > 10 || d[i+1] > 10 || d[i+2] > 10) nonBlack++;
            }
            // Sample center pixel
            var cx = Math.floor(c.width/2), cy = Math.floor(c.height/2);
            var idx = (cy * c.width + cx) * 4;
            return {
                width: c.width, height: c.height,
                nonBlack: nonBlack, total: total,
                centerR: d[idx], centerG: d[idx+1], centerB: d[idx+2]
            };
        }''')
        print(f"\nCanvas: {pixel['width']}x{pixel['height']}")
        print(f"Non-black pixels: {pixel['nonBlack']}/{pixel['total']} ({100*pixel['nonBlack']//max(1,pixel['total'])}%)")
        print(f"Center pixel: rgb({pixel['centerR']},{pixel['centerG']},{pixel['centerB']})")

        if pixel['nonBlack'] > pixel['total'] * 0.1:
            print("\nSUCCESS: Canvas has visible content!")
        else:
            print("\nFAILURE: Canvas appears black/empty")

        browser.close()

finally:
    server.send_signal(signal.SIGTERM)
    server.wait()
