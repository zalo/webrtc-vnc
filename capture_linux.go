//go:build linux

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// captureMethod describes how we grab frames from the display on Linux.
type captureMethod int

const (
	captureX11Grab  captureMethod = iota // CPU readback via XShm
	captureKMSGrab                       // DRM/KMS framebuffer — GPU-direct (Intel/AMD)
	capturePipeWire                      // PipeWire DMA-BUF — GPU-direct on Wayland
	captureNvFBC                         // NVIDIA Frame Buffer Capture — GPU-direct on X11
)

// osCapture holds Linux-specific Capture fields. Embedded into Capture.
type osCapture struct {
	captureMethod captureMethod
	driCard       string
	cachedMethod  captureMethod
}

func (c *Capture) captureMethodLabel() string {
	switch c.captureMethod {
	case captureNvFBC:
		return "nvfbc"
	case captureKMSGrab:
		return "kmsgrab"
	case capturePipeWire:
		return "pipewire"
	case captureX11Grab:
		return "x11grab"
	}
	return ""
}

// resolveFFmpeg prefers a local ffmpeg-cap with cap_sys_admin (for kmsgrab),
// falling back to the system ffmpeg.
func resolveFFmpeg() string {
	exe, _ := os.Executable()
	dir := filepath.Dir(exe)
	local := filepath.Join(dir, "ffmpeg-cap")
	if _, err := os.Stat(local); err == nil {
		log.Printf("Using local ffmpeg with capabilities: %s", local)
		return local
	}
	return "ffmpeg"
}

func signalIDR(p *os.Process) {
	_ = p.Signal(syscall.SIGUSR1)
}

func (c *Capture) subprocessEnv() []string {
	return append(os.Environ(), "DISPLAY="+c.config.Display)
}

func audioInputArgs() []string {
	return []string{"-f", "pulse", "-i", "default"}
}

func (c *Capture) handleConsecutiveFailures(n int) {
	if n >= 3 && c.cachedMethod != captureX11Grab {
		log.Printf("Capture method failed %d times in a row, falling back to x11grab", n)
		c.cachedMethod = captureX11Grab
	}
}

// findDRICard finds the first /dev/dri/cardN device with a connected display output.
func findDRICard() string {
	entries, err := os.ReadDir("/dev/dri")
	if err != nil {
		return ""
	}
	for _, e := range entries {
		name := e.Name()
		if len(name) < 5 || name[:4] != "card" {
			continue
		}
		devPath := "/dev/dri/" + name
		connectors, _ := filepath.Glob(fmt.Sprintf("/sys/class/drm/%s-*/status", name))
		for _, statusFile := range connectors {
			data, err := os.ReadFile(statusFile)
			if err == nil && strings.TrimSpace(string(data)) == "connected" {
				log.Printf("DRI: %s has connected output (%s)", devPath, filepath.Base(filepath.Dir(statusFile)))
				return devPath
			}
		}
		if len(connectors) == 0 {
			return devPath
		}
	}
	return ""
}

