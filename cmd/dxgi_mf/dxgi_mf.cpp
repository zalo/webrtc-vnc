// dxgi_mf — Windows native capture+encode helper for webrtc-vnc.
//
// Captures the primary monitor with the DXGI Desktop Duplication API and
// encodes H.264 with the Media Foundation Sink Writer, which transparently
// dispatches to NVIDIA NVENC, Intel QuickSync, or AMD AMF depending on the
// installed GPU. Writes H.264 Annex B (with SPS/PPS prepended to every IDR)
// to stdout — same protocol the Go server already speaks for nvfbc_nvenc.
//
// CLI flags (matches Linux nvfbc_nvenc):
//   -w <width>      output width   (default 1920)
//   -h <height>     output height  (default 1080)
//   -f <fps>        framerate      (default 60)
//   -b <bitrate>    bitrate kbps   (default 8000)
//
// Build with cl.exe / clang-cl using cmd/dxgi_mf/build.bat.
//
// Notes:
//   - We deliberately avoid the Windows console handler entirely; SIGINT-style
//     termination comes from the parent process closing stdout, which makes
//     our writes fail with ERROR_BROKEN_PIPE and we exit cleanly.
//   - On Windows there's no SIGUSR1; instead we listen on stdin for a single
//     'k' byte to mean "force keyframe on next encode". The Go side will need
//     to write that byte through StdinPipe when it wants an IDR (TODO on the
//     Go side; for now keyframes occur on the regular GOP boundary).

#define WIN32_LEAN_AND_MEAN
#define NOMINMAX
#include <windows.h>
#include <wrl/client.h>
#include <d3d11.h>
#include <d3d11_1.h>
#include <dxgi1_2.h>
#include <mfapi.h>
#include <mfidl.h>
#include <mfreadwrite.h>
#include <mferror.h>
#include <codecapi.h>
#include <wmcodecdsp.h>
#include <io.h>
#include <fcntl.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <atomic>
#include <vector>
#include <thread>
#include <chrono>
#include <cstdio>

#pragma comment(lib, "d3d11.lib")
#pragma comment(lib, "dxgi.lib")
#pragma comment(lib, "mf.lib")
#pragma comment(lib, "mfplat.lib")
#pragma comment(lib, "mfreadwrite.lib")
#pragma comment(lib, "mfuuid.lib")
#pragma comment(lib, "ole32.lib")
#pragma comment(lib, "wmcodecdspuuid.lib")

using Microsoft::WRL::ComPtr;

struct Config {
    int width = 1920;
    int height = 1080;
    int fps = 60;
    int bitrateKbps = 8000;
};

static std::atomic<bool> g_running{true};
static std::atomic<bool> g_force_idr{false};

static void log_err(const char *fmt, ...) {
    char buf[1024];
    va_list ap; va_start(ap, fmt);
    int n = vsnprintf(buf, sizeof(buf), fmt, ap);
    va_end(ap);
    if (n < 0) return;
    fputs("[dxgi_mf] ", stderr); fwrite(buf, 1, (size_t)n, stderr); fputc('\n', stderr);
}

static Config parse_args(int argc, char **argv) {
    Config c;
    for (int i = 1; i + 1 < argc; i += 2) {
        const char *flag = argv[i];
        const char *val  = argv[i + 1];
        if      (!strcmp(flag, "-w")) c.width = atoi(val);
        else if (!strcmp(flag, "-h")) c.height = atoi(val);
        else if (!strcmp(flag, "-f")) c.fps = atoi(val);
        else if (!strcmp(flag, "-b")) c.bitrateKbps = atoi(val);
        else log_err("ignoring unknown flag %s", flag);
    }
    return c;
}

// ---------------------------------------------------------------------------
// Annex B writer: thread-safe stdout. Writes a 4-byte start code then payload.
// ---------------------------------------------------------------------------

static CRITICAL_SECTION g_writer_lock;
static bool g_writer_inited = false;

static void writer_init() {
    if (!g_writer_inited) {
        InitializeCriticalSection(&g_writer_lock);
        g_writer_inited = true;
        // Ensure stdout is binary — text-mode would mangle 0x0A bytes on Windows.
        _setmode(_fileno(stdout), _O_BINARY);
    }
}

static bool write_all(const void *buf, size_t n) {
    HANDLE h = GetStdHandle(STD_OUTPUT_HANDLE);
    const uint8_t *p = (const uint8_t *)buf;
    while (n > 0) {
        DWORD wrote = 0;
        if (!WriteFile(h, p, (DWORD)n, &wrote, NULL) || wrote == 0) return false;
        p += wrote; n -= wrote;
    }
    return true;
}

static bool write_nal(const uint8_t *buf, size_t n) {
    static const uint8_t SC[4] = { 0, 0, 0, 1 };
    EnterCriticalSection(&g_writer_lock);
    bool ok = write_all(SC, 4) && write_all(buf, n);
    LeaveCriticalSection(&g_writer_lock);
    return ok;
}

// ---------------------------------------------------------------------------
// Walk an AVCC payload (4-byte big-endian length prefixes) and emit each NAL
// in Annex B framing.
// ---------------------------------------------------------------------------

