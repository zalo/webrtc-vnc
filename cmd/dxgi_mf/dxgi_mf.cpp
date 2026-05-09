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
// MediaFoundation encoder
// ---------------------------------------------------------------------------

class MFEncoder {
public:
    HRESULT init(const Config &cfg) {
        cfg_ = cfg;

        HRESULT hr = MFStartup(MF_VERSION);
        if (FAILED(hr)) return hr;

        // Output type — H.264 baseline at target bitrate
        ComPtr<IMFMediaType> outType;
        MFCreateMediaType(&outType);
        outType->SetGUID(MF_MT_MAJOR_TYPE, MFMediaType_Video);
        outType->SetGUID(MF_MT_SUBTYPE,    MFVideoFormat_H264);
        outType->SetUINT32(MF_MT_AVG_BITRATE, (UINT32)(cfg.bitrateKbps * 1000));
        outType->SetUINT32(MF_MT_INTERLACE_MODE, MFVideoInterlace_Progressive);
        outType->SetUINT32(MF_MT_MPEG2_PROFILE, eAVEncH264VProfile_Base);
        MFSetAttributeSize(outType.Get(), MF_MT_FRAME_SIZE, cfg.width, cfg.height);
        MFSetAttributeRatio(outType.Get(), MF_MT_FRAME_RATE, cfg.fps, 1);
        MFSetAttributeRatio(outType.Get(), MF_MT_PIXEL_ASPECT_RATIO, 1, 1);

        // Input type — NV12 surfaces from DXGI
        ComPtr<IMFMediaType> inType;
        MFCreateMediaType(&inType);
        inType->SetGUID(MF_MT_MAJOR_TYPE, MFMediaType_Video);
        inType->SetGUID(MF_MT_SUBTYPE,    MFVideoFormat_NV12);
        inType->SetUINT32(MF_MT_INTERLACE_MODE, MFVideoInterlace_Progressive);
        MFSetAttributeSize(inType.Get(), MF_MT_FRAME_SIZE, cfg.width, cfg.height);
        MFSetAttributeRatio(inType.Get(), MF_MT_FRAME_RATE, cfg.fps, 1);
        MFSetAttributeRatio(inType.Get(), MF_MT_PIXEL_ASPECT_RATIO, 1, 1);

        // SinkWriter writing to a "null" sink — we'll fish out raw samples instead.
        // We use the in-memory pattern: MFCreateSinkWriterFromURL with NULL stream
        // is awkward, so instead we go via IMFTransform directly. That gives us
        // direct access to the encoded NAL payload sample.
        ComPtr<IMFTransform> mft;
        hr = CoCreateInstance(CLSID_CMSH264EncoderMFT, NULL, CLSCTX_INPROC_SERVER,
                              IID_PPV_ARGS(&mft));
        if (FAILED(hr)) return hr;

        ComPtr<IMFAttributes> mftAttrs;
        if (SUCCEEDED(mft->GetAttributes(&mftAttrs))) {
            mftAttrs->SetUINT32(MF_LOW_LATENCY, TRUE);
            mftAttrs->SetUINT32(CODECAPI_AVEncCommonRateControlMode, eAVEncCommonRateControlMode_CBR);
            mftAttrs->SetUINT32(CODECAPI_AVLowLatencyMode, TRUE);
            mftAttrs->SetUINT32(CODECAPI_AVEncMPVGOPSize, (UINT32)cfg.fps);
            mftAttrs->SetUINT32(CODECAPI_AVEncCommonQuality, 70);
        }

        // Output type must be set first
        hr = mft->SetOutputType(0, outType.Get(), 0);
        if (FAILED(hr)) return hr;
        hr = mft->SetInputType(0, inType.Get(), 0);
        if (FAILED(hr)) return hr;

        // Cache the output type so we can fish out the SPS/PPS sequence header.
        ComPtr<IMFMediaType> negotiatedOut;
        hr = mft->GetOutputCurrentType(0, &negotiatedOut);
        if (SUCCEEDED(hr)) outputType_ = negotiatedOut;

        hr = mft->ProcessMessage(MFT_MESSAGE_NOTIFY_BEGIN_STREAMING, 0);
        if (FAILED(hr)) return hr;
        mft_ = mft;
        sentSPSPPS_ = false;
        return S_OK;
    }

