package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Peer represents a connected client.
type Peer struct {
	ID              string
	Name            string
	Slot            int  // 0=spectator, 1-4=player
	IsHost          bool
	KeyboardEnabled bool
	MouseEnabled    bool
	GamepadSlots    map[int]bool // server gamepad slot → claimed

	conn       *websocket.Conn
	pc         *webrtc.PeerConnection
	dc         *webrtc.DataChannel // input channel
	videoDC    *webrtc.DataChannel // video frames channel
	room       *Room
	videoDCMode string // "reliable-ordered", "reliable-unordered", "unreliable-ordered", "unreliable-unordered"
}

// Room manages the streaming session and all connected peers.
type Room struct {
	mu    sync.RWMutex
	peers map[string]*Peer
	host    *Peer
	input   *Input
	capture *Capture

	videoTrack    *webrtc.TrackLocalStaticRTP
	audioTrack    *webrtc.TrackLocalStaticRTP
	videoSender   *VideoRTPSender

	// Quality settings (controlled by host)
	bitrate   int
	framerate int
	width     int
	height    int

	// Default guest permissions (toggled by host)
	guestKeyboard bool
	guestMouse    bool

	// Next peer ID counter
	nextPeerID int

	// Gamepad slot allocation (0-3)
	gamepadSlots [4]*Peer
}

func NewRoom(input *Input) (*Room, error) {
	videoTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
		"video", "screen",
	)
	if err != nil {
		return nil, err
	}

	audioTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
		"audio", "screen",
	)
	if err != nil {
		return nil, err
	}

	return &Room{
		peers:         make(map[string]*Peer),
		input:         input,
		videoTrack:    videoTrack,
		audioTrack:    audioTrack,
		bitrate:       3000,
		framerate:     60,
		width:         1920,
		height:        1080,
		guestKeyboard: true,
		guestMouse:    true,
	}, nil
}

// maxDCMessageSize is the max size for a single DataChannel message.
// SCTP fragments larger messages, but some browsers have limits.
// We chunk frames larger than this and the JS side reassembles them.
const maxDCMessageSize = 64 * 1024 // 64KB

// BroadcastVideoFrame sends an H.264 access unit to all connected peers
// via data channels. Large frames are chunked with a simple protocol:
//   - Chunk: [0x01][uint32 BE total_len][chunk_data...]
//   - Complete small frame: [0x00][frame_data...]
func (r *Room) BroadcastVideoFrame(data []byte) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Count active peers first
	var activePeers []*Peer
	for _, p := range r.peers {
		if p.videoDC != nil && p.videoDC.ReadyState() == webrtc.DataChannelStateOpen {
			activePeers = append(activePeers, p)
		}
	}
	if len(activePeers) == 0 {
		return false
	}

	// Build messages once, send to all peers
	if len(data) <= maxDCMessageSize-1 {
		msg := make([]byte, 1+len(data))
		msg[0] = 0x00
		copy(msg[1:], data)
		for _, p := range activePeers {
			p.videoDC.Send(msg)
		}
	} else {
		totalLen := len(data)
		for offset := 0; offset < totalLen; {
			end := offset + maxDCMessageSize - 5
			if end > totalLen {
				end = totalLen
			}
			chunk := make([]byte, 5+(end-offset))
			chunk[0] = 0x01
			chunk[1] = byte(totalLen >> 24)
			chunk[2] = byte(totalLen >> 16)
			chunk[3] = byte(totalLen >> 8)
			chunk[4] = byte(totalLen)
			copy(chunk[5:], data[offset:end])
			for _, p := range activePeers {
				p.videoDC.Send(chunk)
			}
			offset = end
		}
	}
	return true
}

func (r *Room) SetCapture(c *Capture) {
	r.capture = c
}

func (r *Room) VideoTrack() *webrtc.TrackLocalStaticRTP {
	return r.videoTrack
}

func (r *Room) VideoSender() *VideoRTPSender {
	return r.videoSender
}

func (r *Room) InitVideoSender(fps int) {
	r.videoSender = NewVideoRTPSender(r.videoTrack, fps)
}

func (r *Room) AudioTrack() *webrtc.TrackLocalStaticRTP {
	return r.audioTrack
}

