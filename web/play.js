/**
 * WebRTC VNC Client
 * Adapted from Sunshine's WebRTC player for use with the Go-based VNC server.
 */
class WebRTCVNC {
  constructor() {
    this.ws = null;
    this.pc = null;
    this.dataChannel = null;

    this.roomCode = null;
    this.playerId = null;
    this.playerSlot = 0;
    this.isHost = false;
    this.players = [];

    this.gamepads = new Map();
    this.gamepadPollingId = null;
    this.lastGamepadState = new Map();

    this.keyboardEnabled = false;
    this.mouseEnabled = false;
    this.pointerLocked = false;
    this.activePointers = new Map();

    // Trackpad mode state (mobile portrait)
    this.trackpadMode = false;
    this.virtualCursorX = 32768;
    this.virtualCursorY = 32768;
    this.trackpadSensitivity = 0.8;
    this.gestureState = 'idle';
    this.gestureTouchStart = null;
    this.gestureLastPos = null;
    this.gesturePointerId = null;
    this.gestureTimer = null;
    this.GESTURE_TAP_MAX_MOVE = 10;
    this.GESTURE_TAP_MAX_DURATION = 250;
    this.GESTURE_TAP_RETOUCH_WINDOW = 250;

    this.inertiaVelocityX = 0;
    this.inertiaVelocityY = 0;
    this.inertiaAnimationId = null;
    this.inertiaLastTime = 0;
    this.INERTIA_FRICTION = 0.94;
    this.INERTIA_MIN_VELOCITY = 0.3;

    this.stats = {
      bitrate: 0, fps: 0, rtt: 0, packetsLost: 0,
      framesDecoded: 0, lastStatsTime: 0, lastBytesReceived: 0, lastFramesDecoded: 0
    };
    this.statsIntervalId = null;

    this.freezeDetection = {
      lastFrameCount: 0, freezeStartTime: null,
      idrRequested: false, reconnectAttempted: false
    };
    this.FREEZE_THRESHOLD_MS = 500;
    this.IDR_RETRY_THRESHOLD_MS = 1000;
    this.RECONNECT_THRESHOLD_MS = 2000;
    this.ICE_CONNECTION_TIMEOUT_MS = 3000;
    this.iceConnectionTimer = null;
    this.iceRetryCount = 0;
    this.MAX_ICE_RETRIES = 10;

    this.pendingMessages = [];
    this.elements = {};

    // Signaling URL: WebSocket on same host, /ws path
    const wsProtocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    this.config = {
      signalingUrl: `${wsProtocol}//${window.location.host}/ws`,
      iceServers: [
        { urls: 'stun:stun.l.google.com:19302' },
        { urls: 'stun:stun1.l.google.com:19302' }
      ],
      gamepadPollRate: 16,
      statsUpdateRate: 1000
    };

    this.init();
  }

  init() {
    this.elements = {
      startOverlay: document.getElementById('startOverlay'),
      playerNameInput: document.getElementById('playerName'),
      connectBtn: document.getElementById('connectBtn'),
      videoContainer: document.getElementById('videoContainer'),
      videoElement: document.getElementById('videoElement'),
      sidebar: document.getElementById('sidebar'),
      sidebarToggle: document.getElementById('sidebarToggle'),
      playerList: document.getElementById('playerList'),
      joinPlayerSection: document.getElementById('joinPlayerSection'),
      joinAsPlayerBtn: document.getElementById('joinAsPlayerBtn'),
      permissionsPanel: document.getElementById('permissionsPanel'),
      qualityPanel: document.getElementById('qualityPanel'),
      bitrateSlider: document.getElementById('bitrateSlider'),
      bitrateValue: document.getElementById('bitrateValue'),
      framerateSelect: document.getElementById('framerateSelect'),
      resolutionSelect: document.getElementById('resolutionSelect'),
      applyQualityBtn: document.getElementById('applyQualityBtn'),
      statsBitrate: document.getElementById('statsBitrate'),
      statsFps: document.getElementById('statsFps'),
      statsRtt: document.getElementById('statsRtt'),
      statsPacketLoss: document.getElementById('statsPacketLoss'),
      statsEncoder: document.getElementById('statsEncoder'),
      gamepadIndicator: document.getElementById('gamepadIndicator'),
      fullscreenBtn: document.getElementById('fullscreenBtn'),
      keyboardBtn: document.getElementById('keyboardBtn'),
      mobileKeyboardInput: document.getElementById('mobileKeyboardInput'),
      leaveBtn: document.getElementById('leaveBtn'),
      connectionStatus: document.getElementById('connectionStatus'),
      allowKeyboard: document.getElementById('allowKeyboard'),
      allowMouse: document.getElementById('allowMouse'),
      consoleLog: document.getElementById('consoleLog')
    };

    // Video path: server only offers the WebRTC media track now.
    // The legacy WebCodecs-over-DataChannel path is gone.
    this.useDataChannel = false;
    this.videoDCMode = 'reliable-ordered';

    // Console capture is done inline in index.html before this script loads
    this.bindEvents();
    this.loadSavedName();
  }

  setupConsoleCapture() {
    const el = this.elements.consoleLog;
    if (!el) {
      // If element not found, don't break — just skip
      return;
    }

    const MAX_ENTRIES = 200;
    const origLog = console.log.bind(console);
    const origWarn = console.warn.bind(console);
    const origError = console.error.bind(console);
    const self = this;

    function addEntry(level, args) {
      try {
        var text = '';
        for (var i = 0; i < args.length; i++) {
          var a = args[i];
          if (i > 0) text += ' ';
          if (a === null) text += 'null';
          else if (a === undefined) text += 'undefined';
          else if (typeof a === 'object') {
            try { text += JSON.stringify(a); } catch(e) { text += String(a); }
          } else {
            text += String(a);
          }
        }

        var div = document.createElement('div');
        div.style.marginBottom = '1px';
        var d = new Date();
        var time = ('0'+d.getHours()).slice(-2)+':'+('0'+d.getMinutes()).slice(-2)+':'+('0'+d.getSeconds()).slice(-2);
        var color = level === 'error' ? '#e74c3c' : level === 'warn' ? '#f5a623' : '#aaa';
        var span1 = document.createElement('span');
        span1.style.color = '#555';
        span1.textContent = time + ' ';
        var span2 = document.createElement('span');
        span2.style.color = color;
        span2.textContent = text;
        div.appendChild(span1);
        div.appendChild(span2);
        el.appendChild(div);

        while (el.children.length > MAX_ENTRIES) el.removeChild(el.firstChild);
        el.scrollTop = el.scrollHeight;
      } catch(e2) {
        // Never let logging break the app
      }
    }

    console.log = function() { origLog.apply(console, arguments); addEntry('log', arguments); };
    console.warn = function() { origWarn.apply(console, arguments); addEntry('warn', arguments); };
    console.error = function() { origError.apply(console, arguments); addEntry('error', arguments); };

    console.log('Console capture initialized');

    window.addEventListener('error', function(e) { addEntry('error', [e.message + ' at ' + e.filename + ':' + e.lineno]); });
    window.addEventListener('unhandledrejection', function(e) { addEntry('error', ['Promise: ' + e.reason]); });

    // Fetch server-side logs periodically
    this._serverLogOffset = 0;
    this._addEntry = addEntry;
    setInterval(function() { self.fetchServerLogs(addEntry); }, 3000);
  }

  async fetchServerLogs(addEntry) {
    try {
      const res = await fetch('/api/server-logs?offset=' + this._serverLogOffset);
      if (!res.ok) return;
      const data = await res.json();
      if (data.logs) {
        for (const line of data.logs) {
          addEntry('log', ['[server] ' + line]);
        }
        this._serverLogOffset += data.logs.length;
      }
    } catch {}
  }