static bool emit_avcc_as_annexb(const uint8_t *avcc, size_t total) {
    size_t off = 0;
    while (off + 4 <= total) {
        uint32_t n = ((uint32_t)avcc[off]   << 24) |
                     ((uint32_t)avcc[off+1] << 16) |
                     ((uint32_t)avcc[off+2] << 8)  |
                      (uint32_t)avcc[off+3];
        off += 4;
        if (n == 0 || off + n > total) return false;
        if (!write_nal(avcc + off, n)) return false;
        off += n;
    }
    return true;
}

// Emit SPS/PPS extracted from MF_MT_MPEG_SEQUENCE_HEADER.
static bool emit_sps_pps(IMFMediaType *outputType) {
    UINT32 size = 0;
    if (FAILED(outputType->GetBlobSize(MF_MT_MPEG_SEQUENCE_HEADER, &size)) || size == 0) return true;
    std::vector<uint8_t> blob(size);
    if (FAILED(outputType->GetBlob(MF_MT_MPEG_SEQUENCE_HEADER, blob.data(), size, NULL))) return true;

    // The blob is itself in Annex-B-ish framing (start codes 0x00000001 between NALs).
    // Walk it and emit each NAL via write_nal so we get consistent 4-byte start codes.
    size_t i = 0;
    while (i + 4 <= blob.size()) {
        if (blob[i] == 0 && blob[i+1] == 0 && blob[i+2] == 0 && blob[i+3] == 1) {
            size_t start = i + 4;
            size_t end = start;
            while (end + 4 <= blob.size()) {
                if (blob[end] == 0 && blob[end+1] == 0 && blob[end+2] == 0 && blob[end+3] == 1) break;
                end++;
            }
            if (end + 4 > blob.size()) end = blob.size();
            if (!write_nal(blob.data() + start, end - start)) return false;
            i = end;
        } else {
            i++;
        }
    }
    return true;
}

// ---------------------------------------------------------------------------
// stdin watcher: read 'k' to force a keyframe on the next encode.
// ---------------------------------------------------------------------------

static void stdin_watcher() {
    HANDLE h = GetStdHandle(STD_INPUT_HANDLE);
    if (h == INVALID_HANDLE_VALUE) return;
    uint8_t b;
    DWORD n;
    while (g_running && ReadFile(h, &b, 1, &n, NULL) && n == 1) {
        if (b == 'k') g_force_idr.store(true);
    }
}

// ---------------------------------------------------------------------------
// MediaFoundation encoder.
//
// Two paths:
//
//   1. Hardware MFT (preferred): MFTEnumEx finds the NVENC / QSV / AMF MFT
//      that advertises NV12->H264. We attach a D3D11 device manager so the
//      encoder consumes our DXGI textures directly with zero CPU copy.
//
//      Hardware MFTs on Windows are async — events are delivered via
//      IMFAsyncCallback (BeginGetEvent / EndGetEvent), NOT via synchronous
//      GetEvent. Driving them with GetEvent appears to work but the MFT's
//      internal event queue is never populated, so calls block forever.
//
//   2. Software MFT (fallback): the in-box CMSH264EncoderMFT. It does not
//      accept GPU surfaces, so we copy the NV12 texture into a STAGING
//      texture, Map() it to system memory, and feed it as a memory buffer.
//
// Both paths emit H.264 Annex B (SPS/PPS prepended to every IDR) on stdout
// via the shared write_nal/emit_avcc_as_annexb helpers above.
// ---------------------------------------------------------------------------

class MFEncoder; // fwd decl

// CaptureCallback is invoked from the MF worker thread when the encoder needs
// a fresh frame (METransformNeedInput). Implementations should populate `out`
// with an NV12 ID3D11Texture2D and return S_OK. They run on the MF worker
// thread, so D3D11/DXGI calls inside must coordinate with anything else that
// touches the device. In our case all D3D11 work happens on this same worker
// thread, so no extra synchronisation is needed.
using CaptureCallback = std::function<HRESULT(ComPtr<ID3D11Texture2D> &)>;

// MFEventSink listens for METransformNeedInput / METransformHaveOutput on the
// MFT's IMFMediaEventGenerator and dispatches back into MFEncoder. Async MFTs
// require this callback flavour — the synchronous GetEvent path doesn't get
// populated.
class MFEventSink : public IMFAsyncCallback {
public:
    explicit MFEventSink(MFEncoder *owner) : owner_(owner) {}

