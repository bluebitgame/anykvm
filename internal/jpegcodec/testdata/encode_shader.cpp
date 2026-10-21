// The texture addressing is unchanged; feed BGRA samples directly to the actual
// shared chroma/DCT shader and compare its coefficients with the Go oracle.
#include <algorithm>
#include <atomic>
#include <cmath>
#include <condition_variable>
#include <cstdint>
#include <iostream>
#include <mutex>
#include <thread>
#include <vector>
#define _Static_assert static_assert
#include "../../gpuconv/jpeg_layout.h"
#undef _Static_assert
using uint=uint32_t;
using std::clamp;using std::floor;using std::min;using std::max;
struct uint2 {
    uint x,y;
    uint2(uint x,uint y):x(x),y(y) {}
};
struct uint3 { uint x,y,z; uint3(uint x=0,uint y=0,uint z=0):x(x),y(y),z(z){} };
struct uint4 { uint x=0,y=0,z=0,w=0; };
uint2 operator+(uint2 a,uint2 b) { return {a.x+b.x,a.y+b.y}; }
uint2 operator*(uint2 a,uint b) { return {a.x*b,a.y*b}; }
struct float2 { float x,y;float2(float x,float y):x(x),y(y) {} };
struct float3 {
    float x,y,z;
    float3(float x,float y,float z):x(x),y(y),z(z) {}
};
float3 operator+(float3 a,float3 b) { return {a.x+b.x,a.y+b.y,a.z+b.z}; }
float3 operator*(float3 a,float b) { return {a.x*b,a.y*b,a.z*b}; }
float dot(float3 a,float3 b) { return a.x*b.x+a.y*b.y+a.z*b.z; }
float sign(float a) { return a<0?-1:a>0?1:0; }
struct float4 {
    float x,y,z,w;
    float4(float x=0,float y=0,float z=0,float w=0):x(x),y(y),z(z),w(w) {}
};
float4 operator+(float4 a,float4 b) { return {a.x+b.x,a.y+b.y,a.z+b.z,a.w+b.w}; }
float4 operator-(float4 a) { return {-a.x,-a.y,-a.z,-a.w}; }
float dot(float4 a,float4 b) { return a.x*b.x+a.y*b.y+a.z*b.z+a.w*b.w; }
uint width,height,mcuColumns,mcuCount;
uint2 outputSize(0,0);
uint4 tileState;
std::vector<uint> dirtyTiles;
float quantizationReciprocals[32][4];
std::vector<uint> outputCoefficients;
std::vector<uint> segmentInfo;
std::vector<uint8_t> inputPixels;
std::atomic<uint> sampleLoads{0};
float3 loadRGB(uint2 p) {
    sampleLoads++;
    uint x=std::min(p.x,width-1),y=std::min(p.y,height-1);
    uint offset=(y*width+x)*4;
    return {float(inputPixels[offset+2]),float(inputPixels[offset+1]),float(inputPixels[offset])};
}
class Barrier {
    std::mutex mutex;
    std::condition_variable changed;
    unsigned count=0,generation=0;
public:
    void wait() {
        std::unique_lock<std::mutex> lock(mutex);
        unsigned old=generation;
        if (++count==64) { count=0;generation++;changed.notify_all(); }
        else changed.wait(lock,[&]{return generation!=old;});
    }
} barrier;
void GroupMemoryBarrierWithGroupSync() { barrier.wait(); }
void DeviceMemoryBarrierWithGroupSync() { barrier.wait(); }
std::mutex interlockedMutex;
void InterlockedOr(uint &destination,uint bits) {
    std::lock_guard<std::mutex> lock(interlockedMutex);
    destination|=bits;
}
#ifdef FUSED_COMPARE
void InterlockedAdd(uint &destination,uint value,uint &old) {
    std::lock_guard<std::mutex> lock(interlockedMutex);old=destination;destination+=value;
}
uint jpegWorkBase();
struct MockBuffer {
    std::vector<uint> words;
    uint Load(uint offset) {return words.at(offset/4);}
    void Store(uint offset,uint value) {words.at(offset/4)=value;}
    void InterlockedAdd(uint offset,uint value,uint &old) {old=words.at(offset/4);words.at(offset/4)+=value;}
} previousPixels,jpegHeader,dispatchArguments;
uint compareWidth(){return width;}
uint compareHeight(){return height;}
uint compareForce(){return tileState.x;}
uint comparePixel(uint x,uint y){auto rgb=loadRGB({x,y});return uint(rgb.x)|(uint(rgb.y)<<8)|(uint(rgb.z)<<16);}
uint comparePrevious(uint offset){return previousPixels.Load(offset);}
void compareStore(uint offset,uint value){previousPixels.Store(offset,value);}

