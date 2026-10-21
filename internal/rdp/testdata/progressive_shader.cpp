// Execute the actual integer HLSL on a CPU to compare against the independent
// Go codec on build hosts without D3D11/Vulkan. This does not emulate a driver.
#include <algorithm>
#include <atomic>
#include <condition_variable>
#include <cstdint>
#include <cstdlib>
#include <cstring>
#include <iostream>
#include <mutex>
#include <stdexcept>
#include <thread>
#include <vector>
using uint=uint32_t;
using std::min; using std::max; using std::clamp;
struct uint3 { uint x,y,z; };
struct ByteBuffer {
    std::vector<uint> words;
    uint Load(uint offset) { return words.at(offset/4); }
    void Store(uint offset,uint value) { words.at(offset/4)=value; }
};
ByteBuffer pixels,metadata,segments,output;
std::vector<int> coefficients;
std::vector<uint> info(112);
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
void InterlockedOr(std::atomic<uint> &value,uint bits) { value.fetch_or(bits); }

ByteBuffer sourcePixels,surfaceParameters,currentPixels,previousPixels;
std::vector<uint> dirtyTiles;
#define __spirv__ 1
#include "surface.hpp"

#include "shader.hpp"

int main() {
    try {
        for (;;) {
            metadata.words.resize(16);
            std::cin.read((char *)metadata.words.data(),64);
            if (std::cin.eof()) return 0;
            if (!std::cin) return 2;
            uint bytes; std::cin.read((char *)&bytes,4);
            pixels.words.resize(bytes/4); std::cin.read((char *)pixels.words.data(),bytes);
            if (!std::cin) return 3;
            if(metadata.words[15]==0x53555246) {
                surfaceParameters.words.assign(metadata.words.begin(),metadata.words.begin()+12);
                uint sourceWords=surfaceParameters.words[2]*surfaceParameters.words[1]/4;
                uint w=surfaceParameters.words[4],h=surfaceParameters.words[5];
                sourcePixels.words.assign(pixels.words.begin(),pixels.words.begin()+sourceWords);
                previousPixels.words.assign(pixels.words.begin()+sourceWords,pixels.words.end());
                currentPixels.words.resize(w*h);dirtyTiles.resize(((w+63)/64)*((h+63)/64));
                for(uint y=0;y<(h+63)/64;y++)for(uint x=0;x<(w+63)/64;x++) {
                    std::vector<std::thread> threads;
                    for(uint lane=0;lane<64;lane++)threads.emplace_back([=]{prepareTile({x,y,0},lane);});
                    for(auto &thread:threads)thread.join();
                }
                uint size=(w*h+(uint)dirtyTiles.size())*4,lengths[48]={w*h*4,(uint)dirtyTiles.size()};
                std::cout.write((char *)&size,4);std::cout.write((char *)lengths,sizeof(lengths));
                std::cout.write((char *)currentPixels.words.data(),w*h*4);
                std::cout.write((char *)dirtyTiles.data(),dirtyTiles.size()*4);std::cout.flush();continue;
            }
            coefficients.resize(metadata.words[5]*12288);
            segments.words.resize(48*32768/4); output.words.resize(48*32768/4);
            for (uint tile=0;tile<metadata.words[0];tile++) {
                for (uint c=0;c<3;c++) {
                    if (metadata.words[4]!=2) {
                        std::vector<std::thread> threads;
                        for (uint lane=0;lane<64;lane++) threads.emplace_back([=]{transformTile({tile,c,0},lane);});
                        for (auto &thread:threads) thread.join();
                    }
                    encodeTiles({tile,c,0});
                }
            }
            indexSegments({0,0,0});
            for (uint segment=0;segment<metadata.words[0]*6;segment++)
                for (uint lane=0;lane<64;lane++) packSegments({segment,0,0},lane);
            if (info[49]||info[48]>48*32768) return 4;
            std::cout.write((char *)&info[48],4);
            std::cout.write((char *)info.data(),48*4);
            std::cout.write((char *)output.words.data(),info[48]); std::cout.flush();
        }
    } catch (const std::exception &error) { std::cerr<<error.what();return 5; }
}