    ULONG STDMETHODCALLTYPE AddRef() override {
        return InterlockedIncrement(&ref_);
    }
    ULONG STDMETHODCALLTYPE Release() override {
        ULONG c = (ULONG)InterlockedDecrement(&ref_);
        if (c == 0) delete this;
        return c;
    }
    HRESULT STDMETHODCALLTYPE QueryInterface(REFIID iid, void **out) override {
        if (out == NULL) return E_POINTER;
        if (iid == IID_IUnknown || iid == __uuidof(IMFAsyncCallback)) {
            *out = static_cast<IMFAsyncCallback *>(this);
            AddRef();
            return S_OK;
        }
        *out = NULL;
        return E_NOINTERFACE;
    }
    HRESULT STDMETHODCALLTYPE GetParameters(DWORD *flags, DWORD *queue) override {
        if (flags) *flags = 0;
        if (queue) *queue = 0;
        return E_NOTIMPL; // default queue
    }
    HRESULT STDMETHODCALLTYPE Invoke(IMFAsyncResult *result) override; // defined after MFEncoder

private:
    LONG ref_ = 1;
    MFEncoder *owner_; // non-owning; MFEncoder outlives the sink
};

class MFEncoder {
public:
    HRESULT init(const Config &cfg, ID3D11Device *device, ID3D11DeviceContext *context) {
        cfg_     = cfg;
        device_  = device;
        context_ = context;
        hnsPerFrame_ = 10000000LL / cfg.fps;

        HRESULT hr = MFStartup(MF_VERSION);
        if (FAILED(hr)) return hr;

        if (SUCCEEDED(initHardware())) {
            log_err("encoder: hardware MFT (async=%d)", isAsync_ ? 1 : 0);
            return S_OK;
        }
        log_err("encoder: hardware MFT not available, falling back to software");
        return initSoftware();
    }

    bool isAsync() const { return isAsync_; }

    // Sync-MFT path. The software fallback (and rare sync hardware MFTs)
    // process synchronously: caller hands us a frame, we feed + drain.
    HRESULT encodeSync(ID3D11Texture2D *nv12, INT64 ptsHns) {
        HRESULT hr;
        if (isHardware_) {
            hr = feedGPUSample(nv12, ptsHns);
        } else {
            hr = feedSystemMemorySample(nv12, ptsHns);
        }
        if (FAILED(hr)) return hr;
        return drainAll();
    }

    // Async-MFT path. Hand us a capture callback and we'll drive the encoder's
    // event loop until shutdown. All MFT and D3D11 calls happen on the MF
    // worker thread that fires onAsyncEvent.
    HRESULT startAsyncEvents(CaptureCallback cb) {
        if (!eventGen_) return E_UNEXPECTED;
        captureCb_ = std::move(cb);
        eventSink_ = new MFEventSink(this);
        // Arm the first event request — subsequent re-arms happen inside Invoke().
        return eventGen_->BeginGetEvent(eventSink_, NULL);
    }

    // Invoked from MFEventSink::Invoke — runs on an MF worker thread.
    void onAsyncEvent(MediaEventType type) {
        switch (type) {
        case METransformNeedInput: {
            if (!captureCb_) return;
            ComPtr<ID3D11Texture2D> nv12;
            HRESULT hr = captureCb_(nv12);
            if (FAILED(hr) || !nv12) return; // we'll get another NeedInput soon
            INT64 pts = frames_ * hnsPerFrame_;
            hr = feedGPUSample(nv12.Get(), pts);
            if (FAILED(hr)) {
                log_err("async feedGPUSample 0x%lx", hr);
                return;
            }
            frames_++;
            break;
        }
        case METransformHaveOutput:
            drainOnce();
            break;
        case METransformDrainComplete:
            // we don't currently issue COMMAND_DRAIN on the encoder, so this
            // shouldn't fire — but if it does, drain anything pending.
            drainAll();
            break;
        default:
            break;
        }
    }

    // Friends — MFEventSink::Invoke pokes the event generator + dispatches.
    friend class MFEventSink;

