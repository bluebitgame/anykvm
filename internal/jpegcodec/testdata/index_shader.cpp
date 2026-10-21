// Compare actual HLSL scans with a serial oracle, including scratch reuse,
// invalid lengths and compact work lists whose order differs from MCU order.
#include <algorithm>
#include <cstdint>
#include <iostream>
#include <stdexcept>
#include <vector>
#define _Static_assert static_assert
#include "../../gpuconv/jpeg_layout.h"
#undef _Static_assert
using uint=uint32_t;
using std::min;
struct uint2 {uint x,y;};
struct uint3 {uint x,y,z;};
struct uint4 {uint x=0,y=0,z=0,w=0;};
uint2 outputSize;
uint4 tileState;
uint mcuCount,mcuColumns,jpegHeaderBytes=623;
std::vector<uint> dirtyTiles;
struct CheckedInfo : std::vector<uint> {
    std::vector<bool> allowed;
    uint &operator[](size_t index) {
        if(index<mcuCount*2 && !allowed.at(index/2)) throw std::runtime_error("clean MCU metadata accessed");
        return at(index);
    }
} segmentInfo;
struct Arguments {
    std::vector<uint> words;
    void Store(uint offset,uint value) {words.at(offset/4)=value;}
} dispatchArguments;
#include "index_shader_threads.h"
#include "index_shader.hpp"

enum Mode {FullJPEG, SparseRecords, CompactRecords};

bool check(uint count,Mode mode,uint pattern,int invalid=-1,uint badLength=0) {
    mcuCount=mcuColumns=count;outputSize={count*16,16};
    tileState={0,1,mode!=FullJPEG,mode==CompactRecords};
    uint tiles=(count+3)/4;
    dirtyTiles.assign(tiles+1+count,0);
    for(uint tile=0;tile<tiles;tile++) {
        dirtyTiles[tile]=pattern==1 || pattern==4 || (pattern==2 && (tile==0 || tile+1==tiles)) || (pattern==3 && tile%2==0);
    }
    std::vector<uint> work;
    for(uint mcu=0;mcu<count;mcu++) if(mode!=CompactRecords || dirtyTiles[mcu/4]) work.push_back(mcu);
    if(mode==CompactRecords) {
        std::reverse(work.begin(),work.end());
        if(work.size()>37) std::rotate(work.begin(),work.begin()+37,work.end());
        dirtyTiles[tiles]=work.size();
        std::copy(work.begin(),work.end(),dirtyTiles.begin()+tiles+1);
    }
    segmentInfo.assign(bk_jpeg_segment_info_words(count),0xdeadbeef);
    segmentInfo.allowed.assign(count,mode!=CompactRecords);
    for(uint mcu:work) segmentInfo.allowed[mcu]=true;
    std::vector<uint> lengths(count),offsets(count,0xdeadbeef);
    // Unselected records may have no cached length and must not cause failure.
    for(uint mcu=0;mcu<count;mcu++) {
        lengths[mcu]=(mode!=FullJPEG && !dirtyTiles[mcu/4])?0:1+(mcu*67)%SEGMENT_CAPACITY;
        if(mode!=FullJPEG && pattern==4 && mcu%3==0) lengths[mcu]=BK_JPEG_SOLID_MCU_TAG|((mcu%2)?BK_JPEG_SOLID_DC_HIGH:0);
        segmentInfo.at(mcu*2)=lengths[mcu];
    }
    if(invalid>=0) segmentInfo.at(uint(invalid)*2)=lengths[invalid]=badLength;
    uint expected=mode==FullJPEG?jpegHeaderBytes:4;
    bool failed=false;
    for(uint mcu:work) {
        bool selected=mode==FullJPEG || dirtyTiles[mcu/4];
        uint length=lengths[mcu];
        offsets[mcu]=expected;
        if(selected) {
            bool constant=mode!=FullJPEG && (length&BK_JPEG_SOLID_MCU_TAG)!=0;
            if(constant) {
                if(length & ~(BK_JPEG_SOLID_MCU_TAG|BK_JPEG_SOLID_DC_HIGH)) failed=true;
                else expected+=BK_JPEG_REGION_HEADER_BYTES;
            } else if(length==0 || length>SEGMENT_CAPACITY) failed=true;
            else expected+=mode==FullJPEG?length:BK_JPEG_REGION_HEADER_BYTES+((length+3)&~3u);
        }
    }
    expected=failed?0:expected+(mode==FullJPEG?2:0);
    dispatchArguments.words.assign(BK_JPEG_ARGUMENT_BYTES/4,0xdeadbeef);
    runIndexShaders(jpegWorkCount(),SEGMENT_SCAN_THREADS);
    if(segmentInfo.at(count*2)!=expected || dispatchArguments.words[3]!=expected) return false;
    for(uint i=count*2+1+bk_jpeg_scan_groups(count)*2;i<segmentInfo.size();i++) {
        if(segmentInfo.at(i)!=0xdeadbeef) return false; // Scans must preserve resident colors.
    }
    for(uint mcu=0;mcu<count;mcu++) {
        if(segmentInfo.at(mcu*2)!=lengths[mcu]) return false;
        // Failed frames do not publish usable offsets, but cached lengths survive.
        if(!failed && segmentInfo.at(mcu*2+1)!=offsets[mcu]) return false;
    }
    uint packGroups=(mode==CompactRecords && work.empty())?0:(expected+255)/256;
    return dispatchArguments.words[0]==min(packGroups,1024u) &&
           dispatchArguments.words[1]==(packGroups+1023)/1024 && dispatchArguments.words[2]==1;
}

int main() {
    if(SEGMENT_SCAN_THREADS!=BK_JPEG_SCAN_THREADS) return 1;
    for(uint count:{1u,255u,256u,257u,513u,BK_JPEG_STORAGE_LIMIT/BK_JPEG_SEGMENT_CAPACITY}) {
        if(!check(count,FullJPEG,0)) {std::cerr<<"full JPEG count "<<count;return 2;}
    }
    for(Mode mode:{SparseRecords,CompactRecords}) for(uint pattern=0;pattern<5;pattern++) {
        if(!check(513,mode,pattern)) {std::cerr<<"records mode "<<mode<<" pattern "<<pattern;return 3;}
    }
    for(int mcu:{0,256,512}) for(uint length:{0u,SEGMENT_CAPACITY+1,0xffffffffu}) {
        if(!check(513,FullJPEG,0,mcu,length) || !check(513,CompactRecords,1,mcu,length)) {
            std::cerr<<"invalid MCU "<<mcu<<" length "<<length;return 4;
        }
    }
    // Reusing scratch after a failure must not carry the error into a new frame.
    if(!check(257,FullJPEG,0) || !check(513,CompactRecords,0)) return 5;
}
