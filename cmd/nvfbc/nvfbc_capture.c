/*
 * NvFBC screen capture helper for webrtc-vnc.
 *
 * Captures the X11 framebuffer using NVIDIA's NvFBC API and writes
 * raw NV12 frames to stdout. Pipe into FFmpeg for NVENC encoding:
 *
 *   ./nvfbc_capture -w 1920 -h 1080 -f 60 | \
 *     ffmpeg -f rawvideo -pix_fmt nv12 -video_size 1920x1080 -framerate 60 \
 *       -i pipe:0 -c:v h264_nvenc -preset p1 -tune ull -f h264 pipe:1
 *
 * Uses magic private data (same as Sunshine) to enable NvFBC on consumer GPUs.
 * NvFBC outputs NV12 directly — zero CPU color conversion.
 */

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <dlfcn.h>
#include <time.h>
#include <signal.h>
#include <stdint.h>

#include "NvFBC.h"

static volatile int running = 1;

static void handle_signal(int sig) {
    (void)sig;
    running = 0;
}

int main(int argc, char **argv) {
    int req_width = 0, req_height = 0, fps = 60, display_output = -1, max_frames = 0;

    for (int i = 1; i < argc; i++) {
        if (!strcmp(argv[i], "-w") && i + 1 < argc) req_width = atoi(argv[++i]);
        else if (!strcmp(argv[i], "-h") && i + 1 < argc) req_height = atoi(argv[++i]);
        else if (!strcmp(argv[i], "-f") && i + 1 < argc) fps = atoi(argv[++i]);
        else if (!strcmp(argv[i], "-d") && i + 1 < argc) display_output = atoi(argv[++i]);
        else if (!strcmp(argv[i], "-n") && i + 1 < argc) max_frames = atoi(argv[++i]);
        else {
            fprintf(stderr, "Usage: %s [-w width] [-h height] [-f fps] [-d output] [-n frames]\n", argv[0]);
            return 1;
        }
    }

    signal(SIGINT, handle_signal);
    signal(SIGTERM, handle_signal);
    signal(SIGPIPE, handle_signal);

    /* Load NvFBC */
    void *lib = dlopen("libnvidia-fbc.so.1", RTLD_NOW);
    if (!lib) lib = dlopen("libnvidia-fbc.so", RTLD_NOW);
    if (!lib) {
        fprintf(stderr, "nvfbc: failed to load libnvidia-fbc.so.1: %s\n", dlerror());
        return 1;
    }

    PNVFBCCREATEINSTANCE createInstance =
        (PNVFBCCREATEINSTANCE)dlsym(lib, "NvFBCCreateInstance");
    if (!createInstance) {
        fprintf(stderr, "nvfbc: NvFBCCreateInstance not found\n");
        return 1;
    }

    NVFBC_API_FUNCTION_LIST fn = {0};
    fn.dwVersion = NVFBC_VERSION;
    if (createInstance(&fn) != NVFBC_SUCCESS) {
        fprintf(stderr, "nvfbc: NvFBCCreateInstance failed\n");
        return 1;
    }

    /* Create handle — magic private data enables consumer GPU support */
    NVFBC_SESSION_HANDLE handle;
    NVFBC_CREATE_HANDLE_PARAMS hp = {0};
    hp.dwVersion = NVFBC_CREATE_HANDLE_PARAMS_VER;
    static const unsigned int MAGIC[4] = {0xAEF57AC5, 0x401D1A39, 0x1B856BBE, 0x9ED0CEBA};
    hp.privateData = MAGIC;
    hp.privateDataSize = sizeof(MAGIC);

    if (fn.nvFBCCreateHandle(&handle, &hp) != NVFBC_SUCCESS) {
        fprintf(stderr, "nvfbc: CreateHandle failed: %s\n", fn.nvFBCGetLastErrorStr(handle));
        return 1;
    }

    /* Get screen info */
    NVFBC_GET_STATUS_PARAMS sp = {0};
    sp.dwVersion = NVFBC_GET_STATUS_PARAMS_VER;
    if (fn.nvFBCGetStatus(handle, &sp) != NVFBC_SUCCESS) {
        fprintf(stderr, "nvfbc: GetStatus failed: %s\n", fn.nvFBCGetLastErrorStr(handle));
        return 1;
    }

    int out_w, out_h;
    if (display_output >= 0 && display_output < (int)sp.dwOutputNum) {
        out_w = req_width > 0 ? req_width : sp.outputs[display_output].trackedBox.w;
        out_h = req_height > 0 ? req_height : sp.outputs[display_output].trackedBox.h;
    } else {
        out_w = req_width > 0 ? req_width : sp.screenSize.w;
        out_h = req_height > 0 ? req_height : sp.screenSize.h;
    }

    /* NvFBC NV12 requires width multiple of 4, height multiple of 2 */
    out_w = (out_w + 3) & ~3;
    out_h = (out_h + 1) & ~1;

    fprintf(stderr, "nvfbc: screen=%dx%d capture=%dx%d@%dfps\n",
            sp.screenSize.w, sp.screenSize.h, out_w, out_h, fps);

    /* Create capture session — NV12 format, no CPU conversion needed */
    NVFBC_CREATE_CAPTURE_SESSION_PARAMS cp = {0};
    cp.dwVersion = NVFBC_CREATE_CAPTURE_SESSION_PARAMS_VER;
    cp.eCaptureType = NVFBC_CAPTURE_TO_SYS;
    cp.bWithCursor = NVFBC_TRUE;
    cp.frameSize.w = out_w;
    cp.frameSize.h = out_h;
    cp.dwSamplingRateMs = 1000 / fps;
    cp.bAllowDirectCapture = NVFBC_FALSE;
    cp.bPushModel = NVFBC_FALSE; /* polling mode: oversample beyond display refresh rate */

    if (display_output >= 0 && display_output < (int)sp.dwOutputNum) {
        cp.eTrackingType = NVFBC_TRACKING_OUTPUT;
        cp.dwOutputId = sp.outputs[display_output].dwId;
    } else {
        cp.eTrackingType = NVFBC_TRACKING_SCREEN;
    }

    if (fn.nvFBCCreateCaptureSession(handle, &cp) != NVFBC_SUCCESS) {
        fprintf(stderr, "nvfbc: CreateCaptureSession failed: %s\n",
                fn.nvFBCGetLastErrorStr(handle));
        return 1;
    }

    /* Setup — request NV12 directly from NvFBC */
    void *frame_buf = NULL;
    NVFBC_TOSYS_SETUP_PARAMS sup = {0};
    sup.dwVersion = NVFBC_TOSYS_SETUP_PARAMS_VER;
    sup.eBufferFormat = NVFBC_BUFFER_FORMAT_NV12;
    sup.ppBuffer = &frame_buf;

    if (fn.nvFBCToSysSetUp(handle, &sup) != NVFBC_SUCCESS) {
        fprintf(stderr, "nvfbc: ToSysSetUp failed: %s\n",
                fn.nvFBCGetLastErrorStr(handle));
        return 1;
    }

    size_t frame_size = (size_t)out_w * out_h * 3 / 2; /* NV12 */

    fprintf(stderr, "nvfbc: streaming NV12 %dx%d (%zu bytes/frame)\n",
            out_w, out_h, frame_size);

    struct timespec next;
    long frame_ns = 1000000000L / fps;
    clock_gettime(CLOCK_MONOTONIC, &next);

    int frame_count = 0;
    while (running) {
        NVFBC_FRAME_GRAB_INFO info = {0};
        NVFBC_TOSYS_GRAB_FRAME_PARAMS gp = {0};
        gp.dwVersion = NVFBC_TOSYS_GRAB_FRAME_PARAMS_VER;
        gp.dwFlags = NVFBC_TOSYS_GRAB_FLAGS_NOWAIT;
        gp.pFrameGrabInfo = &info;

        NVFBCSTATUS st = fn.nvFBCToSysGrabFrame(handle, &gp);
        if (st == NVFBC_ERR_MUST_RECREATE) {
            fprintf(stderr, "nvfbc: session must be recreated\n");
            break;
        }
        if (st != NVFBC_SUCCESS) {
            fprintf(stderr, "nvfbc: GrabFrame failed: %s\n",
                    fn.nvFBCGetLastErrorStr(handle));
            break;
        }

        /* Write NV12 frame to stdout */
        size_t written = 0;
        while (written < frame_size && running) {
            ssize_t n = write(STDOUT_FILENO, (uint8_t *)frame_buf + written,
                              frame_size - written);
            if (n <= 0) { running = 0; break; }
            written += n;
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

    NVFBC_DESTROY_CAPTURE_SESSION_PARAMS dp = {0};
    dp.dwVersion = NVFBC_DESTROY_CAPTURE_SESSION_PARAMS_VER;
    fn.nvFBCDestroyCaptureSession(handle, &dp);

    NVFBC_DESTROY_HANDLE_PARAMS dhp = {0};
    dhp.dwVersion = NVFBC_DESTROY_HANDLE_PARAMS_VER;
    fn.nvFBCDestroyHandle(handle, &dhp);

    dlclose(lib);
    return 0;
}
