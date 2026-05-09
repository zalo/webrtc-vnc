// screenkit_vt — macOS native capture+encode helper for webrtc-vnc.
//
// Captures the main display via ScreenCaptureKit and encodes H.264 in real-time
// via VideoToolbox, writing H.264 Annex B (with SPS/PPS prepended to every IDR)
// to stdout. The Go server consumes the same stream nvfbc_nvenc produces on
// Linux.
//
// CLI flags match the Linux helper:
//   -w <width>      output width  (default 1920)
//   -h <height>     output height (default 1080)
//   -f <fps>        framerate     (default 60)
//   -b <bitrate>    bitrate kbps  (default 8000)
//
// Build: see cmd/screenkit_vt/Makefile

import Foundation
import ScreenCaptureKit
import VideoToolbox
import CoreMedia
import CoreVideo
import IOKit
import Darwin

// ---------------------------------------------------------------------------
// CLI
// ---------------------------------------------------------------------------

struct Config {
    var width: Int = 1920
    var height: Int = 1080
    var fps: Int = 60
    var bitrateKbps: Int = 8000
}

func parseArgs() -> Config {
    var cfg = Config()
    var args = Array(CommandLine.arguments.dropFirst())
    while !args.isEmpty {
        let flag = args.removeFirst()
        guard !args.isEmpty else { break }
        let val = args.removeFirst()
        switch flag {
        case "-w": cfg.width = Int(val) ?? cfg.width
        case "-h": cfg.height = Int(val) ?? cfg.height
        case "-f": cfg.fps = Int(val) ?? cfg.fps
        case "-b": cfg.bitrateKbps = Int(val) ?? cfg.bitrateKbps
        default:
            FileHandle.standardError.write("[screenkit_vt] ignoring unknown flag \(flag)\n".data(using: .utf8)!)
        }
    }
    return cfg
}

func logErr(_ s: String) {
    FileHandle.standardError.write("[screenkit_vt] \(s)\n".data(using: .utf8)!)
}

// ---------------------------------------------------------------------------
// Annex B writer (writes one H.264 Annex B stream to stdout, thread-safe)
// ---------------------------------------------------------------------------

final class AnnexBWriter {
    private let lock = NSLock()
    private let startCode: [UInt8] = [0x00, 0x00, 0x00, 0x01]

    func writeNAL(_ buf: UnsafePointer<UInt8>, _ length: Int) {
        lock.lock(); defer { lock.unlock() }
        startCode.withUnsafeBufferPointer { sc in
            _ = write(1, sc.baseAddress, sc.count)
        }
        _ = write(1, buf, length)
    }

    func writeNALData(_ data: Data) {
        data.withUnsafeBytes { raw in
            guard let base = raw.bindMemory(to: UInt8.self).baseAddress else { return }
            writeNAL(base, raw.count)
        }
    }
}

let writer = AnnexBWriter()

// ---------------------------------------------------------------------------
// IDR-on-demand: SIGUSR1 forces the next frame to be a keyframe
// ---------------------------------------------------------------------------

final class IDRRequest {
    private let lock = NSLock()
    private var pending = 0
    func request() {
        lock.lock(); pending += 1; lock.unlock()
    }
    func consume() -> Bool {
        lock.lock(); defer { lock.unlock() }
        if pending == 0 { return false }
        pending -= 1
        return true
    }
}
let idr = IDRRequest()

func installSignalHandler() {
    signal(SIGUSR1) { _ in idr.request() }
    signal(SIGPIPE, SIG_IGN)
}

// ---------------------------------------------------------------------------
// VideoToolbox encoder
// ---------------------------------------------------------------------------

final class VTEncoder {
    private var session: VTCompressionSession?
    private let cfg: Config
    private var emittedSPS = false

    init(_ cfg: Config) {
        self.cfg = cfg
    }

    func start() throws {
        var spec: CFDictionary? = nil
        if #available(macOS 11.3, *) {
            spec = [
                kVTVideoEncoderSpecification_EnableLowLatencyRateControl as String: true
            ] as CFDictionary
        }

        let status = VTCompressionSessionCreate(
            allocator: kCFAllocatorDefault,
            width: Int32(cfg.width),
            height: Int32(cfg.height),
            codecType: kCMVideoCodecType_H264,
            encoderSpecification: spec,
            imageBufferAttributes: nil,
            compressedDataAllocator: nil,
            outputCallback: encoderOutputCallback,
            refcon: Unmanaged.passUnretained(self).toOpaque(),
            compressionSessionOut: &session
        )
        guard status == noErr, let session = session else {
            throw NSError(domain: "VTEncoder", code: Int(status),
                          userInfo: [NSLocalizedDescriptionKey: "VTCompressionSessionCreate failed (\(status))"])
        }

