package main

import (
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// serverLogBuf is a thread-safe ring buffer of recent log lines.
var serverLogBuf = &logBuffer{max: 500}

type logBuffer struct {
	mu    sync.Mutex
	lines []string
	max   int
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = append(b.lines, string(p))
	if len(b.lines) > b.max {
		b.lines = b.lines[len(b.lines)-b.max:]
	}
	return len(p), nil
}

func (b *logBuffer) Since(offset int) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if offset >= len(b.lines) {
		return nil
	}
	if offset < 0 {
		offset = 0
	}
	cp := make([]string, len(b.lines)-offset)
	copy(cp, b.lines[offset:])
	return cp
}

//go:embed web/*
var webFiles embed.FS

func main() {
	port := flag.Int("port", 8080, "HTTP server port")
	display := flag.String("display", "", "X11 display (default: $DISPLAY)")
	width := flag.Int("width", 854, "Capture width")
	height := flag.Int("height", 480, "Capture height")
	fps := flag.Int("fps", 144, "Capture framerate")
	bitrate := flag.Int("bitrate", 1000, "Video bitrate in kbps")
	encoder := flag.String("encoder", "auto", "Video encoder: auto, nvenc, vaapi, software")
	noAudio := flag.Bool("no-audio", false, "Disable audio capture")
	flag.Parse()

	if *display == "" {
		*display = os.Getenv("DISPLAY")
		if *display == "" {
			*display = ":0"
		}
	}

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetOutput(io.MultiWriter(os.Stderr, serverLogBuf))
	log.Printf("Starting WebRTC VNC server on port %d", *port)
	log.Printf("Capture: %dx%d@%dfps, bitrate=%dkbps, encoder=%s, display=%s",
		*width, *height, *fps, *bitrate, *encoder, *display)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create input handler (needs /dev/uinput access)
	input, err := NewInput()
	if err != nil {
		log.Printf("WARNING: Failed to create input handler: %v", err)
		log.Printf("  Input injection will be disabled (view-only mode).")
		log.Printf("  To enable input, add your user to the 'input' group:")
		log.Printf("    sudo usermod -aG input $USER")
		log.Printf("  Then log out and back in.")
		input = nil
	} else {
		defer input.Close()
	}

	// Create room/session manager with WebRTC tracks
	room, err := NewRoom(input)
	if err != nil {
		log.Fatalf("Failed to create room: %v", err)
	}

	// Start screen capture
	captureConfig := CaptureConfig{
		Display: *display,
		Width:   *width,
		Height:  *height,
		FPS:     *fps,
		Bitrate: *bitrate,
		Encoder: *encoder,
		Audio:   !*noAudio,
	}
	capture := NewCapture(captureConfig, room)
	room.SetCapture(capture)
	capture.Start(ctx)
	defer capture.Stop()

	// HTTP server
	mux := http.NewServeMux()

	// Serve frontend static files, with / serving play.html directly
	webFS, err := fs.Sub(webFiles, "web")
	if err != nil {
		log.Fatalf("Failed to create web filesystem: %v", err)
	}
	mux.Handle("/", http.FileServer(http.FS(webFS)))

	// WebSocket signaling endpoint
	mux.HandleFunc("/ws", room.HandleWebSocket)

	// API endpoints
	mux.HandleFunc("/api/encoder", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":true,"encoder":"%s"}`, capture.EncoderName())
	})

	mux.HandleFunc("/api/server-logs", func(w http.ResponseWriter, r *http.Request) {
		offset := 0
		if v := r.URL.Query().Get("offset"); v != "" {
			fmt.Sscanf(v, "%d", &offset)
		}
		logs := serverLogBuf.Since(offset)
		w.Header().Set("Content-Type", "application/json")
		data, _ := json.Marshal(map[string]interface{}{"logs": logs})
		w.Write(data)
	})

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", *port),
		Handler: mux,
	}

	// Graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		log.Println("Shutting down...")
		cancel()
		server.Close()
	}()

	log.Printf("Open http://localhost:%d in your browser", *port)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("HTTP server error: %v", err)
	}
}
