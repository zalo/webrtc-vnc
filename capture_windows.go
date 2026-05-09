//go:build windows

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// captureMethod (Windows) — picks between native helper and ffmpeg fallback.
type captureMethod int

const (
	captureDXGIMF  captureMethod = iota // native dxgi_mf.exe: DXGI Desktop Duplication + MediaFoundation
	captureDDAGrab                      // ffmpeg -f ddagrab + nvenc/qsv/amf/libx264 (ffmpeg 6.0+)
	captureGDIGrab                      // ffmpeg -f gdigrab CPU fallback (any ffmpeg)
)

type osCapture struct {
	captureMethod captureMethod
	cachedMethod  captureMethod
}

func (c *Capture) captureMethodLabel() string {
	switch c.captureMethod {
	case captureDXGIMF:
		return "dxgi_mf"
	case captureDDAGrab:
		return "ddagrab"
	case captureGDIGrab:
		return "gdigrab"
	}
	return ""
}

func resolveFFmpeg() string {
	exe, _ := os.Executable()
	dir := filepath.Dir(exe)
	for _, name := range []string{"ffmpeg.exe", "ffmpeg"} {
		local := filepath.Join(dir, name)
		if _, err := os.Stat(local); err == nil {
			return local
		}
	}
	return "ffmpeg"
}

// Windows has no SIGUSR1; the dxgi_mf helper accepts a 'k' byte on stdin to
// force a keyframe. The Capture struct doesn't currently hold the helper's
// stdin pipe, so for now this is a no-op — encoders generate keyframes on
// the regular GOP boundary anyway.
func signalIDR(p *os.Process) {
	_ = p
}

func (c *Capture) subprocessEnv() []string { return os.Environ() }

func audioInputArgs() []string {
	// Windows audio capture via dshow needs a specific device name, which
	// has to be enumerated. Skip by default — the user can flip this on
	// later by editing this function with their device name.
	return nil
}

func (c *Capture) handleConsecutiveFailures(n int) {
	// Walk down the fallback chain: dxgi_mf → ddagrab → gdigrab.
	if n < 3 {
		return
	}
	switch c.cachedMethod {
	case captureDXGIMF:
		next := captureDDAGrab
		if !ddagrabAvailable(c.ffmpegBin) {
			log.Printf("Capture: ddagrab not available in this ffmpeg, falling straight to gdigrab")
			next = captureGDIGrab
		} else {
			log.Printf("Capture method failed %d times, falling back to ffmpeg ddagrab", n)
		}
		c.cachedMethod = next
	case captureDDAGrab:
		log.Printf("Capture method failed %d times, falling back to ffmpeg gdigrab", n)
		c.cachedMethod = captureGDIGrab
	}
}

func (c *Capture) detectCaptureMethod() captureMethod {
	exe, _ := os.Executable()
	helper := filepath.Join(filepath.Dir(exe), "dxgi_mf.exe")
	if _, err := os.Stat(helper); err == nil {
		log.Printf("Capture: dxgi_mf available (DXGI Duplication + MediaFoundation, native)")
		return captureDXGIMF
	}
	if ddagrabAvailable(c.ffmpegBin) {
		log.Printf("Capture: falling back to ffmpeg ddagrab")
		return captureDDAGrab
	}
	log.Printf("Capture: falling back to ffmpeg gdigrab (ddagrab not in ffmpeg)")
	return captureGDIGrab
}

// ddagrabAvailable probes the local ffmpeg binary for ddagrab support.
// ddagrab landed in ffmpeg 6.0 (Feb 2023); older builds report it as an
// unknown input format.
func ddagrabAvailable(ffmpeg string) bool {
	cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error",
		"-h", "demuxer=ddagrab")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	// ffmpeg prints "Demuxer ddagrab [...]" header when the demuxer exists.
	return strings.Contains(strings.ToLower(string(out)), "demuxer ddagrab")
}