    HRESULT encode(ID3D11Texture2D *texture, INT64 ptsHns) {
        // Wrap the GPU texture in an IMFSample. The MFT will read from it directly.
        ComPtr<IMFMediaBuffer> buffer;
        HRESULT hr = MFCreateDXGISurfaceBuffer(IID_ID3D11Texture2D, texture, 0, FALSE, &buffer);
        if (FAILED(hr)) return hr;

        ComPtr<IMFSample> sample;
        MFCreateSample(&sample);
        sample->AddBuffer(buffer.Get());
        sample->SetSampleTime(ptsHns);
        sample->SetSampleDuration(10000000LL / cfg_.fps);

        if (g_force_idr.exchange(false)) {
            sample->SetUINT32(MFSampleExtension_CleanPoint, TRUE);
            // Some MFTs honor MFT_OUTPUT_DATA_BUFFER_INCOMPLETE; on most NVENC/QSV
            // builds setting CleanPoint plus AVEncVideoForceKeyFrame is enough.
            ComPtr<IMFAttributes> mftAttrs;
            if (SUCCEEDED(mft_->GetAttributes(&mftAttrs))) {
                mftAttrs->SetUINT32(CODECAPI_AVEncVideoForceKeyFrame, 1);
            }
        }

        hr = mft_->ProcessInput(0, sample.Get(), 0);
        if (FAILED(hr)) return hr;
        return drain();
    }

    HRESULT drain() {
        for (;;) {
            DWORD status = 0;
            MFT_OUTPUT_DATA_BUFFER outBuf{};
            outBuf.dwStreamID = 0;

            // We let the MFT allocate the output sample (CMSH264EncoderMFT does
            // since GetOutputStreamInfo reports MFT_OUTPUT_STREAM_PROVIDES_SAMPLES).
            HRESULT hr = mft_->ProcessOutput(0, 1, &outBuf, &status);
            if (hr == MF_E_TRANSFORM_NEED_MORE_INPUT) return S_OK;
            if (FAILED(hr)) return hr;

            ComPtr<IMFSample> sample(outBuf.pSample);
            if (outBuf.pEvents) outBuf.pEvents->Release();

            if (!sentSPSPPS_ && outputType_) {
                emit_sps_pps(outputType_.Get());
                sentSPSPPS_ = true;
            }

            // Detect keyframe via MFSampleExtension_CleanPoint
            UINT32 clean = 0;
            sample->GetUINT32(MFSampleExtension_CleanPoint, &clean);
            if (clean && outputType_) {
                emit_sps_pps(outputType_.Get());
            }

            ComPtr<IMFMediaBuffer> buf;
            sample->ConvertToContiguousBuffer(&buf);
            BYTE *data = NULL; DWORD size = 0;
            buf->Lock(&data, NULL, &size);
            emit_avcc_as_annexb(data, size);
            buf->Unlock();
        }
    }

    void shutdown() {
        if (mft_) mft_->ProcessMessage(MFT_MESSAGE_NOTIFY_END_STREAMING, 0);
        mft_.Reset();
        outputType_.Reset();
        MFShutdown();
    }

private:
    Config cfg_;
    ComPtr<IMFTransform> mft_;
    ComPtr<IMFMediaType> outputType_;
    bool sentSPSPPS_ = false;
};

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
    hr = enc.init(cfg);
    if (FAILED(hr)) { log_err("MF encoder init 0x%lx", hr); return 2; }

    log_err("capture started: %dx%d@%d %dkbps", cfg.width, cfg.height, cfg.fps, cfg.bitrateKbps);

    std::thread stdinThr(stdin_watcher);
    stdinThr.detach();

    using clk = std::chrono::high_resolution_clock;
    auto t0 = clk::now();
    INT64 frames = 0;
    const INT64 hnsPerFrame = 10000000LL / cfg.fps;

    while (g_running) {
        ComPtr<ID3D11Texture2D> tex;
        hr = dup.acquire(tex, 1000 / cfg.fps + 5);
        if (hr == DXGI_ERROR_WAIT_TIMEOUT) continue;
        if (FAILED(hr)) { log_err("acquire 0x%lx", hr); break; }

        ComPtr<ID3D11Texture2D> nv12;
        hr = conv.convert(tex.Get(), nv12);
        dup.release();
        if (FAILED(hr)) { log_err("convert 0x%lx", hr); break; }

        INT64 pts = frames * hnsPerFrame;
        hr = enc.encode(nv12.Get(), pts);
        frames++;
        if (FAILED(hr)) { log_err("encode 0x%lx", hr); break; }
    }

    enc.shutdown();
    CoUninitialize();
    return 0;
}