        // Real-time, baseline, no B-frames, ~1s GOP, target bitrate.
        VTSessionSetProperty(session, key: kVTCompressionPropertyKey_RealTime, value: kCFBooleanTrue)
        VTSessionSetProperty(session, key: kVTCompressionPropertyKey_AllowFrameReordering, value: kCFBooleanFalse)
        VTSessionSetProperty(session, key: kVTCompressionPropertyKey_ProfileLevel, value: kVTProfileLevel_H264_Baseline_AutoLevel)
        VTSessionSetProperty(session, key: kVTCompressionPropertyKey_ExpectedFrameRate, value: NSNumber(value: cfg.fps))
        VTSessionSetProperty(session, key: kVTCompressionPropertyKey_MaxKeyFrameInterval, value: NSNumber(value: cfg.fps))
        VTSessionSetProperty(session, key: kVTCompressionPropertyKey_MaxKeyFrameIntervalDuration, value: NSNumber(value: 1.0))
        VTSessionSetProperty(session, key: kVTCompressionPropertyKey_AverageBitRate, value: NSNumber(value: cfg.bitrateKbps * 1000))
        // Cap peak bitrate as ~1.5x average over 1s — keeps the rate honest.
        let peak = NSNumber(value: cfg.bitrateKbps * 1500)
        let limits: [Any] = [peak, NSNumber(value: 1.0)]
        VTSessionSetProperty(session, key: kVTCompressionPropertyKey_DataRateLimits, value: limits as CFArray)

        VTCompressionSessionPrepareToEncodeFrames(session)
    }

    func encode(_ pixelBuffer: CVPixelBuffer, pts: CMTime) {
        guard let session = session else { return }
        var props: [String: Any] = [:]
        if idr.consume() {
            props[kVTEncodeFrameOptionKey_ForceKeyFrame as String] = true
        }
        VTCompressionSessionEncodeFrame(
            session,
            imageBuffer: pixelBuffer,
            presentationTimeStamp: pts,
            duration: .invalid,
            frameProperties: props as CFDictionary,
            sourceFrameRefcon: nil,
            infoFlagsOut: nil
        )
    }

    func stop() {
        if let session = session {
            VTCompressionSessionCompleteFrames(session, untilPresentationTimeStamp: .invalid)
            VTCompressionSessionInvalidate(session)
        }
        session = nil
    }

    fileprivate func handle(_ status: OSStatus, _ infoFlags: VTEncodeInfoFlags, _ sample: CMSampleBuffer?) {
        guard status == noErr, let sample = sample, CMSampleBufferDataIsReady(sample) else { return }

        // Detect keyframe: NotSync attachment is false/missing on IDRs.
        var isKeyframe = false
        if let attachments = CMSampleBufferGetSampleAttachmentsArray(sample, createIfNecessary: false) as? [[CFString: Any]],
           let first = attachments.first {
            if let notSync = first[kCMSampleAttachmentKey_NotSync] as? Bool {
                isKeyframe = !notSync
            } else {
                isKeyframe = true
            }
        }

        // For each IDR, emit fresh SPS/PPS first.
        if isKeyframe {
            if let format = CMSampleBufferGetFormatDescription(sample) {
                emitParameterSets(format)
            }
        }

        // Walk AVCC NAL units (4-byte big-endian length prefix) and rewrite to Annex B.
        guard let blockBuffer = CMSampleBufferGetDataBuffer(sample) else { return }
        var totalLength = 0
        var dataPointer: UnsafeMutablePointer<Int8>?
        let st = CMBlockBufferGetDataPointer(blockBuffer, atOffset: 0, lengthAtOffsetOut: nil,
                                             totalLengthOut: &totalLength, dataPointerOut: &dataPointer)
        guard st == kCMBlockBufferNoErr, let dp = dataPointer else { return }

        let bytes = UnsafeMutableRawPointer(dp).assumingMemoryBound(to: UInt8.self)
        var offset = 0
        while offset + 4 <= totalLength {
            let n = (UInt32(bytes[offset]) << 24) |
                    (UInt32(bytes[offset+1]) << 16) |
                    (UInt32(bytes[offset+2]) << 8)  |
                     UInt32(bytes[offset+3])
            offset += 4
            let len = Int(n)
            if len <= 0 || offset + len > totalLength { break }
            writer.writeNAL(bytes.advanced(by: offset), len)
            offset += len
        }
    }

    private func emitParameterSets(_ format: CMFormatDescription) {
        var count = 0
        var nalUnitHeaderLength: Int32 = 0
        if CMVideoFormatDescriptionGetH264ParameterSetAtIndex(format, parameterSetIndex: 0,
                                                              parameterSetPointerOut: nil,
                                                              parameterSetSizeOut: nil,
                                                              parameterSetCountOut: &count,
                                                              nalUnitHeaderLengthOut: &nalUnitHeaderLength) != noErr {
            return
        }
        for i in 0..<count {
            var ptr: UnsafePointer<UInt8>? = nil
            var size: Int = 0
            if CMVideoFormatDescriptionGetH264ParameterSetAtIndex(format, parameterSetIndex: i,
                                                                  parameterSetPointerOut: &ptr,
                                                                  parameterSetSizeOut: &size,
                                                                  parameterSetCountOut: nil,
                                                                  nalUnitHeaderLengthOut: nil) == noErr,
               let ptr = ptr {
                writer.writeNAL(ptr, size)
            }
        }
        emittedSPS = true
    }
}