func (r *Room) HandleWebSocket(w http.ResponseWriter, req *http.Request) {
	conn, err := upgrader.Upgrade(w, req, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}

	log.Printf("New WebSocket connection from %s", conn.RemoteAddr())

	// WebSocket keepalive: send pings every 5s, timeout after 20s.
	// Cloudflared kills idle WebSockets aggressively, so ping frequently.
	conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(20 * time.Second))
		return nil
	})

	// Ping ticker in background
	pingDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := writeControl(conn, websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
					return
				}
			case <-pingDone:
				return
			}
		}
	}()
	defer close(pingDone)

	// Read messages
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			if !websocket.IsCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Printf("WebSocket read error: %v", err)
			}
			r.handleDisconnect(conn)
			return
		}

		// Reset read deadline on any message
		conn.SetReadDeadline(time.Now().Add(30 * time.Second))

		var msg map[string]interface{}
		if err := json.Unmarshal(data, &msg); err != nil {
			log.Printf("Invalid JSON: %v", err)
			continue
		}

		msgType, _ := msg["type"].(string)
		r.handleMessage(conn, msgType, msg)
	}
}

func (r *Room) handleMessage(conn *websocket.Conn, msgType string, msg map[string]interface{}) {
	switch msgType {
	case "join":
		r.handleJoin(conn, msg)
	case "sdp":
		r.handleSDP(conn, msg)
	case "ice":
		r.handleICE(conn, msg)
	case "leave_room":
		r.handleDisconnect(conn)
	case "join_as_player":
		r.handleJoinAsPlayer(conn)
	case "claim_gamepad":
		r.handleClaimGamepad(conn, msg)
	case "release_gamepad":
		r.handleReleaseGamepad(conn, msg)
	case "set_quality":
		r.handleSetQuality(conn, msg)
	case "set_guest_keyboard":
		r.handleSetGuestKeyboard(conn, msg)
	case "set_guest_mouse":
		r.handleSetGuestMouse(conn, msg)
	case "request_idr":
		if r.capture != nil {
			r.capture.RequestIDR()
		}
	case "reconnect":
		r.handleReconnect(conn)
	default:
		log.Printf("Unknown message type: %s", msgType)
	}
}

func (r *Room) handleJoin(conn *websocket.Conn, msg map[string]interface{}) {
	name, _ := msg["player_name"].(string)
	if name == "" {
		name = "Player"
	}

	r.mu.Lock()

	r.nextPeerID++
	peerID := peerIDFromInt(r.nextPeerID)

	isHost := r.host == nil
	slot := 0
	if isHost {
		slot = 1 // Host is always Player 1
	}

	videoDCMode, _ := msg["video_dc_mode"].(string)
	if videoDCMode == "" {
		videoDCMode = "reliable-ordered"
	}

	peer := &Peer{
		ID:              peerID,
		Name:            name,
		Slot:            slot,
		IsHost:          isHost,
		KeyboardEnabled: isHost || r.guestKeyboard,
		MouseEnabled:    isHost || r.guestMouse,
		GamepadSlots:    make(map[int]bool),
		videoDCMode:     videoDCMode,
		conn:            conn,
		room:            r,
	}

	r.peers[peerID] = peer
	if isHost {
		r.host = peer
	}

	players := r.buildPlayerList()
	r.mu.Unlock()

	log.Printf("Peer %s (%s) joined as %s (slot %d)", peerID, name, roleStr(isHost), slot)

	// Send room created/joined response
	responseType := "room_joined"
	if isHost {
		responseType = "room_created"
	}

	sendJSON(conn, map[string]interface{}{
		"type":             responseType,
		"room_code":        "STREAM",
		"peer_id":          peerID,
		"player_slot":      slot,
		"is_host":          isHost,
		"is_spectator":     slot == 0,
		"keyboard_enabled": peer.KeyboardEnabled,
		"mouse_enabled":    peer.MouseEnabled,
		"players":          players,
		"video_settings": map[string]interface{}{
			"bitrate":   r.bitrate,
			"framerate": r.framerate,
			"width":     r.width,
			"height":    r.height,
		},
	})

	// Notify other peers
	r.broadcastExcept(peerID, map[string]interface{}{
		"type": "player_joined",
		"player": map[string]interface{}{
			"peer_id":       peerID,
			"name":          name,
			"slot":          slot,
			"is_host":       isHost,
			"is_spectator":  slot == 0,
			"gamepad_count": 0,
		},
	})

	// Set up WebRTC peer connection
	go r.setupPeerConnection(peer)
}

