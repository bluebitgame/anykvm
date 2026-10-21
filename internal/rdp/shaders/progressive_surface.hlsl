// Immutable GPU snapshot -> fitted BGRA, plus exact 64x64 dirty flags.
// Previous pixels are committed only after the complete RDP frame is sent.
#ifdef __spirv__
#define BIND(n) [[vk::binding(n, 0)]]
BIND(0) ByteAddressBuffer sourcePixels : register(t0);
#else
#define BIND(n)
BIND(0) Texture2D<float4> sourceTexture : register(t0);
#endif
BIND(1) ByteAddressBuffer surfaceParameters : register(t1);
BIND(2) RWByteAddressBuffer currentPixels : register(u0);
BIND(3) RWByteAddressBuffer previousPixels : register(u1);
BIND(4) RWStructuredBuffer<uint> dirtyTiles : register(u2);
uint sp(uint i) { return surfaceParameters.Load(i*4); }
uint sourcePixel(uint x,uint y) {
    uint sx=x,sy=y;
    if (sp(3)==90) { sx=y;sy=sp(1)-1-x; }
    else if (sp(3)==180) { sx=sp(0)-1-x;sy=sp(1)-1-y; }
    else if (sp(3)==270) { sx=sp(0)-1-y;sy=x; }
#ifdef __spirv__
    return sourcePixels.Load(sy*sp(2)+sx*4)&0x00ffffff;
#else
    float3 rgb=sourceTexture.Load(int3(sx,sy,0)).rgb;
    uint3 bgr=uint3(floor(rgb.bgr*255.0+0.5));
    return bgr.x|(bgr.y<<8)|(bgr.z<<16);
#endif
}
void axis(uint x,uint source,uint target,out uint lo,out uint hi,out uint weight) {
    int numerator=int((2*x+1)*source)-int(target),denominator=int(2*target);
    numerator=clamp(numerator,0,int(source-1)*denominator);
    lo=uint(numerator)/uint(denominator);hi=min(lo+1,source-1);
    weight=(uint(numerator)%uint(denominator)*256+uint(denominator)/2)/uint(denominator);
}
uint fittedPixel(uint x,uint y) {
    if (x<sp(6)||y<sp(7)||x>=sp(6)+sp(8)||y>=sp(7)+sp(9)) return 0xff000000;
    uint sw=sp(0),sh=sp(1);
    if (sp(3)==90||sp(3)==270) { sw=sp(1);sh=sp(0); }
    uint x0,x1,wx,y0,y1,wy;
    axis(x-sp(6),sw,sp(8),x0,x1,wx);axis(y-sp(7),sh,sp(9),y0,y1,wy);
    uint a=sourcePixel(x0,y0),b=sourcePixel(x1,y0),c=sourcePixel(x0,y1),d=sourcePixel(x1,y1);
    uint pixel=0xff000000;
    for (uint channel=0;channel<3;channel++) {
        uint shift=channel*8;
        uint top=((a>>shift)&255)*(256-wx)+((b>>shift)&255)*wx;
        uint bottom=((c>>shift)&255)*(256-wx)+((d>>shift)&255)*wx;
        pixel|=((top*(256-wy)+bottom*wy+32768)>>16)<<shift;
    }
    return pixel;
}
uint compareWidth() { return sp(4); }
uint compareHeight() { return sp(5); }
uint compareForce() { return sp(10); }
uint comparePixel(uint x,uint y) { return fittedPixel(x,y); }
uint comparePrevious(uint offset) {
    if (sp(11)!=0) {
        uint x=(offset/4)%sp(4),y=(offset/4)/sp(4);
        uint tile=(y/64)*((sp(4)+63)/64)+x/64;
        uint sx=sp(12+tile*2),sy=sp(13+tile*2);
        if (sx!=0xffffffff) offset=((sy+y%64)*sp(4)+sx+x%64)*4;
    }
    return previousPixels.Load(offset);
}
void compareStore(uint offset,uint pixel) { currentPixels.Store(offset,pixel); }
void compareFlag(uint index,uint dirty) { dirtyTiles[index]=dirty; }
// ANYKVM_TILE_COMPARE