    void shutdown() {
        if (mft_) {
            mft_->ProcessMessage(MFT_MESSAGE_NOTIFY_END_OF_STREAM, 0);
            mft_->ProcessMessage(MFT_MESSAGE_COMMAND_DRAIN, 0);
            mft_->ProcessMessage(MFT_MESSAGE_NOTIFY_END_STREAMING, 0);
        }
        mft_.Reset();
        outputType_.Reset();
        deviceManager_.Reset();
        if (eventSink_) {
            eventSink_->Release();
            eventSink_ = nullptr;
        }
        eventGen_.Reset();
        stagingNV12_.Reset();
        MFShutdown();
    }

private:
    HRESULT initHardware() {
        MFT_REGISTER_TYPE_INFO inInfo  = { MFMediaType_Video, MFVideoFormat_NV12 };
        MFT_REGISTER_TYPE_INFO outInfo = { MFMediaType_Video, MFVideoFormat_H264 };

        IMFActivate **activates = NULL;
        UINT32 count = 0;
        UINT32 flags = MFT_ENUM_FLAG_HARDWARE | MFT_ENUM_FLAG_SORTANDFILTER;
        HRESULT hr = MFTEnumEx(MFT_CATEGORY_VIDEO_ENCODER, flags, &inInfo, &outInfo, &activates, &count);
        if (FAILED(hr) || count == 0) {
            if (activates) CoTaskMemFree(activates);
            return E_FAIL;
        }

        ComPtr<IMFTransform> mft;
        hr = activates[0]->ActivateObject(IID_PPV_ARGS(&mft));
        for (UINT32 i = 0; i < count; i++) activates[i]->Release();
        CoTaskMemFree(activates);
        if (FAILED(hr)) return hr;

        ComPtr<IMFAttributes> attrs;
        hr = mft->GetAttributes(&attrs);
        if (FAILED(hr)) return hr;

        UINT32 isAsync = 0;
        attrs->GetUINT32(MF_TRANSFORM_ASYNC, &isAsync);
        isAsync_ = (isAsync != 0);
        if (isAsync_) {
            attrs->SetUINT32(MF_TRANSFORM_ASYNC_UNLOCK, TRUE);
        }

        // Attach D3D11 device so the MFT can consume our textures directly.
        hr = MFCreateDXGIDeviceManager(&deviceManagerToken_, &deviceManager_);
        if (FAILED(hr)) return hr;
        hr = deviceManager_->ResetDevice(device_, deviceManagerToken_);
        if (FAILED(hr)) return hr;
        hr = mft->ProcessMessage(MFT_MESSAGE_SET_D3D_MANAGER, (ULONG_PTR)deviceManager_.Get());
        if (FAILED(hr)) {
            log_err("MFT_MESSAGE_SET_D3D_MANAGER failed 0x%lx", hr);
            return hr;
        }

        if ((hr = configureTypes(mft.Get())) != S_OK) return hr;

        // Codec config — low latency + CBR + 1s GOP.
        attrs->SetUINT32(CODECAPI_AVEncCommonRateControlMode, eAVEncCommonRateControlMode_CBR);
        attrs->SetUINT32(CODECAPI_AVLowLatencyMode, TRUE);
        attrs->SetUINT32(CODECAPI_AVEncMPVGOPSize, (UINT32)cfg_.fps);
        attrs->SetUINT32(MF_LOW_LATENCY, TRUE);

        if (isAsync_) {
            hr = mft.As(&eventGen_);
            if (FAILED(hr)) return hr;
        }

        hr = mft->ProcessMessage(MFT_MESSAGE_COMMAND_FLUSH, 0);
        if (FAILED(hr)) return hr;
        hr = mft->ProcessMessage(MFT_MESSAGE_NOTIFY_BEGIN_STREAMING, 0);
        if (FAILED(hr)) return hr;
        hr = mft->ProcessMessage(MFT_MESSAGE_NOTIFY_START_OF_STREAM, 0);
        if (FAILED(hr)) return hr;

        mft_         = mft;
        isHardware_  = true;
        return S_OK;
    }

    HRESULT initSoftware() {
        ComPtr<IMFTransform> mft;
        HRESULT hr = CoCreateInstance(CLSID_CMSH264EncoderMFT, NULL, CLSCTX_INPROC_SERVER,
                                      IID_PPV_ARGS(&mft));
        if (FAILED(hr)) return hr;

        ComPtr<IMFAttributes> mftAttrs;
        if (SUCCEEDED(mft->GetAttributes(&mftAttrs))) {
            mftAttrs->SetUINT32(MF_LOW_LATENCY, TRUE);
            mftAttrs->SetUINT32(CODECAPI_AVEncCommonRateControlMode, eAVEncCommonRateControlMode_CBR);
            mftAttrs->SetUINT32(CODECAPI_AVLowLatencyMode, TRUE);
            mftAttrs->SetUINT32(CODECAPI_AVEncMPVGOPSize, (UINT32)cfg_.fps);
        }

        if ((hr = configureTypes(mft.Get())) != S_OK) return hr;

        // Staging NV12 texture for GPU→system memory readback (software MFT
        // cannot consume DXGI surfaces).
        D3D11_TEXTURE2D_DESC sd{};
        sd.Width            = cfg_.width;
        sd.Height           = cfg_.height;
        sd.MipLevels        = 1;
        sd.ArraySize        = 1;
        sd.Format           = DXGI_FORMAT_NV12;
        sd.SampleDesc.Count = 1;
        sd.Usage            = D3D11_USAGE_STAGING;
        sd.BindFlags        = 0;
        sd.CPUAccessFlags   = D3D11_CPU_ACCESS_READ;
        hr = device_->CreateTexture2D(&sd, NULL, &stagingNV12_);
        if (FAILED(hr)) return hr;

        hr = mft->ProcessMessage(MFT_MESSAGE_NOTIFY_BEGIN_STREAMING, 0);
        if (FAILED(hr)) return hr;

        mft_        = mft;
        isHardware_ = false;
        isAsync_    = false;
        return S_OK;
    }