  bindEvents() {
    this.elements.connectBtn?.addEventListener('click', () => this.connect());
    this.elements.playerNameInput?.addEventListener('keypress', (e) => {
      if (e.key === 'Enter') this.connect();
    });

    this.elements.sidebarToggle?.addEventListener('click', () => this.toggleSidebar());
    this.elements.joinAsPlayerBtn?.addEventListener('click', () => this.requestJoinAsPlayer());
    this.elements.fullscreenBtn?.addEventListener('click', () => this.toggleFullscreen());
    this.elements.keyboardBtn?.addEventListener('click', () => this.toggleMobileKeyboard());
    this.elements.leaveBtn?.addEventListener('click', () => this.disconnect());

    if (this.elements.mobileKeyboardInput) {
      this.elements.mobileKeyboardInput.addEventListener('input', (e) => this.handleMobileKeyboardInput(e));
      this.elements.mobileKeyboardInput.addEventListener('keydown', (e) => this.handleMobileKeyDown(e));
      this.elements.mobileKeyboardInput.addEventListener('blur', () => {
        this.elements.keyboardBtn?.classList.remove('active');
      });
    }

    this.elements.allowKeyboard?.addEventListener('change', (e) => {
      this.setGuestPermission('set_guest_keyboard', e.target.checked);
    });
    this.elements.allowMouse?.addEventListener('change', (e) => {
      this.setGuestPermission('set_guest_mouse', e.target.checked);
    });

    // Re-evaluate mobile layout on rotation/resize so portrait/landscape
    // changes flip cover↔contain and recompute pan.
    const onLayoutChange = () => this.applyMobileLayout();
    window.addEventListener('resize', onLayoutChange);
    window.addEventListener('orientationchange', onLayoutChange);

    // Re-pan once we know video dimensions (videoWidth/Height arrive late).
    if (this.elements.videoElement) {
      this.elements.videoElement.addEventListener('loadedmetadata', () => this.applyMobileLayout());
      this.elements.videoElement.addEventListener('resize', () => this.applyMobileLayout());
    }


    this.elements.bitrateSlider?.addEventListener('input', (e) => {
      if (this.elements.bitrateValue) this.elements.bitrateValue.textContent = e.target.value;
    });
    this.elements.applyQualityBtn?.addEventListener('click', () => this.applyQualitySettings());

    if (this.elements.videoContainer) {
      this.elements.videoContainer.tabIndex = 0;
    }

    document.addEventListener('keydown', (e) => this.handleKeyDown(e));
    document.addEventListener('keyup', (e) => this.handleKeyUp(e));

    const videoEl = this.elements.videoElement;
    if (videoEl) {
      videoEl.style.touchAction = 'none';
      videoEl.style.userSelect = 'none';
      videoEl.style.webkitUserSelect = 'none';
      videoEl.style.webkitTouchCallout = 'none';

      videoEl.addEventListener('pointermove', (e) => this.handlePointerMove(e), { passive: false });
      videoEl.addEventListener('pointerdown', (e) => this.handlePointerDown(e), { passive: false });
      videoEl.addEventListener('pointerup', (e) => this.handlePointerUp(e), { passive: false });
      videoEl.addEventListener('pointercancel', (e) => this.handlePointerUp(e), { passive: false });
      videoEl.addEventListener('wheel', (e) => this.handleMouseWheel(e), { passive: false });

      const suppress = (e) => { e.preventDefault(); e.stopPropagation(); };
      videoEl.addEventListener('touchstart', suppress, { passive: false });
      videoEl.addEventListener('touchmove', suppress, { passive: false });
      videoEl.addEventListener('touchend', suppress, { passive: false });
      videoEl.addEventListener('touchcancel', suppress, { passive: false });
      videoEl.addEventListener('contextmenu', suppress);
    }

    document.addEventListener('pointerlockchange', () => this.handlePointerLockChange());
    window.addEventListener('gamepadconnected', (e) => this.handleGamepadConnected(e));
    window.addEventListener('gamepaddisconnected', (e) => this.handleGamepadDisconnected(e));
    document.addEventListener('visibilitychange', () => this.handleVisibilityChange());
    window.addEventListener('beforeunload', () => this.cleanup());
  }

  loadSavedName() {
    const saved = localStorage.getItem('webrtcVncName');
    if (saved && this.elements.playerNameInput) {
      this.elements.playerNameInput.value = saved;
    }
  }

  // ============== Signaling ==============

  connectSignaling() {
    return new Promise((resolve, reject) => {
      this.ws = new WebSocket(this.config.signalingUrl);
      this.ws.onopen = () => {
        console.log('Signaling connected');
        this.updateConnectionStatus('connected');
        resolve();
      };
      this.ws.onclose = (e) => {
        console.log('Signaling disconnected:', e.code, e.reason);
        this.updateConnectionStatus('disconnected');
        this.handleSignalingDisconnect();
      };
      this.ws.onerror = (e) => {
        console.error('Signaling error:', e);
        this.updateConnectionStatus('error');
        reject(e);
      };
      this.ws.onmessage = (e) => {
        try {
          this.handleSignalingMessage(JSON.parse(e.data));
        } catch (err) {
          console.error('Failed to parse signaling message:', err);
        }
      };
    });
  }

  sendSignaling(type, payload = {}) {
    if (this.ws?.readyState === WebSocket.OPEN) {
      this.ws.send(JSON.stringify({ type, ...payload }));
    }
  }

  handleSignalingMessage(msg) {
    switch (msg.type) {
      case 'room_created': this.handleRoomCreated(msg); break;
      case 'room_joined': this.handleRoomJoined(msg); break;
      case 'room_updated': this.handleRoomUpdated(msg); break;
      case 'promoted_to_player': this.handlePromotedToPlayer(msg); break;
      case 'gamepad_claimed': this.handleGamepadClaimed(msg); break;
      case 'gamepad_released': this.handleGamepadReleased(msg); break;
      case 'stream_ready': this.handleStreamReady(msg); break;
      case 'sdp': this.handleRemoteSDP(msg); break;
      case 'ice': this.handleRemoteICE(msg); break;
      case 'reconnected': this.handleReconnected(msg); break;
      case 'quality_updated': this.handleQualityUpdated(msg); break;
      case 'stream_reset': this.handleStreamReset(); break;
      case 'permission_changed': this.handlePermissionChanged(msg); break;
      case 'player_joined':
        if (msg.player) this.showNotification(`${msg.player.name} joined`);
        break;
      case 'player_left':
        this.showNotification('A player left');
        break;
      case 'error':
        this.showError(msg.message || 'An error occurred');
        break;
      default:
        console.warn('Unknown message type:', msg.type);
    }
  }

  // ============== Connection ==============

  async connect() {
    const name = this.elements.playerNameInput?.value.trim() || 'Player';
    localStorage.setItem('webrtcVncName', name);

    try {
      this.showLoading('Connecting...');
      await this.connectSignaling();
      this.sendSignaling('join', {
        player_name: name,
        video_dc_mode: this.videoDCMode || 'reliable-ordered',
      });
    } catch (err) {
      this.hideLoading();
      this.showError('Failed to connect to server');
    }
  }

  handleRoomCreated(msg) {
    this.hideLoading();
    this.roomCode = msg.room_code;
    this.playerId = msg.peer_id;
    this.playerSlot = 1;
    this.isHost = true;
    this.players = msg.players || [];
    this.keyboardEnabled = msg.keyboard_enabled ?? true;
    this.mouseEnabled = msg.mouse_enabled ?? true;

    this.initPeerConnection();
    this.showStreamUI();
    this.updateRoomUI();
    if (msg.video_settings) this.updateQualityUI(msg.video_settings);
  }

  handleRoomJoined(msg) {
    this.hideLoading();
    this.roomCode = msg.room_code;
    this.playerId = msg.peer_id;
    this.playerSlot = msg.slot || msg.player_slot || 0;
    this.isHost = msg.is_host || false;
    this.players = msg.players || [];
    this.keyboardEnabled = msg.keyboard_enabled ?? false;
    this.mouseEnabled = msg.mouse_enabled ?? false;

    this.initPeerConnection();
    this.showStreamUI();
    this.updateRoomUI();
    if (msg.video_settings) this.updateQualityUI(msg.video_settings);
  }

  handleRoomUpdated(msg) {
    this.players = msg.players || [];
    this.updatePlayerList();
  }

  requestJoinAsPlayer() {
    if (this.playerSlot === 0) {
      this.sendSignaling('join_as_player');
    }
  }

  handlePromotedToPlayer(msg) {
    this.playerSlot = msg.player_slot || msg.slot;
    if (msg.keyboard_enabled !== undefined) this.keyboardEnabled = msg.keyboard_enabled;
    if (msg.mouse_enabled !== undefined) this.mouseEnabled = msg.mouse_enabled;
    this.showNotification(`You are now Player ${this.playerSlot}`);
    this.updateRoomUI();
    this.startGamepadPolling();
  }

  handleReconnected(msg) {
    this.playerSlot = msg.player_slot || 0;
    this.isHost = msg.is_host || false;
    this.keyboardEnabled = msg.keyboard_enabled ?? false;
    this.mouseEnabled = msg.mouse_enabled ?? false;
    this.initPeerConnection();
    this.showNotification('Reconnected to stream');
    this.updateRoomUI();
  }

  disconnect() {
    this.sendSignaling('leave_room');
    this.cleanup();
    this.showStartOverlay();
  }

  // ============== WebRTC ==============

  handleStreamReady(msg) {
    if (msg.ice_servers && !this.pc) {
      this.config.iceServers = msg.ice_servers;
      this.initPeerConnection();
    }
  }

  initPeerConnection() {
    this.pc = new RTCPeerConnection({
      iceServers: this.config.iceServers,
      iceCandidatePoolSize: 10
    });

    this.pc.onicecandidate = (e) => {
      if (e.candidate) {
        this.sendSignaling('ice', {
          candidate: e.candidate.candidate,
          sdpMid: e.candidate.sdpMid,
          sdpMLineIndex: e.candidate.sdpMLineIndex
        });
      }
    };

    this.pc.oniceconnectionstatechange = () => {
      const state = this.pc.iceConnectionState;
      console.log('ICE state:', state);
      this.updateConnectionStatus(state);
      if (state === 'connected' || state === 'completed') {
        this.clearIceConnectionTimer();
        this.iceRetryCount = 0;
      }
      if (state === 'failed' || state === 'disconnected') {
        this.handleIceConnectionFailure();
      }
    };

    this.pc.ontrack = (e) => {
      console.log('Track received:', e.track.kind);

      // Minimize jitter buffer: tell the browser we want near-zero playout delay.
      // This trades smoothness for latency — exactly what we want for VNC.
      if (e.receiver && e.receiver.playoutDelayHint !== undefined) {
        e.receiver.playoutDelayHint = 0;
      }
      // Also try jitterBufferTarget (newer API)
      if (e.receiver && e.receiver.jitterBufferTarget !== undefined) {
        e.receiver.jitterBufferTarget = 0;
      }

      if (!this.elements.videoElement.srcObject) {
        this.elements.videoElement.srcObject = e.streams[0];
      }
      this.elements.videoElement.play().catch(() => {});
    };

    this.pc.ondatachannel = (e) => {
      console.log('DataChannel received:', e.channel.label, 'state:', e.channel.readyState);
      if (e.channel.label === 'input') {
        this.setupDataChannel(e.channel);
      } else if (e.channel.label === 'video') {
        this._videoDC = e.channel;
        if (this.useDataChannel) {
          this.setupVideoDataChannel(e.channel);
        } else {
          console.log('Video DC available but Media Track mode selected');
        }
      }
    };

    this.processPendingMessages();
    this.startIceConnectionTimer();
  }