#endif
#include "encode_shader.hpp"

#ifdef FUSED_COMPARE
int checkFused(const std::vector<uint> &baseline,const uint *quantization) {
    // Stress signed rounding and UNORM8 normalization with constant inputs,
    // comparing the shortcut against the actual two-pass AAN butterfly.
    uint random=91;
    for(uint i=0;i<1024;i++){
        random=random*1664525u+1013904223u;
        float3 rgb(i<256?i:(random&255),i<256?i:((random>>8)&255),i<256?i:((random>>16)&255));
        if(float(float(uint(rgb.x))/255.0f)*255.0f!=rgb.x)return 29;
        float values[3]={dot(rgb,{0.299f,0.587f,0.114f})-128.0f,dot(rgb,{-0.168736f,-0.331264f,0.5f}),dot(rgb,{0.5f,-0.418688f,-0.081312f})};
        for(uint c=0;c<3;c++){
            float v=values[c];auto row=aanDCT({v,v,v,v},{v,v,v,v});v=row.values[0];
            auto column=aanDCT({v,v,v,v},{v,v,v,v});uint block=c==0?0:c+3;
            if(quantizeCoefficient(column.values[0],block,0)!=quantizeCoefficient(values[c]*64.0f,block,0))return 30;
        }
    }
    outputSize={width,height};
    uint tiles=((width+63)/64)*((height+63)/64);
    dirtyTiles.assign(bk_jpeg_work_words(tiles,mcuCount),0);
    segmentInfo.assign(bk_jpeg_segment_info_words(mcuCount),0);
    previousPixels.words.assign(width*height,0);
    dispatchArguments.words.assign(BK_JPEG_ARGUMENT_BYTES/4,0);
    jpegHeader.words.assign(20,0);jpegHeader.words[1]=quantization[0]<<24;jpegHeader.words[18]=quantization[64];
    outputCoefficients.assign(baseline.size(),0x5a5a5a5a);
    tileState={1,1,1,1};
    auto compare=[&]{
        dispatchArguments.words[4]=0;dispatchArguments.words[14]=0;
        for(uint y=0;y<(height+63)/64;y++)for(uint x=0;x<(width+63)/64;x++){
            std::vector<std::thread> threads;
            for(uint lane=0;lane<64;lane++)threads.emplace_back([=]{compareTile({x,y,0},lane);});
            for(auto &t:threads)t.join();
        }
        prepareWork({0,0,0});
    };
    sampleLoads=0;compare();uint compareLoads=sampleLoads;
    if(compareLoads!=width*height || dirtyTiles[tiles]!=mcuCount)return 20;
    uint pure=0,dctLoads=0;
    std::vector<bool> scheduled(mcuCount,false);
    for(uint slot=0;slot<jpegComputeCount();slot++){
        uint mcu=jpegComputeMCU(slot);
        if(mcu>=mcuCount || scheduled[mcu] || (segmentInfo[mcu*2]&BK_JPEG_SOLID_MCU_TAG))return 32;
        scheduled[mcu]=true;
        uint before=sampleLoads;
        for(uint b=0;b<dispatchArguments.words[7];b++){
            std::vector<std::thread> threads;
            for(uint lane=0;lane<64;lane++)threads.emplace_back([=]{transformMain({slot,0,b},{lane%8,lane/8,0});});
            for(auto &t:threads)t.join();
        }
        if(sampleLoads-before!=512u)return 21;
        dctLoads+=sampleLoads-before;
    }
    if(dispatchArguments.words[5]!=jpegComputeCount() || dispatchArguments.words[8]!=(jpegComputeCount()+63)/64 ||
       dispatchArguments.words[11]!=(mcuCount+SEGMENT_SCAN_THREADS-1)/SEGMENT_SCAN_THREADS)return 33;
    for(uint slot=0;slot<mcuCount;slot++){
        uint mcu=jpegWorkMCU(slot),tag=segmentInfo[mcu*2],word=segmentInfo[jpegSolidOffset(mcu)];
        bool solid=(tag&BK_JPEG_SOLID_MCU_TAG)!=0;
        if(scheduled[mcu]==solid)return 34;
        int dc[3]={int32_t(word<<21)>>21,int32_t((word>>11)<<21)>>21,int32_t(((word>>22)|((tag>>30)&1)<<10)<<21)>>21};
        for(uint b=0;b<6;b++){
            uint offset=(mcu*6+b)*COEFFICIENT_WORDS,mask=acMaskOffset(offset);
            for(uint i=0;i<COEFFICIENT_WORDS;i++){
                uint expected=solid?(i==0?uint(uint16_t(dc[b<4?0:b-3])):0):outputCoefficients[offset+i];
                if(expected!=baseline[offset+i]){std::cerr<<"fused DC/DCT mismatch at MCU "<<mcu<<" block "<<b;return 22;}
                if(solid && outputCoefficients[offset+i]!=0x5a5a5a5a)return 23;
            }
            if(solid){if(baseline[mask]||baseline[mask+1])return 24;}
            else if(outputCoefficients[mask]!=baseline[mask]||outputCoefficients[mask+1]!=baseline[mask+1])return 25;
        }
        if(solid)pure++;
    }
    // Run the post-DCT selector too: near-flat RGB can become a descriptor
    // only here. Compare its queue against the independent full-DCT baseline.
    std::vector<std::thread> selectors;
    for(uint lane=0;lane<64;lane++)selectors.emplace_back([=]{
        for(uint slot=lane;slot<((jpegComputeCount()+63)/64)*64;slot+=64)selectEntropy({slot,0,0});
    });
    for(auto &lane:selectors)lane.join();
    prepareEntropy({0,0,0});
    std::vector<bool> entropyQueued(mcuCount,false);
    for(uint slot=0;slot<jpegEntropyCount();slot++){
        uint mcu=jpegEntropyMCU(slot);
        if(mcu>=mcuCount || entropyQueued[mcu])return 36;
        entropyQueued[mcu]=true;
    }
    for(uint mcu=0;mcu<mcuCount;mcu++){
        bool flat=true;int yDC=int16_t(baseline[mcu*6*COEFFICIENT_WORDS]);
        for(uint b=0;b<6;b++){
            uint off=(mcu*6+b)*COEFFICIENT_WORDS;
            int dc=int16_t(baseline[off]),sample=dc*int(quantization[b<4?0:64]);
            if((b<4 && dc!=yDC)||sample < -2048||sample > 2047)flat=false;
            for(uint i=1;i<64;i++)if(int16_t(baseline[off+i/2]>>((i&1)*16))!=0)flat=false;
        }
        if(entropyQueued[mcu]==flat || bool(segmentInfo[mcu*2]&BK_JPEG_SOLID_MCU_TAG)!=flat)return 37;
    }
    if(dispatchArguments.words[8]!=(jpegEntropyCount()+63)/64)return 38;
    auto metadata=segmentInfo;
    tileState.x=0;compare();
    if(dirtyTiles[tiles]!=0 || jpegComputeCount()!=0 || dispatchArguments.words[5]!=0 || dispatchArguments.words[8]!=0 || segmentInfo!=metadata)return 26;
    for(size_t i=3;i<inputPixels.size();i+=4)inputPixels[i]^=255;
    compare();if(dirtyTiles[tiles]!=0 || jpegComputeCount()!=0 || dispatchArguments.words[5]!=0 || dispatchArguments.words[8]!=0 || segmentInfo!=metadata)return 27;
    if(tiles>1){
        // Sparse DXGI candidates must neither sample nor overwrite metadata
        // outside the selected tile, even when those pixels have changed.
        inputPixels[0]^=1;
        tileState.w=3;dirtyTiles[bk_jpeg_candidate_offset(tiles,mcuCount)]=tiles-1;
        dispatchArguments.words[4]=0;uint before=sampleLoads;
        std::vector<std::thread> threads;
        for(uint lane=0;lane<64;lane++)threads.emplace_back([=]{compareTile({0,0,0},lane);});
        for(auto &t:threads)t.join();
        uint tileX=(tiles-1)%((width+63)/64),tileY=(tiles-1)/((width+63)/64);
        uint visible=std::min(64u,width-tileX*64)*std::min(64u,height-tileY*64);
        if(sampleLoads-before!=visible || dispatchArguments.words[4]!=0 || segmentInfo!=metadata)return 31;
        inputPixels[0]^=1;tileState.w=1;
    }
    // A previously constant MCU becoming textured must lose its old skip tag.
    if(width>1){
        inputPixels[0]^=1;compare();
        bool uniform=true;
        for(uint y=0;y<std::min(16u,height);y++)for(uint x=0;x<std::min(16u,width);x++){
            uint off=(y*width+x)*4;
            for(uint c=0;c<3;c++)if(inputPixels[off+c]!=inputPixels[c])uniform=false;
        }
        if(!uniform && (segmentInfo[0]&BK_JPEG_SOLID_MCU_TAG))return 28;
        bool queued=false;
        for(uint slot=0;slot<jpegComputeCount();slot++)if(jpegComputeMCU(slot)==0)queued=true;
        if(queued==uniform)return 35;
    }
    std::cout.write((char *)&pure,4);std::cout.write((char *)&compareLoads,4);std::cout.write((char *)&dctLoads,4);
    return 0;
}
#endif