    HRESULT configureTypes(IMFTransform *mft) {
        ComPtr<IMFMediaType> outType;
        MFCreateMediaType(&outType);
        outType->SetGUID(MF_MT_MAJOR_TYPE,   MFMediaType_Video);
        outType->SetGUID(MF_MT_SUBTYPE,      MFVideoFormat_H264);
        outType->SetUINT32(MF_MT_AVG_BITRATE,      (UINT32)(cfg_.bitrateKbps * 1000));
        outType->SetUINT32(MF_MT_INTERLACE_MODE,    MFVideoInterlace_Progressive);
        outType->SetUINT32(MF_MT_MPEG2_PROFILE,     eAVEncH264VProfile_Base);
        MFSetAttributeSize (outType.Get(), MF_MT_FRAME_SIZE,         cfg_.width, cfg_.height);
        MFSetAttributeRatio(outType.Get(), MF_MT_FRAME_RATE,         cfg_.fps, 1);
        MFSetAttributeRatio(outType.Get(), MF_MT_PIXEL_ASPECT_RATIO, 1, 1);

        ComPtr<IMFMediaType> inType;
        MFCreateMediaType(&inType);
        inType->SetGUID(MF_MT_MAJOR_TYPE, MFMediaType_Video);
        inType->SetGUID(MF_MT_SUBTYPE,    MFVideoFormat_NV12);
        inType->SetUINT32(MF_MT_INTERLACE_MODE, MFVideoInterlace_Progressive);
        MFSetAttributeSize (inType.Get(), MF_MT_FRAME_SIZE,         cfg_.width, cfg_.height);
        MFSetAttributeRatio(inType.Get(), MF_MT_FRAME_RATE,         cfg_.fps, 1);
        MFSetAttributeRatio(inType.Get(), MF_MT_PIXEL_ASPECT_RATIO, 1, 1);

        // Output type must be set first for both software and hardware MFTs.
        HRESULT hr = mft->SetOutputType(0, outType.Get(), 0);
        if (FAILED(hr)) { log_err("SetOutputType 0x%lx", hr); return hr; }
        hr = mft->SetInputType(0, inType.Get(), 0);
        if (FAILED(hr)) { log_err("SetInputType 0x%lx", hr);  return hr; }

        // Cache the post-negotiation output type so we can fish SPS/PPS out of
        // its MF_MT_MPEG_SEQUENCE_HEADER blob.
        mft->GetOutputCurrentType(0, &outputType_);
        return S_OK;
    }

    HRESULT feedGPUSample(ID3D11Texture2D *nv12, INT64 ptsHns) {
        ComPtr<IMFMediaBuffer> buffer;
        HRESULT hr = MFCreateDXGISurfaceBuffer(__uuidof(ID3D11Texture2D), nv12, 0, FALSE, &buffer);
        if (FAILED(hr)) { log_err("MFCreateDXGISurfaceBuffer 0x%lx", hr); return hr; }

        ComPtr<IMFSample> sample;
        MFCreateSample(&sample);
        sample->AddBuffer(buffer.Get());
        sample->SetSampleTime(ptsHns);
        sample->SetSampleDuration(10000000LL / cfg_.fps);

        applyForceIDRIfRequested(sample.Get());

        hr = mft_->ProcessInput(0, sample.Get(), 0);
        if (FAILED(hr)) { log_err("ProcessInput (gpu) 0x%lx", hr); return hr; }
        return S_OK;
    }

    HRESULT feedSystemMemorySample(ID3D11Texture2D *nv12, INT64 ptsHns) {
        // GPU NV12 texture → staging → system memory MF buffer.
        context_->CopyResource(stagingNV12_.Get(), nv12);

        D3D11_MAPPED_SUBRESOURCE map{};
        HRESULT hr = context_->Map(stagingNV12_.Get(), 0, D3D11_MAP_READ, 0, &map);
        if (FAILED(hr)) { log_err("Map staging 0x%lx", hr); return hr; }

        DWORD totalBytes = (DWORD)(cfg_.width * cfg_.height * 3 / 2);
        ComPtr<IMFMediaBuffer> buffer;
        hr = MFCreateMemoryBuffer(totalBytes, &buffer);
        if (FAILED(hr)) { context_->Unmap(stagingNV12_.Get(), 0); return hr; }

        BYTE *dst = NULL;
        DWORD maxLen = 0, currentLen = 0;
        buffer->Lock(&dst, &maxLen, &currentLen);

        const uint8_t *srcY = (const uint8_t *)map.pData;
        for (int row = 0; row < cfg_.height; row++) {
            memcpy(dst + (size_t)row * cfg_.width, srcY + (size_t)row * map.RowPitch, cfg_.width);
        }
        BYTE *uvDst         = dst + (size_t)cfg_.width * cfg_.height;
        const uint8_t *srcUV = (const uint8_t *)map.pData + (size_t)map.RowPitch * cfg_.height;
        for (int row = 0; row < cfg_.height / 2; row++) {
            memcpy(uvDst + (size_t)row * cfg_.width, srcUV + (size_t)row * map.RowPitch, cfg_.width);
        }

        buffer->SetCurrentLength(totalBytes);
        buffer->Unlock();
        context_->Unmap(stagingNV12_.Get(), 0);

        ComPtr<IMFSample> sample;
        MFCreateSample(&sample);
        sample->AddBuffer(buffer.Get());
        sample->SetSampleTime(ptsHns);
        sample->SetSampleDuration(10000000LL / cfg_.fps);
        applyForceIDRIfRequested(sample.Get());

        hr = mft_->ProcessInput(0, sample.Get(), 0);
        if (FAILED(hr)) { log_err("ProcessInput (cpu) 0x%lx", hr); return hr; }
        return S_OK;
    }