  startIceConnectionTimer() {
    this.clearIceConnectionTimer();
    this.iceConnectionTimer = setTimeout(() => {
      if (this.pc && this.pc.iceConnectionState !== 'connected' && this.pc.iceConnectionState !== 'completed') {
        this.handleIceConnectionFailure();
      }
    }, this.ICE_CONNECTION_TIMEOUT_MS);
  }

  clearIceConnectionTimer() {
    if (this.iceConnectionTimer) {
      clearTimeout(this.iceConnectionTimer);
      this.iceConnectionTimer = null;
    }
  }

  handleIceConnectionFailure() {
    this.clearIceConnectionTimer();
    if (this.iceRetryCount >= this.MAX_ICE_RETRIES) {
      this.showNotification('Connection failed - please refresh');
      return;
    }
    this.iceRetryCount++;
    this.showNotification(`Reconnecting... (${this.iceRetryCount}/${this.MAX_ICE_RETRIES})`);
    if (this.pc) { this.pc.close(); this.pc = null; }
    this.sendSignaling('reconnect');
  }

  async processPendingMessages() {
    for (const pending of this.pendingMessages) {
      if (pending.type === 'sdp') await this.handleRemoteSDP(pending.msg);
      else if (pending.type === 'ice') await this.handleRemoteICE(pending.msg);
    }
    this.pendingMessages = [];
  }

  setupDataChannel(channel) {
    channel.onopen = () => {
      console.log('Data channel open');
      this.startStatsPolling();
    };
    channel.onclose = () => console.log('Data channel closed');
    channel.onerror = (e) => console.error('Data channel error:', e);
    this.dataChannel = channel;
  }

  setupVideoDataChannel(channel) {
    channel.binaryType = 'arraybuffer';

    if (typeof VideoDecoder === 'undefined' || typeof EncodedVideoChunk === 'undefined') {
      console.log('WebCodecs not available — using media track');
      return;
    }

    console.log('DC video: setting up WebCodecs pipeline');

    // Canvas for rendering decoded frames
    var canvas = document.createElement('canvas');
    // pointer-events:none so clicks pass through to the video element underneath
    // (which has the pointer event handlers attached)
    canvas.style.cssText = 'position:absolute;top:0;left:0;width:100%;height:100%;z-index:1;pointer-events:none';
    this.elements.videoContainer.appendChild(canvas);
    this._videoCanvas = canvas;
    var ctx = canvas.getContext('2d');

    var renderCount = 0, decCount = 0, recvCount = 0, lastLog = Date.now();
    var configured = false;
    var self = this;
    var latestFrame = null;
    var latestFrameW = 0, latestFrameH = 0;
    var drawScheduled = false;
    var idrCount = 0;

    var decoder = new VideoDecoder({
      output: function(frame) {
        // Keep only the latest frame. Close previous if not yet drawn.
        if (latestFrame) latestFrame.close();
        latestFrame = frame;
        latestFrameW = frame.displayWidth;
        latestFrameH = frame.displayHeight;
        // Schedule a draw on next animation frame (if not already scheduled)
        if (!drawScheduled) {
          drawScheduled = true;
          requestAnimationFrame(function() {
            drawScheduled = false;
            if (latestFrame) {
              var cw = canvas.clientWidth || latestFrameW;
              var ch = canvas.clientHeight || latestFrameH;
              if (canvas.width !== cw || canvas.height !== ch) {
                canvas.width = cw;
                canvas.height = ch;
              }
              canvas.setAttribute('data-fw', latestFrameW);
              canvas.setAttribute('data-fh', latestFrameH);

              var va = latestFrameW / latestFrameH;
              var ca = cw / ch;
              var dw, dh, dx, dy;

              if (self.trackpadMode) {
                // Cover mode: fill height, pan horizontally (like object-fit:cover)
                dh = ch;
                dw = ch * va;
                dy = 0;
                // Pan based on virtual cursor X position (0-65535 -> 0-1)
                var panPct = self.virtualCursorX / 65535;
                dx = -(dw - cw) * panPct;
              } else {
                // Contain mode: letterbox (like object-fit:contain)
                if (va > ca) {
                  dw = cw; dh = cw / va; dx = 0; dy = (ch - dh) / 2;
                } else {
                  dh = ch; dw = ch * va; dy = 0; dx = (cw - dw) / 2;
                }
              }

              ctx.fillStyle = '#000';
              ctx.fillRect(0, 0, cw, ch);
              ctx.drawImage(latestFrame, dx, dy, dw, dh);
              latestFrame.close();
              latestFrame = null;
              renderCount++;
            }
          });
        }
      },
      error: function(e) {
        console.error('VideoDecoder error: ' + (e.message || e.name || String(e)));
      }
    });

    // Request IDR when channel opens
    var onOpen = function() {
      console.log('DC video: channel open, requesting IDR');
      self.sendSignaling('request_idr');
    };
    channel.onopen = onOpen;
    if (channel.readyState === 'open') onOpen();

    channel.onclose = function() {
      console.log('DC video: channel closed');
      if (decoder.state !== 'closed') decoder.close();
      if (canvas.parentNode) canvas.parentNode.removeChild(canvas);
      self._videoCanvas = null;
    };

    // Chunk reassembly for large frames (IDRs can be 100KB+)
    var chunkBuf = null, chunkExpected = 0, chunkReceived = 0;

    channel.onmessage = function(e) {
      if (decoder.state === 'closed') return;

      // Handle stream reset (quality change) — wait for new keyframe
      if (self._streamResetPending) {
        self._streamResetPending = false;
        configured = false;
        decCount = 0;
        chunkBuf = null;
        console.log('DC: decoder reset, waiting for keyframe');
      }

      var raw = new Uint8Array(e.data);
      recvCount++;
      if (raw.length < 2) return;

      var data;
      if (raw[0] === 0x00) {
        // Complete small frame (prefix byte stripped)
        data = raw.subarray(1);
      } else if (raw[0] === 0x01) {
        // Chunk of a large frame: [0x01][4-byte total length][chunk data]
        var totalLen = (raw[1] << 24) | (raw[2] << 16) | (raw[3] << 8) | raw[4];
        var chunkData = raw.subarray(5);
        if (!chunkBuf || chunkExpected !== totalLen) {
          chunkBuf = new Uint8Array(totalLen);
          chunkExpected = totalLen;
          chunkReceived = 0;
        }
        chunkBuf.set(chunkData, chunkReceived);
        chunkReceived += chunkData.length;
        if (chunkReceived >= chunkExpected) {
          data = chunkBuf;
          chunkBuf = null;
        } else {
          return; // wait for more chunks
        }
      } else {
        return;
      }

      if (data.length < 5) return;

      // Scan for NAL types in the Annex B stream
      var hasIDR = false, hasSPS = false, hasPPS = false;
      for (var i = 0; i <= data.length - 5; i++) {
        if (data[i] === 0 && data[i+1] === 0 && data[i+2] === 0 && data[i+3] === 1) {
          var nt = data[i+4] & 0x1F;
          if (nt === 5) hasIDR = true;
          if (nt === 7) hasSPS = true;
          if (nt === 8) hasPPS = true;
        }
      }

      // Configure decoder on first keyframe using AVCC description
      if (!configured && hasSPS && hasPPS && hasIDR) {
        // Find last SPS and PPS NAL (skip duplicates)
        var spsNal = null, ppsNal = null;
        for (var i = 0; i <= data.length - 5; i++) {
          if (data[i]===0 && data[i+1]===0 && data[i+2]===0 && data[i+3]===1) {
            var nt2 = data[i+4] & 0x1F;
            if (nt2 === 7) {
              // Find end of this NAL
              var end = data.length;
              for (var j = i+5; j <= data.length-4; j++) {
                if (data[j]===0 && data[j+1]===0 && data[j+2]===0 && data[j+3]===1) { end = j; break; }
              }
              spsNal = data.subarray(i+4, end);
              // Trim trailing zeros
              while (spsNal.length > 0 && spsNal[spsNal.length-1] === 0) spsNal = spsNal.subarray(0, spsNal.length-1);
            }
            if (nt2 === 8) {
              var end = data.length;
              for (var j = i+5; j <= data.length-4; j++) {
                if (data[j]===0 && data[j+1]===0 && data[j+2]===0 && data[j+3]===1) { end = j; break; }
              }
              ppsNal = data.subarray(i+4, end);
              while (ppsNal.length > 0 && ppsNal[ppsNal.length-1] === 0) ppsNal = ppsNal.subarray(0, ppsNal.length-1);
            }
          }
        }

        if (spsNal && ppsNal) {
          var codec = 'avc1.' + ('0'+spsNal[1].toString(16)).slice(-2) + ('0'+spsNal[2].toString(16)).slice(-2) + ('0'+spsNal[3].toString(16)).slice(-2);
          // Build AVCDecoderConfigurationRecord
          var desc = new Uint8Array(11 + spsNal.length + ppsNal.length);
          desc[0]=1; desc[1]=spsNal[1]; desc[2]=spsNal[2]; desc[3]=spsNal[3]; desc[4]=0xFF; desc[5]=0xE1;
          desc[6]=(spsNal.length>>8)&0xFF; desc[7]=spsNal.length&0xFF;
          desc.set(spsNal, 8);
          var o=8+spsNal.length; desc[o]=1; desc[o+1]=(ppsNal.length>>8)&0xFF; desc[o+2]=ppsNal.length&0xFF;
          desc.set(ppsNal, o+3);

          console.log('DC video: configuring AVCC ' + codec + ' sps=' + spsNal.length + ' pps=' + ppsNal.length);
          try {
            decoder.configure({ codec: codec, description: desc.buffer, optimizeForLatency: true });
            configured = true;
          } catch(ce) {
            console.error('DC video: configure failed: ' + ce.message);
            return;
          }
        }
      }

      if (!configured) return;

      // Skip all frames until first keyframe
      if (decCount === 0 && !hasIDR) return;

      // For AVCC mode: strip SPS/PPS from data and convert VCL NALs to AVCC format
      // Find VCL NAL positions and build AVCC data
      var vclParts = [];
      for (var i = 0; i <= data.length - 5; i++) {
        if (data[i]===0 && data[i+1]===0 && data[i+2]===0 && data[i+3]===1) {
          var nt3 = data[i+4] & 0x1F;
          if (nt3 >= 1 && nt3 <= 5) {
            var end = data.length;
            for (var j = i+5; j <= data.length-4; j++) {
              if (data[j]===0 && data[j+1]===0 && data[j+2]===0 && data[j+3]===1) { end = j; break; }
            }
            // Trim trailing zeros
            while (end > i+5 && data[end-1] === 0) end--;
            vclParts.push(data.subarray(i+4, end));
          }
        }
      }
      if (vclParts.length === 0) return;

      // Build AVCC: [4-byte length][NAL data] per VCL NAL
      var totalAvcc = 0;
      for (var v = 0; v < vclParts.length; v++) totalAvcc += 4 + vclParts[v].length;
      var avccBuf = new Uint8Array(totalAvcc);
      var pos = 0;
      for (var v = 0; v < vclParts.length; v++) {
        var len = vclParts[v].length;
        avccBuf[pos]=(len>>>24)&0xFF; avccBuf[pos+1]=(len>>>16)&0xFF; avccBuf[pos+2]=(len>>>8)&0xFF; avccBuf[pos+3]=len&0xFF;
        avccBuf.set(vclParts[v], pos+4);
        pos += 4 + len;
      }
      data = avccBuf;

      // Stats
      var now = Date.now();
      if (hasIDR) idrCount++;
      if (now - lastLog > 5000) {
        var el = (now - lastLog) / 1000;
        console.log('DC: recv=' + Math.round(recvCount/el) + ' dec=' + Math.round(decCount/el) + ' render=' + Math.round(renderCount/el) + ' idr=' + idrCount + ' q=' + decoder.decodeQueueSize);
        recvCount = 0; decCount = 0; renderCount = 0; idrCount = 0; lastLog = now;
      }

      try {
        if (decoder.decodeQueueSize > 10) return;
        decoder.decode(new EncodedVideoChunk({
          type: hasIDR ? 'key' : 'delta',
          timestamp: decCount * (1000000 / 144),
          data: data.buffer ? data.buffer : data,
        }));
        decCount++;
      } catch(err) {
        console.warn('DC decode: ' + (err.message || err));
        configured = false;
        decCount = 0;
      }
    };
  }