func (r *Room) setupPeerConnection(peer *Peer) {
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	}

	pc, err := webrtc.NewPeerConnection(config)
	if err != nil {
		log.Printf("Failed to create PeerConnection for %s: %v", peer.ID, err)
		return
	}

	peer.pc = pc

	// Add both audio and video media tracks.
	// Video track is the fallback for browsers without WebCodecs (iOS, Safari).
	// Browsers with WebCodecs will use the unreliable data channel instead.
	if _, err := pc.AddTrack(r.videoTrack); err != nil {
		log.Printf("Failed to add video track: %v", err)
		return
	}
	if _, err := pc.AddTrack(r.audioTrack); err != nil {
		log.Printf("Failed to add audio track: %v", err)
		return
	}

	// Video data channel — reliability configured per peer
	var videoDCInit *webrtc.DataChannelInit
	switch peer.videoDCMode {
	case "reliable-unordered":
		ordered := false
		videoDCInit = &webrtc.DataChannelInit{Ordered: &ordered}
	case "unreliable-ordered":
		maxRetransmits := uint16(0)
		videoDCInit = &webrtc.DataChannelInit{MaxRetransmits: &maxRetransmits}
	case "unreliable-unordered":
		ordered := false
		maxRetransmits := uint16(0)
		videoDCInit = &webrtc.DataChannelInit{Ordered: &ordered, MaxRetransmits: &maxRetransmits}
	default: // "reliable-ordered"
		videoDCInit = nil
	}
	log.Printf("Video DC mode for %s: %s", peer.ID, peer.videoDCMode)
	videoDC, err := pc.CreateDataChannel("video", videoDCInit)
	if err != nil {
		log.Printf("Failed to create video data channel: %v", err)
		return
	}
	peer.videoDC = videoDC

	videoDC.OnOpen(func() {
		log.Printf("Video data channel open for peer %s, requesting IDR", peer.ID)
		if r.capture != nil {
			r.capture.RequestIDR()
		}
	})

	// Input: unreliable + unordered (same as Sunshine)
	// Old input events are stale — we only care about latest mouse/key state.
	// No retransmissions, no ordering delay = lowest latency.
	inputOrdered := false
	inputMaxRetransmits := uint16(0)
	dc, err := pc.CreateDataChannel("input", &webrtc.DataChannelInit{
		Ordered:        &inputOrdered,
		MaxRetransmits: &inputMaxRetransmits,
	})
	if err != nil {
		log.Printf("Failed to create input data channel: %v", err)
		return
	}
	peer.dc = dc

	dc.OnOpen(func() {
		log.Printf("Input data channel open for peer %s", peer.ID)
	})

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if !msg.IsString {
			r.handleInputMessage(peer, msg.Data)
		}
	})

	// ICE candidate handling
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		j := c.ToJSON()
		sendJSON(peer.conn, map[string]interface{}{
			"type":          "ice",
			"candidate":     j.Candidate,
			"sdpMid":        j.SDPMid,
			"sdpMLineIndex": j.SDPMLineIndex,
		})
	})

	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Printf("Peer %s ICE state: %s", peer.ID, state.String())
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("Peer %s connection state: %s", peer.ID, state.String())
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			r.handleDisconnect(peer.conn)
		}
	})

	// Create and send SDP offer
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		log.Printf("Failed to create offer for %s: %v", peer.ID, err)
		return
	}

	if err := pc.SetLocalDescription(offer); err != nil {
		log.Printf("Failed to set local description for %s: %v", peer.ID, err)
		return
	}

	sendJSON(peer.conn, map[string]interface{}{
		"type":     "sdp",
		"sdp_type": "offer",
		"sdp":      offer.SDP,
	})
}

func (r *Room) handleSDP(conn *websocket.Conn, msg map[string]interface{}) {
	peer := r.findPeerByConn(conn)
	if peer == nil || peer.pc == nil {
		return
	}

	sdpType, _ := msg["sdp_type"].(string)
	sdp, _ := msg["sdp"].(string)

	if sdpType == "answer" {
		answer := webrtc.SessionDescription{
			Type: webrtc.SDPTypeAnswer,
			SDP:  sdp,
		}
		if err := peer.pc.SetRemoteDescription(answer); err != nil {
			log.Printf("Failed to set remote description for %s: %v", peer.ID, err)
		}
	}
}