    void applyForceIDRIfRequested(IMFSample *sample) {
        if (!g_force_idr.exchange(false)) return;
        sample->SetUINT32(MFSampleExtension_CleanPoint, TRUE);
        ComPtr<IMFAttributes> mftAttrs;
        if (SUCCEEDED(mft_->GetAttributes(&mftAttrs))) {
            mftAttrs->SetUINT32(CODECAPI_AVEncVideoForceKeyFrame, 1);
        }
    }

    // Pull every output sample currently available. Used after a sync
    // ProcessInput. Async MFTs use drainOnce() driven by HaveOutput events.
    HRESULT drainAll() {
        for (;;) {
            HRESULT hr = drainOnce();
            if (hr == MF_E_TRANSFORM_NEED_MORE_INPUT) return S_OK;
            if (FAILED(hr)) return hr;
        }
    }

    HRESULT drainOnce() {
        MFT_OUTPUT_STREAM_INFO si{};
        mft_->GetOutputStreamInfo(0, &si);

        DWORD status = 0;
        MFT_OUTPUT_DATA_BUFFER outBuf{};
        outBuf.dwStreamID = 0;

        ComPtr<IMFSample> ourSample;
        if (!(si.dwFlags & (MFT_OUTPUT_STREAM_PROVIDES_SAMPLES |
                            MFT_OUTPUT_STREAM_CAN_PROVIDE_SAMPLES))) {
            // Caller must allocate the output buffer.
            HRESULT hr = MFCreateSample(&ourSample);
            if (FAILED(hr)) return hr;
            ComPtr<IMFMediaBuffer> outBuffer;
            hr = MFCreateMemoryBuffer(si.cbSize, &outBuffer);
            if (FAILED(hr)) return hr;
            ourSample->AddBuffer(outBuffer.Get());
            outBuf.pSample = ourSample.Get();
            outBuf.pSample->AddRef();
        }

        HRESULT hr = mft_->ProcessOutput(0, 1, &outBuf, &status);
        if (hr == MF_E_TRANSFORM_NEED_MORE_INPUT) return hr;

        // The MFT may renegotiate the output type partway through (e.g. when
        // it discovers the actual encoder profile). Honor that and keep going.
        if (hr == MF_E_TRANSFORM_STREAM_CHANGE) {
            ComPtr<IMFMediaType> newOut;
            DWORD typeIdx = 0;
            while (mft_->GetOutputAvailableType(0, typeIdx++, &newOut) == S_OK) {
                GUID sub;
                if (SUCCEEDED(newOut->GetGUID(MF_MT_SUBTYPE, &sub)) && sub == MFVideoFormat_H264) {
                    mft_->SetOutputType(0, newOut.Get(), 0);
                    outputType_ = newOut;
                    break;
                }
                newOut.Reset();
            }
            return S_OK;
        }
        if (FAILED(hr)) {
            log_err("ProcessOutput 0x%lx", hr);
            return hr;
        }

        ComPtr<IMFSample> sample;
        sample.Attach(outBuf.pSample);
        if (outBuf.pEvents) outBuf.pEvents->Release();

        // Emit SPS/PPS for the very first compressed sample, and re-emit on
        // any keyframe (CleanPoint) so the receiver can decode mid-stream.
        if (!sentSPSPPS_ && outputType_) {
            emit_sps_pps(outputType_.Get());
            sentSPSPPS_ = true;
        }
        UINT32 clean = 0;
        sample->GetUINT32(MFSampleExtension_CleanPoint, &clean);
        if (clean && outputType_) {
            emit_sps_pps(outputType_.Get());
        }

        ComPtr<IMFMediaBuffer> buf;
        sample->ConvertToContiguousBuffer(&buf);
        BYTE *data = NULL;
        DWORD size = 0;
        buf->Lock(&data, NULL, &size);
        emit_avcc_as_annexb(data, size);
        buf->Unlock();
        return S_OK;
    }

    Config cfg_{};
    ID3D11Device         *device_  = nullptr;
    ID3D11DeviceContext  *context_ = nullptr;
    ComPtr<IMFTransform>  mft_;
    ComPtr<IMFMediaType>  outputType_;
    ComPtr<IMFDXGIDeviceManager> deviceManager_;
    UINT                  deviceManagerToken_ = 0;
    ComPtr<IMFMediaEventGenerator> eventGen_;
    ComPtr<ID3D11Texture2D> stagingNV12_;
    bool isHardware_ = false;
    bool isAsync_    = false;
    bool sentSPSPPS_ = false;

    // Async path state
    CaptureCallback captureCb_;
    MFEventSink    *eventSink_ = nullptr;
    INT64           hnsPerFrame_ = 0;
    INT64           frames_      = 0;
};