  async handleRemoteSDP(msg) {
    if (!this.pc) {
      this.pendingMessages.push({ type: 'sdp', msg });
      return;
    }
    try {
      const desc = new RTCSessionDescription({
        type: msg.sdp_type || 'answer',
        sdp: msg.sdp
      });
      await this.pc.setRemoteDescription(desc);

      if (desc.type === 'offer') {
        const answer = await this.pc.createAnswer();
        await this.pc.setLocalDescription(answer);
        this.sendSignaling('sdp', { sdp_type: 'answer', sdp: answer.sdp });
      }
    } catch (err) {
      console.error('SDP error:', err);
    }
  }

  async handleRemoteICE(msg) {
    if (!this.pc) {
      this.pendingMessages.push({ type: 'ice', msg });
      return;
    }
    try {
      await this.pc.addIceCandidate(new RTCIceCandidate({
        candidate: msg.candidate,
        sdpMid: msg.sdpMid || msg.mid,
        sdpMLineIndex: msg.sdpMLineIndex
      }));
    } catch (err) {
      console.error('ICE error:', err);
    }
  }

  // ============== Gamepad Input ==============

  handleGamepadConnected(e) {
    console.log('Gamepad connected:', e.gamepad.index, e.gamepad.id);
    this.updateGamepadIndicator();
    if (this.playerSlot > 0 && !this.gamepads.has(e.gamepad.index)) {
      this.claimGamepad(e.gamepad.index);
    }
  }

  handleGamepadDisconnected(e) {
    const serverSlot = this.gamepads.get(e.gamepad.index);
    if (serverSlot !== undefined) this.releaseGamepad(e.gamepad.index);
    this.updateGamepadIndicator();
  }

  claimGamepad(browserIndex) {
    this.sendSignaling('claim_gamepad', { browser_index: browserIndex });
  }

  releaseGamepad(browserIndex) {
    const serverSlot = this.gamepads.get(browserIndex);
    if (serverSlot !== undefined) {
      this.sendSignaling('release_gamepad', { server_slot: serverSlot });
      this.gamepads.delete(browserIndex);
    }
  }

  handleGamepadClaimed(msg) {
    this.gamepads.set(msg.browser_index, msg.server_slot);
    this.updateGamepadIndicator();
  }

  handleGamepadReleased(msg) {
    for (const [bi, ss] of this.gamepads.entries()) {
      if (ss === msg.server_slot) { this.gamepads.delete(bi); break; }
    }
    this.updateGamepadIndicator();
  }

  startGamepadPolling() {
    if (this.gamepadPollingId) return;
    this.gamepadPollingId = setInterval(() => this.pollGamepads(), this.config.gamepadPollRate);
  }

  stopGamepadPolling() {
    if (this.gamepadPollingId) { clearInterval(this.gamepadPollingId); this.gamepadPollingId = null; }
  }

  pollGamepads() {
    if (!this.dataChannel || this.dataChannel.readyState !== 'open') return;
    if (this.playerSlot === 0) return;

    for (const gamepad of navigator.getGamepads()) {
      if (!gamepad) continue;
      const serverSlot = this.gamepads.get(gamepad.index);
      if (serverSlot === undefined) {
        if (this.playerSlot > 0) this.claimGamepad(gamepad.index);
        continue;
      }

      const state = this.getGamepadState(gamepad);
      const last = this.lastGamepadState.get(gamepad.index);
      if (!last || !this.gamepadStatesEqual(state, last)) {
        this.sendGamepadState(serverSlot, state);
        this.lastGamepadState.set(gamepad.index, state);
      }
    }
  }

  getGamepadState(gp) {
    return {
      buttons: gp.buttons.map(b => ({ pressed: b.pressed, value: b.value })),
      axes: Array.from(gp.axes)
    };
  }

  gamepadStatesEqual(a, b) {
    if (a.axes.length !== b.axes.length || a.buttons.length !== b.buttons.length) return false;
    for (let i = 0; i < a.axes.length; i++) {
      if (Math.abs(a.axes[i] - b.axes[i]) > 0.01) return false;
    }
    for (let i = 0; i < a.buttons.length; i++) {
      if (a.buttons[i].pressed !== b.buttons[i].pressed) return false;
      if (Math.abs(a.buttons[i].value - b.buttons[i].value) > 0.01) return false;
    }
    return true;
  }

  sendGamepadState(serverSlot, state) {
    const buffer = new ArrayBuffer(20);
    const view = new DataView(buffer);
    view.setUint8(0, 0x01);
    view.setUint8(1, serverSlot);

    let buttonMask = 0;
    for (let i = 0; i < Math.min(16, state.buttons.length); i++) {
      if (state.buttons[i].pressed) buttonMask |= (1 << i);
    }
    view.setUint16(2, buttonMask, true);

    for (let i = 0; i < Math.min(4, state.axes.length); i++) {
      view.setInt16(4 + i * 2, Math.round(state.axes[i] * 32767), true);
    }

    const lt = state.buttons[6]?.value || 0;
    const rt = state.buttons[7]?.value || 0;
    view.setUint8(12, Math.round(lt * 255));
    view.setUint8(13, Math.round(rt * 255));

    this.dataChannel.send(buffer);
  }

  // ============== Keyboard Input ==============

  handleKeyDown(e) {
    if (!this.keyboardEnabled || this.playerSlot === 0) return;
    if (!this.dataChannel || this.dataChannel.readyState !== 'open') return;
    const focused = document.activeElement;
    const isVideoFocused = focused === this.elements.videoElement ||
      focused === this.elements.videoContainer ||
      this.elements.videoContainer?.contains(focused);
    if (!isVideoFocused) return;
    e.preventDefault();
    this.sendKeyEvent(e.code, true);
  }

