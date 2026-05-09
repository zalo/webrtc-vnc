package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4/pkg/media/h264reader"
)

// CaptureConfig is set once at startup from CLI flags.
type CaptureConfig struct {
	Display string // X11 display id on Linux; ignored elsewhere
	Width   int
	Height  int
	FPS     int
	Bitrate int // kbps
	Encoder string
	Audio   bool
}

// encoderInfo names an H.264 encoder + the FFmpeg flags that select it.
// Native helpers (nvfbc_nvenc, screenkit_vt, dxgi_mf) bypass FFmpeg entirely
// and just use the name for telemetry.
type encoderInfo struct {
	name string
	args []string
}

// videoPipeline describes how to launch capture+encode for one platform.
//
// Three modes the runner supports:
//   - captureCmd set + ffmpegArgs nil  → single-process native helper, H.264 on stdout
//   - captureCmd set + ffmpegArgs set  → captureCmd | ffmpeg pipeline (e.g. nvfbc_capture | ffmpeg)
//   - captureCmd empty + ffmpegArgs set → ffmpeg-only (x11grab / kmsgrab / avfoundation / ddagrab)
type videoPipeline struct {
	captureCmd  string
	captureArgs []string
	ffmpegArgs  []string
}

// Capture owns the platform-specific subprocess(es) that produce H.264 and
// pushes samples into the WebRTC track.
type Capture struct {
	config        CaptureConfig
	room          *Room
	mu            sync.Mutex
	cancel        context.CancelFunc
	encoderName   string
	ffmpegBin     string
	videoCmd      *exec.Cmd
	audioCmd      *exec.Cmd
	captureProc   *os.Process

	probeOnce     sync.Once
	cachedEncoder encoderInfo

	osCapture // platform-specific fields (driCard on Linux, etc.)
}

func NewCapture(config CaptureConfig, room *Room) *Capture {
	return &Capture{
		config:    config,
		room:      room,
		ffmpegBin: resolveFFmpeg(),
	}
}

// EncoderName returns a human-readable encoder + capture method label for the UI.
func (c *Capture) EncoderName() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	name := c.encoderName
	if tag := c.captureMethodLabel(); tag != "" {
		name += " (" + tag + ")"
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
		_ = c.videoCmd.Process.Kill()
	}
	if c.audioCmd != nil && c.audioCmd.Process != nil {
		_ = c.audioCmd.Process.Kill()
	}
}

// RequestIDR signals the capture process to produce an IDR (keyframe) on the
// next frame. Implementation is platform-specific (POSIX SIGUSR1 vs. Windows
// no-op, encoders generate keyframes regularly anyway).
func (c *Capture) RequestIDR() {
	c.mu.Lock()
	proc := c.captureProc
	c.mu.Unlock()
	if proc != nil {
		signalIDR(proc)
	}
}

// RestartVideo kills the current video subprocess so the runner restarts it
// with new dimensions/bitrate.
func (c *Capture) RestartVideo(width, height, fps, bitrate int) {
	c.mu.Lock()
	c.config.Width = width
	c.config.Height = height
	c.config.FPS = fps
	c.config.Bitrate = bitrate
	if c.videoCmd != nil && c.videoCmd.Process != nil {
		_ = c.videoCmd.Process.Kill()
	}
	c.mu.Unlock()
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

		switch {
		case p.captureCmd != "" && p.ffmpegArgs == nil:
			c.runNativeCapture(ctx, p)

		case p.captureCmd != "" && p.ffmpegArgs != nil:
			c.runPipedCapture(ctx, p)

		case p.captureCmd == "" && p.ffmpegArgs != nil:
			c.runFFmpegCapture(ctx, p)

		default:
			log.Printf("Capture: no pipeline available; sleeping")
			time.Sleep(2 * time.Second)
			continue
		}

		elapsed := time.Since(startTime)
		if elapsed < 2*time.Second {
			consecutiveFailures++
			c.handleConsecutiveFailures(consecutiveFailures)
			time.Sleep(time.Second)
		} else {
			consecutiveFailures = 0
			time.Sleep(500 * time.Millisecond)
		}
	}
}

func (c *Capture) runNativeCapture(ctx context.Context, p videoPipeline) {
	log.Printf("Starting capture: %s %s", p.captureCmd, strings.Join(p.captureArgs, " "))

	cmd := exec.CommandContext(ctx, p.captureCmd, p.captureArgs...)
	cmd.Env = c.subprocessEnv()
	cmd.Stderr = os.Stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("capture: stdout pipe: %v", err)
		return
	}
	if err := cmd.Start(); err != nil {
		log.Printf("capture: start %s: %v", p.captureCmd, err)
		return
	}

	c.mu.Lock()
	c.videoCmd = cmd
	c.captureProc = cmd.Process
	c.mu.Unlock()

	c.readH264Stream(ctx, stdout)
	_ = cmd.Wait()
}