// Check the unquantized butterfly too: rounding to integers could hide a
// small frequency-dependent scaling error in the image fixtures.
bool checkDCT() {
    constexpr double pi=3.14159265358979323846;
    uint random=41;
    for (uint test=0;test<138;test++) {
        float samples[8];
        double magnitude=0;
        for (uint i=0;i<8;i++) {
            random=random*1664525u+1013904223u;
            if (test<8) samples[i]=i==test?127.5f:0;
            else if (test==8) samples[i]=128;
            else if (test==9) samples[i]=(i&1)?-128:128;
            else samples[i]=float(int(random>>16)-32768)*(test&1?0.0625f:0.00390625f);
            magnitude+=std::abs(samples[i]);
        }
        DCTResult result=aanDCT({samples[0],samples[1],samples[2],samples[3]},
                               {samples[4],samples[5],samples[6],samples[7]});
        for (uint frequency=0;frequency<8;frequency++) {
            double expected=0;
            for (uint i=0;i<8;i++) expected+=samples[i]*std::cos((2*i+1)*frequency*pi/16);
            expected*=frequency==0?std::sqrt(1.0/8):0.5;
            double scale=frequency==0?std::sqrt(8.0):4*std::cos(frequency*pi/16);
            if (std::abs(result.values[frequency]/scale-expected)>1e-5+1e-6*magnitude) {
                std::cerr<<"AAN mismatch: input "<<test<<", frequency "<<frequency;
                return false;
            }
        }
    }
    return true;
}