  handleKeyUp(e) {
    if (!this.keyboardEnabled || this.playerSlot === 0) return;
    if (!this.dataChannel || this.dataChannel.readyState !== 'open') return;
    const focused = document.activeElement;
    const isVideoFocused = focused === this.elements.videoElement ||
      focused === this.elements.videoContainer ||
      this.elements.videoContainer?.contains(focused);
    if (!isVideoFocused) return;
    e.preventDefault();
    this.sendKeyEvent(e.code, false);
  }

  sendKeyEvent(code, pressed) {
    const buffer = new ArrayBuffer(5);
    const view = new DataView(buffer);
    const vkCode = typeof code === 'number' ? code : this.keyCodeToVK(code);
    view.setUint8(0, 0x02);
    view.setUint16(1, vkCode, true);
    view.setUint8(3, 0);
    view.setUint8(4, pressed ? 1 : 0);
    this.dataChannel.send(buffer);
  }

  keyCodeToVK(code) {
    const m = {
      'KeyA': 0x41, 'KeyB': 0x42, 'KeyC': 0x43, 'KeyD': 0x44,
      'KeyE': 0x45, 'KeyF': 0x46, 'KeyG': 0x47, 'KeyH': 0x48,
      'KeyI': 0x49, 'KeyJ': 0x4A, 'KeyK': 0x4B, 'KeyL': 0x4C,
      'KeyM': 0x4D, 'KeyN': 0x4E, 'KeyO': 0x4F, 'KeyP': 0x50,
      'KeyQ': 0x51, 'KeyR': 0x52, 'KeyS': 0x53, 'KeyT': 0x54,
      'KeyU': 0x55, 'KeyV': 0x56, 'KeyW': 0x57, 'KeyX': 0x58,
      'KeyY': 0x59, 'KeyZ': 0x5A,
      'Digit0': 0x30, 'Digit1': 0x31, 'Digit2': 0x32, 'Digit3': 0x33,
      'Digit4': 0x34, 'Digit5': 0x35, 'Digit6': 0x36, 'Digit7': 0x37,
      'Digit8': 0x38, 'Digit9': 0x39,
      'F1': 0x70, 'F2': 0x71, 'F3': 0x72, 'F4': 0x73,
      'F5': 0x74, 'F6': 0x75, 'F7': 0x76, 'F8': 0x77,
      'F9': 0x78, 'F10': 0x79, 'F11': 0x7A, 'F12': 0x7B,
      'Backspace': 0x08, 'Tab': 0x09, 'Enter': 0x0D, 'Escape': 0x1B, 'Space': 0x20,
      'CapsLock': 0x14, 'NumLock': 0x90, 'ScrollLock': 0x91,
      'PageUp': 0x21, 'PageDown': 0x22, 'End': 0x23, 'Home': 0x24,
      'ArrowLeft': 0x25, 'ArrowUp': 0x26, 'ArrowRight': 0x27, 'ArrowDown': 0x28,
      'Insert': 0x2D, 'Delete': 0x2E,
      'Semicolon': 0xBA, 'Equal': 0xBB, 'Comma': 0xBC, 'Minus': 0xBD,
      'Period': 0xBE, 'Slash': 0xBF, 'Backquote': 0xC0,
      'BracketLeft': 0xDB, 'Backslash': 0xDC, 'BracketRight': 0xDD, 'Quote': 0xDE,
      'ShiftLeft': 0xA0, 'ShiftRight': 0xA1,
      'ControlLeft': 0xA2, 'ControlRight': 0xA3,
      'AltLeft': 0xA4, 'AltRight': 0xA5,
      'MetaLeft': 0x5B, 'MetaRight': 0x5C,
      'PrintScreen': 0x2C, 'Pause': 0x13
    };
    return m[code] || 0;
  }

  // ============== Pointer Input ==============

  handlePointerMove(e) {
    e.preventDefault();
    e.stopPropagation();

    if (this.trackpadMode && e.pointerType === 'touch') {
      if (!this.mouseEnabled || this.playerSlot === 0) return;
      if (!this.dataChannel || this.dataChannel.readyState !== 'open') return;
      if (e.pointerId !== this.gesturePointerId) return;

      const dx = e.clientX - this.gestureLastPos.x;
      const dy = e.clientY - this.gestureLastPos.y;
      this.gestureLastPos = { x: e.clientX, y: e.clientY };

      if (this.gestureState === 'touched') {
        const totalDx = e.clientX - this.gestureTouchStart.x;
        const totalDy = e.clientY - this.gestureTouchStart.y;
        if (Math.abs(totalDx) > this.GESTURE_TAP_MAX_MOVE || Math.abs(totalDy) > this.GESTURE_TAP_MAX_MOVE) {
          this.gestureState = 'dragging';
        }
      } else if (this.gestureState === 'double_touched') {
        const totalDx = e.clientX - this.gestureTouchStart.x;
        const totalDy = e.clientY - this.gestureTouchStart.y;
        if (Math.abs(totalDx) > this.GESTURE_TAP_MAX_MOVE || Math.abs(totalDy) > this.GESTURE_TAP_MAX_MOVE) {
          this.gestureState = 'click_dragging';
          this.sendMouseMoveAbs(this.virtualCursorX, this.virtualCursorY);
          this.sendMouseButton(0, true);
        }
      }

      if (this.gestureState === 'dragging' || this.gestureState === 'click_dragging') {
        this.applyTrackpadDelta(dx, dy);
        const now = performance.now();
        const dt = now - (this.inertiaLastTime || now);
        this.inertiaLastTime = now;
        if (dt > 0 && dt < 100) {
          this.inertiaVelocityX = 0.6 * (dx / dt) + 0.4 * this.inertiaVelocityX;
          this.inertiaVelocityY = 0.6 * (dy / dt) + 0.4 * this.inertiaVelocityY;
        }
      }
      return;
    }

    if (!this.mouseEnabled || this.playerSlot === 0) return;
    if (!this.dataChannel || this.dataChannel.readyState !== 'open') return;

    const videoRect = this.getVideoContentRect();
    if (!videoRect) return;

    const activePointer = this.activePointers.get(e.pointerId);
    if (!activePointer) {
      if (e.clientX < videoRect.left || e.clientX > videoRect.right ||
          e.clientY < videoRect.top || e.clientY > videoRect.bottom) return;
    }

    const relX = (e.clientX - videoRect.left) / videoRect.width;
    const relY = (e.clientY - videoRect.top) / videoRect.height;
    const absX = Math.round(Math.max(0, Math.min(1, relX)) * 65535);
    const absY = Math.round(Math.max(0, Math.min(1, relY)) * 65535);

    if (activePointer && activePointer.type === 'touch') {
      this.sendMouseButton(activePointer.button, true);
    }

    this.sendMouseMoveAbs(absX, absY);
  }

  handlePointerDown(e) {
    e.preventDefault();
    e.stopPropagation();
    this.elements.videoContainer?.focus();

    if (this.elements.videoElement?.muted) {
      this.elements.videoElement.muted = false;
    }

    if (this.trackpadMode && e.pointerType === 'touch') {
      if (!this.mouseEnabled || this.playerSlot === 0) return;
      if (!this.dataChannel || this.dataChannel.readyState !== 'open') return;
      if (this.gesturePointerId !== null && this.gesturePointerId !== e.pointerId) return;
      this.gesturePointerId = e.pointerId;
      this.stopInertia();
      if (e.target.setPointerCapture) e.target.setPointerCapture(e.pointerId);

      if (this.gestureState === 'tap_up') {
        clearTimeout(this.gestureTimer);
        this.gestureTimer = null;
        this.gestureState = 'double_touched';
      } else {
        this.gestureState = 'touched';
      }
      this.gestureTouchStart = { x: e.clientX, y: e.clientY, time: Date.now() };
      this.gestureLastPos = { x: e.clientX, y: e.clientY };
      return;
    }

    if (!this.mouseEnabled || this.playerSlot === 0) return;
    if (!this.dataChannel || this.dataChannel.readyState !== 'open') return;

    if (e.target.setPointerCapture) e.target.setPointerCapture(e.pointerId);
    const button = e.pointerType === 'touch' ? 0 : e.button;
    this.activePointers.set(e.pointerId, { type: e.pointerType, button });

    const videoRect = this.getVideoContentRect();
    if (videoRect) {
      const relX = (e.clientX - videoRect.left) / videoRect.width;
      const relY = (e.clientY - videoRect.top) / videoRect.height;
      this.sendMouseMoveAbs(
        Math.round(Math.max(0, Math.min(1, relX)) * 65535),
        Math.round(Math.max(0, Math.min(1, relY)) * 65535)
      );
    }
    this.sendMouseButton(button, true);
  }

