// flexvsr upscales raw NV12 video frames with NVIDIA RTX Video Super
// Resolution, through the Direct3D 11 video processor and the driver's own
// switch for it (the stream extension Chrome/Edge and mpv's d3d11vpp filter
// use). No NVIDIA SDK is involved; it needs a GeForce RTX card driving a
// display and a driver with RTX Video super resolution switched on.
//
//   ffmpeg ... -pix_fmt nv12 -f rawvideo - | flexvsr IN_W IN_H OUT_W OUT_H [options] | ffmpeg -f rawvideo -pix_fmt nv12 -s OUT_WxOUT_H -i - ...
//   flexvsr --list          the NVIDIA cards, "index<TAB>name" per line
//
// stdin: NV12 frames of IN_W x IN_H, back to back; stdout: NV12 frames of
// OUT_W x OUT_H in BT.709 (input under 720 lines is taken as BT.601).
// Options: --adapter N uses the card Windows lists as N (default: the first
// NVIDIA card); --off scales the same way with the switch off, to compare;
// --bgra gives BGRA out; --playback uses the "normal playback" usage hint.
// Status goes to stderr. Only enlarging engages super resolution. Exit
// code 4: the driver refused the switch or it changed nothing on the first
// frame (checked by rendering it both ways), so nothing was written.
//
// FLEX runs it from server/vsr.go; the build is committed as
// server/vsr/flexvsr.exe. Rebuild from a Visual Studio developer prompt in
// this folder: cl /O2 /EHsc /nologo flexvsr.cpp /Fe:..\..\server\vsr\flexvsr.exe

#include <d3d11.h>
#include <dxgi1_2.h>
#include <fcntl.h>
#include <io.h>

#include <chrono>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <vector>

#pragma comment(lib, "d3d11.lib")
#pragma comment(lib, "dxgi.lib")

// NVIDIA's video post-processing extension (mpv: NVIDIA_PPE_INTERFACE_GUID).
static const GUID kNvidiaPPE = {0xd43ce1b3, 0x1f4b, 0x48ac, {0xba, 0xee, 0xc3, 0xc2, 0x53, 0x75, 0xe6, 0xf7}};

static void fail(const char *what, HRESULT hr) {
    fprintf(stderr, "flexvsr: %s failed (0x%08lx)\n", what, (unsigned long)hr);
    exit(2);
}

#define CHECK(call, what)                 \
    do {                                  \
        HRESULT hr_ = (call);             \
        if (FAILED(hr_)) fail(what, hr_); \
    } while (0)

static bool read_full(unsigned char *p, size_t n) {
    while (n > 0) {
        size_t got = fread(p, 1, n, stdin);
        if (got == 0) return false;
        p += got;
        n -= got;
    }
    return true;
}

// list prints the NVIDIA cards with their DXGI index.
static int list() {
    IDXGIFactory1 *factory = nullptr;
    CHECK(CreateDXGIFactory1(__uuidof(IDXGIFactory1), (void **)&factory), "CreateDXGIFactory1");
    for (UINT i = 0;; ++i) {
        IDXGIAdapter1 *a = nullptr;
        if (factory->EnumAdapters1(i, &a) == DXGI_ERROR_NOT_FOUND) break;
        DXGI_ADAPTER_DESC1 d;
        a->GetDesc1(&d);
        if (d.VendorId == 0x10DE) printf("%u\t%ls\n", i, d.Description);
        a->Release();
    }
    return 0;
}

