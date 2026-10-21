// Execute the shared HLSL with checked CPU buffers and real group barriers.
// This checks codec math and bounds; driver behavior still needs GPU tests.
#include <algorithm>
#include <cmath>
#include <condition_variable>
#include <cstdint>
#include <iostream>
#include <mutex>
#include <thread>
#include <vector>
using uint = uint32_t;
using std::min; using std::max; using std::clamp; using std::floor;
struct uint2 {
    uint x, y;
    uint2(uint x=0,uint y=0):x(x),y(y) {}
    uint2 &operator&=(uint n) { x&=n;y&=n;return *this; }
    uint2 &operator>>=(uint2 n) { x>>=n.x;y>>=n.y;return *this; }
};
struct uint3 { uint x,y,z; };
#define VEC_OP(op) uint2 operator op(uint2 a,uint2 b) { return {a.x op b.x,a.y op b.y}; }
VEC_OP(+) VEC_OP(-) VEC_OP(*) VEC_OP(/) VEC_OP(>>) VEC_OP(&) VEC_OP(<<)
VEC_OP(<) VEC_OP(>=) VEC_OP(!=)
uint2 operator+(uint2 a,uint b) { return a+uint2(b,b); }
uint2 operator-(uint2 a,uint b) { return a-uint2(b,b); }
uint2 operator<<(uint a,uint2 b) { return uint2(a,a)<<b; }
uint2 min(uint2 a,uint2 b) { return {min(a.x,b.x),min(a.y,b.y)}; }
bool any(uint2 a) { return a.x || a.y; }
struct float3 {
    float x,y,z;
    float3(float x,float y,float z):x(x),y(y),z(z) {}
};
float3 operator+(float3 a,float3 b) { return {a.x+b.x,a.y+b.y,a.z+b.z}; }
float3 operator*(float3 a,float b) { return {a.x*b,a.y*b,a.z*b}; }
float3 clamp(float3 a,double lo,double hi) {
    return {clamp(a.x,float(lo),float(hi)),clamp(a.y,float(lo),float(hi)),clamp(a.z,float(lo),float(hi))};
}
float dot(float3 a,float3 b) { return a.x*b.x+a.y*b.y+a.z*b.z; }
struct Buffer {
    std::vector<uint> words;
    uint Load(uint offset) { return words.at(offset/4); }
    void Store(uint offset,uint value) { words.at(offset/4)=value; }
    void InterlockedOr(uint offset,uint value,uint &old) {
        static std::mutex mutex;
        std::lock_guard<std::mutex> lock(mutex);
        old=words.at(offset/4);words.at(offset/4)|=value;
    }
} entropyInput,metadata,nv12Output,decodeStatus;
std::vector<int> coefficients;
std::vector<float> samples;
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
#include "decode_shader.hpp"

template<class F> void group(F run) {
    std::vector<std::thread> threads;
    for (uint lane=0;lane<64;lane++) threads.emplace_back([=]{run(lane);});
    for (auto &thread:threads) thread.join();
}
int main() {
    try {
        for (;;) {
            uint sizes[2];
            std::cin.read((char *)sizes,sizeof(sizes));
            if (std::cin.eof()) return 0;
            if (!std::cin || sizes[0]<2056 || sizes[1]%4) return 2;
            metadata.words.resize(sizes[0]);entropyInput.words.resize(sizes[1]/4);
            std::cin.read((char *)metadata.words.data(),sizes[0]*4);
            std::cin.read((char *)entropyInput.words.data(),sizes[1]);
            if (!std::cin) return 3;
            uint blocks=meta(6)*meta(7)*meta(8),bytes=meta(2)*meta(3)*3/2;
            coefficients.assign(blocks*64,0);samples.assign(blocks*64,0);
            nv12Output.words.assign((bytes+3)/4,0);decodeStatus.words.assign(1,0);
            for (uint g=0;g<(meta(9)+63)/64;g++)
                group([=](uint lane){entropyMain({g*64+lane,0,0},lane);});
            uint status=decodeStatus.Load(0);
            std::cout.write((char *)&status,4);
            if (status) { std::cout.flush();continue; }
            for (uint y=0;y<meta(7);y++) for (uint x=0;x<meta(6)*meta(8);x++)
                group([=](uint lane){inverseDCT({x,y,0},{lane%8,lane/8,0});});
            for (uint word=0;word<(bytes+3)/4;word++) packNV12({word,0,0});
            for (uint block=0;block<blocks;block++) {
                uint packed=uint(coefficients[block*64]);
                int ac=0;
                for (uint i=1;i<64;i++) ac|=coefficients[block*64+i];
                if (bool(packed&0x1000)!=bool(ac)) return 4;
                coefficients[block*64]=int(packed<<20)>>20;
            }
            std::cout.write((char *)coefficients.data(),coefficients.size()*4);
            std::cout.write((char *)nv12Output.words.data(),bytes);std::cout.flush();
        }
    } catch (const std::exception &error) { std::cerr<<error.what();return 5; }
}
