// MS-RDPEGFX 3.2.8.1: integer ICT, DWT, quantization and progressive entropy.
// Input contains only dirty, edge-extended 64x64 BGRA or BT.709 NV12 tiles.
// All coefficients, including FIRST's retained bits, remain on the GPU.
#ifdef __spirv__
#define BINDING(n) [[vk::binding(n, 0)]]
#else
#define BINDING(n)
#endif
BINDING(0) ByteAddressBuffer pixels : register(t0);
BINDING(1) ByteAddressBuffer metadata : register(t1);
BINDING(2) RWStructuredBuffer<int> coefficients : register(u0);
BINDING(3) RWByteAddressBuffer segments : register(u1);
BINDING(4) RWStructuredBuffer<uint> info : register(u2);
BINDING(5) RWByteAddressBuffer output : register(u3);

// Metadata words: tile count, NV12, quant, omitted bits, pass (SIMPLE/FIRST/UPGRADE),
// cache tile count, reserved, reserved, then one coefficient-cache index per tile.
static const uint SEGMENT_BYTES = 32768;
static const uint MAX_SEGMENTS = 48;
groupshared int a[4096];
groupshared int b[4096];

uint parameter(uint i) { return metadata.Load(i * 4); }
uint byteAt(uint i) { return (pixels.Load(i & ~3u) >> ((i & 3) * 8)) & 255; }
int sampleLine(bool vertical, int lineIndex, int i, int size) {
    if (vertical) return a[i * size + lineIndex];
    return b[lineIndex * size + i];
}
int highValue(bool vertical, int lineIndex, int i, int size, bool reduced) {
    int right = reduced ? 2*i+2 : min(2*i+2, size-2);
    return (2*sampleLine(vertical,lineIndex,2*i+1,size) - sampleLine(vertical,lineIndex,2*i,size)
        - sampleLine(vertical,lineIndex,right,size) + 2) >> 2;
}
int lowValue(bool vertical, int lineIndex, int i, int size, int hi, bool reduced) {
    int value = 2*i < size ? sampleLine(vertical,lineIndex,2*i,size)
        : 2*sampleLine(vertical,lineIndex,63,size)-sampleLine(vertical,lineIndex,62,size);
    int left, right;
    if (reduced && size == 64) {
        left = i <= hi ? highValue(vertical,lineIndex,max(0,i-1),size,reduced) : 0;
        right = i < hi ? highValue(vertical,lineIndex,i,size,reduced) : 0;
    } else {
        left = highValue(vertical,lineIndex,clamp(i-1,0,hi-1),size,reduced);
        right = highValue(vertical,lineIndex,min(i,hi-1),size,reduced);
    }
    return value + ((left+right+1) >> 1);
}