  handlePointerUp(e) {
    e.preventDefault();
    e.stopPropagation();

    if (this.trackpadMode && e.pointerType === 'touch') {
      if (e.pointerId !== this.gesturePointerId) return;
      try { e.target.releasePointerCapture(e.pointerId); } catch {}
      this.gesturePointerId = null;

      if (!this.mouseEnabled || this.playerSlot === 0) return;
      if (!this.dataChannel || this.dataChannel.readyState !== 'open') return;

      if (this.gestureState === 'touched') {
        const elapsed = Date.now() - (this.gestureTouchStart?.time || 0);
        if (elapsed <= this.GESTURE_TAP_MAX_DURATION) {
          this.gestureState = 'tap_up';
          this.gestureTimer = setTimeout(() => {
            this.sendMouseMoveAbs(this.virtualCursorX, this.virtualCursorY);
            this.sendMouseButton(0, true);
            setTimeout(() => { this.sendMouseButton(0, false); this.gestureState = 'idle'; }, 30);
            this.gestureTimer = null;
          }, this.GESTURE_TAP_RETOUCH_WINDOW);
        } else {
          this.gestureState = 'idle';
        }
      } else if (this.gestureState === 'double_touched') {
        const elapsed = Date.now() - (this.gestureTouchStart?.time || 0);
        if (elapsed <= this.GESTURE_TAP_MAX_DURATION) {
          this.sendMouseMoveAbs(this.virtualCursorX, this.virtualCursorY);
          this.sendMouseButton(0, true);
          this.sendMouseButton(0, false);
          this.sendMouseButton(0, true);
          setTimeout(() => this.sendMouseButton(0, false), 30);
        }
        this.gestureState = 'idle';
      } else if (this.gestureState === 'dragging') {
        this.gestureState = 'idle';
        this.startInertia();
      } else if (this.gestureState === 'click_dragging') {
        this.sendMouseButton(0, false);
        this.gestureState = 'idle';
      } else {
        this.gestureState = 'idle';
      }
      return;
    }

    this.activePointers.delete(e.pointerId);
    if (!this.mouseEnabled || this.playerSlot === 0) return;
    if (!this.dataChannel || this.dataChannel.readyState !== 'open') return;
    try { e.target.releasePointerCapture(e.pointerId); } catch {}
    this.sendMouseButton(e.pointerType === 'touch' ? 0 : e.button, false);
  }

  getVideoContentRect() {
    const video = this.elements.videoElement;
    if (!video) return null;

    // Always use the video element's bounding rect for pointer mapping.
    // The canvas is pointer-events:none so all events go to the video element.
    // For aspect ratio, use the actual video dimensions (media track) or
    // the canvas native size (DC path).
    const r = video.getBoundingClientRect();
    let nativeW = video.videoWidth;
    let nativeH = video.videoHeight;

    // If no video dimensions yet (DC-only), use canvas dimensions
    if ((!nativeW || !nativeH) && this._videoCanvas) {
      // The canvas stores the last drawn frame size
      nativeW = this._videoCanvas.getAttribute('data-fw') || this._videoCanvas.width;
      nativeH = this._videoCanvas.getAttribute('data-fh') || this._videoCanvas.height;
    }
    if (!nativeW || !nativeH) return null;

    const va = nativeW / nativeH;
    const ea = r.width / r.height;
    let cw, ch, cl, ct;
    if (va > ea) {
      cw = r.width; ch = r.width / va; cl = r.left; ct = r.top + (r.height - ch) / 2;
    } else {
      ch = r.height; cw = r.height * va; ct = r.top; cl = r.left + (r.width - cw) / 2;
    }
    return { left: cl, top: ct, right: cl + cw, bottom: ct + ch, width: cw, height: ch };
  }

  handleMouseWheel(e) {
    if (!this.mouseEnabled || this.playerSlot === 0) return;
    if (!this.dataChannel || this.dataChannel.readyState !== 'open') return;
    e.preventDefault();
    this.sendMouseScroll(e.deltaX, -e.deltaY);
  }

  sendMouseMoveAbs(absX, absY) {
    const buffer = new ArrayBuffer(6);
    const view = new DataView(buffer);
    view.setUint8(0, 0x03);
    view.setUint8(1, 0x01);
    view.setUint16(2, absX, true);
    view.setUint16(4, absY, true);
    this.dataChannel.send(buffer);
  }

  sendMouseButton(button, pressed) {
    const buffer = new ArrayBuffer(3);
    const view = new DataView(buffer);
    view.setUint8(0, 0x04);
    view.setUint8(1, button);
    view.setUint8(2, pressed ? 1 : 0);
    this.dataChannel.send(buffer);
  }

  sendMouseScroll(dx, dy) {
    const buffer = new ArrayBuffer(6);
    const view = new DataView(buffer);
    view.setUint8(0, 0x05);
    view.setUint8(1, 0);
    view.setInt16(2, Math.round(dx), true);
    view.setInt16(4, Math.round(dy), true);
    this.dataChannel.send(buffer);
  }

  // ============== Trackpad Mode ==============

  applyTrackpadDelta(screenDx, screenDy) {
    const scale = this.trackpadSensitivity * (65535 / (window.innerHeight || 1));
    this.virtualCursorX = Math.max(0, Math.min(65535, this.virtualCursorX + Math.round(screenDx * scale)));
    this.virtualCursorY = Math.max(0, Math.min(65535, this.virtualCursorY + Math.round(screenDy * scale)));
    this.sendMouseMoveAbs(this.virtualCursorX, this.virtualCursorY);
    this.updateCursorPan();
  }

  // updateCursorPan keeps the captured cursor visually centered in the viewport
  // while in portrait trackpad mode, by adjusting CSS object-position on the
  // video element. With object-fit: cover, only one axis overflows; we pan that
  // axis (clamped to [0%,100%], so cursor near edges drifts off-center).
  updateCursorPan() {
    const video = this.elements.videoElement;
    if (!video) return;

    if (!this.trackpadMode || !this.isPortrait()) {
      video.style.objectPosition = '50% 50%';
      return;
    }

    const sw = video.videoWidth, sh = video.videoHeight;
    const vw = video.clientWidth, vh = video.clientHeight;
    if (!sw || !sh || !vw || !vh) return;

    const sourceAR = sw / sh;
    const viewportAR = vw / vh;
    const cx = this.virtualCursorX / 65535;
    const cy = this.virtualCursorY / 65535;

    // Pan the axis that overflows under object-fit: cover.
    let posX = 50, posY = 50;
    if (sourceAR > viewportAR) {
      const visibleFrac = viewportAR / sourceAR;
      if (visibleFrac < 1) {
        const p = (cx - visibleFrac / 2) / (1 - visibleFrac);
        posX = Math.max(0, Math.min(1, p)) * 100;
      }
    } else if (sourceAR < viewportAR) {
      const visibleFrac = sourceAR / viewportAR;
      if (visibleFrac < 1) {
        const p = (cy - visibleFrac / 2) / (1 - visibleFrac);
        posY = Math.max(0, Math.min(1, p)) * 100;
      }
    }
    video.style.objectPosition = `${posX}% ${posY}%`;
  }

  // Phone heuristic: touch input + a phone-sized viewport. Tablets and
  // touch laptops fall through to absolute pointer mode.
  isPhone() {
    if (!this.isTouchDevice()) return false;
    const minDim = Math.min(window.innerWidth, window.innerHeight);
    return minDim <= 600;
  }

  isPortrait() {
    return window.matchMedia('(orientation: portrait)').matches;
  }

  isPortraitTouch() {
    return window.matchMedia('(pointer: coarse) and (orientation: portrait)').matches;
  }

  // Apply phone-specific layout: trackpad mode always; cover+pan for portrait,
  // contain (letterbox) for landscape so the entire screen is visible.
  applyMobileLayout() {
    const video = this.elements.videoElement;
    if (!this.isPhone()) {
      this.trackpadMode = false;
      if (video) {
        video.style.objectFit = '';
        video.style.objectPosition = '';
      }
      return;
    }

    if (!this.trackpadMode) {
      this.trackpadMode = true;
      this.virtualCursorX = 32768;
      this.virtualCursorY = 32768;
    }

    if (video) {
      video.style.objectFit = this.isPortrait() ? 'cover' : 'contain';
    }
    this.updateCursorPan();
  }

  startInertia() {
    if (Math.sqrt(this.inertiaVelocityX ** 2 + this.inertiaVelocityY ** 2) < this.INERTIA_MIN_VELOCITY) return;
    this.inertiaLastTime = performance.now();
    const tick = () => {
      const now = performance.now();
      const dt = now - this.inertiaLastTime;
      this.inertiaLastTime = now;
      this.inertiaVelocityX *= this.INERTIA_FRICTION;
      this.inertiaVelocityY *= this.INERTIA_FRICTION;
      if (Math.sqrt(this.inertiaVelocityX ** 2 + this.inertiaVelocityY ** 2) < this.INERTIA_MIN_VELOCITY) {
        this.stopInertia(); return;
      }
      this.applyTrackpadDelta(this.inertiaVelocityX * dt, this.inertiaVelocityY * dt);
      this.inertiaAnimationId = requestAnimationFrame(tick);
    };
    this.inertiaAnimationId = requestAnimationFrame(tick);
  }

  stopInertia() {
    if (this.inertiaAnimationId) { cancelAnimationFrame(this.inertiaAnimationId); this.inertiaAnimationId = null; }
    this.inertiaVelocityX = 0;
    this.inertiaVelocityY = 0;
  }

  handlePointerLockChange() {
    this.pointerLocked = document.pointerLockElement === this.elements.videoElement;
  }


  // ============== Stats ==============

  startStatsPolling() {
    this.statsIntervalId = setInterval(() => this.updateStats(), this.config.statsUpdateRate);
  }

  stopStatsPolling() {
    if (this.statsIntervalId) { clearInterval(this.statsIntervalId); this.statsIntervalId = null; }
  }