func (r *Room) handleICE(conn *websocket.Conn, msg map[string]interface{}) {
	peer := r.findPeerByConn(conn)
	if peer == nil || peer.pc == nil {
		return
	}

	candidate, _ := msg["candidate"].(string)
	sdpMid, _ := msg["sdpMid"].(string)

	sdpMLineIndex := uint16(0)
	if idx, ok := msg["sdpMLineIndex"].(float64); ok {
		sdpMLineIndex = uint16(idx)
	}

	ice := webrtc.ICECandidateInit{
		Candidate:     candidate,
		SDPMid:        &sdpMid,
		SDPMLineIndex: &sdpMLineIndex,
	}

	if err := peer.pc.AddICECandidate(ice); err != nil {
		log.Printf("Failed to add ICE candidate for %s: %v", peer.ID, err)
	}
}

func (r *Room) handleDisconnect(conn *websocket.Conn) {
	r.mu.Lock()
	var peer *Peer
	for _, p := range r.peers {
		if p.conn == conn {
			peer = p
			break
		}
	}
	if peer == nil {
		r.mu.Unlock()
		return
	}

	log.Printf("Peer %s (%s) disconnected", peer.ID, peer.Name)

	// Release gamepad slots
	for slot := range peer.GamepadSlots {
		r.gamepadSlots[slot] = nil
	}

	// Remove peer
	delete(r.peers, peer.ID)

	// Handle host leaving
	if peer.IsHost {
		r.host = nil
		// Promote next peer to host
		for _, p := range r.peers {
			r.host = p
			p.IsHost = true
			p.Slot = 1
			p.KeyboardEnabled = true
			p.MouseEnabled = true
			break
		}
	}

	players := r.buildPlayerList()
	r.mu.Unlock()

	// Close peer connection
	if peer.pc != nil {
		peer.pc.Close()
	}
	connMutexes.Delete(conn)
	conn.Close()

	// Notify remaining peers
	r.mu.RLock()
	for _, p := range r.peers {
		sendJSON(p.conn, map[string]interface{}{
			"type":    "player_left",
			"peer_id": peer.ID,
			"slot":    peer.Slot,
		})
		sendJSON(p.conn, map[string]interface{}{
			"type":    "room_updated",
			"players": players,
		})
		// Notify new host
		if p.IsHost && peer.IsHost {
			sendJSON(p.conn, map[string]interface{}{
				"type":             "promoted_to_player",
				"player_slot":      1,
				"keyboard_enabled": true,
				"mouse_enabled":    true,
			})
		}
	}
	r.mu.RUnlock()
}

func (r *Room) handleJoinAsPlayer(conn *websocket.Conn) {
	peer := r.findPeerByConn(conn)
	if peer == nil || peer.Slot > 0 {
		return
	}

	r.mu.Lock()
	// Find next available slot (2-4, since 1 is host)
	for slot := 2; slot <= 4; slot++ {
		taken := false
		for _, p := range r.peers {
			if p.Slot == slot {
				taken = true
				break
			}
		}
		if !taken {
			peer.Slot = slot
			peer.KeyboardEnabled = r.guestKeyboard
			peer.MouseEnabled = r.guestMouse
			break
		}
	}
	players := r.buildPlayerList()
	r.mu.Unlock()

	if peer.Slot == 0 {
		sendJSON(conn, map[string]interface{}{
			"type":    "error",
			"message": "All player slots are full",
		})
		return
	}

	sendJSON(conn, map[string]interface{}{
		"type":             "promoted_to_player",
		"player_slot":      peer.Slot,
		"keyboard_enabled": peer.KeyboardEnabled,
		"mouse_enabled":    peer.MouseEnabled,
	})

	// Broadcast updated player list
	r.broadcastAll(map[string]interface{}{
		"type":    "room_updated",
		"players": players,
	})
}

func (r *Room) handleClaimGamepad(conn *websocket.Conn, msg map[string]interface{}) {
	peer := r.findPeerByConn(conn)
	if peer == nil || peer.Slot == 0 {
		return
	}

	browserIndex := int(getFloat(msg, "browser_index"))

	r.mu.Lock()
	// Find next free gamepad slot
	serverSlot := -1
	for i := 0; i < 4; i++ {
		if r.gamepadSlots[i] == nil {
			r.gamepadSlots[i] = peer
			peer.GamepadSlots[i] = true
			serverSlot = i
			break
		}
	}
	r.mu.Unlock()

	if serverSlot < 0 {
		sendJSON(conn, map[string]interface{}{
			"type":    "error",
			"message": "All gamepad slots are full",
		})
		return
	}

	// Create virtual gamepad if input handler exists
	if r.input != nil {
		r.input.EnsureGamepad(serverSlot)
	}

	sendJSON(conn, map[string]interface{}{
		"type":          "gamepad_claimed",
		"browser_index": browserIndex,
		"server_slot":   serverSlot,
	})
}

