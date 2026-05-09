package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// trycloudflareURLPattern matches https://*.trycloudflare.com URLs in cloudflared
// output. cloudflared boxes the URL in an ASCII frame, so we just scan for the
// URL itself rather than parsing the box.
var trycloudflareURLPattern = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

// findCloudflaredBinary locates the cloudflared executable.
// Search order:
//  1. ./cloudflared-bin (built from third_party/cloudflared submodule)
//  2. $PATH cloudflared
func findCloudflaredBinary() (string, error) {
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "cloudflared-bin")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	if wd, err := os.Getwd(); err == nil {
		candidate := filepath.Join(wd, "cloudflared-bin")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	if path, err := exec.LookPath("cloudflared"); err == nil {
		return path, nil
	}
	return "", errors.New("cloudflared binary not found (build the submodule with `go build -o cloudflared-bin ./third_party/cloudflared/cmd/cloudflared`)")
}

// Tunnel manages a cloudflared quick tunnel with auto-restart. The trycloudflare
// API is occasionally flaky (returns 500 with HTML), and cloudflared makes only
// a single attempt before exiting — so we supervise it and relaunch as needed.
type Tunnel struct {
	ctx    context.Context
	cancel context.CancelFunc
	bin    string
	port   int

	mu     sync.Mutex
	url    string
	urlCh  chan string
	cmd    *exec.Cmd
	closed bool
}

// StartCloudflaredTunnel begins a supervised cloudflared quick tunnel. The
// returned Tunnel publishes its public URL via WaitForURL once cloudflared
// reports one. On unexpected cloudflared exits the tunnel is relaunched.
func StartCloudflaredTunnel(parent context.Context, port int) (*Tunnel, error) {
	bin, err := findCloudflaredBinary()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	t := &Tunnel{
		ctx:    ctx,
		cancel: cancel,
		bin:    bin,
		port:   port,
		urlCh:  make(chan string, 1),
	}
	go t.supervise()
	return t, nil
}

func (t *Tunnel) supervise() {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for {
		if t.ctx.Err() != nil {
			return
		}
		hadURL, err := t.runOnce()
		if t.ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("[cloudflared] run failed: %v", err)
		} else if hadURL {
			log.Printf("[cloudflared] tunnel exited; relaunching (URL will change)")
			t.mu.Lock()
			t.url = ""
			t.mu.Unlock()
			backoff = time.Second
		} else {
			log.Printf("[cloudflared] no URL yet; retrying in %s", backoff)
		}
		select {
		case <-time.After(backoff):
		case <-t.ctx.Done():
			return
		}
		if !hadURL {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// runOnce launches cloudflared once. Returns hadURL=true if a URL was published
// before this attempt exited.
func (t *Tunnel) runOnce() (bool, error) {
	args := []string{
		"tunnel",
		"--no-autoupdate",
		"--url", fmt.Sprintf("http://localhost:%d", t.port),
	}
	cmd := exec.CommandContext(t.ctx, t.bin, args...)

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return false, fmt.Errorf("stderr pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return false, fmt.Errorf("start cloudflared: %w", err)
	}

	t.mu.Lock()
	t.cmd = cmd
	urlSeenBefore := t.url != ""
	t.mu.Unlock()

	wg := sync.WaitGroup{}
	wg.Add(2)
	go func() { defer wg.Done(); t.scan(stderr, "stderr") }()
	go func() { defer wg.Done(); t.scan(stdout, "stdout") }()

	wg.Wait()
	waitErr := cmd.Wait()

	t.mu.Lock()
	hadURL := t.url != "" && !urlSeenBefore
	t.cmd = nil
	t.mu.Unlock()

	if waitErr != nil && t.ctx.Err() == nil {
		return hadURL, fmt.Errorf("cloudflared exited: %w", waitErr)
	}
	return hadURL, nil
}

func (t *Tunnel) scan(r io.Reader, tag string) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		log.Printf("[cloudflared %s] %s", tag, line)
		if match := trycloudflareURLPattern.FindString(line); match != "" {
			t.mu.Lock()
			if t.url == "" {
				t.url = match
				select {
				case t.urlCh <- match:
				default:
				}
			}
			t.mu.Unlock()
		}
	}
}

// WaitForURL blocks until cloudflared prints the trycloudflare URL, the
// timeout elapses, or the tunnel is closed.
func (t *Tunnel) WaitForURL(timeout time.Duration) (string, error) {
	t.mu.Lock()
	if t.url != "" {
		u := t.url
		t.mu.Unlock()
		return u, nil
	}
	t.mu.Unlock()
	select {
	case u := <-t.urlCh:
		return u, nil
	case <-t.ctx.Done():
		return "", t.ctx.Err()
	case <-time.After(timeout):
		return "", fmt.Errorf("timeout after %s waiting for cloudflared tunnel URL", timeout)
	}
}

// URL returns the current tunnel URL, or empty string if not yet known.
func (t *Tunnel) URL() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.url
}

// Close terminates the tunnel and stops the supervisor.
func (t *Tunnel) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	cmd := t.cmd
	t.mu.Unlock()

	t.cancel()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _, _ = cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
		}
	}
	return nil
}