// MFEventSink::Invoke — defined after MFEncoder so it can dispatch into it.
inline HRESULT MFEventSink::Invoke(IMFAsyncResult *result) {
    if (!owner_ || !owner_->eventGen_) return S_OK;
    ComPtr<IMFMediaEvent> evt;
    HRESULT hr = owner_->eventGen_->EndGetEvent(result, &evt);
    if (FAILED(hr)) {
        log_err("EndGetEvent 0x%lx", hr);
        return S_OK;
    }
    MediaEventType type = MEUnknown;
    evt->GetType(&type);
    owner_->onAsyncEvent(type);

    if (g_running) {
        // Re-arm. If the MFT is shutting down this returns MF_E_SHUTDOWN; ignore.
        owner_->eventGen_->BeginGetEvent(this, NULL);
    }
    return S_OK;
}

// ---------------------------------------------------------------------------
// DXGI Desktop Duplication
// ---------------------------------------------------------------------------

class DesktopDup {
public:
    HRESULT init() {
        ComPtr<IDXGIFactory1> factory;
        HRESULT hr = CreateDXGIFactory1(IID_PPV_ARGS(&factory));
        if (FAILED(hr)) return hr;

        ComPtr<IDXGIAdapter1> adapter;
        hr = factory->EnumAdapters1(0, &adapter);
        if (FAILED(hr)) return hr;

        D3D_FEATURE_LEVEL fl;
        hr = D3D11CreateDevice(adapter.Get(), D3D_DRIVER_TYPE_UNKNOWN, NULL,
                               D3D11_CREATE_DEVICE_BGRA_SUPPORT,
                               NULL, 0, D3D11_SDK_VERSION, &device_, &fl, &context_);
        if (FAILED(hr)) return hr;

        ComPtr<IDXGIOutput> output;
        hr = adapter->EnumOutputs(0, &output);
        if (FAILED(hr)) return hr;
        ComPtr<IDXGIOutput1> out1;
        hr = output.As(&out1);
        if (FAILED(hr)) return hr;

        hr = out1->DuplicateOutput(device_.Get(), &dup_);
        return hr;
    }

    // Acquire next desktop frame as a BGRA texture. Caller must release.
    HRESULT acquire(ComPtr<ID3D11Texture2D> &outTex, UINT64 timeoutMs) {
        DXGI_OUTDUPL_FRAME_INFO info{};
        ComPtr<IDXGIResource> resource;
        HRESULT hr = dup_->AcquireNextFrame((UINT)timeoutMs, &info, &resource);
        if (hr == DXGI_ERROR_WAIT_TIMEOUT) return hr;
        if (FAILED(hr)) return hr;
        hr = resource.As(&outTex);
        return hr;
    }

    void release() { dup_->ReleaseFrame(); }

    ID3D11Device *device() { return device_.Get(); }
    ID3D11DeviceContext *context() { return context_.Get(); }

private:
    ComPtr<ID3D11Device> device_;
    ComPtr<ID3D11DeviceContext> context_;
    ComPtr<IDXGIOutputDuplication> dup_;
};

// ---------------------------------------------------------------------------
// BGRA → NV12 conversion via an ID3D11VideoProcessor (GPU-only path).
// ---------------------------------------------------------------------------

class NV12Converter {
public:
    HRESULT init(ID3D11Device *device, ID3D11DeviceContext *context, int w, int h) {
        device_ = device;
        context_ = context;
        width_ = w; height_ = h;

        HRESULT hr = device->QueryInterface(IID_PPV_ARGS(&videoDevice_));
        if (FAILED(hr)) return hr;
        hr = context->QueryInterface(IID_PPV_ARGS(&videoContext_));
        if (FAILED(hr)) return hr;

        D3D11_VIDEO_PROCESSOR_CONTENT_DESC desc{};
        desc.InputFrameFormat = D3D11_VIDEO_FRAME_FORMAT_PROGRESSIVE;
        desc.InputWidth = w; desc.InputHeight = h;
        desc.OutputWidth = w; desc.OutputHeight = h;
        desc.Usage = D3D11_VIDEO_USAGE_PLAYBACK_NORMAL;
        hr = videoDevice_->CreateVideoProcessorEnumerator(&desc, &enumerator_);
        if (FAILED(hr)) return hr;
        hr = videoDevice_->CreateVideoProcessor(enumerator_.Get(), 0, &processor_);
        if (FAILED(hr)) return hr;

        // Output NV12 texture (ENCODER bind so it can be passed to MF MFT).
        D3D11_TEXTURE2D_DESC td{};
        td.Width = w; td.Height = h; td.MipLevels = 1; td.ArraySize = 1;
        td.Format = DXGI_FORMAT_NV12;
        td.SampleDesc.Count = 1;
        td.Usage = D3D11_USAGE_DEFAULT;
        td.BindFlags = D3D11_BIND_RENDER_TARGET | D3D11_BIND_SHADER_RESOURCE;
        td.MiscFlags = 0;
        hr = device->CreateTexture2D(&td, NULL, &outputTex_);
        if (FAILED(hr)) return hr;
        return S_OK;
    }