int main(int argc, char **argv) {
    if (argc == 2 && !strcmp(argv[1], "--list")) return list();
    if (argc < 5) {
        fprintf(stderr, "usage: flexvsr IN_W IN_H OUT_W OUT_H [--adapter N] [--off] [--bgra] [--playback]\n"
                        "       flexvsr --list\n");
        return 1;
    }
    const UINT iw = atoi(argv[1]), ih = atoi(argv[2]), ow = atoi(argv[3]), oh = atoi(argv[4]);
    bool off = false, bgra = false, playback = false;
    int index = -1;
    for (int i = 5; i < argc; ++i) {
        if (!strcmp(argv[i], "--off")) off = true;
        else if (!strcmp(argv[i], "--bgra")) bgra = true;
        else if (!strcmp(argv[i], "--playback")) playback = true;
        else if (!strcmp(argv[i], "--adapter") && i + 1 < argc) index = atoi(argv[++i]);
    }
    if (!iw || !ih || !ow || !oh || (iw | ih | ow | oh) & 1) {
        fprintf(stderr, "flexvsr: sizes must be even and non-zero\n");
        return 1;
    }
    _setmode(_fileno(stdin), _O_BINARY);
    _setmode(_fileno(stdout), _O_BINARY);

    // the card given by --adapter, else the first NVIDIA card
    IDXGIFactory1 *factory = nullptr;
    CHECK(CreateDXGIFactory1(__uuidof(IDXGIFactory1), (void **)&factory), "CreateDXGIFactory1");
    IDXGIAdapter1 *adapter = nullptr;
    for (UINT i = 0;; ++i) {
        IDXGIAdapter1 *a = nullptr;
        if (factory->EnumAdapters1(i, &a) == DXGI_ERROR_NOT_FOUND) break;
        DXGI_ADAPTER_DESC1 d;
        a->GetDesc1(&d);
        if (d.VendorId == 0x10DE && !adapter && (index < 0 || (int)i == index)) {
            adapter = a;
        } else {
            a->Release();
        }
    }
    if (!adapter) {
        fprintf(stderr, "flexvsr: no NVIDIA graphics card%s\n", index >= 0 ? " at that index" : "");
        return 3;
    }
    DXGI_ADAPTER_DESC1 desc;
    adapter->GetDesc1(&desc);
    fprintf(stderr, "flexvsr: %ls, %ux%u -> %ux%u\n", desc.Description, iw, ih, ow, oh);

    ID3D11Device *dev = nullptr;
    ID3D11DeviceContext *ctx = nullptr;
    CHECK(D3D11CreateDevice(adapter, D3D_DRIVER_TYPE_UNKNOWN, nullptr, D3D11_CREATE_DEVICE_VIDEO_SUPPORT, nullptr, 0,
                            D3D11_SDK_VERSION, &dev, nullptr, &ctx),
          "D3D11CreateDevice");
    ID3D11VideoDevice *vdev = nullptr;
    ID3D11VideoContext *vctx = nullptr;
    CHECK(dev->QueryInterface(__uuidof(ID3D11VideoDevice), (void **)&vdev), "ID3D11VideoDevice");
    CHECK(ctx->QueryInterface(__uuidof(ID3D11VideoContext), (void **)&vctx), "ID3D11VideoContext");

    D3D11_VIDEO_PROCESSOR_CONTENT_DESC cd = {};
    cd.InputFrameFormat = D3D11_VIDEO_FRAME_FORMAT_PROGRESSIVE;
    cd.InputWidth = iw;
    cd.InputHeight = ih;
    cd.OutputWidth = ow;
    cd.OutputHeight = oh;
    cd.InputFrameRate = {24, 1};
    cd.OutputFrameRate = {24, 1};
    cd.Usage = playback ? D3D11_VIDEO_USAGE_PLAYBACK_NORMAL : D3D11_VIDEO_USAGE_OPTIMAL_QUALITY;
    ID3D11VideoProcessorEnumerator *ven = nullptr;
    ID3D11VideoProcessor *vp = nullptr;
    CHECK(vdev->CreateVideoProcessorEnumerator(&cd, &ven), "CreateVideoProcessorEnumerator");
    CHECK(vdev->CreateVideoProcessor(ven, 0, &vp), "CreateVideoProcessor");

    // input: a staging texture the CPU writes, copied to one the processor reads
    D3D11_TEXTURE2D_DESC td = {};
    td.Width = iw;
    td.Height = ih;
    td.MipLevels = 1;
    td.ArraySize = 1;
    td.Format = DXGI_FORMAT_NV12;
    td.SampleDesc.Count = 1;
    td.Usage = D3D11_USAGE_DEFAULT;
    td.BindFlags = D3D11_BIND_RENDER_TARGET | D3D11_BIND_SHADER_RESOURCE;
    ID3D11Texture2D *in = nullptr, *up = nullptr, *out = nullptr, *down = nullptr;
    CHECK(dev->CreateTexture2D(&td, nullptr, &in), "input texture");
    td.Usage = D3D11_USAGE_STAGING;
    td.BindFlags = 0;
    td.CPUAccessFlags = D3D11_CPU_ACCESS_WRITE;
    CHECK(dev->CreateTexture2D(&td, nullptr, &up), "upload texture");
    td.Width = ow;
    td.Height = oh;
    td.Format = bgra ? DXGI_FORMAT_B8G8R8A8_UNORM : DXGI_FORMAT_NV12;
    td.Usage = D3D11_USAGE_DEFAULT;
    td.BindFlags = D3D11_BIND_RENDER_TARGET;
    td.CPUAccessFlags = 0;
    CHECK(dev->CreateTexture2D(&td, nullptr, &out), "output texture");
    td.Usage = D3D11_USAGE_STAGING;
    td.BindFlags = 0;
    td.CPUAccessFlags = D3D11_CPU_ACCESS_READ;
    CHECK(dev->CreateTexture2D(&td, nullptr, &down), "readback texture");

    D3D11_VIDEO_PROCESSOR_INPUT_VIEW_DESC ivd = {};
    ivd.ViewDimension = D3D11_VPIV_DIMENSION_TEXTURE2D;
    D3D11_VIDEO_PROCESSOR_OUTPUT_VIEW_DESC ovd = {};
    ovd.ViewDimension = D3D11_VPOV_DIMENSION_TEXTURE2D;
    ID3D11VideoProcessorInputView *iv = nullptr;
    ID3D11VideoProcessorOutputView *ov = nullptr;
    CHECK(vdev->CreateVideoProcessorInputView(in, ven, &ivd, &iv), "input view");
    CHECK(vdev->CreateVideoProcessorOutputView(out, ven, &ovd, &ov), "output view");

    RECT src = {0, 0, (LONG)iw, (LONG)ih}, dst = {0, 0, (LONG)ow, (LONG)oh};
    vctx->VideoProcessorSetStreamFrameFormat(vp, 0, D3D11_VIDEO_FRAME_FORMAT_PROGRESSIVE);
    vctx->VideoProcessorSetStreamSourceRect(vp, 0, TRUE, &src);
    vctx->VideoProcessorSetStreamDestRect(vp, 0, TRUE, &dst);
    vctx->VideoProcessorSetOutputTargetRect(vp, TRUE, &dst);
    vctx->VideoProcessorSetStreamAutoProcessingMode(vp, 0, FALSE);
    // limited range in and out; SD comes in as BT.601 and leaves as the BT.709
    // an HD picture is (as FLEX's other upscalers do)
    D3D11_VIDEO_PROCESSOR_COLOR_SPACE in_cs = {}, out_cs = {};
    in_cs.YCbCr_Matrix = ih >= 720 ? 1 : 0;
    in_cs.Nominal_Range = D3D11_VIDEO_PROCESSOR_NOMINAL_RANGE_16_235;
    out_cs.YCbCr_Matrix = 1;
    out_cs.Nominal_Range = D3D11_VIDEO_PROCESSOR_NOMINAL_RANGE_16_235;
    vctx->VideoProcessorSetStreamColorSpace(vp, 0, &in_cs);
    if (bgra) {
        D3D11_VIDEO_PROCESSOR_COLOR_SPACE rgb = {}; // full-range RGB out
        vctx->VideoProcessorSetOutputColorSpace(vp, &rgb);
    } else {
        vctx->VideoProcessorSetOutputColorSpace(vp, &out_cs);
    }

    struct {
        UINT version, method, enable;
    } ext = {1, 2, off ? 0u : 1u};
    HRESULT hr = vctx->VideoProcessorSetStreamExtension(vp, 0, &kNvidiaPPE, sizeof ext, &ext);
    fprintf(stderr, "flexvsr: RTX Video Super Resolution %s (0x%08lx)\n",
            FAILED(hr) ? "not available" : off ? "switched off" : "switched on", (unsigned long)hr);
    if (FAILED(hr) && !off) return 4; // a plain scale is worse than FLEX's other upscalers

    const size_t outRow = bgra ? (size_t)ow * 4 : ow, outRows = bgra ? oh : (size_t)oh * 3 / 2;
    std::vector<unsigned char> frame((size_t)iw * ih * 3 / 2), result(outRow * outRows), plain;
    // process runs the frame in `frame` through the processor into `result`
    auto process = [&](UINT n) {
        D3D11_MAPPED_SUBRESOURCE m;
        CHECK(ctx->Map(up, 0, D3D11_MAP_WRITE, 0, &m), "map upload");
        for (UINT y = 0; y < ih * 3 / 2; ++y) // luma rows, then the interleaved chroma rows
            memcpy((unsigned char *)m.pData + (size_t)y * m.RowPitch, &frame[(size_t)y * iw], iw);
        ctx->Unmap(up, 0);
        ctx->CopyResource(in, up);
        D3D11_VIDEO_PROCESSOR_STREAM stream = {};
        stream.Enable = TRUE;
        stream.pInputSurface = iv;
        CHECK(vctx->VideoProcessorBlt(vp, ov, n, 1, &stream), "VideoProcessorBlt");
        ctx->CopyResource(down, out);
        CHECK(ctx->Map(down, 0, D3D11_MAP_READ, 0, &m), "map readback");
        for (size_t y = 0; y < outRows; ++y)
            memcpy(&result[y * outRow], (unsigned char *)m.pData + y * m.RowPitch, outRow);
        ctx->Unmap(down, 0);
    };
    auto t0 = std::chrono::steady_clock::now();
    UINT n = 0;
    while (read_full(frame.data(), frame.size())) {
        if (n == 0 && !off) {
            // The driver can accept the switch and still not apply it (no RTX
            // card on this display, the option off, the display asleep). Run
            // the first frame without it too: the same picture means no
            // effect, and a plain scale is worse than FLEX's other upscalers.
            ext.enable = 0;
            vctx->VideoProcessorSetStreamExtension(vp, 0, &kNvidiaPPE, sizeof ext, &ext);
            process(n);
            plain = result;
            ext.enable = 1;
            vctx->VideoProcessorSetStreamExtension(vp, 0, &kNvidiaPPE, sizeof ext, &ext);
            process(n);
            if (result == plain) {
                fprintf(stderr, "flexvsr: RTX Video Super Resolution has no effect here; stopping\n");
                return 4;
            }
        } else {
            process(n);
        }
        if (fwrite(result.data(), 1, result.size(), stdout) != result.size()) break;
        ++n;
    }
    fflush(stdout);
    double secs = std::chrono::duration<double>(std::chrono::steady_clock::now() - t0).count();
    fprintf(stderr, "flexvsr: %u frames in %.1f s (%.0f fps)\n", n, secs, secs > 0 ? n / secs : 0);
    return 0;
}
