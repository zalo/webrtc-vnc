package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4/pkg/media/h264reader"
)

// captureMethod describes how we grab frames from the display.
type captureMethod int

const (
	captureX11Grab  captureMethod = iota // CPU readback via XShm
	captureKMSGrab                       // DRM/KMS framebuffer — GPU-direct (Intel/AMD or NVIDIA+Wayland)
	capturePipeWire                      // PipeWire DMA-BUF — GPU-direct on Wayland
	captureNvFBC                         // NVIDIA Frame Buffer Capture — GPU-direct on X11
)

type CaptureConfig struct {
	Display string
	Width   int
	Height  int
	FPS     int
	Bitrate int // kbps
	Encoder string
	Audio   bool
}

type Capture struct {
	config        CaptureConfig
	room          *Room
	videoCmd      *exec.Cmd
	audioCmd      *exec.Cmd
	mu            sync.Mutex
	cancel        context.CancelFunc
	encoderName   string
	captureMethod captureMethod
	driCard       string // e.g. "/dev/dri/card1"
	ffmpegBin     string // path to ffmpeg binary (may be local copy with cap_sys_admin)

	// Cached probe results (only run once at startup)
	probeOnce      sync.Once
	cachedMethod   captureMethod
	cachedEncoder  encoderInfo

	// For signaling IDR requests to the capture process
	captureProc *os.Process
}

// ffmpeg returns the path to the ffmpeg binary to use.
// Prefers ./ffmpeg-cap (local copy with cap_sys_admin for kmsgrab) over system ffmpeg.
func resolveFFmpeg() string {
	// Check for local copy with capabilities (created by setup.sh)
	exe, _ := os.Executable()
	dir := filepath.Dir(exe)
	local := filepath.Join(dir, "ffmpeg-cap")
	if _, err := os.Stat(local); err == nil {
		log.Printf("Using local ffmpeg with capabilities: %s", local)
		return local
	}
	return "ffmpeg"
}

func NewCapture(config CaptureConfig, room *Room) *Capture {
	return &Capture{
		config:    config,
		room:      room,
		ffmpegBin: resolveFFmpeg(),
	}
}

func (c *Capture) EncoderName() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	name := c.encoderName
	switch c.captureMethod {
	case captureNvFBC:
		name += " (nvfbc)"
	case captureKMSGrab:
		name += " (kmsgrab)"
	case capturePipeWire:
		name += " (pipewire)"
	case captureX11Grab:
		name += " (x11grab)"
	}
	return name
}

func (c *Capture) Start(ctx context.Context) {
	ctx, c.cancel = context.WithCancel(ctx)
	go c.runVideoCapture(ctx)
	if c.config.Audio {
		go c.runAudioCapture(ctx)
	}
}

func (c *Capture) Stop() {
	if c.cancel != nil {
		c.cancel()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.videoCmd != nil && c.videoCmd.Process != nil {
		c.videoCmd.Process.Kill()
	}
	if c.audioCmd != nil && c.audioCmd.Process != nil {
		c.audioCmd.Process.Kill()
	}
}

// RequestIDR signals the capture process to produce an IDR (keyframe) on the next frame.
func (c *Capture) RequestIDR() {
	c.mu.Lock()
	proc := c.captureProc
	c.mu.Unlock()
	if proc != nil {
		proc.Signal(syscall.SIGUSR1)
	}
}

// RestartVideo kills the current FFmpeg process so the capture loop restarts with new settings.
func (c *Capture) RestartVideo(width, height, fps, bitrate int) {
	c.mu.Lock()
	c.config.Width = width
	c.config.Height = height
	c.config.FPS = fps
	c.config.Bitrate = bitrate
	if c.videoCmd != nil && c.videoCmd.Process != nil {
		c.videoCmd.Process.Kill()
	}
	c.mu.Unlock()
	// The runVideoCapture loop will restart automatically
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
		// Check if this card has any connected outputs
		connectors, _ := filepath.Glob(fmt.Sprintf("/sys/class/drm/%s-*/status", name))
		for _, statusFile := range connectors {
			data, err := os.ReadFile(statusFile)
			if err == nil && strings.TrimSpace(string(data)) == "connected" {
				log.Printf("DRI: %s has connected output (%s)", devPath, filepath.Base(filepath.Dir(statusFile)))
				return devPath
			}
		}
		// If no sysfs info, still return the device — might work
		if len(connectors) == 0 {
			return devPath
		}
	}
	return ""
}

