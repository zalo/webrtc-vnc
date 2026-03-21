/*
 * NvFBC + NVENC zero-copy capture+encode for webrtc-vnc.
 *
 * Captures the X11 framebuffer via NvFBC directly into CUDA memory,
 * then encodes with NVENC — the raw frame data never touches CPU RAM.
 * Only the compressed H.264 bitstream (~few KB/frame) is written to stdout.
 *
 * This is the same capture path Sunshine uses for lowest latency.
 *
 * Usage:
 *   ./nvfbc_nvenc -w 1920 -h 1080 -f 60 -b 5000 > stream.h264
 *   ./nvfbc_nvenc -w 1920 -h 1080 -f 60 -b 5000 | (pipe to Go process)
 */

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <dlfcn.h>
#include <time.h>
#include <signal.h>
#include <stdint.h>
#include <fcntl.h>
#include <errno.h>

#include <cuda.h>
#include "NvFBC.h"
#include <ffnvcodec/nvEncodeAPI.h>

static volatile int running = 1;
static volatile int force_idr = 0;
static void handle_signal(int sig) { (void)sig; running = 0; }
static void handle_usr1(int sig) { (void)sig; force_idr = 1; }

/* Dynamically loaded NVENC function pointers */
typedef NVENCSTATUS (NVENCAPI *PFN_NvEncodeAPICreateInstance)(NV_ENCODE_API_FUNCTION_LIST *);
static NV_ENCODE_API_FUNCTION_LIST nvenc = {0};

static int init_nvenc(void) {
    void *lib = dlopen("libnvidia-encode.so.1", RTLD_NOW);
    if (!lib) lib = dlopen("libnvidia-encode.so", RTLD_NOW);
    if (!lib) { fprintf(stderr, "Failed to load libnvidia-encode.so\n"); return -1; }

    PFN_NvEncodeAPICreateInstance createInstance =
        (PFN_NvEncodeAPICreateInstance)dlsym(lib, "NvEncodeAPICreateInstance");
    if (!createInstance) { fprintf(stderr, "NvEncodeAPICreateInstance not found\n"); return -1; }

    nvenc.version = NV_ENCODE_API_FUNCTION_LIST_VER;
    if (createInstance(&nvenc) != NV_ENC_SUCCESS) {
        fprintf(stderr, "NvEncodeAPICreateInstance failed\n");
        return -1;
    }
    return 0;
}

