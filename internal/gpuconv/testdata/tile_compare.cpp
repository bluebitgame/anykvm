// Execute the shared production kernel with 64 real lanes and atomic reduction.
#include <algorithm>
#include <atomic>
#include <condition_variable>
#include <cstdint>
#include <mutex>
#include <thread>
#include <vector>
using uint=uint32_t;
struct uint3 {uint x,y,z;};
uint width=129,height=131,force=1;
bool inPlace=false;
std::vector<uint> source(width*height),previous(width*height,0xdeadbeef),current(width*height),flags(9);
uint compareWidth(){return width;}
uint compareHeight(){return height;}
uint compareForce(){return force;}
uint comparePixel(uint x,uint y){return source[y*width+x];}
uint comparePrevious(uint offset){return previous[offset/4];}
void compareStore(uint offset,uint pixel){if(inPlace)previous[offset/4]=pixel;else current[offset/4]=pixel;}
void compareFlag(uint i,uint dirty){flags[i]=dirty;}
class Barrier {
    std::mutex mutex;
    std::condition_variable changed;
    unsigned count=0,generation=0;
public:
    void wait(){std::unique_lock<std::mutex> lock(mutex);unsigned old=generation;
        if(++count==64){count=0;generation++;changed.notify_all();}
        else changed.wait(lock,[&]{return generation!=old;});}
} barrier;
void GroupMemoryBarrierWithGroupSync(){barrier.wait();}
void InterlockedOr(std::atomic<uint> &value,uint bits){value.fetch_or(bits);}
#include "tile_compare.hpp"
void run(){for(uint y=0;y<3;y++)for(uint x=0;x<3;x++){
    std::vector<std::thread> lanes;
    for(uint lane=0;lane<64;lane++)lanes.emplace_back([=]{compareTile({x,y,0},lane);});
    for(auto &lane:lanes)lane.join();
}}
int main(){
    run();if(flags!=std::vector<uint>(9,1)||current!=source)return 1;
    previous=current;force=0;run();if(flags!=std::vector<uint>(9,0))return 2;
    for(auto &pixel:source)pixel^=0xff000000;
    run();if(flags!=std::vector<uint>(9,0))return 3;
    // JPEG's in-place comparison must read previous RGB before overwriting it.
    inPlace=true;source[65*width+65]^=4;source[130*width+128]^=2;
    run();auto expected=std::vector<uint>(9,0);expected[4]=expected[8]=1;
    if(flags!=expected||previous!=source)return 4;
    run();if(flags!=std::vector<uint>(9,0))return 5;
    force=1;run();if(flags!=std::vector<uint>(9,1))return 6;
    return 0;
}