  async updateStats() {
    if (!this.pc) return;
    try {
      const stats = await this.pc.getStats();
      const now = Date.now();
      stats.forEach(report => {
        if (report.type === 'inbound-rtp' && report.kind === 'video') {
          const bytes = report.bytesReceived || 0;
          const frames = report.framesDecoded || 0;
          if (this.stats.lastStatsTime > 0) {
            const elapsed = (now - this.stats.lastStatsTime) / 1000;
            this.stats.bitrate = Math.round(((bytes - this.stats.lastBytesReceived) * 8) / elapsed / 1000);
            this.stats.fps = Math.round((frames - this.stats.lastFramesDecoded) / elapsed);
          }
          this.stats.lastBytesReceived = bytes;
          this.stats.lastFramesDecoded = frames;
          this.stats.packetsLost = report.packetsLost || 0;

          // Monitor jitter buffer delay. If it grows past 150ms,
          // request an IDR — the fresh keyframe resets the decoder
          // pipeline and effectively flushes accumulated delay.
          const jbDelay = report.jitterBufferDelay;
          const jbEmitted = report.jitterBufferEmittedCount;
          if (jbDelay !== undefined && jbEmitted !== undefined && jbEmitted > 0) {
            const avgJbMs = (jbDelay / jbEmitted) * 1000;
            this.stats.jitterBuffer = Math.round(avgJbMs);

            if (avgJbMs > 80 && (!this._lastJbReset || now - this._lastJbReset > 2000)) {
              console.log('JB ' + Math.round(avgJbMs) + 'ms > 80ms, requesting IDR');
              this.sendSignaling('request_idr');
              this._lastJbReset = now;

              // Re-apply playout delay hint aggressively
              this.pc.getReceivers().forEach(r => {
                if (r.track?.kind === 'video') {
                  if (r.playoutDelayHint !== undefined) r.playoutDelayHint = 0;
                  if (r.jitterBufferTarget !== undefined) r.jitterBufferTarget = 0;
                }
              });
            }
          }

          this.checkForFreeze(frames, now);
        }
        if (report.type === 'candidate-pair' && report.state === 'succeeded') {
          this.stats.rtt = report.currentRoundTripTime ? Math.round(report.currentRoundTripTime * 1000) : 0;
        }
      });
      this.stats.lastStatsTime = now;
      this.updateStatsUI();
      this.checkPathQuality(now);
    } catch {}
  }

  // checkPathQuality watches RTT and packet loss and asks the server
  // for an ICE restart when the selected pair has been bad for a few
  // seconds. ICE picks pairs by static priority (RFC 8445), not by
  // measured performance, so a freshly-restarted ICE round can land on
  // a different — possibly better — path. Cooldown prevents thrashing.
  checkPathQuality(now) {
    if (!this._iceWindow) {
      this._iceWindow = [];
      this._iceLastRestart = 0;
    }
    if (!this.stats.rtt) return; // no candidate-pair data yet

    this._iceWindow.push({ t: now, rtt: this.stats.rtt, lost: this.stats.packetsLost || 0 });
    while (this._iceWindow.length > 0 && now - this._iceWindow[0].t > 3000) {
      this._iceWindow.shift();
    }
    if (this._iceWindow.length < 3) return;
    if (now - this._iceLastRestart < 10000) return;

    const avgRtt = this._iceWindow.reduce((s, p) => s + p.rtt, 0) / this._iceWindow.length;
    const lostDelta = this._iceWindow[this._iceWindow.length - 1].lost - this._iceWindow[0].lost;

    if (avgRtt > 200 || lostDelta > 50) {
      console.log('Path regressed: avgRtt=' + Math.round(avgRtt) + 'ms, lost+=' + lostDelta + ' over ' + this._iceWindow.length + 's; requesting ICE restart');
      this.sendSignaling('request_ice_restart');
      this._iceLastRestart = now;
      this._iceWindow = []; // start measuring fresh after restart
    }
  }

  checkForFreeze(framesDecoded, now) {
    // After a reconnect the RTCPeerConnection's framesDecoded restarts
    // from 0 while lastFrameCount still holds the old session's count,
    // so a `>` comparison would never fire and the detector would loop.
    // Treat any backwards jump as a fresh stream and reset.
    if (framesDecoded < this.freezeDetection.lastFrameCount) {
      this.freezeDetection.lastFrameCount = framesDecoded;
      this.freezeDetection.freezeStartTime = null;
      this.freezeDetection.idrRequested = false;
      this.freezeDetection.reconnectAttempted = false;
      return;
    }
    if (framesDecoded > this.freezeDetection.lastFrameCount) {
      this.freezeDetection.lastFrameCount = framesDecoded;
      this.freezeDetection.freezeStartTime = null;
      this.freezeDetection.idrRequested = false;
      this.freezeDetection.reconnectAttempted = false;
      return;
    }
    if (this.freezeDetection.freezeStartTime === null) {
      this.freezeDetection.freezeStartTime = now;
      return;
    }
    const frozen = now - this.freezeDetection.freezeStartTime;
    if (frozen >= this.FREEZE_THRESHOLD_MS && !this.freezeDetection.idrRequested) {
      this.sendSignaling('request_idr');
      this.freezeDetection.idrRequested = true;
    }
    if (frozen >= this.RECONNECT_THRESHOLD_MS && !this.freezeDetection.reconnectAttempted) {
      this.freezeDetection.reconnectAttempted = true;
      this.showNotification('Video frozen, reconnecting...');
      if (this.pc) { this.pc.close(); this.pc = null; }
      this.sendSignaling('reconnect');
      this.freezeDetection.freezeStartTime = null;
      this.freezeDetection.idrRequested = false;
      this.freezeDetection.reconnectAttempted = false;
      this.freezeDetection.lastFrameCount = 0;
    }
  }

  // ============== UI ==============

  showStartOverlay() {
    if (this.elements.startOverlay) this.elements.startOverlay.style.display = 'flex';
    if (this.elements.videoContainer) this.elements.videoContainer.classList.add('hidden');
    if (this.elements.sidebar) this.elements.sidebar.classList.add('hidden');
  }

  showStreamUI() {
    if (this.elements.startOverlay) this.elements.startOverlay.style.display = 'none';
    if (this.elements.videoContainer) this.elements.videoContainer.classList.remove('hidden');
    if (this.elements.sidebar) {
      this.elements.sidebar.classList.remove('hidden');
      if (!this.isTouchDevice()) this.elements.sidebar.classList.add('open');
    }
    if (this.elements.sidebarToggle) this.elements.sidebarToggle.classList.remove('hidden');
    if (this.elements.fullscreenBtn) this.elements.fullscreenBtn.classList.remove('hidden');
    if (this.elements.keyboardBtn) this.elements.keyboardBtn.classList.remove('hidden');

    this.applyMobileLayout();

    this.fetchEncoderInfo();
  }

  async fetchEncoderInfo() {
    try {
      const res = await fetch('/api/encoder');
      if (res.ok) {
        const data = await res.json();
        if (data.status && this.elements.statsEncoder) {
          this.elements.statsEncoder.textContent = data.encoder || '--';
        }
      }
    } catch {}
  }

  updateRoomUI() {
    this.updatePlayerList();
    if (this.elements.joinPlayerSection) {
      this.elements.joinPlayerSection.style.display = this.playerSlot === 0 ? '' : 'none';
    }
    if (this.elements.permissionsPanel) {
      this.elements.permissionsPanel.style.display = this.isHost ? '' : 'none';
    }
    if (this.elements.qualityPanel) {
      this.elements.qualityPanel.style.display = this.isHost ? '' : 'none';
    }
    if (this.playerSlot > 0) this.startGamepadPolling();
  }

  updatePlayerList() {
    if (!this.elements.playerList) return;
    this.elements.playerList.innerHTML = '';
    for (const player of this.players) {
      const slot = player.slot || 0;
      const isMe = player.peer_id === this.playerId;
      const item = document.createElement('div');
      item.className = 'player-item';
      item.innerHTML = `
        <span class="player-slot ${slot > 0 ? 'player' : 'spectator'}">
          ${slot > 0 ? `P${slot}` : 'S'}
        </span>
        <span class="player-name">${this.escapeHtml(player.name)}${isMe ? ' (You)' : ''}</span>
        <span class="player-gamepads">${player.gamepad_count || 0} GP</span>
      `;
      this.elements.playerList.appendChild(item);
    }
  }

  updateStatsUI() {
    if (this.elements.statsBitrate) this.elements.statsBitrate.textContent = `${this.stats.bitrate} kbps`;
    if (this.elements.statsFps) this.elements.statsFps.textContent = `${this.stats.fps} fps`;
    if (this.elements.statsRtt) {
      let rttText = `${this.stats.rtt} ms`;
      if (!this.useDataChannel && this.stats.jitterBuffer) {
        rttText += ` / jb:${this.stats.jitterBuffer}ms`;
      }
      this.elements.statsRtt.textContent = rttText;
    }
    if (this.elements.statsPacketLoss) this.elements.statsPacketLoss.textContent = `${this.stats.packetsLost}`;
  }

  updateGamepadIndicator() {
    if (!this.elements.gamepadIndicator) return;
    const connected = Array.from(navigator.getGamepads()).filter(g => g !== null).length;
    const claimed = this.gamepads.size;
    const countEl = document.getElementById('gamepadCount');
    if (countEl) countEl.textContent = `${claimed}/${connected}`;
    this.elements.gamepadIndicator.style.display = connected > 0 ? 'flex' : 'none';
    this.elements.gamepadIndicator.classList.toggle('active', claimed > 0);
  }