// detectCaptureMethod probes for GPU-direct capture, falling back to x11grab.
// Priority: nvfbc > kmsgrab > pipewire > x11grab.
func (c *Capture) detectCaptureMethod() captureMethod {
	exe, _ := os.Executable()
	for _, binName := range []string{"nvfbc_nvenc", "nvfbc_capture"} {
		nvfbcBin := filepath.Join(filepath.Dir(exe), binName)
		if _, err := os.Stat(nvfbcBin); err != nil {
			continue
		}
		cmd := exec.Command(nvfbcBin, "-w", "256", "-h", "256", "-f", "1", "-b", "1000", "-n", "1")
		cmd.Env = append(os.Environ(), "DISPLAY="+c.config.Display)
		var probeStderr strings.Builder
		cmd.Stderr = &probeStderr
		out, err := cmd.Output()
		if err == nil && len(out) > 0 {
			if binName == "nvfbc_nvenc" {
				log.Printf("Capture: NvFBC+NVENC available (zero-copy GPU pipeline)")
			} else {
				log.Printf("Capture: NvFBC available (GPU capture, CPU encode)")
			}
			return captureNvFBC
		}
		log.Printf("Capture: %s probe failed: %s", binName, strings.TrimSpace(probeStderr.String()))
	}

	card := findDRICard()
	if card != "" {
		c.driCard = card

		isNvidia := false
		if data, err := os.ReadFile("/sys/class/drm/" + filepath.Base(card) + "/device/vendor"); err == nil {
			isNvidia = strings.TrimSpace(string(data)) == "0x10de"
		}
		isXorg := os.Getenv("WAYLAND_DISPLAY") == "" && os.Getenv("XDG_SESSION_TYPE") != "wayland"

		if isNvidia && isXorg {
			log.Printf("Capture: skipping kmsgrab (NVIDIA + Xorg doesn't expose DRM planes)")
		} else {
			cmd := exec.Command(c.ffmpegBin, "-hide_banner", "-loglevel", "error",
				"-device", card, "-f", "kmsgrab", "-i", "-",
				"-frames:v", "1", "-vf", "hwdownload,format=bgr0", "-f", "null", "-")
			out, err := cmd.CombinedOutput()
			if err == nil {
				log.Printf("Capture: kmsgrab available on %s (GPU-direct, zero-copy)", card)
				return captureKMSGrab
			}
			outStr := strings.TrimSpace(string(out))
			if outStr != "" {
				log.Printf("Capture: kmsgrab probe failed: %s", outStr)
			}
		}
	}

	if os.Getenv("WAYLAND_DISPLAY") != "" || os.Getenv("XDG_SESSION_TYPE") == "wayland" {
		cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
			"-f", "pipewire", "-framerate", "1", "-i", "default",
			"-frames:v", "1", "-f", "null", "-")
		if err := cmd.Run(); err == nil {
			log.Printf("Capture: PipeWire available (DMA-BUF, GPU-direct possible)")
			return capturePipeWire
		}
	}

	log.Printf("Capture: falling back to x11grab (CPU readback)")
	return captureX11Grab
}

