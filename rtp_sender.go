package main

import (
	"sync"
	"sync/atomic"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

const (
	rtpMTU         = 1200 // max RTP payload size
	rtpClockRate   = 90000
	rtpPayloadType = 96
	fuaType        = 28 // FU-A fragmentation unit
)

// VideoRTPSender handles H.264 → RTP packetization and async sending.
// It implements RFC 6184 FU-A fragmentation and sends via TrackLocalStaticRTP.
type VideoRTPSender struct {
	track *webrtc.TrackLocalStaticRTP
	fps   int

	seq       uint16
	timestamp uint32
	tsIncr    uint32

	// Async frame queue — latest frame wins, old frames are dropped.
	frameCh   chan h264Frame
	closeCh   chan struct{}
	closeOnce sync.Once

	framesSent atomic.Int64
	bytesSent  atomic.Int64
}

type h264Frame struct {
	data  []byte // Annex B access unit (with start codes)
	isIDR bool
}

func NewVideoRTPSender(track *webrtc.TrackLocalStaticRTP, fps int) *VideoRTPSender {
	s := &VideoRTPSender{
		track:   track,
		fps:     fps,
		tsIncr:  uint32(rtpClockRate / fps),
		frameCh: make(chan h264Frame, 2), // small buffer — drop old frames
		closeCh: make(chan struct{}),
	}
	go s.sendLoop()
	return s
}

func (s *VideoRTPSender) Close() {
	s.closeOnce.Do(func() {
		close(s.closeCh)
	})
}

// SendFrame queues an H.264 access unit (Annex B) for RTP packetization.
// If the sender is behind, the oldest queued frame is replaced (latest wins).
func (s *VideoRTPSender) SendFrame(data []byte, isIDR bool) {
	frame := h264Frame{data: data, isIDR: isIDR}
	select {
	case s.frameCh <- frame:
	default:
		// Channel full — drop oldest, push newest
		select {
		case <-s.frameCh:
		default:
		}
		select {
		case s.frameCh <- frame:
		default:
		}
	}
}

func (s *VideoRTPSender) Stats() (frames int64, bytes int64) {
	return s.framesSent.Load(), s.bytesSent.Load()
}

func (s *VideoRTPSender) sendLoop() {
	for {
		select {
		case <-s.closeCh:
			return
		case frame := <-s.frameCh:
			s.packetizeAndSend(frame)
		}
	}
}

func (s *VideoRTPSender) packetizeAndSend(frame h264Frame) {
	// Parse Annex B into NAL units
	nals := splitAnnexB(frame.data)
	if len(nals) == 0 {
		return
	}

	// Send each NAL unit as RTP packets
	for i, nal := range nals {
		isLast := i == len(nals)-1
		s.sendNAL(nal, isLast)
	}

	s.timestamp += s.tsIncr
	s.framesSent.Add(1)
}

// sendNAL sends a single NAL unit, fragmenting with FU-A if needed.
func (s *VideoRTPSender) sendNAL(nal []byte, markerBit bool) {
	if len(nal) == 0 {
		return
	}

	if len(nal) <= rtpMTU {
		// Single NAL unit packet — fits in one RTP packet
		s.writeRTP(nal, markerBit)
	} else {
		// FU-A fragmentation — split NAL into multiple packets
		nalHeader := nal[0]
		nri := nalHeader & 0x60       // NRI bits
		nalType := nalHeader & 0x1F   // original NAL type
		payload := nal[1:]            // skip the NAL header byte

		first := true
		for len(payload) > 0 {
			chunkSize := rtpMTU - 2 // 2 bytes for FU indicator + FU header
			if chunkSize > len(payload) {
				chunkSize = len(payload)
			}

			fuIndicator := nri | fuaType // FU indicator: NRI + type=28
			fuHeader := nalType           // FU header: original NAL type
			if first {
				fuHeader |= 0x80 // S (start) bit
				first = false
			}
			last := chunkSize >= len(payload)
			if last {
				fuHeader |= 0x40 // E (end) bit
			}

			pkt := make([]byte, 2+chunkSize)
			pkt[0] = fuIndicator
			pkt[1] = fuHeader
			copy(pkt[2:], payload[:chunkSize])

			s.writeRTP(pkt, markerBit && last)
			s.bytesSent.Add(int64(len(pkt)))

			payload = payload[chunkSize:]
		}
	}
}

func (s *VideoRTPSender) writeRTP(payload []byte, marker bool) {
	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    rtpPayloadType,
			SequenceNumber: s.seq,
			Timestamp:      s.timestamp,
			Marker:         marker,
		},
		Payload: payload,
	}
	s.seq++

	if err := s.track.WriteRTP(pkt); err != nil {
		// No peers connected — normal
	}
}

// splitAnnexB splits an Annex B byte stream into individual NAL units
// (without start codes). Fast — scans 4 bytes at a time.
func splitAnnexB(data []byte) [][]byte {
	var nals [][]byte
	var positions []int

	// Find all 4-byte start codes (00 00 00 01)
	for i := 0; i <= len(data)-4; i++ {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 0 && data[i+3] == 1 {
			positions = append(positions, i+4)
		}
	}

	for i, start := range positions {
		end := len(data)
		if i+1 < len(positions) {
			end = positions[i+1] - 4
		}
		// Trim trailing zeros (could be part of next start code)
		for end > start && data[end-1] == 0 {
			end--
		}
		if end > start {
			nals = append(nals, data[start:end])
		}
	}

	return nals
}

// h264TimestampFromFPS returns the RTP timestamp increment for a given FPS.
func h264TimestampFromFPS(fps int) uint32 {
	if fps <= 0 {
		fps = 60
	}
	return uint32(rtpClockRate / fps)
}