    HRESULT convert(ID3D11Texture2D *bgra, ComPtr<ID3D11Texture2D> &outNV12) {
        ComPtr<ID3D11VideoProcessorInputView> inView;
        D3D11_VIDEO_PROCESSOR_INPUT_VIEW_DESC ivd{};
        ivd.FourCC = 0;
        ivd.ViewDimension = D3D11_VPIV_DIMENSION_TEXTURE2D;
        ivd.Texture2D.MipSlice = 0;
        ivd.Texture2D.ArraySlice = 0;
        HRESULT hr = videoDevice_->CreateVideoProcessorInputView(bgra, enumerator_.Get(), &ivd, &inView);
        if (FAILED(hr)) return hr;

        ComPtr<ID3D11VideoProcessorOutputView> outView;
        D3D11_VIDEO_PROCESSOR_OUTPUT_VIEW_DESC ovd{};
        ovd.ViewDimension = D3D11_VPOV_DIMENSION_TEXTURE2D;
        ovd.Texture2D.MipSlice = 0;
        hr = videoDevice_->CreateVideoProcessorOutputView(outputTex_.Get(), enumerator_.Get(), &ovd, &outView);
        if (FAILED(hr)) return hr;

        D3D11_VIDEO_PROCESSOR_STREAM stream{};
        stream.Enable = TRUE;
        stream.OutputIndex = 0;
        stream.InputFrameOrField = 0;
        stream.PastFrames = 0; stream.FutureFrames = 0;
        stream.pInputSurface = inView.Get();
        hr = videoContext_->VideoProcessorBlt(processor_.Get(), outView.Get(), 0, 1, &stream);
        if (FAILED(hr)) return hr;

        outNV12 = outputTex_;
        return S_OK;
    }

private:
    ID3D11Device *device_ = nullptr;
    ID3D11DeviceContext *context_ = nullptr;
    ComPtr<ID3D11VideoDevice> videoDevice_;
    ComPtr<ID3D11VideoContext> videoContext_;
    ComPtr<ID3D11VideoProcessorEnumerator> enumerator_;
    ComPtr<ID3D11VideoProcessor> processor_;
    ComPtr<ID3D11Texture2D> outputTex_;
    int width_ = 0, height_ = 0;
};

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

int main(int argc, char **argv) {
    Config cfg = parse_args(argc, argv);
    writer_init();
    HRESULT hr = CoInitializeEx(NULL, COINIT_MULTITHREADED);
    if (FAILED(hr)) { log_err("CoInitializeEx %lx", hr); return 1; }

    DesktopDup dup;
    hr = dup.init();
    if (FAILED(hr)) { log_err("DesktopDup init failed (0x%lx) — desktop duplication may be blocked", hr); return 2; }

    NV12Converter conv;
    hr = conv.init(dup.device(), dup.context(), cfg.width, cfg.height);
    if (FAILED(hr)) { log_err("NV12 converter init 0x%lx", hr); return 2; }

    MFEncoder enc;
    hr = enc.init(cfg, dup.device(), dup.context());
    if (FAILED(hr)) { log_err("MF encoder init 0x%lx", hr); return 2; }

    log_err("capture started: %dx%d@%d %dkbps", cfg.width, cfg.height, cfg.fps, cfg.bitrateKbps);

    std::thread stdinThr(stdin_watcher);
    stdinThr.detach();

    INT64 frames = 0;
    const INT64 hnsPerFrame = 10000000LL / cfg.fps;

    auto captureFrame = [&](ComPtr<ID3D11Texture2D> &out) -> HRESULT {
        for (;;) {
            if (!g_running) return E_ABORT;
            ComPtr<ID3D11Texture2D> tex;
            HRESULT h = dup.acquire(tex, 1000 / cfg.fps + 5);
            if (h == DXGI_ERROR_WAIT_TIMEOUT) continue;
            if (FAILED(h)) return h;
            ComPtr<ID3D11Texture2D> nv12;
            h = conv.convert(tex.Get(), nv12);
            dup.release();
            if (FAILED(h)) return h;
            out = nv12;
            return S_OK;
        }
    };

    if (enc.isAsync()) {
        // Hardware MFT — kick off the IMFAsyncCallback event flow and let it
        // run on MF worker threads. Main thread just waits for shutdown.
        hr = enc.startAsyncEvents([&](ComPtr<ID3D11Texture2D> &out) -> HRESULT {
            return captureFrame(out);
        });
        if (FAILED(hr)) {
            log_err("startAsyncEvents 0x%lx", hr);
            enc.shutdown();
            CoUninitialize();
            return 3;
        }
        while (g_running) {
            Sleep(100);
        }
    } else {
        // Sync MFT (software fallback or rare sync hardware MFT) — the
        // classic capture/encode/drain loop.
        while (g_running) {
            ComPtr<ID3D11Texture2D> nv12;
            hr = captureFrame(nv12);
            if (hr == E_ABORT) break;
            if (FAILED(hr)) { log_err("capture 0x%lx", hr); break; }

            INT64 pts = frames * hnsPerFrame;
            hr = enc.encodeSync(nv12.Get(), pts);
            frames++;
            if (FAILED(hr)) { log_err("encode 0x%lx", hr); break; }
        }
    }

    enc.shutdown();
    CoUninitialize();
    return 0;
}