// VideoToolbox C-style callback bridges back into the Swift VTEncoder instance.
let encoderOutputCallback: VTCompressionOutputCallback = { refcon, _, status, infoFlags, sample in
    guard let refcon = refcon else { return }
    let enc = Unmanaged<VTEncoder>.fromOpaque(refcon).takeUnretainedValue()
    enc.handle(status, infoFlags, sample)
}

// ---------------------------------------------------------------------------
// ScreenCaptureKit pump
// ---------------------------------------------------------------------------

@available(macOS 12.3, *)
final class CaptureSession: NSObject, SCStreamOutput, SCStreamDelegate {
    let cfg: Config
    let encoder: VTEncoder
    var stream: SCStream?

    init(_ cfg: Config, _ encoder: VTEncoder) {
        self.cfg = cfg
        self.encoder = encoder
    }

    func start() async throws {
        let content = try await SCShareableContent.excludingDesktopWindows(false, onScreenWindowsOnly: true)
        guard let display = content.displays.first else {
            throw NSError(domain: "CaptureSession", code: 1,
                          userInfo: [NSLocalizedDescriptionKey: "no displays available"])
        }

        let filter = SCContentFilter(display: display, excludingWindows: [])
        let streamCfg = SCStreamConfiguration()
        streamCfg.width = cfg.width
        streamCfg.height = cfg.height
        streamCfg.minimumFrameInterval = CMTime(value: 1, timescale: CMTimeScale(cfg.fps))
        streamCfg.pixelFormat = kCVPixelFormatType_420YpCbCr8BiPlanarFullRange
        streamCfg.queueDepth = 5
        streamCfg.showsCursor = true
        streamCfg.scalesToFit = true
        if #available(macOS 13.0, *) {
            streamCfg.capturesAudio = false
        }

        let s = SCStream(filter: filter, configuration: streamCfg, delegate: self)
        try s.addStreamOutput(self, type: .screen, sampleHandlerQueue: DispatchQueue(label: "screenkit_vt.sample"))
        try await s.startCapture()
        stream = s
    }

    func stop() async {
        if let stream = stream {
            try? await stream.stopCapture()
        }
    }

    // SCStreamDelegate
    func stream(_ stream: SCStream, didStopWithError error: Error) {
        logErr("SCStream stopped: \(error.localizedDescription)")
        exit(1)
    }

    // SCStreamOutput
    func stream(_ stream: SCStream, didOutputSampleBuffer sampleBuffer: CMSampleBuffer, of type: SCStreamOutputType) {
        guard type == .screen, CMSampleBufferIsValid(sampleBuffer) else { return }
        guard let attachments = CMSampleBufferGetSampleAttachmentsArray(sampleBuffer, createIfNecessary: false) as? [[SCStreamFrameInfo: Any]],
              let info = attachments.first,
              let statusRaw = info[.status] as? Int,
              let status = SCFrameStatus(rawValue: statusRaw),
              status == .complete else {
            return
        }
        guard let imageBuffer = CMSampleBufferGetImageBuffer(sampleBuffer) else { return }
        let pts = CMSampleBufferGetPresentationTimeStamp(sampleBuffer)
        encoder.encode(imageBuffer, pts: pts)
    }
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

let cfg = parseArgs()
installSignalHandler()

if #available(macOS 12.3, *) {
    let encoder = VTEncoder(cfg)
    do { try encoder.start() } catch {
        logErr("encoder init failed: \(error)")
        exit(2)
    }

    let session = CaptureSession(cfg, encoder)
    let semaphore = DispatchSemaphore(value: 0)

    Task {
        do {
            try await session.start()
            logErr("capture started: \(cfg.width)x\(cfg.height)@\(cfg.fps) bitrate=\(cfg.bitrateKbps)kbps")
        } catch {
            logErr("start failed: \(error)")
            logErr("hint: grant Screen Recording permission in System Settings → Privacy & Security")
            exit(3)
        }
    }

    signal(SIGINT)  { _ in exit(0) }
    signal(SIGTERM) { _ in exit(0) }

    semaphore.wait() // run forever; signal handlers exit the process
} else {
    logErr("ScreenCaptureKit requires macOS 12.3 or newer")
    exit(2)
}