// detectEncoder picks the best Linux H.264 encoder, falling back to libx264.
func (c *Capture) detectEncoder() encoderInfo {
	encoders := []encoderInfo{
		{"h264_nvenc", []string{"-c:v", "h264_nvenc", "-preset", "p1", "-tune", "ull",
			"-profile:v", "baseline", "-rc", "cbr"}},
		{"h264_vaapi", []string{"-vaapi_device", "/dev/dri/renderD128",
			"-c:v", "h264_vaapi", "-profile:v", "constrained_baseline",
			"-rc_mode", "CBR"}},
		{"h264_v4l2m2m", []string{"-c:v", "h264_v4l2m2m", "-profile:v", "baseline"}},
		{"libx264", []string{"-c:v", "libx264", "-preset", "ultrafast",
			"-tune", "zerolatency", "-profile:v", "baseline"}},
	}

	if c.config.Encoder != "auto" && c.config.Encoder != "software" {
		for _, enc := range encoders {
			if strings.Contains(enc.name, c.config.Encoder) {
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
		probeInput := []string{"-hide_banner", "-loglevel", "error",
			"-f", "lavfi", "-i", "nullsrc=s=256x256:d=0.1"}
		if enc.name == "h264_v4l2m2m" {
			probeInput = append(probeInput, "-pix_fmt", "yuv420p")
		}
		probeArgs := append(probeInput, enc.args...)
		probeArgs = append(probeArgs, "-frames:v", "1", "-f", "null", "-")
		cmd := exec.Command("ffmpeg", probeArgs...)
		if err := cmd.Run(); err == nil {
			log.Printf("Encoder: %s detected", enc.name)
			return enc
		}
	}

	log.Printf("Encoder: using software fallback (libx264)")
	return encoders[len(encoders)-1]
}

// getNativeResolution reads the current mode from the DRI card's first connected output.
func getNativeResolution(driCard string) (int, int) {
	if driCard == "" {
		return 0, 0
	}
	cardName := filepath.Base(driCard)
	connectors, _ := filepath.Glob(fmt.Sprintf("/sys/class/drm/%s-*/status", cardName))
	for _, statusFile := range connectors {
		data, _ := os.ReadFile(statusFile)
		if strings.TrimSpace(string(data)) != "connected" {
			continue
		}
		modesFile := filepath.Join(filepath.Dir(statusFile), "modes")
		modesData, err := os.ReadFile(modesFile)
		if err != nil {
			continue
		}
		firstLine := strings.TrimSpace(strings.SplitN(string(modesData), "\n", 2)[0])
		var w, h int
		if n, _ := fmt.Sscanf(firstLine, "%dx%d", &w, &h); n == 2 {
			log.Printf("Native resolution: %dx%d (from %s)", w, h, filepath.Base(filepath.Dir(statusFile)))
			return w, h
		}
	}
	return 0, 0
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
	var args []string
	args = append(args, "-hide_banner", "-loglevel", "error")

	switch method {
	case captureNvFBC:
		exe, _ := os.Executable()
		nvfbcNvenc := filepath.Join(filepath.Dir(exe), "nvfbc_nvenc")
		if _, err := os.Stat(nvfbcNvenc); err == nil {
			p.captureCmd = nvfbcNvenc
			p.captureArgs = []string{
				"-w", fmt.Sprintf("%d", c.config.Width),
				"-h", fmt.Sprintf("%d", c.config.Height),
				"-f", fmt.Sprintf("%d", c.config.FPS),
				"-b", fmt.Sprintf("%d", c.config.Bitrate),
			}
			p.ffmpegArgs = nil
			return p
		}

		p.captureCmd = filepath.Join(filepath.Dir(exe), "nvfbc_capture")
		p.captureArgs = []string{
			"-w", fmt.Sprintf("%d", c.config.Width),
			"-h", fmt.Sprintf("%d", c.config.Height),
			"-f", fmt.Sprintf("%d", c.config.FPS),
		}
		args = append(args,
			"-f", "rawvideo",
			"-pix_fmt", "nv12",
			"-video_size", fmt.Sprintf("%dx%d", c.config.Width, c.config.Height),
			"-framerate", fmt.Sprintf("%d", c.config.FPS),
			"-i", "pipe:0",
		)

	case captureKMSGrab:
		args = append(args,
			"-device", c.driCard,
			"-f", "kmsgrab",
			"-framerate", fmt.Sprintf("%d", c.config.FPS),
			"-i", "-",
		)
		nativeW, nativeH := getNativeResolution(c.driCard)
		needsScale := nativeW != c.config.Width || nativeH != c.config.Height
		if nativeW == 0 {
			needsScale = true
		}
		switch enc.name {
		case "h264_nvenc":
			if needsScale {
				args = append(args, "-vf",
					fmt.Sprintf("hwmap=derive_device=cuda,scale_cuda=%d:%d:format=nv12", c.config.Width, c.config.Height))
			} else {
				args = append(args, "-vf", "hwmap=derive_device=cuda,format=cuda")
			}
		case "h264_vaapi":
			if needsScale {
				args = append(args, "-vf",
					fmt.Sprintf("hwmap=derive_device=vaapi,scale_vaapi=%d:%d:format=nv12", c.config.Width, c.config.Height))
			} else {
				args = append(args, "-vf", "hwmap=derive_device=vaapi,format=vaapi")
			}
		default:
			if needsScale {
				args = append(args, "-vf",
					fmt.Sprintf("hwdownload,format=bgr0,scale=%d:%d", c.config.Width, c.config.Height))
			} else {
				args = append(args, "-vf", "hwdownload,format=bgr0")
			}
		}

	case capturePipeWire:
		args = append(args,
			"-f", "pipewire",
			"-framerate", fmt.Sprintf("%d", c.config.FPS),
			"-video_size", fmt.Sprintf("%dx%d", c.config.Width, c.config.Height),
			"-i", "default",
		)
		if enc.name == "h264_vaapi" {
			args = append(args, "-vf", "format=nv12,hwupload")
		}

	default: // captureX11Grab
		args = append(args,
			"-f", "x11grab",
			"-video_size", fmt.Sprintf("%dx%d", c.config.Width, c.config.Height),
			"-framerate", fmt.Sprintf("%d", c.config.FPS),
			"-i", c.config.Display,
		)
		switch enc.name {
		case "h264_vaapi":
			args = append(args, "-vf", "format=nv12,hwupload")
		case "h264_nvenc":
			args = append(args, "-pix_fmt", "nv12")
		default:
			args = append(args, "-pix_fmt", "yuv420p")
		}
	}

	args = append(args, enc.args...)
	args = append(args,
		"-b:v", fmt.Sprintf("%dk", c.config.Bitrate),
		"-maxrate", fmt.Sprintf("%dk", c.config.Bitrate),
		"-bufsize", fmt.Sprintf("%dk", c.config.Bitrate/2),
		"-g", fmt.Sprintf("%d", c.config.FPS),
		"-keyint_min", fmt.Sprintf("%d", c.config.FPS),
	)
	args = append(args, "-f", "h264", "pipe:1")

	p.ffmpegArgs = args
	return p
}