int main() {
    if (!checkDCT()) return 7;
    std::cin.read((char *)&width,4);std::cin.read((char *)&height,4);
    uint quantization[128];
    std::cin.read((char *)quantization,sizeof(quantization));
    inputPixels.resize(width*height*4);
    std::cin.read((char *)inputPixels.data(),inputPixels.size());
    bk_jpeg_parameters parameters={};
    bk_jpeg_prepare_quantization(&parameters,quantization);
    for (uint i=0;i<128;i++) quantizationReciprocals[i/4][i%4]=parameters.quantization_reciprocals[i];
    if (!std::cin || !width || !height) return 2;
    mcuColumns=(width+15)/16;
    uint rows=(height+15)/16;
    mcuCount=mcuColumns*rows;
    outputCoefficients.resize(mcuCount*MCU_WORDS);
    for (uint y=0;y<rows;y++) for (uint x=0;x<mcuColumns;x++) for (uint block=0;block<5;block++) {
        std::vector<std::thread> threads;
        for (uint lane=0;lane<64;lane++) threads.emplace_back([=]{transformMain({x,y,block},{lane%8,lane/8,0});});
        for (auto &thread:threads) thread.join();
    }
    // Execute the real DCT kernel with all tiles clean: no texture sample or
    // coefficient store may occur. Then enable only the first 64px column.
    auto baseline=outputCoefficients;
#ifdef FUSED_COMPARE
    return checkFused(baseline,quantization);
#endif
    tileState.y=1;
    dirtyTiles.assign(((width+63)/64)*((height+63)/64),0);
    auto runTransform=[&] {
        for(uint y=0;y<rows;y++)for(uint x=0;x<mcuColumns;x++)for(uint b=0;b<5;b++) {
            std::vector<std::thread> threads;
            for(uint lane=0;lane<64;lane++)threads.emplace_back([=]{transformMain({x,y,b},{lane%8,lane/8,0});});
            for(auto &t:threads)t.join();
        }
    };
    uint before=sampleLoads.load();runTransform();
    if(sampleLoads!=before||outputCoefficients!=baseline) return 3;
    for(uint y=0;y<(height+63)/64;y++)dirtyTiles[y*((width+63)/64)]=1;
    before=sampleLoads.load();runTransform();
    if(sampleLoads-before!=std::min(4u,mcuColumns)*rows*512u||outputCoefficients!=baseline) return 4;

    outputSize={width,height};
    uint base=dirtyTiles.size();
    auto flags=dirtyTiles;
    dirtyTiles.resize(base+3+3*mcuCount);
    uint work=0;
    for(uint m=mcuCount;m>0;m--) {
        uint index=m-1,tile=(index/mcuColumns/4)*((mcuColumns+3)/4)+(index%mcuColumns/4);
        if(flags[tile])dirtyTiles[base+1+work++]=index;
    }
    dirtyTiles[base]=work;
    dirtyTiles[base+1+mcuCount]=work;
    std::copy_n(dirtyTiles.begin()+base+1,work,dirtyTiles.begin()+base+2+mcuCount);
    tileState.w=1;
    before=sampleLoads.load();
    for(uint slot=0;slot<work;slot++)for(uint b=0;b<5;b++) {
        std::vector<std::thread> threads;
        for(uint lane=0;lane<64;lane++)threads.emplace_back([=]{transformMain({slot,0,b},{lane%8,lane/8,0});});
        for(auto &t:threads)t.join();
    }
    if(sampleLoads-before!=work*512u||outputCoefficients!=baseline)return 6;
    for (uint offset=0;offset<mcuCount*BLOCKS_PER_MCU*COEFFICIENT_WORDS;offset+=COEFFICIENT_WORDS) {
        uint masks[2]={0,0};
        for (uint scan=1;scan<BLOCK_COEFFICIENTS;scan++) {
            uint natural=zigzag[scan],packed=outputCoefficients[offset+natural/2];
            int value=int(int16_t(packed>>((natural&1)*16)));
            if(value!=0)masks[scan/32]|=1u<<(scan&31);
        }
        if(outputCoefficients[acMaskOffset(offset)]!=masks[0] ||
           outputCoefficients[acMaskOffset(offset)+1]!=masks[1]) return 5;
        for(uint word=0;word<COEFFICIENT_WORDS;word++) {
            uint packed=outputCoefficients[offset+word];
            int values[2]={int(int16_t(packed & 0xffff)),int(int16_t(packed >> 16))};
            std::cout.write((char *)values,sizeof(values));
        }
    }
}
