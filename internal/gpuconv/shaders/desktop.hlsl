// Shared desktop conversion source. Vulkan compiles this HLSL to SPIR-V.
// Resource registers remain usable by a future Direct3D compute backend.
#ifdef __spirv__
#define BIND(n) [[vk::binding(n, 0)]]
#else
#define BIND(n)
#endif
BIND(0) Texture2D<float4> sourceImage : register(t0);
BIND(1) SamplerState linearClamp : register(s0);
BIND(2) RWByteAddressBuffer destination : register(u0);
BIND(3) cbuffer Parameters : register(b0) {
    uint2 sourceSize;
    uint2 outputSize;
    uint2 cropOrigin;
    uint2 cropSize;
    uint4 content; // x, y, width, height in output pixels
    uint pitch;
    uint nv12;
    uint rotation; // clockwise degrees
    uint flipY;
};
float3 pixel(uint2 p) {
    if (any(p >= outputSize) || any(p < content.xy) || any(p >= content.xy + content.zw)) return 0.0;
    float2 uv = (float2(p - content.xy) + 0.5) / float2(content.zw);
    if (rotation == 90) uv = float2(uv.y, 1.0 - uv.x);
    else if (rotation == 180) uv = 1.0 - uv;
    else if (rotation == 270) uv = float2(1.0 - uv.y, uv.x);
    if (flipY != 0) uv.y = 1.0 - uv.y;
    uv = (float2(cropOrigin) + uv * float2(cropSize)) / float2(sourceSize);
    return sourceImage.SampleLevel(linearClamp, uv, 0).rgb;
}
uint quantize(float v) {return (uint)clamp(floor(v + 0.5), 0.0, 255.0);}
uint luma(float3 rgb) {return quantize(16.0 + 219.0 * dot(rgb, float3(0.2126, 0.7152, 0.0722)));}
uint2 chroma(float3 rgb) {
    float y = dot(rgb, float3(0.2126, 0.7152, 0.0722));
    return uint2(quantize(128.0 + 112.0 * (rgb.b - y) / (1.0 - 0.0722)),
                 quantize(128.0 + 112.0 * (rgb.r - y) / (1.0 - 0.2126)));
}
uint pack4(uint a, uint b, uint c, uint d) {return a | (b << 8) | (c << 16) | (d << 24);}
[numthreads(8, 8, 1)]
void main(uint3 tid : SV_DispatchThreadID) {
    uint2 p = tid.xy * uint2(4, 2);
    if (any(p >= outputSize)) return;
    float3 a = pixel(p), b = pixel(p + uint2(1,0)), c = pixel(p + uint2(2,0)), d = pixel(p + uint2(3,0));
    float3 e = pixel(p + uint2(0,1)), f = pixel(p + uint2(1,1)), g = pixel(p + uint2(2,1)), h = pixel(p + uint2(3,1));
    if (nv12 != 0) {
        // Each invocation owns complete 32-bit words; no sub-word races.
        destination.Store(p.y * pitch + p.x, pack4(luma(a), luma(b), luma(c), luma(d)));
        destination.Store((p.y + 1) * pitch + p.x, pack4(luma(e), luma(f), luma(g), luma(h)));
        uint2 uv0 = chroma((a+b+e+f)*0.25), uv1 = chroma((c+d+g+h)*0.25);
        destination.Store(outputSize.y * pitch + (p.y / 2) * pitch + p.x, pack4(uv0.x, uv0.y, uv1.x, uv1.y));
    } else {
        for (uint row=0; row<2; row++) for (uint col=0; col<4; col++) {
            uint2 q = p + uint2(col, row);
            if (all(q < outputSize)) {
                float3 rgb = pixel(q);
                destination.Store(q.y * pitch + q.x * 4, pack4(quantize(rgb.b*255.0), quantize(rgb.g*255.0), quantize(rgb.r*255.0), 255));
            }
        }
    }
}