func (r *Room) handleReleaseGamepad(conn *websocket.Conn, msg map[string]interface{}) {
	peer := r.findPeerByConn(conn)
	if peer == nil {
		return
	}

	serverSlot := int(getFloat(msg, "server_slot"))
	if serverSlot < 0 || serverSlot >= 4 {
		return
	}

	r.mu.Lock()
	if r.gamepadSlots[serverSlot] == peer {
		r.gamepadSlots[serverSlot] = nil
		delete(peer.GamepadSlots, serverSlot)
	}
	r.mu.Unlock()

	sendJSON(conn, map[string]interface{}{
		"type":        "gamepad_released",
		"server_slot": serverSlot,
	})
}

func (r *Room) handleSetQuality(conn *websocket.Conn, msg map[string]interface{}) {
	peer := r.findPeerByConn(conn)
	if peer == nil || !peer.IsHost {
		return
	}

	r.mu.Lock()
	if v := getFloat(msg, "bitrate"); v > 0 {
		r.bitrate = clampInt(int(v), 500, 150000)
	}
	if v := getFloat(msg, "framerate"); v > 0 {
		r.framerate = clampInt(int(v), 15, 240)
	}
	if v := getFloat(msg, "width"); v > 0 {
		r.width = clampInt(int(v), 640, 7680)
	}
	if v := getFloat(msg, "height"); v > 0 {
		r.height = clampInt(int(v), 480, 4320)
	}
	settings := map[string]interface{}{
		"type":      "quality_updated",
		"success":   true,
		"bitrate":   r.bitrate,
		"framerate": r.framerate,
		"width":     r.width,
		"height":    r.height,
	}
	r.mu.Unlock()

	// Broadcast quality update + stream reset to all peers
	r.broadcastAll(settings)
	// Tell clients to reset their decoders — the stream is about to restart
	r.broadcastAll(map[string]interface{}{
		"type": "stream_reset",
	})

	log.Printf("Quality updated: %dx%d@%dfps, %dkbps", r.width, r.height, r.framerate, r.bitrate)

	// Restart video capture with new settings
	if r.capture != nil {
		r.capture.RestartVideo(r.width, r.height, r.framerate, r.bitrate)
	}
}

func (r *Room) handleSetGuestKeyboard(conn *websocket.Conn, msg map[string]interface{}) {
	peer := r.findPeerByConn(conn)
	if peer == nil || !peer.IsHost {
		return
	}

	enabled, _ := msg["enabled"].(bool)
	targetPeerID, _ := msg["peer_id"].(string)

	r.mu.Lock()
	r.guestKeyboard = enabled

	if targetPeerID != "" {
		if target, ok := r.peers[targetPeerID]; ok && !target.IsHost {
			target.KeyboardEnabled = enabled
			sendJSON(target.conn, map[string]interface{}{
				"type":             "permission_changed",
				"keyboard_enabled": enabled,
			})
		}
	} else {
		// Update all non-host peers
		for _, p := range r.peers {
			if !p.IsHost && p.Slot > 0 {
				p.KeyboardEnabled = enabled
				sendJSON(p.conn, map[string]interface{}{
					"type":             "permission_changed",
					"keyboard_enabled": enabled,
				})
			}
		}
	}
	r.mu.Unlock()
}

func (r *Room) handleSetGuestMouse(conn *websocket.Conn, msg map[string]interface{}) {
	peer := r.findPeerByConn(conn)
	if peer == nil || !peer.IsHost {
		return
	}

	enabled, _ := msg["enabled"].(bool)
	targetPeerID, _ := msg["peer_id"].(string)

	r.mu.Lock()
	r.guestMouse = enabled

	if targetPeerID != "" {
		if target, ok := r.peers[targetPeerID]; ok && !target.IsHost {
			target.MouseEnabled = enabled
			sendJSON(target.conn, map[string]interface{}{
				"type":           "permission_changed",
				"mouse_enabled":  enabled,
			})
		}
	} else {
		for _, p := range r.peers {
			if !p.IsHost && p.Slot > 0 {
				p.MouseEnabled = enabled
				sendJSON(p.conn, map[string]interface{}{
					"type":          "permission_changed",
					"mouse_enabled": enabled,
				})
			}
		}
	}
	r.mu.Unlock()
}