func (c *Capture) detectEncoder() encoderInfo {
	encoders := []encoderInfo{
		{"h264_nvenc", []string{"-c:v", "h264_nvenc", "-preset", "p1", "-tune", "ull",
			"-profile:v", "baseline", "-rc", "cbr"}},
		{"h264_qsv", []string{"-c:v", "h264_qsv", "-preset", "veryfast", "-profile:v", "baseline"}},
		{"h264_amf", []string{"-c:v", "h264_amf", "-quality", "speed", "-usage", "lowlatency",
			"-profile:v", "baseline"}},
		{"h264_mf", []string{"-c:v", "h264_mf", "-rate_control", "cbr", "-scenario", "live_streaming"}},
		{"libx264", []string{"-c:v", "libx264", "-preset", "ultrafast",
			"-tune", "zerolatency", "-profile:v", "baseline"}},
	}

	if c.config.Encoder != "auto" && c.config.Encoder != "software" {
		for _, enc := range encoders {
			if enc.name == "h264_"+c.config.Encoder || enc.name == c.config.Encoder {
				return enc
			}
		}
	}
	if c.config.Encoder == "software" {
		return encoders[len(encoders)-1]
	}

	for _, enc := range encoders {
		if enc.name == "libx264" {
			continue
		}
		probeArgs := []string{"-hide_banner", "-loglevel", "error",
			"-f", "lavfi", "-i", "nullsrc=s=256x256:d=0.1"}
		probeArgs = append(probeArgs, enc.args...)
		probeArgs = append(probeArgs, "-frames:v", "1", "-f", "null", "-")
		cmd := exec.Command(c.ffmpegBin, probeArgs...)
		if err := cmd.Run(); err == nil {
			log.Printf("Encoder: %s detected", enc.name)
			return enc
		}
	}
	log.Printf("Encoder: using software fallback (libx264)")
	return encoders[len(encoders)-1]
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
	case captureDXGIMF:
		exe, _ := os.Executable()
		p.captureCmd = filepath.Join(filepath.Dir(exe), "dxgi_mf.exe")
		p.captureArgs = []string{
			"-w", fmt.Sprintf("%d", c.config.Width),
			"-h", fmt.Sprintf("%d", c.config.Height),
			"-f", fmt.Sprintf("%d", c.config.FPS),
			"-b", fmt.Sprintf("%d", c.config.Bitrate),
		}
		// Native helper writes H.264 Annex B to stdout — no ffmpeg.
		return p

	case captureDDAGrab:
		args := []string{
			"-hide_banner", "-loglevel", "error",
			"-f", "ddagrab",
			"-framerate", fmt.Sprintf("%d", c.config.FPS),
			"-i", "desktop",
		}
		// ddagrab outputs BGRA on a D3D11 surface. For NVENC we can hwmap to CUDA
		// and stay on GPU; for QSV we hwmap to QSV; otherwise hwdownload.
		switch enc.name {
		case "h264_nvenc":
			args = append(args, "-vf",
				fmt.Sprintf("hwmap=derive_device=cuda,scale_cuda=%d:%d:format=nv12",
					c.config.Width, c.config.Height))
		case "h264_qsv":
			args = append(args, "-vf",
				fmt.Sprintf("hwmap=derive_device=qsv,scale_qsv=%d:%d:format=nv12",
					c.config.Width, c.config.Height))
		default:
			args = append(args, "-vf",
				fmt.Sprintf("hwdownload,format=bgra,scale=%d:%d,format=nv12",
					c.config.Width, c.config.Height))
		}
		args = append(args, enc.args...)
		args = appendCommonRateArgs(args, c.config)
		p.ffmpegArgs = args
		return p

	default: // captureGDIGrab — CPU desktop grab, works on any ffmpeg
		args := []string{
			"-hide_banner", "-loglevel", "error",
			"-f", "gdigrab",
			"-framerate", fmt.Sprintf("%d", c.config.FPS),
			"-i", "desktop",
			// gdigrab outputs BGRA in CPU memory. Convert + scale once on the CPU.
			"-vf", fmt.Sprintf("scale=%d:%d,format=nv12", c.config.Width, c.config.Height),
		}
		args = append(args, enc.args...)
		args = appendCommonRateArgs(args, c.config)
		p.ffmpegArgs = args
		return p
	}
}

func appendCommonRateArgs(args []string, cfg CaptureConfig) []string {
	args = append(args,
		"-b:v", fmt.Sprintf("%dk", cfg.Bitrate),
		"-maxrate", fmt.Sprintf("%dk", cfg.Bitrate),
		"-bufsize", fmt.Sprintf("%dk", cfg.Bitrate/2),
		"-g", fmt.Sprintf("%d", cfg.FPS),
		"-keyint_min", fmt.Sprintf("%d", cfg.FPS),
	)
	args = append(args, "-f", "h264", "pipe:1")
	return args
}
