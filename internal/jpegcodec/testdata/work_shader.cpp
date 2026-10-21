#include <algorithm>
#include <atomic>
#include <cstdint>
#include <iostream>
#include <stdexcept>
#include <thread>
#include <vector>
using uint=uint32_t;
using std::min;
struct uint2 {uint x,y;};
struct uint3 {uint x,y,z;};
struct uint4 {uint x=0,y=0,z=0,w=0;};
uint2 outputSize;
uint4 tileState;
uint mcuColumns,mcuCount;
struct Checked : std::vector<uint> { uint &operator[](size_t i) {return at(i);} } dirtyTiles, segmentInfo;
struct Arguments {
 std::atomic<uint> words[16]{};
 uint Load(uint offset) {return words[offset/4].load();}
 void Store(uint offset,uint value) {words[offset/4].store(value);}
 void InterlockedAdd(uint offset,uint value,uint &original) {original=words[offset/4].fetch_add(value);}
} dispatchArguments;
#include "work_shader.hpp"
int main() {
 try {
  for(auto size:std::vector<uint2>{{1,1},{65,17},{193,131},{256,256},{8191,17}}) {
   outputSize=size;mcuColumns=(size.x+15)/16;mcuCount=mcuColumns*((size.y+15)/16);
   uint tiles=((size.x+63)/64)*((size.y+63)/64);
   for(uint solidPass=0;solidPass<3;solidPass++)for(uint pass=0;pass<4;pass++) {
    dirtyTiles.assign(tiles+3+3*mcuCount,0xdeadbeef);
    segmentInfo.assign(mcuCount*2,0);
    auto solid=[=](uint m){return solidPass==1 || (solidPass==2 && m%3!=0);};
    for(uint m=0;m<mcuCount;m++)if(solid(m))segmentInfo[m*2]=SOLID_MCU_TAG;
    dispatchArguments.Store(16,0);dispatchArguments.Store(56,0);tileState={0,1,1,1};
    auto selected=[=](uint tile){return pass==3 || (pass==1 && tile+1==tiles) || (pass==2 && tile%2==0);};
    std::vector<std::thread> groups;
    for(uint i=tiles;i>0;i--)groups.emplace_back([=]{compareFlag(i-1,selected(i-1));});
    for(auto &group:groups)group.join();
    prepareWork({0,0,0});
    std::vector<bool> seen(mcuCount,false), computed(mcuCount,false);
    uint count=jpegWorkCount(),expected=0,computeCount=jpegComputeCount(),expectedCompute=0;
    for(uint slot=0;slot<computeCount;slot++){
     uint m=jpegComputeMCU(slot);
     if(m>=mcuCount||computed[m]||solid(m))return 5;
     computed[m]=true;
    }
    for(uint slot=0;slot<count;slot++) {
     uint m=jpegWorkMCU(slot);
     if(m>=mcuCount||seen[m])return 1;
     seen[m]=true;
    }
    for(uint m=0;m<mcuCount;m++) {
     uint tile=(m/mcuColumns/4)*((mcuColumns+3)/4)+(m%mcuColumns/4);
     if(seen[m]!=selected(tile))return 2;
     expected+=selected(tile);
     if(computed[m]!=(selected(tile)&&!solid(m)))return 6;
     expectedCompute+=selected(tile)&&!solid(m);
    }
    if(count!=expected || count>mcuCount || computeCount!=expectedCompute || dispatchArguments.Load(20)!=computeCount ||
       dispatchArguments.Load(24)!=1 || dispatchArguments.Load(28)!=5 ||
       dispatchArguments.Load(32)!=(computeCount+63)/64 || dispatchArguments.Load(36)!=1 || dispatchArguments.Load(40)!=1 ||
       dispatchArguments.Load(44)!=(count+SEGMENT_SCAN_THREADS-1)/SEGMENT_SCAN_THREADS || dispatchArguments.Load(48)!=1 || dispatchArguments.Load(52)!=1)return 3;
   }
  }
 } catch(const std::exception &e) {std::cerr<<e.what();return 4;}
}