// detectCaptureMethod probes for GPU-direct capture, falling back to x11grab.
// Priority: nvfbc > kmsgrab > pipewire > x11grab.
func (c *Capture) detectCaptureMethod() captureMethod {
	// NvFBC: NVIDIA's proprietary Frame Buffer Capture API.
	// Works on NVIDIA + X11, captures directly from GPU — same as Sunshine.
	// Requires libnvidia-fbc.so.1 (ships with NVIDIA driver).
	exe, _ := os.Executable()
	// Try nvfbc_nvenc first (zero-copy), fall back to nvfbc_capture (NV12 pipe)
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

	// Find the right DRI card (may be card0, card1, etc.)
	card := findDRICard()
	if card != "" {
		c.driCard = card

		// kmsgrab: only works when display compositor uses DRM planes.
		// NVIDIA + Xorg doesn't expose DRM planes, so skip.
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

	// PipeWire screen capture — works on Wayland, uses DMA-BUF so the
	// captured buffer can stay in GPU memory when paired with a hw encoder.
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

type encoderInfo struct {
	name string
	args []string
}

// detectEncoder picks the best hardware encoder, falling back to software.
func (c *Capture) detectEncoder() encoderInfo {
	encoders := []encoderInfo{
		{"h264_nvenc", []string{"-c:v", "h264_nvenc", "-preset", "p1", "-tune", "ull",
			"-profile:v", "baseline", "-rc", "cbr"}},
		{"h264_vaapi", []string{"-vaapi_device", "/dev/dri/renderD128",
			"-c:v", "h264_vaapi", "-profile:v", "constrained_baseline",
			"-rc_mode", "CBR"}},
		// Raspberry Pi 4b / VideoCore VI — V4L2 memory-to-memory HW encoder.
		// Accepts yuv420p from CPU; no special hwupload needed.
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

	// Auto-detect: try hardware encoders first
	// Use 256x256 for probe — NVENC rejects anything smaller than ~128x128
	for _, enc := range encoders {
		if enc.name == "libx264" {
			continue
		}
		// v4l2m2m needs yuv420p input; other HW encoders handle nullsrc natively
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
		// Read modes — first line is the preferred/current mode
		modesFile := filepath.Join(filepath.Dir(statusFile), "modes")
		modesData, err := os.ReadFile(modesFile)
		if err != nil {
			continue
		}
		// Format: "1920x1080" (one per line, first is preferred)
		firstLine := strings.TrimSpace(strings.SplitN(string(modesData), "\n", 2)[0])
		var w, h int
		if n, _ := fmt.Sscanf(firstLine, "%dx%d", &w, &h); n == 2 {
			log.Printf("Native resolution: %dx%d (from %s)", w, h, filepath.Base(filepath.Dir(statusFile)))
			return w, h
		}
	}
	return 0, 0
}

// videoPipeline describes how to launch the capture+encode process(es).
type videoPipeline struct {
	// For NvFBC: separate capture process piped into ffmpeg
	captureCmd  string
	captureArgs []string
	// FFmpeg args (reads from pipe:0 for NvFBC, or captures directly otherwise)
	ffmpegArgs []string
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
		// NvFBC+NVENC: zero-copy GPU pipeline. The nvfbc_nvenc binary captures
		// the framebuffer and encodes H.264 entirely on GPU — no FFmpeg needed.
		// Only the compressed bitstream touches CPU.
		exe, _ := os.Executable()
		nvfbcNvenc := filepath.Join(filepath.Dir(exe), "nvfbc_nvenc")
		if _, err := os.Stat(nvfbcNvenc); err == nil {
			// Use the all-GPU binary — outputs H.264 Annex B directly
			p.captureCmd = nvfbcNvenc
			p.captureArgs = []string{
				"-w", fmt.Sprintf("%d", c.config.Width),
				"-h", fmt.Sprintf("%d", c.config.Height),
				"-f", fmt.Sprintf("%d", c.config.FPS),
				"-b", fmt.Sprintf("%d", c.config.Bitrate),
			}
			// No ffmpeg needed — captureCmd outputs H.264 directly to stdout
			p.ffmpegArgs = nil
			return p
		}

		// Fallback: NvFBC raw NV12 → pipe → FFmpeg NVENC
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
		// KMS grab: the framebuffer is already a GPU surface.
		// With NVENC or VAAPI the encode happens entirely on GPU — zero CPU copies.
		args = append(args,
			"-device", c.driCard,
			"-f", "kmsgrab",
			"-framerate", fmt.Sprintf("%d", c.config.FPS),
			"-i", "-",
		)

		// Check if we need to scale at all
		nativeW, nativeH := getNativeResolution(c.driCard)
		needsScale := nativeW != c.config.Width || nativeH != c.config.Height
		if nativeW == 0 {
			needsScale = true // couldn't detect, scale to be safe
		}

		// The captured DRM frame is a hardware surface — keep it on GPU.
		switch enc.name {
		case "h264_nvenc":
			if needsScale {
				args = append(args, "-vf",
					fmt.Sprintf("hwmap=derive_device=cuda,scale_cuda=%d:%d:format=nv12", c.config.Width, c.config.Height))
			} else {
				// Native res — just hwmap to CUDA, no scale. Maximum throughput.
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
		// PipeWire: can provide DMA-BUF handles, keeping frames in GPU memory
		// when used with hardware encoders.
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
		// X11 grab: reads the framebuffer via XShm into CPU memory.
		// x11grab produces bgr0 (4:4:4) — must convert to nv12/yuv420p for encoding.
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
			// NVENC can accept nv12 from CPU upload
			args = append(args, "-pix_fmt", "nv12")
		default:
			// Software encoder — convert bgr0→yuv420p (baseline needs 4:2:0)
			args = append(args, "-pix_fmt", "yuv420p")
		}
	}

	// Encoder settings
	args = append(args, enc.args...)

	// Bitrate and keyframe interval
	args = append(args,
		"-b:v", fmt.Sprintf("%dk", c.config.Bitrate),
		"-maxrate", fmt.Sprintf("%dk", c.config.Bitrate),
		"-bufsize", fmt.Sprintf("%dk", c.config.Bitrate/2),
		"-g", fmt.Sprintf("%d", c.config.FPS), // keyframe every ~1 second
		"-keyint_min", fmt.Sprintf("%d", c.config.FPS),
	)

	// Output: H.264 Annex B to stdout
	args = append(args, "-f", "h264", "pipe:1")

	p.ffmpegArgs = args
	return p
}

func (c *Capture) runVideoCapture(ctx context.Context) {
	consecutiveFailures := 0

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		p := c.buildVideoPipeline()
		startTime := time.Now()

		var captureProc *exec.Cmd
		var ffmpegCmd *exec.Cmd

		if p.captureCmd != "" && p.ffmpegArgs == nil {
			// Single-process GPU pipeline: capture binary outputs H.264 directly
			log.Printf("Starting capture: %s %s",
				p.captureCmd, strings.Join(p.captureArgs, " "))

			captureProc := exec.CommandContext(ctx, p.captureCmd, p.captureArgs...)
			captureProc.Env = append(os.Environ(), "DISPLAY="+c.config.Display)
			captureProc.Stderr = os.Stderr

			stdout, err := captureProc.StdoutPipe()
			if err != nil {
				log.Printf("Failed to create stdout pipe: %v", err)
				time.Sleep(time.Second)
				continue
			}

			if err := captureProc.Start(); err != nil {
				log.Printf("Failed to start capture: %v", err)
				time.Sleep(time.Second)
				continue
			}

			c.mu.Lock()
			c.videoCmd = captureProc
			c.captureProc = captureProc.Process
			c.mu.Unlock()

			c.readH264Stream(ctx, stdout)
			captureProc.Wait()

		} else if p.captureCmd != "" {
			// Two-process pipeline: capture → pipe → ffmpeg
			log.Printf("Starting capture: %s %s | ffmpeg %s",
				p.captureCmd, strings.Join(p.captureArgs, " "),
				strings.Join(p.ffmpegArgs, " "))

			captureProc = exec.CommandContext(ctx, p.captureCmd, p.captureArgs...)
			captureProc.Env = append(os.Environ(), "DISPLAY="+c.config.Display)
			captureProc.Stderr = os.Stderr

			capStdout, err := captureProc.StdoutPipe()
			if err != nil {
				log.Printf("Failed to create capture stdout pipe: %v", err)
				time.Sleep(time.Second)
				continue
			}

			ffmpegCmd = exec.CommandContext(ctx, c.ffmpegBin, p.ffmpegArgs...)
			ffmpegCmd.Stdin = capStdout
			ffmpegCmd.Stderr = os.Stderr

			ffmpegStdout, err := ffmpegCmd.StdoutPipe()
			if err != nil {
				log.Printf("Failed to create ffmpeg stdout pipe: %v", err)
				time.Sleep(time.Second)
				continue
			}

			if err := captureProc.Start(); err != nil {
				log.Printf("Failed to start capture process: %v", err)
				time.Sleep(time.Second)
				continue
			}
			if err := ffmpegCmd.Start(); err != nil {
				log.Printf("Failed to start ffmpeg: %v", err)
				captureProc.Process.Kill()
				captureProc.Wait()
				time.Sleep(time.Second)
				continue
			}

			c.mu.Lock()
			c.videoCmd = ffmpegCmd
			c.mu.Unlock()

			c.readH264Stream(ctx, ffmpegStdout)

			ffmpegCmd.Process.Kill()
			captureProc.Process.Kill()
			ffmpegCmd.Wait()
			captureProc.Wait()
		} else {
			// Single-process: ffmpeg captures and encodes
			log.Printf("Starting video capture: ffmpeg %s", strings.Join(p.ffmpegArgs, " "))

			ffmpegCmd = exec.CommandContext(ctx, c.ffmpegBin, p.ffmpegArgs...)
			ffmpegCmd.Stderr = os.Stderr

			stdout, err := ffmpegCmd.StdoutPipe()
			if err != nil {
				log.Printf("Failed to create stdout pipe: %v", err)
				time.Sleep(time.Second)
				continue
			}

			if err := ffmpegCmd.Start(); err != nil {
				log.Printf("Failed to start ffmpeg video: %v", err)
				time.Sleep(time.Second)
				continue
			}

			c.mu.Lock()
			c.videoCmd = ffmpegCmd
			c.mu.Unlock()

			c.readH264Stream(ctx, stdout)
			ffmpegCmd.Wait()
		}

		elapsed := time.Since(startTime)
		if elapsed < 2*time.Second {
			consecutiveFailures++
			if consecutiveFailures >= 3 && c.cachedMethod != captureX11Grab {
				log.Printf("Capture method failed %d times in a row, falling back to x11grab", consecutiveFailures)
				c.cachedMethod = captureX11Grab
				consecutiveFailures = 0
			}
			time.Sleep(time.Second)
		} else {
			consecutiveFailures = 0
			time.Sleep(500 * time.Millisecond)
		}
	}
}

func (c *Capture) readH264Stream(ctx context.Context, reader io.Reader) {
	h264, err := h264reader.NewReader(reader)
	if err != nil {
		log.Printf("Failed to create H264 reader: %v", err)
		return
	}



	var latestSPS, latestPPS []byte
	var pendingNALs [][]byte
	writeCount := 0
	lastLog := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		nal, err := h264.NextNAL()
		if err != nil {
			if err != io.EOF {
				log.Printf("H264 read error: %v", err)
			}
			return
		}

		if len(nal.Data) == 0 {
			continue
		}

		nalType := nal.Data[0] & 0x1F

		if nalType == 7 {
			latestSPS = append(latestSPS[:0], nal.Data...)
		} else if nalType == 8 {
			latestPPS = append(latestPPS[:0], nal.Data...)
		}

		if nalType >= 1 && nalType <= 5 {
			isIDR := nalType == 5
			var sample []byte

			if isIDR && latestSPS != nil {
				sample = append(sample, 0x00, 0x00, 0x00, 0x01)
				sample = append(sample, latestSPS...)
			}
			if isIDR && latestPPS != nil {
				sample = append(sample, 0x00, 0x00, 0x00, 0x01)
				sample = append(sample, latestPPS...)
			}
			for _, pending := range pendingNALs {
				sample = append(sample, 0x00, 0x00, 0x00, 0x01)
				sample = append(sample, pending...)
			}
			sample = append(sample, 0x00, 0x00, 0x00, 0x01)
			sample = append(sample, nal.Data...)

			// Send via RTP (manual packetization, async) for media track
			if sender := c.room.VideoSender(); sender != nil {
				sender.SendFrame(sample, isIDR)
			}
			// Also send via DataChannel for WebCodecs browsers
			c.room.BroadcastVideoFrame(sample)

			writeCount++

			if time.Since(lastLog) >= 5*time.Second {
				elapsed := time.Since(lastLog).Seconds()
				log.Printf("Video: %.0f fps, sample=%d bytes",
					float64(writeCount)/elapsed, len(sample))
				writeCount = 0
				lastLog = time.Now()
			}

			pendingNALs = pendingNALs[:0]
		} else {
			// Non-VCL NAL (SPS, PPS, SEI, etc.) — buffer for next frame
			pendingNALs = append(pendingNALs, nal.Data)
		}
	}
}

func (c *Capture) runAudioCapture(ctx context.Context) {
	// Find a free UDP port for audio RTP
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		log.Printf("Failed to listen for audio RTP: %v", err)
		return
	}
	localAddr := conn.LocalAddr().(*net.UDPAddr)
	log.Printf("Audio RTP listener on port %d", localAddr.Port)

	// Start audio receiver goroutine
	go c.receiveAudioRTP(ctx, conn)

	for {
		select {
		case <-ctx.Done():
			conn.Close()
			return
		default:
		}

		args := []string{
			"-hide_banner", "-loglevel", "error",
			"-f", "pulse", "-i", "default",
			"-c:a", "libopus",
			"-b:a", "128k",
			"-ar", "48000",
			"-ac", "2",
			"-ssrc", "1",
			"-payload_type", "111",
			"-f", "rtp", fmt.Sprintf("rtp://127.0.0.1:%d", localAddr.Port),
		}

		log.Printf("Starting audio capture: ffmpeg %s", strings.Join(args, " "))

		cmd := exec.CommandContext(ctx, c.ffmpegBin, args...)
		if err := cmd.Start(); err != nil {
			log.Printf("Failed to start ffmpeg audio: %v", err)
			time.Sleep(time.Second)
			continue
		}

		c.mu.Lock()
		c.audioCmd = cmd
		c.mu.Unlock()

		cmd.Wait()

		select {
		case <-ctx.Done():
			conn.Close()
			return
		default:
			log.Printf("Audio capture exited, restarting...")
			time.Sleep(500 * time.Millisecond)
		}
	}
}

func (c *Capture) receiveAudioRTP(ctx context.Context, conn net.PacketConn) {
	buf := make([]byte, 1500)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		conn.SetReadDeadline(time.Now().Add(time.Second))
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			continue
		}

		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}

		if err := c.room.AudioTrack().WriteRTP(pkt); err != nil {
			// No peers connected yet
		}
	}
}