  updateConnectionStatus(status) {
    if (!this.elements.connectionStatus) return;
    const map = {
      'connected': { text: 'Connected', cls: 'connected' },
      'connecting': { text: 'Connecting...', cls: 'connecting' },
      'disconnected': { text: 'Disconnected', cls: 'disconnected' },
      'checking': { text: 'Connecting...', cls: 'connecting' },
      'completed': { text: 'Connected', cls: 'connected' },
      'failed': { text: 'Failed', cls: 'error' },
      'error': { text: 'Error', cls: 'error' }
    };
    const info = map[status] || { text: status, cls: '' };
    this.elements.connectionStatus.textContent = info.text;
    this.elements.connectionStatus.className = `connection-status ${info.cls}`;
  }

  toggleSidebar() { this.elements.sidebar?.classList.toggle('open'); }

  toggleFullscreen() {
    if (document.fullscreenElement) document.exitFullscreen();
    else this.elements.videoContainer?.requestFullscreen();
  }

  isTouchDevice() {
    return ('ontouchstart' in window) || (navigator.maxTouchPoints > 0) ||
      window.matchMedia('(pointer: coarse)').matches;
  }

  toggleMobileKeyboard() {
    const input = this.elements.mobileKeyboardInput;
    const btn = this.elements.keyboardBtn;
    if (!input) return;
    if (document.activeElement === input) { input.blur(); btn?.classList.remove('active'); }
    else { input.focus(); btn?.classList.add('active'); }
  }

  handleMobileKeyboardInput(e) {
    const text = e.target.value;
    if (!text || !this.keyboardEnabled || this.playerSlot === 0) return;
    if (!this.dataChannel || this.dataChannel.readyState !== 'open') return;
    for (const char of text) {
      const kc = this.charToKeyCode(char);
      if (kc) { this.sendKeyEvent(kc, true); this.sendKeyEvent(kc, false); }
    }
    e.target.value = '';
  }

  handleMobileKeyDown(e) {
    if (!this.keyboardEnabled || this.playerSlot === 0) return;
    if (!this.dataChannel || this.dataChannel.readyState !== 'open') return;
    const special = { 'Backspace': 0x08, 'Enter': 0x0D, 'Tab': 0x09, 'Escape': 0x1B,
      'ArrowLeft': 0x25, 'ArrowUp': 0x26, 'ArrowRight': 0x27, 'ArrowDown': 0x28 };
    const kc = special[e.key];
    if (kc) { e.preventDefault(); this.sendKeyEvent(kc, true); this.sendKeyEvent(kc, false); }
  }

  charToKeyCode(char) {
    const code = char.toUpperCase().charCodeAt(0);
    if (code >= 65 && code <= 90) return code;
    if (code >= 48 && code <= 57) return code;
    if (char === ' ') return 0x20;
    const punct = { '.': 0xBE, ',': 0xBC, '/': 0xBF, ';': 0xBA, "'": 0xDE,
      '[': 0xDB, ']': 0xDD, '\\': 0xDC, '-': 0xBD, '=': 0xBB, '`': 0xC0 };
    return punct[char] || null;
  }

  // ============== Quality Settings ==============

  applyQualitySettings() {
    if (!this.isHost) return;
    const bitrate = parseInt(this.elements.bitrateSlider?.value || '3', 10);
    const framerate = parseInt(this.elements.framerateSelect?.value || '60', 10);
    const resolution = this.elements.resolutionSelect?.value || '1080';
    const resMap = {
      '480': { width: 854, height: 480 }, '720': { width: 1280, height: 720 },
      '1080': { width: 1920, height: 1080 }, '1440': { width: 2560, height: 1440 },
      '4k': { width: 3840, height: 2160 }
    };
    const dims = resMap[resolution] || resMap['1080'];
    this.sendSignaling('set_quality', {
      bitrate: bitrate * 1000, framerate, width: dims.width, height: dims.height
    });
    this.showNotification('Applying quality settings...');
  }

  setGuestPermission(type, enabled) {
    if (!this.isHost) return;
    for (const player of this.players) {
      if (player.peer_id !== this.playerId && player.slot > 0) {
        this.sendSignaling(type, { peer_id: player.peer_id, enabled });
      }
    }
    this.sendSignaling(type, { peer_id: '', enabled });
  }

  handlePermissionChanged(msg) {
    if (msg.keyboard_enabled !== undefined) {
      this.keyboardEnabled = msg.keyboard_enabled;
      this.showNotification(`Keyboard ${this.keyboardEnabled ? 'enabled' : 'disabled'}`);
    }
    if (msg.mouse_enabled !== undefined) {
      this.mouseEnabled = msg.mouse_enabled;
      this.showNotification(`Mouse ${this.mouseEnabled ? 'enabled' : 'disabled'}`);
    }
  }

  handleQualityUpdated(msg) {
    if (msg.success) {
      this.updateQualityUI(msg);
      if (this.isHost) this.showNotification('Quality settings applied');
    }
  }

  handleStreamReset() {
    console.log('Stream reset — waiting for new keyframe');
    // Reset the WebCodecs decoder so it reconfigures on the next IDR
    if (this._videoDecoder && this._videoDecoder.state !== 'closed') {
      this._videoDecoder.reset();
    }
    // The setupVideoDataChannel's internal state needs resetting too.
    // Set a flag that the onmessage handler checks.
    this._streamResetPending = true;
  }

  updateQualityUI(settings) {
    if (settings.bitrate && this.elements.bitrateSlider) {
      const mbps = Math.round(settings.bitrate / 1000);
      this.elements.bitrateSlider.value = mbps;
      if (this.elements.bitrateValue) this.elements.bitrateValue.textContent = mbps;
    }
    if (settings.framerate && this.elements.framerateSelect) {
      this.elements.framerateSelect.value = settings.framerate;
    }
    if (settings.height && this.elements.resolutionSelect) {
      if (settings.height <= 480) this.elements.resolutionSelect.value = '480';
      else if (settings.height <= 720) this.elements.resolutionSelect.value = '720';
      else if (settings.height <= 1080) this.elements.resolutionSelect.value = '1080';
      else if (settings.height <= 1440) this.elements.resolutionSelect.value = '1440';
      else this.elements.resolutionSelect.value = '4k';
    }
  }

  // ============== Helpers ==============

  escapeHtml(text) {
    const div = document.createElement('div');
    div.textContent = text;
    return div.innerHTML;
  }

  showLoading(msg) {
    if (this.elements.connectBtn) {
      this.elements.connectBtn.disabled = true;
      this.elements.connectBtn.textContent = msg;
    }
  }

  hideLoading() {
    if (this.elements.connectBtn) {
      this.elements.connectBtn.disabled = false;
      this.elements.connectBtn.textContent = 'Connect';
    }
  }

  showError(message) { alert(message); }

  showNotification(message) {
    const toast = document.createElement('div');
    toast.className = 'toast';
    toast.textContent = message;
    document.body.appendChild(toast);
    setTimeout(() => toast.classList.add('show'), 10);
    setTimeout(() => {
      toast.classList.remove('show');
      setTimeout(() => toast.remove(), 300);
    }, 3000);
  }

  // ============== Lifecycle ==============

  handleVisibilityChange() {
    if (document.hidden) this.stopGamepadPolling();
    else if (this.playerSlot > 0) this.startGamepadPolling();
  }

  handleSignalingDisconnect() {
    this.cleanup();
    this.showStartOverlay();
    this.showError('Connection lost');
  }

  cleanup() {
    this.stopGamepadPolling();
    this.stopStatsPolling();
    this.clearIceConnectionTimer();
    this.stopInertia();
    if (this.gestureTimer) { clearTimeout(this.gestureTimer); this.gestureTimer = null; }
    if (document.pointerLockElement) document.exitPointerLock();
    if (this.dataChannel) { this.dataChannel.close(); this.dataChannel = null; }
    if (this.pc) { this.pc.close(); this.pc = null; }
    if (this.ws) { this.ws.close(); this.ws = null; }

    // Per-session detector / stats state must reset so the next session
    // starts with fresh baselines (the new pc's counters start at 0).
    this.freezeDetection = {
      lastFrameCount: 0, freezeStartTime: null,
      idrRequested: false, reconnectAttempted: false
    };
    this._iceWindow = [];
    this._iceLastRestart = 0;
    this._lastJbReset = 0;
    this.stats = {
      bitrate: 0, fps: 0, rtt: 0, packetsLost: 0,
      framesDecoded: 0, lastStatsTime: 0,
      lastBytesReceived: 0, lastFramesDecoded: 0
    };

    this.trackpadMode = false;
    this.gestureState = 'idle';
    this.gesturePointerId = null;
    this.roomCode = null;
    this.playerId = null;
    this.playerSlot = 0;
    this.isHost = false;
    this.players = [];
    this.gamepads.clear();
    this.lastGamepadState.clear();
    if (this.elements.videoElement) this.elements.videoElement.srcObject = null;
  }
}

// Initialize and auto-connect
document.addEventListener('DOMContentLoaded', () => {
  window.webrtcVNC = new WebRTCVNC();
  setTimeout(() => window.webrtcVNC.connect(), 100);
});
