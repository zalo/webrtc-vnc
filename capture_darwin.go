//go:build darwin

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// captureMethod (macOS) — picks between the native helper and ffmpeg fallback.
type captureMethod int

const (
	captureScreenKitVT captureMethod = iota // native screenkit_vt: ScreenCaptureKit + VideoToolbox
	captureAVFoundation                     // ffmpeg -f avfoundation + h264_videotoolbox
)

type osCapture struct {
	captureMethod captureMethod
	cachedMethod  captureMethod
}

func (c *Capture) captureMethodLabel() string {
	switch c.captureMethod {
	case captureScreenKitVT:
		return "screenkit"
	case captureAVFoundation:
		return "avfoundation"
	}
	return ""
}

func resolveFFmpeg() string { return "ffmpeg" }

func signalIDR(p *os.Process) {
	_ = p.Signal(syscall.SIGUSR1)
}

func (c *Capture) subprocessEnv() []string { return os.Environ() }

// audioInputArgs — AVFoundation lets ffmpeg select the default audio input
// when the audio device index is omitted (":0" = video screen 0, no audio
// would be "video:none"; "none:0" = no video, audio device 0). Adjust per
// device if needed.
func audioInputArgs() []string {
	return []string{"-f", "avfoundation", "-i", "none:0"}
}

func (c *Capture) handleConsecutiveFailures(n int) {
	if n >= 3 && c.cachedMethod != captureAVFoundation {
		log.Printf("Capture method failed %d times, falling back to avfoundation+videotoolbox", n)
		c.cachedMethod = captureAVFoundation
	}
}

func (c *Capture) detectCaptureMethod() captureMethod {
	exe, _ := os.Executable()
	helper := filepath.Join(filepath.Dir(exe), "screenkit_vt")
	if _, err := os.Stat(helper); err == nil {
		log.Printf("Capture: screenkit_vt available (ScreenCaptureKit + VideoToolbox, native)")
		return captureScreenKitVT
	}
	log.Printf("Capture: falling back to ffmpeg avfoundation + h264_videotoolbox")
	return captureAVFoundation
}

func (c *Capture) detectEncoder() encoderInfo {
	// VideoToolbox is the only HW encoder on macOS — works on every Mac since ~2013.
	enc := encoderInfo{
		name: "h264_videotoolbox",
		args: []string{
			"-c:v", "h264_videotoolbox",
			"-realtime", "1",
			"-allow_sw", "0",
			"-profile:v", "baseline",
		},
	}
	if c.config.Encoder == "software" {
		return encoderInfo{name: "libx264", args: []string{
			"-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency", "-profile:v", "baseline",
		}}
	}
	return enc
}

func (c *Capture) buildVideoPipeline() videoPipeline {
	c.probeOnce.Do(func() {
		c.cachedMethod = c.detectCaptureMethod()
		c.cachedEncoder = c.detectEncoder()
	})
	method := c.cachedMethod
	enc := c.cachedEncoder

	c.mu.Lock()
	c.encoderName = enc.name
	c.captureMethod = method
	c.mu.Unlock()

	var p videoPipeline

	switch method {
	case captureScreenKitVT:
		exe, _ := os.Executable()
		p.captureCmd = filepath.Join(filepath.Dir(exe), "screenkit_vt")
		p.captureArgs = []string{
			"-w", fmt.Sprintf("%d", c.config.Width),
			"-h", fmt.Sprintf("%d", c.config.Height),
			"-f", fmt.Sprintf("%d", c.config.FPS),
			"-b", fmt.Sprintf("%d", c.config.Bitrate),
		}
		// Native helper writes H.264 Annex B directly to stdout — no ffmpeg.
		return p

	default: // captureAVFoundation
		args := []string{
			"-hide_banner", "-loglevel", "error",
			"-f", "avfoundation",
			"-capture_cursor", "1",
			"-framerate", fmt.Sprintf("%d", c.config.FPS),
			// "Capture screen 0" is index 0 on macOS; the empty audio half ("none")
			// keeps avfoundation from probing audio devices that may need permission.
			"-i", "0:none",
		}
		// Force resolution scale if requested (avfoundation grabs at native res).
		args = append(args, "-vf", fmt.Sprintf("scale=%d:%d:flags=fast_bilinear", c.config.Width, c.config.Height))

		args = append(args, enc.args...)
		args = append(args,
			"-b:v", fmt.Sprintf("%dk", c.config.Bitrate),
			"-maxrate", fmt.Sprintf("%dk", c.config.Bitrate),
			"-bufsize", fmt.Sprintf("%dk", c.config.Bitrate/2),
			"-g", fmt.Sprintf("%d", c.config.FPS),
			"-keyint_min", fmt.Sprintf("%d", c.config.FPS),
			"-pix_fmt", "nv12",
		)
		args = append(args, "-f", "h264", "pipe:1")

		p.ffmpegArgs = args
		return p
	}
}

// silence "imported and not used" if some build paths trim helpers
var _ = exec.Command