func (r *Room) handleReconnect(conn *websocket.Conn) {
	peer := r.findPeerByConn(conn)
	if peer == nil {
		return
	}

	// Close old peer connection
	if peer.pc != nil {
		peer.pc.Close()
		peer.pc = nil
	}

	// Set up new peer connection
	go r.setupPeerConnection(peer)

	sendJSON(conn, map[string]interface{}{
		"type":             "reconnected",
		"peer_id":          peer.ID,
		"player_slot":      peer.Slot,
		"is_host":          peer.IsHost,
		"is_spectator":     peer.Slot == 0,
		"keyboard_enabled": peer.KeyboardEnabled,
		"mouse_enabled":    peer.MouseEnabled,
	})
}

func (r *Room) handleInputMessage(peer *Peer, data []byte) {
	if len(data) < 1 || r.input == nil {
		return
	}

	msgType := data[0]

	switch msgType {
	case 0x01: // Gamepad
		if len(data) < 14 || peer.Slot == 0 {
			return
		}
		slot := int(data[1])
		if slot < 0 || slot >= 4 {
			return
		}
		r.mu.RLock()
		owner := r.gamepadSlots[slot]
		r.mu.RUnlock()
		if owner != peer {
			return
		}
		r.input.HandleGamepad(slot, data)

	case 0x02: // Keyboard
		if len(data) < 5 || !peer.KeyboardEnabled || peer.Slot == 0 {
			return
		}
		r.input.HandleKeyboard(data)

	case 0x03: // Mouse move
		if len(data) < 6 || !peer.MouseEnabled || peer.Slot == 0 {
			return
		}
		r.input.HandleMouseMove(data)

	case 0x04: // Mouse button
		if len(data) < 3 || !peer.MouseEnabled || peer.Slot == 0 {
			return
		}
		r.input.HandleMouseButton(data)

	case 0x05: // Mouse scroll
		if len(data) < 6 || !peer.MouseEnabled || peer.Slot == 0 {
			return
		}
		r.input.HandleMouseScroll(data)
	}
}

// Helper functions

func (r *Room) findPeerByConn(conn *websocket.Conn) *Peer {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.peers {
		if p.conn == conn {
			return p
		}
	}
	return nil
}

func (r *Room) buildPlayerList() []map[string]interface{} {
	var players []map[string]interface{}
	for _, p := range r.peers {
		players = append(players, map[string]interface{}{
			"peer_id":       p.ID,
			"name":          p.Name,
			"slot":          p.Slot,
			"is_host":       p.IsHost,
			"is_spectator":  p.Slot == 0,
			"gamepad_count": len(p.GamepadSlots),
		})
	}
	return players
}

func (r *Room) broadcastAll(msg map[string]interface{}) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.peers {
		sendJSON(p.conn, msg)
	}
}

func (r *Room) broadcastExcept(excludeID string, msg map[string]interface{}) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.peers {
		if p.ID != excludeID {
			sendJSON(p.conn, msg)
		}
	}
}

// connMutexes protects concurrent writes to WebSocket connections.
// gorilla/websocket requires writes to be serialized.
var connMutexes sync.Map // *websocket.Conn → *sync.Mutex

func getConnMu(conn *websocket.Conn) *sync.Mutex {
	v, _ := connMutexes.LoadOrStore(conn, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func sendJSON(conn *websocket.Conn, msg map[string]interface{}) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	mu := getConnMu(conn)
	mu.Lock()
	conn.WriteMessage(websocket.TextMessage, data)
	mu.Unlock()
}

func writeControl(conn *websocket.Conn, messageType int, data []byte, deadline time.Time) error {
	mu := getConnMu(conn)
	mu.Lock()
	defer mu.Unlock()
	return conn.WriteControl(messageType, data, deadline)
}

func peerIDFromInt(n int) string {
	return "peer_" + itoa(n)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}

func roleStr(isHost bool) string {
	if isHost {
		return "host"
	}
	return "guest"
}

func getFloat(msg map[string]interface{}, key string) float64 {
	if v, ok := msg[key].(float64); ok {
		return v
	}
	return 0
}

func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