func (c *Capture) runPipedCapture(ctx context.Context, p videoPipeline) {
	log.Printf("Starting capture: %s %s | ffmpeg %s",
		p.captureCmd, strings.Join(p.captureArgs, " "), strings.Join(p.ffmpegArgs, " "))

	captureProc := exec.CommandContext(ctx, p.captureCmd, p.captureArgs...)
	captureProc.Env = c.subprocessEnv()
	captureProc.Stderr = os.Stderr

	capStdout, err := captureProc.StdoutPipe()
	if err != nil {
		log.Printf("capture: stdout pipe: %v", err)
		return
	}

	ffmpegCmd := exec.CommandContext(ctx, c.ffmpegBin, p.ffmpegArgs...)
	ffmpegCmd.Stdin = capStdout
	ffmpegCmd.Stderr = os.Stderr

	ffmpegStdout, err := ffmpegCmd.StdoutPipe()
	if err != nil {
		log.Printf("ffmpeg: stdout pipe: %v", err)
		return
	}

	if err := captureProc.Start(); err != nil {
		log.Printf("capture: start: %v", err)
		return
	}
	if err := ffmpegCmd.Start(); err != nil {
		log.Printf("ffmpeg: start: %v", err)
		_ = captureProc.Process.Kill()
		_ = captureProc.Wait()
		return
	}

	c.mu.Lock()
	c.videoCmd = ffmpegCmd
	c.captureProc = captureProc.Process
	c.mu.Unlock()

	c.readH264Stream(ctx, ffmpegStdout)

	_ = ffmpegCmd.Process.Kill()
	_ = captureProc.Process.Kill()
	_ = ffmpegCmd.Wait()
	_ = captureProc.Wait()
}

func (c *Capture) runFFmpegCapture(ctx context.Context, p videoPipeline) {
	log.Printf("Starting video capture: ffmpeg %s", strings.Join(p.ffmpegArgs, " "))

	cmd := exec.CommandContext(ctx, c.ffmpegBin, p.ffmpegArgs...)
	cmd.Stderr = os.Stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("ffmpeg: stdout pipe: %v", err)
		return
	}
	if err := cmd.Start(); err != nil {
		log.Printf("ffmpeg: start: %v", err)
		return
	}

	c.mu.Lock()
	c.videoCmd = cmd
	c.captureProc = nil
	c.mu.Unlock()

	c.readH264Stream(ctx, stdout)
	_ = cmd.Wait()
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

	var lastFrameTime time.Time
	defaultFrameDuration := time.Second / time.Duration(c.config.FPS)
	if defaultFrameDuration <= 0 {
		defaultFrameDuration = time.Second / 60
	}

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
		switch nalType {
		case 7:
			latestSPS = append(latestSPS[:0], nal.Data...)
		case 8:
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

			now := time.Now()
			frameDuration := defaultFrameDuration
			if !lastFrameTime.IsZero() {
				if d := now.Sub(lastFrameTime); d > 0 && d < time.Second {
					frameDuration = d
				}
			}
			lastFrameTime = now
			_ = c.room.WriteVideoSample(sample, frameDuration)

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
			pendingNALs = append(pendingNALs, nal.Data)
		}
	}
}

func (c *Capture) runAudioCapture(ctx context.Context) {
	inputArgs := audioInputArgs()
	if inputArgs == nil {
		log.Printf("Audio: not supported on this platform; skipping")
		return
	}

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		log.Printf("Failed to listen for audio RTP: %v", err)
		return
	}
	localAddr := conn.LocalAddr().(*net.UDPAddr)
	log.Printf("Audio RTP listener on port %d", localAddr.Port)

	go c.receiveAudioRTP(ctx, conn)

	for {
		select {
		case <-ctx.Done():
			conn.Close()
			return
		default:
		}

		args := append([]string{"-hide_banner", "-loglevel", "error"}, inputArgs...)
		args = append(args,
			"-c:a", "libopus",
			"-b:a", "128k",
			"-ar", "48000",
			"-ac", "2",
			"-ssrc", "1",
			"-payload_type", "111",
			"-f", "rtp", fmt.Sprintf("rtp://127.0.0.1:%d", localAddr.Port),
		)

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

		_ = cmd.Wait()

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

		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
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
		_ = c.room.AudioTrack().WriteRTP(pkt)
	}
}
