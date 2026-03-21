#!/usr/bin/env python3
import subprocess, sys, time, os, signal

DIR = os.path.dirname(os.path.abspath(__file__))
PORT = 9877

# Start simple server
server = subprocess.Popen([sys.executable, '-m', 'http.server', str(PORT), '-d', DIR],
                          stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
time.sleep(1)

try:
    from playwright.sync_api import sync_playwright
    with sync_playwright() as p:
        browser = p.chromium.launch(headless=False, args=['--enable-features=WebCodecs'])
        page = browser.new_page()
        logs = []
        page.on('console', lambda m: logs.append(m.text))
        page.goto(f'http://127.0.0.1:{PORT}/codec_probe.html')
        for _ in range(20):
            time.sleep(0.5)
            if page.evaluate('window._probeDone'): break
        result = page.inner_text('#out')
        print(result)
        browser.close()
finally:
    server.send_signal(signal.SIGTERM)
    server.wait()
