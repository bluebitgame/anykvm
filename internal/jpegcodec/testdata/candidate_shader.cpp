#include <atomic>
#include <condition_variable>
#include <cstdint>
#include <mutex>
#include <thread>
#include <vector>
using uint=uint32_t;
struct uint2 {uint x,y;};
struct uint3 {uint x,y,z;uint3(uint x,uint y,uint z):x(x),y(y),z(z){}};
struct uint4 {uint x=0,y=0,z=0,w=0;};
uint2 outputSize={193,131};uint4 tileState;
uint mcuColumns=13,mcuCount=13*9;
struct Checked:std::vector<uint>{uint &operator[](size_t i){return at(i);}} dirtyTiles;
std::vector<uint> source(193*131),previous(193*131,0),flags(12,77);
std::atomic<uint> samples{0};uint force=0;
uint compareWidth(){return outputSize.x;}uint compareHeight(){return outputSize.y;}uint compareForce(){return force;}
uint comparePixel(uint x,uint y){samples++;return source[y*outputSize.x+x];}
uint comparePrevious(uint offset){return previous[offset/4];}
void compareStore(uint offset,uint pixel){previous[offset/4]=pixel;}
void compareFlag(uint tile,uint dirty){flags.at(tile)=dirty;}
class Barrier{
 std::mutex mutex;std::condition_variable changed;unsigned count=0,generation=0;
 public:void wait(){std::unique_lock<std::mutex> lock(mutex);unsigned old=generation;
 if(++count==64){count=0;generation++;changed.notify_all();}else changed.wait(lock,[&]{return generation!=old;});}
} barrier;
void GroupMemoryBarrierWithGroupSync(){barrier.wait();}
void InterlockedOr(std::atomic<uint> &a,uint value){a.fetch_or(value);}
#include "candidate_shader.hpp"
void candidates(std::vector<uint> tiles){
 uint base=jpegEntropyBase()+1+mcuCount;
 dirtyTiles.resize(base+tiles.size());
 for(uint i=0;i<tiles.size();i++)dirtyTiles[base+i]=tiles[i];
 tileState.w=3;
 for(uint slot=0;slot<tiles.size();slot++){
  std::vector<std::thread> lanes;
  for(uint lane=0;lane<64;lane++)lanes.emplace_back([=]{compareTile({slot,0,0},lane);});
  for(auto &lane:lanes)lane.join();
 }
}
int main(){
 // Dirty data outside candidate tiles must neither be sampled nor overwrite
 // the resident baseline. Only the sparse group mapping may pick screen tiles.
 source[1]=7;source[65*193+65]=9;source[130*193+192]=11;
 candidates({5});if(samples!=4096||flags[5]!=1||previous[65*193+65]!=9||previous[1]!=0||previous.back()!=0)return 1;
 candidates({11});if(samples!=4099||flags[11]!=1||previous.back()!=11)return 2;
 samples=0;candidates({});if(samples!=0||previous[1]!=0)return 3;
 candidates({5,11});if(samples!=4099||flags[5]!=0||flags[11]!=0)return 4;
 for(uint i=0;i<flags.size();i++)if(i!=5&&i!=11&&flags[i]!=77)return 5;
 return 0;
}
