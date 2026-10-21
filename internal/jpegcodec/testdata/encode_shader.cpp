// The texture addressing is unchanged; feed BGRA samples directly to the actual
// shared chroma/DCT shader and compare its coefficients with the Go oracle.
#include <algorithm>
#include <cmath>
#include <condition_variable>
#include <cstdint>
#include <iostream>
#include <mutex>
#include <thread>
#include <vector>
using uint=uint32_t;
using std::clamp;using std::floor;
struct uint2 {
    uint x,y;
    uint2(uint x,uint y):x(x),y(y) {}
};
struct uint3 { uint x,y,z; };
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
uint width,height,mcuColumns;
float quantizationReciprocals[32][4];
std::vector<uint> outputCoefficients;
float3 loadRGB(uint2 p) {
    uint x=std::min(p.x,width-1),y=std::min(p.y,height-1);
    return {float(uint8_t(x*2+y*3)),float(uint8_t(x+y*2)),float(uint8_t(x*3+y))};
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
#include "encode_shader.hpp"
int main() {
    std::cin.read((char *)&width,4);std::cin.read((char *)&height,4);
    uint quantization[128];
    std::cin.read((char *)quantization,sizeof(quantization));
    for (uint i=0;i<128;i++) quantizationReciprocals[i/4][i%4]=1.0f/quantization[i];
    if (!std::cin || !width || !height) return 2;
    mcuColumns=(width+15)/16;
    uint rows=(height+15)/16;
    outputCoefficients.resize(mcuColumns*rows*192);
    for (uint y=0;y<rows;y++) for (uint x=0;x<mcuColumns;x++) for (uint block=0;block<5;block++) {
        std::vector<std::thread> threads;
        for (uint lane=0;lane<64;lane++) threads.emplace_back([=]{transformMain({x,y,block},{lane%8,lane/8,0});});
        for (auto &thread:threads) thread.join();
    }
    for (uint packed:outputCoefficients) {
        int values[2]={int(int16_t(packed & 0xffff)),int(int16_t(packed >> 16))};
        std::cout.write((char *)values,sizeof(values));
    }
}