[numthreads(64,1,1)]
void main(uint3 group : SV_GroupID, uint lane : SV_GroupIndex) {
    if (parameter(4) == 2) return;
    uint tile=group.x, component=group.y;
    uint cacheTile=parameter(4)==0?tile:parameter(8+tile);
    uint base=cacheTile*12288+component*4096;
    bool nv12=parameter(1)!=0, reduced=parameter(4)==1;
    uint tileStart=tile*(nv12?6144:16384);
    for (uint i=lane;i<4096;i+=64) {
        int r,g,blue;
        if (nv12) {
            uint uv=tileStart+4096+(i/64/2)*64+(i%64/2)*2;
            int y=298*((int)byteAt(tileStart+i)-16);
            int u=(int)byteAt(uv)-128, v=(int)byteAt(uv+1)-128;
            r=clamp((y+459*v+128)>>8,0,255);
            g=clamp((y-55*u-136*v+128)>>8,0,255);
            blue=clamp((y+541*u+128)>>8,0,255);
        } else {
            uint address=tileStart+i*4;
            if (parameter(6)!=0) {
                uint width=parameter(6)/4,columns=(width+63)/64,index=parameter(8+tile);
                uint x=min(index%columns*64+i%64,width-1);
                uint y=min(index/columns*64+i/64,parameter(7)-1);
                address=y*parameter(6)+x*4;
            }
            uint p=pixels.Load(address);
            blue=p&255; g=(p>>8)&255; r=(p>>16)&255;
        }
        a[i]=component==0 ? ((9798*r+19235*g+3735*blue)>>10)-4096
            : component==1 ? (-5535*r-10868*g+16403*blue)>>10
            : (16377*r-13714*g-2663*blue)>>10;
    }
    GroupMemoryBarrierWithGroupSync();
    int size=64;
    uint offset=0;
    for (int level=0;level<3;level++) {
        int hi=size/2, lo=(size+1)/2;
        if (reduced && size==64) { hi=31; lo=33; }
        for (int i=(int)lane;i<size*(lo+hi);i+=64) {
            int x=i%size,y=i/size;
            b[i]=y<lo ? lowValue(true,x,y,size,hi,reduced) : highValue(true,x,y-lo,size,reduced);
        }
        GroupMemoryBarrierWithGroupSync();
        int hl=hi*lo,lh=lo*hi,hh=hi*hi;
        for (int i=(int)lane;i<size*(lo+hi);i+=64) {
            int x=i%(lo+hi),y=i/(lo+hi),position,value;
            if (x<lo) {
                value=lowValue(false,y,x,size,hi,reduced);
                position=y<lo ? hl+lh+hh+y*lo+x : hl+(y-lo)*lo+x;
            } else {
                value=highValue(false,y,x-lo,size,reduced);
                position=y<lo ? y*hi+x-lo : hl+lh+(y-lo)*hi+x-lo;
            }
            coefficients[base+offset+position]=value;
        }
        DeviceMemoryBarrierWithGroupSync();
        offset+=hl+lh+hh;
        for (int i=(int)lane;i<lo*lo;i+=64) a[i]=coefficients[base+offset+i];
        GroupMemoryBarrierWithGroupSync();
        size=lo;
    }
    uint shift=parameter(2)-1;
    for (uint i=lane;i<4096;i+=64) coefficients[base+i]=(coefficients[base+i]+(1 << (shift-1))) >> shift;
}

// One writer owns a dword-aligned segment. Finish zero-pads its last dword;
// segment lengths exclude padding. Overflow is signaled before any OOB store.
struct Writer { uint start; uint bits; uint word; };
uint networkWord(uint v) {
    return (v>>24)|((v>>8)&0xff00)|((v<<8)&0xff0000)|(v<<24);
}
Writer writer(uint segment) {
    Writer w; w.start=segment*SEGMENT_BYTES; w.bits=0; w.word=0; return w;
}
void writeBits(inout Writer w,uint value,uint count) {
    if (count==0) return;
    // Each append contains at most 32 bits and crosses at most one word.
    value&=0xffffffffu>>(32-count);
    uint remaining=32-(w.bits&31);
    w.bits+=count;
    if (count<remaining) {
        w.word|=value<<(remaining-count);
        return;
    }
    uint carry=count-remaining;
    uint word=w.word|(value>>carry);
    uint offset=(w.bits/32-1)*4;
    if (offset<SEGMENT_BYTES) segments.Store(w.start+offset,networkWord(word));
    w.word=carry!=0?value<<(32-carry):0;
}
void finish(Writer w,uint segment) {
    uint bytes=(w.bits+7)/8;
    if ((w.bits&31)!=0 && bytes<=SEGMENT_BYTES) segments.Store(w.start+(w.bits/32)*4,networkWord(w.word));
    info[segment]=bytes;
}
int entropyValue(uint base,uint i) {
    int v=coefficients[base+i];
    if (parameter(4)==1 && i<4015) v/=int(1u<<parameter(3));
    return v;
}
int deltaValue(uint base,uint i) {
    int v=entropyValue(base,i);
    uint ll=parameter(4)==1?4015:4032;
    if (i>ll) v-=entropyValue(base,i-1);
    return v;
}
void golomb(inout Writer w,uint value,inout int krp) {
    uint kr=krp>>3, quotient=value>>kr;
    uint count=quotient+1+kr;
    if (count<=32) {
        uint code=(0xffffffffu<<(kr+1))|(value&((1u<<kr)-1));
        writeBits(w,code,count);
    } else {
        uint unary=quotient;
        while (unary>=32) { writeBits(w,0xffffffffu,32); unary-=32; }
        if (unary!=0) writeBits(w,(1u<<unary)-1,unary);
        writeBits(w,0,1); writeBits(w,value,kr);
    }
    if (quotient==0) krp=max(0,krp-2);
    else if (quotient>1) krp=min(80,krp+(int)quotient);
}