int main(int argc, char **argv) {
    int req_width = 0, req_height = 0, fps = 60, bitrate_kbps = 5000;
    int display_output = -1, max_frames = 0;

    for (int i = 1; i < argc; i++) {
        if (!strcmp(argv[i], "-w") && i+1 < argc) req_width = atoi(argv[++i]);
        else if (!strcmp(argv[i], "-h") && i+1 < argc) req_height = atoi(argv[++i]);
        else if (!strcmp(argv[i], "-f") && i+1 < argc) fps = atoi(argv[++i]);
        else if (!strcmp(argv[i], "-b") && i+1 < argc) bitrate_kbps = atoi(argv[++i]);
        else if (!strcmp(argv[i], "-d") && i+1 < argc) display_output = atoi(argv[++i]);
        else if (!strcmp(argv[i], "-n") && i+1 < argc) max_frames = atoi(argv[++i]);
        else {
            fprintf(stderr, "Usage: %s [-w W] [-h H] [-f fps] [-b kbps] [-d output] [-n frames]\n", argv[0]);
            return 1;
        }
    }

    signal(SIGINT, handle_signal);
    signal(SIGTERM, handle_signal);
    signal(SIGPIPE, handle_signal);
    signal(SIGUSR1, handle_usr1);

    /* ---- CUDA init ---- */
    if (cuInit(0) != CUDA_SUCCESS) {
        fprintf(stderr, "cuInit failed\n");
        return 1;
    }

    CUdevice cuDevice;
    CUcontext cuCtx;
    if (cuDeviceGet(&cuDevice, 0) != CUDA_SUCCESS) {
        fprintf(stderr, "cuDeviceGet failed\n");
        return 1;
    }
    if (cuCtxCreate(&cuCtx, 0, cuDevice) != CUDA_SUCCESS) {
        fprintf(stderr, "cuCtxCreate failed\n");
        return 1;
    }

    /* ---- NvFBC init ---- */
    void *fbc_lib = dlopen("libnvidia-fbc.so.1", RTLD_NOW);
    if (!fbc_lib) fbc_lib = dlopen("libnvidia-fbc.so", RTLD_NOW);
    if (!fbc_lib) { fprintf(stderr, "Failed to load libnvidia-fbc.so\n"); return 1; }

    PNVFBCCREATEINSTANCE fbcCreateInstance =
        (PNVFBCCREATEINSTANCE)dlsym(fbc_lib, "NvFBCCreateInstance");
    if (!fbcCreateInstance) { fprintf(stderr, "NvFBCCreateInstance not found\n"); return 1; }

    NVFBC_API_FUNCTION_LIST fbc = {0};
    fbc.dwVersion = NVFBC_VERSION;
    if (fbcCreateInstance(&fbc) != NVFBC_SUCCESS) {
        fprintf(stderr, "NvFBCCreateInstance failed\n");
        return 1;
    }

    NVFBC_SESSION_HANDLE fbcHandle;
    NVFBC_CREATE_HANDLE_PARAMS hp = {0};
    hp.dwVersion = NVFBC_CREATE_HANDLE_PARAMS_VER;
    static const unsigned int MAGIC[4] = {0xAEF57AC5, 0x401D1A39, 0x1B856BBE, 0x9ED0CEBA};
    hp.privateData = MAGIC;
    hp.privateDataSize = sizeof(MAGIC);
    if (fbc.nvFBCCreateHandle(&fbcHandle, &hp) != NVFBC_SUCCESS) {
        fprintf(stderr, "nvFBCCreateHandle failed: %s\n", fbc.nvFBCGetLastErrorStr(fbcHandle));
        return 1;
    }

    NVFBC_GET_STATUS_PARAMS sp = {0};
    sp.dwVersion = NVFBC_GET_STATUS_PARAMS_VER;
    if (fbc.nvFBCGetStatus(fbcHandle, &sp) != NVFBC_SUCCESS) {
        fprintf(stderr, "nvFBCGetStatus failed\n");
        return 1;
    }

    int out_w = req_width > 0 ? req_width : sp.screenSize.w;
    int out_h = req_height > 0 ? req_height : sp.screenSize.h;

    /* NvFBC NV12 requires width multiple of 4, height multiple of 2 */
    out_w = (out_w + 3) & ~3;
    out_h = (out_h + 1) & ~1;

    /* NvFBC capture session: CUDA mode (frames stay in GPU memory).
     * Use polling mode (bPushModel=FALSE) so we can oversample beyond
     * the display refresh rate. At 144fps on a 60Hz display, ~60 frames
     * contain new content and ~84 are duplicates that NVENC compresses
     * to ~80 bytes each. This reduces worst-case capture latency from
     * 16.7ms (60Hz) to 6.9ms (144Hz). */
    NVFBC_CREATE_CAPTURE_SESSION_PARAMS cp = {0};
    cp.dwVersion = NVFBC_CREATE_CAPTURE_SESSION_PARAMS_VER;
    cp.eCaptureType = NVFBC_CAPTURE_SHARED_CUDA;
    cp.bWithCursor = NVFBC_TRUE;
    cp.frameSize.w = out_w;
    cp.frameSize.h = out_h;
    cp.dwSamplingRateMs = 1000 / fps;
    cp.eTrackingType = NVFBC_TRACKING_SCREEN;
    cp.bAllowDirectCapture = NVFBC_TRUE;
    cp.bPushModel = NVFBC_FALSE; /* polling mode: grab at our rate, not display rate */

    if (display_output >= 0 && display_output < (int)sp.dwOutputNum) {
        cp.eTrackingType = NVFBC_TRACKING_OUTPUT;
        cp.dwOutputId = sp.outputs[display_output].dwId;
    }

    if (fbc.nvFBCCreateCaptureSession(fbcHandle, &cp) != NVFBC_SUCCESS) {
        fprintf(stderr, "nvFBCCreateCaptureSession failed: %s\n", fbc.nvFBCGetLastErrorStr(fbcHandle));
        return 1;
    }

    NVFBC_TOCUDA_SETUP_PARAMS tsp = {0};
    tsp.dwVersion = NVFBC_TOCUDA_SETUP_PARAMS_VER;
    tsp.eBufferFormat = NVFBC_BUFFER_FORMAT_NV12;
    if (fbc.nvFBCToCudaSetUp(fbcHandle, &tsp) != NVFBC_SUCCESS) {
        fprintf(stderr, "nvFBCToCudaSetUp failed: %s\n", fbc.nvFBCGetLastErrorStr(fbcHandle));
        return 1;
    }

    /* ---- NVENC init ---- */
    if (init_nvenc() < 0) return 1;

    NV_ENC_OPEN_ENCODE_SESSION_EX_PARAMS osp = {0};
    osp.version = NV_ENC_OPEN_ENCODE_SESSION_EX_PARAMS_VER;
    osp.deviceType = NV_ENC_DEVICE_TYPE_CUDA;
    osp.device = cuCtx;
    osp.apiVersion = NVENCAPI_VERSION;

    void *encoder = NULL;
    if (nvenc.nvEncOpenEncodeSessionEx(&osp, &encoder) != NV_ENC_SUCCESS) {
        fprintf(stderr, "nvEncOpenEncodeSessionEx failed\n");
        return 1;
    }

    /* Encoder config */
    NV_ENC_PRESET_CONFIG presetCfg = {0};
    presetCfg.version = NV_ENC_PRESET_CONFIG_VER;
    presetCfg.presetCfg.version = NV_ENC_CONFIG_VER;
    nvenc.nvEncGetEncodePresetConfigEx(encoder, NV_ENC_CODEC_H264_GUID,
        NV_ENC_PRESET_P1_GUID, NV_ENC_TUNING_INFO_ULTRA_LOW_LATENCY, &presetCfg);

    NV_ENC_CONFIG encCfg = presetCfg.presetCfg;
    encCfg.rcParams.rateControlMode = NV_ENC_PARAMS_RC_CBR;
    encCfg.rcParams.averageBitRate = bitrate_kbps * 1000;
    encCfg.rcParams.maxBitRate = bitrate_kbps * 1000;
    encCfg.rcParams.vbvBufferSize = bitrate_kbps * 500; /* half-second buffer */
    encCfg.gopLength = fps / 2; /* keyframe every ~0.5 second */
    encCfg.frameIntervalP = 1; /* no B-frames */
    encCfg.encodeCodecConfig.h264Config.idrPeriod = fps / 2;
    encCfg.encodeCodecConfig.h264Config.repeatSPSPPS = 1;
    /* Level auto-selected by NVENC based on resolution+fps */
    encCfg.profileGUID = NV_ENC_H264_PROFILE_BASELINE_GUID;

    NV_ENC_INITIALIZE_PARAMS initParams = {0};
    initParams.version = NV_ENC_INITIALIZE_PARAMS_VER;
    initParams.encodeGUID = NV_ENC_CODEC_H264_GUID;
    initParams.presetGUID = NV_ENC_PRESET_P1_GUID;
    initParams.tuningInfo = NV_ENC_TUNING_INFO_ULTRA_LOW_LATENCY;
    initParams.encodeWidth = out_w;
    initParams.encodeHeight = out_h;
    initParams.darWidth = out_w;
    initParams.darHeight = out_h;
    initParams.frameRateNum = fps;
    initParams.frameRateDen = 1;
    initParams.enablePTD = 1;
    initParams.encodeConfig = &encCfg;

    if (nvenc.nvEncInitializeEncoder(encoder, &initParams) != NV_ENC_SUCCESS) {
        fprintf(stderr, "nvEncInitializeEncoder failed\n");
        return 1;
    }

    /* Create output bitstream buffer */
    NV_ENC_CREATE_BITSTREAM_BUFFER bsBuf = {0};
    bsBuf.version = NV_ENC_CREATE_BITSTREAM_BUFFER_VER;
    if (nvenc.nvEncCreateBitstreamBuffer(encoder, &bsBuf) != NV_ENC_SUCCESS) {
        fprintf(stderr, "nvEncCreateBitstreamBuffer failed\n");
        return 1;
    }

    fprintf(stderr, "nvfbc+nvenc: %dx%d@%dfps %dkbps (zero-copy GPU pipeline)\n",
            out_w, out_h, fps, bitrate_kbps);

    /* Do initial grab to get the CUDA buffer pointer and pitch */
    CUdeviceptr cuFrame = 0;
    NVFBC_FRAME_GRAB_INFO grabInfo = {0};
    {
        NVFBC_TOCUDA_GRAB_FRAME_PARAMS gp = {0};
        gp.dwVersion = NVFBC_TOCUDA_GRAB_FRAME_PARAMS_VER;
        gp.dwFlags = NVFBC_TOCUDA_GRAB_FLAGS_NOWAIT;
        gp.pCUDADeviceBuffer = &cuFrame;
        gp.pFrameGrabInfo = &grabInfo;
        if (fbc.nvFBCToCudaGrabFrame(fbcHandle, &gp) != NVFBC_SUCCESS) {
            fprintf(stderr, "Initial grab failed: %s\n", fbc.nvFBCGetLastErrorStr(fbcHandle));
            return 1;
        }
    }

    /* Query actual pitch from the CUDA allocation.
     * For NV12, the total allocation is pitch * height * 3/2.
     * byteSize from NvFBC is the total allocation size. */
    uint32_t pitch;
    if (grabInfo.dwHeight > 0) {
        /* NV12: total = pitch * height * 3/2, so pitch = total * 2 / (height * 3) */
        pitch = grabInfo.dwByteSize * 2 / (grabInfo.dwHeight * 3);
    } else {
        pitch = (grabInfo.dwWidth + 255) & ~255u;
    }
    fprintf(stderr, "nvfbc: width=%u height=%u byteSize=%u pitch=%u cuPtr=%p\n",
            grabInfo.dwWidth, grabInfo.dwHeight, grabInfo.dwByteSize, pitch, (void *)cuFrame);

    /* Register the CUDA buffer with NVENC once — NvFBC reuses the same buffer */
    NV_ENC_REGISTER_RESOURCE reg = {0};
    reg.version = NV_ENC_REGISTER_RESOURCE_VER;
    reg.resourceType = NV_ENC_INPUT_RESOURCE_TYPE_CUDADEVICEPTR;
    reg.resourceToRegister = (void *)cuFrame;
    reg.width = out_w;
    reg.height = out_h;
    reg.pitch = pitch;
    reg.bufferFormat = NV_ENC_BUFFER_FORMAT_NV12;
    reg.bufferUsage = NV_ENC_INPUT_IMAGE;

    if (nvenc.nvEncRegisterResource(encoder, &reg) != NV_ENC_SUCCESS) {
        fprintf(stderr, "nvEncRegisterResource failed\n");
        return 1;
    }

    struct timespec next;
    long frame_ns = 1000000000L / fps;
    clock_gettime(CLOCK_MONOTONIC, &next);
    int frame_count = 0;

    while (running) {
        /* Grab frame — NvFBC overwrites the same CUDA buffer in-place */
        NVFBC_FRAME_GRAB_INFO gi = {0};
        NVFBC_TOCUDA_GRAB_FRAME_PARAMS gp = {0};
        gp.dwVersion = NVFBC_TOCUDA_GRAB_FRAME_PARAMS_VER;
        gp.dwFlags = NVFBC_TOCUDA_GRAB_FLAGS_NOWAIT;
        gp.pCUDADeviceBuffer = &cuFrame;
        gp.pFrameGrabInfo = &gi;

        NVFBCSTATUS fbcSt = fbc.nvFBCToCudaGrabFrame(fbcHandle, &gp);
        if (fbcSt == NVFBC_ERR_MUST_RECREATE) {
            fprintf(stderr, "nvfbc: session must be recreated\n");
            break;
        }
        if (fbcSt != NVFBC_SUCCESS) {
            fprintf(stderr, "nvfbc grab failed: %s\n", fbc.nvFBCGetLastErrorStr(fbcHandle));
            break;
        }

        /* Map, encode, unmap (resource stays registered) */
        NV_ENC_MAP_INPUT_RESOURCE map = {0};
        map.version = NV_ENC_MAP_INPUT_RESOURCE_VER;
        map.registeredResource = reg.registeredResource;

        if (nvenc.nvEncMapInputResource(encoder, &map) != NV_ENC_SUCCESS) {
            fprintf(stderr, "nvEncMapInputResource failed\n");
            break;
        }

        NV_ENC_PIC_PARAMS pic = {0};
        pic.version = NV_ENC_PIC_PARAMS_VER;
        pic.inputWidth = out_w;
        pic.inputHeight = out_h;
        pic.inputPitch = pitch;
        pic.inputBuffer = map.mappedResource;
        pic.outputBitstream = bsBuf.bitstreamBuffer;
        pic.bufferFmt = NV_ENC_BUFFER_FORMAT_NV12;
        pic.pictureStruct = NV_ENC_PIC_STRUCT_FRAME;

        /* Force IDR on SIGUSR1 (sent by Go when a peer connects or requests IDR) */
        if (force_idr) {
            pic.encodePicFlags = NV_ENC_PIC_FLAG_FORCEIDR | NV_ENC_PIC_FLAG_OUTPUT_SPSPPS;
            force_idr = 0;
        }

        NVENCSTATUS encSt = nvenc.nvEncEncodePicture(encoder, &pic);

        nvenc.nvEncUnmapInputResource(encoder, map.mappedResource);

        if (encSt != NV_ENC_SUCCESS) {
            fprintf(stderr, "nvEncEncodePicture failed: %d\n", encSt);
            break;
        }

        /* Lock and write bitstream to stdout */
        NV_ENC_LOCK_BITSTREAM lock = {0};
        lock.version = NV_ENC_LOCK_BITSTREAM_VER;
        lock.outputBitstream = bsBuf.bitstreamBuffer;

        if (nvenc.nvEncLockBitstream(encoder, &lock) == NV_ENC_SUCCESS) {
            size_t written = 0;
            while (written < lock.bitstreamSizeInBytes && running) {
                ssize_t n = write(STDOUT_FILENO, (uint8_t *)lock.bitstreamBufferPtr + written,
                                  lock.bitstreamSizeInBytes - written);
                if (n <= 0) { running = 0; break; }
                written += n;
            }
            nvenc.nvEncUnlockBitstream(encoder, bsBuf.bitstreamBuffer);
        }

        frame_count++;
        if (max_frames > 0 && frame_count >= max_frames) break;

        /* Frame pacing */
        next.tv_nsec += frame_ns;
        if (next.tv_nsec >= 1000000000L) {
            next.tv_nsec -= 1000000000L;
            next.tv_sec++;
        }
        clock_nanosleep(CLOCK_MONOTONIC, TIMER_ABSTIME, &next, NULL);
    }

    /* Cleanup */
    nvenc.nvEncUnregisterResource(encoder, reg.registeredResource);
    nvenc.nvEncDestroyBitstreamBuffer(encoder, bsBuf.bitstreamBuffer);
    nvenc.nvEncDestroyEncoder(encoder);

    NVFBC_DESTROY_CAPTURE_SESSION_PARAMS dp = {0};
    dp.dwVersion = NVFBC_DESTROY_CAPTURE_SESSION_PARAMS_VER;
    fbc.nvFBCDestroyCaptureSession(fbcHandle, &dp);
    NVFBC_DESTROY_HANDLE_PARAMS dhp = {0};
    dhp.dwVersion = NVFBC_DESTROY_HANDLE_PARAMS_VER;
    fbc.nvFBCDestroyHandle(fbcHandle, &dhp);

    cuCtxDestroy(cuCtx);
    return 0;
}