[numthreads(1,1,1)]
void encodeTiles(uint3 group : SV_GroupID) {
    uint tile=group.x,c=group.y,segment=tile*6+c*2;
    uint base=(parameter(4)==0?tile:parameter(8+tile))*12288+c*4096;
    Writer w=writer(segment),raw=writer(segment+1);
    int kp=8,krp=8;
    if (parameter(4)==2) {
        uint bits=parameter(3),mask=(1u<<bits)-1;
        uint run=0;
        // SRL sees only coefficients whose magnitude was zero in FIRST.
        for (uint i=0;i<=4015;i++) {
            int v=i<4015?coefficients[base+i]:0;
            uint magnitude=abs(v);
            if (i<4015 && magnitude>mask) { writeBits(raw,magnitude&mask,bits); continue; }
            if (i<4015 && v==0) { run++; continue; }
            // An empty SRL stream has only the mandatory lookahead byte.
            if (i==4015 && run==0) break;
            uint k=kp>>3;
            while (run >= (1u<<k)) { writeBits(w,0,1); run-=1u<<k; kp=min(80,kp+4); k=kp>>3; }
            writeBits(w,1,1); writeBits(w,run,k); kp=max(0,kp-6); run=0;
            if (i==4015) break;
            writeBits(w,v<0?1:0,1);
            writeBits(w,0,magnitude-1);
            if (magnitude<mask) writeBits(w,1,1);
        }
        // Pad to a byte before adding the required SRL lookahead byte.
        if ((w.bits&7)!=0) writeBits(w,0,8-(w.bits&7));
        writeBits(w,0,8);
    } else {
        for (uint i=0;i<4096;i++) {
            int k=kp>>3;
            if (k>0) {
                uint run=0;
                while (i<4096 && deltaValue(base,i)==0) { run++; i++; }
                while (run >= (1u<<k)) { writeBits(w,0,1); run-=1u<<k; kp=min(80,kp+4); k=kp>>3; }
                writeBits(w,1,1); writeBits(w,run,k);
                if (i==4096) break;
                int v=deltaValue(base,i);
                writeBits(w,v<0?1:0,1); golomb(w,abs(v)-1,krp); kp=max(0,kp-6);
            } else {
                int v=deltaValue(base,i);
                uint code=v<0?-v*2-1:v*2;
                golomb(w,code,krp); kp=code==0?min(80,kp+3):max(0,kp-3);
            }
        }
    }
    finish(w,segment); finish(raw,segment+1);
}

[numthreads(1,1,1)]
void indexSegments(uint3 ignored : SV_DispatchThreadID) {
    uint total=0,invalid=0;
    for (uint i=0;i<parameter(0)*6;i++) {
        uint size=info[i];
        if (size>SEGMENT_BYTES) invalid=1;
        info[64+i]=total;
        total+=(size+3)&~3u;
    }
    info[48]=total; info[49]=invalid;
}

[numthreads(64,1,1)]
void packSegments(uint3 group : SV_GroupID,uint lane : SV_GroupIndex) {
    if (info[49]!=0) return;
    uint segment=group.x;
    for (uint i=lane*4;i<info[segment];i+=256)
        output.Store(info[64+segment]+i,segments.Load(segment*SEGMENT_BYTES+i));
}
