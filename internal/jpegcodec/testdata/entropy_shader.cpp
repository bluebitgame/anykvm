// Run the actual entropy/index/packing HLSL with checked CPU buffers.
#include <algorithm>
#include <cstdint>
#include <iostream>
#include <vector>
#define _Static_assert static_assert
#include "../../gpuconv/jpeg_layout.h"
#undef _Static_assert
using uint=uint32_t;
using std::min;
using std::max;
using std::clamp;
struct uint3 {uint x,y,z;};
struct uint2 {uint x=0,y=0;};
uint2 outputSize;
struct Buffer {
    std::vector<uint> words;
    std::vector<bool> forbidden;
    void check(uint offset) {if(!forbidden.empty() && forbidden.at(offset/BK_JPEG_SEGMENT_CAPACITY)) throw std::runtime_error("solid MCU entropy accessed");}
    uint Load(uint offset) {check(offset);return words.at(offset/4);}
    void Store(uint offset,uint value) {check(offset);words.at(offset/4)=value;}
} entropySegments,jpegOutput,jpegHeader,dispatchArguments;
struct uint4 {uint x=0,y=0,z=0,w=0;};
uint4 tileState;
std::vector<uint> dirtyTiles;
uint huffmanCodes[768][4],mcuCount,jpegHeaderBytes,mcuColumns=1;
std::vector<uint> outputCoefficients;
struct CheckedInfo : std::vector<uint> {
    std::vector<bool> allowed;
    bool check=false;
    uint &operator[](size_t index) {
        if(check && index<mcuCount*2 && !allowed.at(index/2)) throw std::runtime_error("clean MCU metadata accessed");
        uint solidBase=mcuCount*2+1+bk_jpeg_scan_groups(mcuCount)*2;
        if(check && index>=solidBase && index<solidBase+mcuCount && !allowed.at(index-solidBase)) throw std::runtime_error("clean MCU color accessed");
        return at(index);
    }
} segmentInfo;
uint64_t coefficientLoads=0;
uint64_t huffmanWrites=0;
int firstbithigh(uint value) {return value?31-__builtin_clz(value):-1;}
int firstbitlow(uint value) {return value?__builtin_ctz(value):-1;}
#include "index_shader_threads.h"
void InterlockedAdd(uint &destination,uint value,uint &old) {
    std::lock_guard<std::mutex> lock(scanAtomicMutex);old=destination;destination+=value;
}
#include "entropy_shader.hpp"
int main() {
    try {
        for (;;) {
            std::cin.read((char *)&mcuCount,4);
            if (std::cin.eof()) return 0;
            std::cin.read((char *)&jpegHeaderBytes,4);
            std::cin.read((char *)huffmanCodes,sizeof(huffmanCodes));
            jpegHeader.words.assign((jpegHeaderBytes+3)/4,0);
            std::cin.read((char *)jpegHeader.words.data(),jpegHeaderBytes);
            outputCoefficients.assign(mcuCount*MCU_WORDS,0);
            std::vector<uint> mcuLoads(mcuCount,BLOCKS_PER_MCU);
            std::vector<bool> constants(mcuCount,true);
            std::vector<int> firstY(mcuCount);
            for (uint block=0;block<mcuCount*BLOCKS_PER_MCU;block++) {
                uint offset=block*COEFFICIENT_WORDS;
                for(uint word=0;word<COEFFICIENT_WORDS;word++) {
                    int32_t values[2];std::cin.read((char *)values,8);
                    outputCoefficients[offset+word]=uint(uint16_t(values[0]))|(uint(uint16_t(values[1]))<<16);
                }
                int dc=int16_t(outputCoefficients[offset]&0xffff);
                if(block%6==0)firstY[block/6]=dc;
                else if(block%6<4 && dc!=firstY[block/6])constants[block/6]=false;
                auto headerBytes=reinterpret_cast<const unsigned char *>(jpegHeader.words.data());
                int dequantized=dc*headerBytes[block%6<4?7:72];
                if(dequantized < -2048 || dequantized > 2047)constants[block/6]=false;
                for(uint scan=1;scan<BLOCK_COEFFICIENTS;scan++) {
                    if(loadCoefficient(offset,zigzag[scan])!=0) {
                        outputCoefficients[acMaskOffset(offset)+scan/32]|=1u<<(scan&31);
                        mcuLoads[block/BLOCKS_PER_MCU]++;
                        constants[block/BLOCKS_PER_MCU]=false;
                    }
                }
            }
            if (!std::cin || !mcuCount) return 2;
            entropySegments.words.assign(mcuCount*SEGMENT_CAPACITY/4,0);
            entropySegments.forbidden.clear();
            tileState={};
            jpegOutput.words.assign((jpegHeaderBytes+mcuCount*(SEGMENT_CAPACITY+BK_JPEG_REGION_HEADER_BYTES)+8)/4,0);
            segmentInfo.assign(bk_jpeg_segment_info_words(mcuCount),0);dispatchArguments.words.assign(BK_JPEG_ARGUMENT_BYTES/4,0);
            segmentInfo.check=false;
            coefficientLoads=0;
            uint64_t expectedLoads=0;
            for(uint reads:mcuLoads)expectedLoads+=reads;
            for (uint m=0;m<mcuCount;m++) encodeMCUs({m,0,0});
            if(coefficientLoads!=expectedLoads) return 8;
            auto cachedSegments=entropySegments.words;auto cachedInfo=segmentInfo;
            auto cachedCoefficients=outputCoefficients;
            tileState.y=1;mcuColumns=mcuCount;
            dirtyTiles.assign((mcuCount+3)/4,0);
            std::fill(outputCoefficients.begin(),outputCoefficients.end(),0x7fff7fff);
            for(uint m=0;m<mcuCount;m++) encodeMCUs({m,0,0});
            if(entropySegments.words!=cachedSegments||segmentInfo!=cachedInfo||coefficientLoads!=expectedLoads) return 5;
            // One dirty 64px tile rewrites only its own four MCUs.
            uint dirtyMCUs=std::min(4u,mcuCount),maskBase=mcuCount*BLOCKS_PER_MCU*COEFFICIENT_WORDS;
            std::copy_n(cachedCoefficients.begin(),dirtyMCUs*BLOCKS_PER_MCU*COEFFICIENT_WORDS,outputCoefficients.begin());
            std::copy_n(cachedCoefficients.begin()+maskBase,dirtyMCUs*BLOCKS_PER_MCU*2,outputCoefficients.begin()+maskBase);
            dirtyTiles[0]=1;
            for(uint m=0;m<mcuCount;m++) encodeMCUs({m,0,0});
            for(uint m=0;m<dirtyMCUs;m++)expectedLoads+=mcuLoads[m];
            if(entropySegments.words!=cachedSegments||segmentInfo!=cachedInfo||coefficientLoads!=expectedLoads) return 6;
            tileState.y=0;
            runIndexShaders(jpegWorkCount(),SEGMENT_SCAN_THREADS);uint bytes=segmentInfo[mcuCount*2];
            if (!bytes) return 3;
            for (uint word=0;word<(bytes+3)/4;word++) packJPEG({word,0,0});
            std::cout.write((char *)&bytes,4);std::cout.write((char *)jpegOutput.words.data(),bytes);std::cout.flush();
            // Exercise actual compact indexing/packing for all, zero and the
            // final (possibly clipped) tile. No clean MCU appears in readback.
            tileState.y=1;tileState.z=1;
            for(uint pass=0;pass<4;pass++) {
                std::fill(dirtyTiles.begin(),dirtyTiles.end(),pass==0?1:0);
                if(pass==2) dirtyTiles.back()=1;
                if(pass==3) for(uint tile=0;tile<dirtyTiles.size();tile++) dirtyTiles[tile]=(tile%2==0);

                outputSize={mcuCount*16,16};
                uint tiles=(mcuCount+3)/4;
                auto flags=dirtyTiles;
                dirtyTiles.resize(tiles+3+3*mcuCount);
                uint work=0;
                segmentInfo.allowed.assign(mcuCount,false);
                // Reverse the screen order to exercise nondeterministic GPU
                // append order. Segment offsets must follow compact order.
                for(uint i=mcuCount;i>0;i--)if(flags[(i-1)/4]) {
                    dirtyTiles[tiles+1+work++]=i-1;
                    segmentInfo.allowed[i-1]=true;
                }
                dirtyTiles[tiles]=work;
                dirtyTiles[tiles+1+mcuCount]=work;
                std::copy_n(dirtyTiles.begin()+tiles+1,work,dirtyTiles.begin()+tiles+2+mcuCount);
                tileState.w=1;
                segmentInfo.check=true;
                // Restore cached coefficients, then invoke only compact tasks.
                outputCoefficients=cachedCoefficients;
                entropySegments.forbidden=constants;
                dirtyTiles[jpegEntropyBase()]=0;
                // Concurrent selector lanes must append each textured MCU once;
                // constants are retained only by the readback queue.
                std::vector<std::thread> selectors;
                for(uint lane=0;lane<64;lane++)selectors.emplace_back([=]{
                    for(uint slot=lane;slot<((work+63)/64)*64;slot+=64)selectEntropy({slot,0,0});
                });
                for(auto &lane:selectors)lane.join();
                prepareEntropy({0,0,0});
                std::vector<bool> queued(mcuCount,false);
                for(uint slot=0;slot<jpegEntropyCount();slot++) {
                    uint source=jpegEntropyMCU(slot);
                    if(source>=mcuCount || constants[source] || queued[source])return 11;
                    queued[source]=true;
                    encodeMCUs({slot,0,0});
                }
                for(uint m=0;m<mcuCount;m++)if(queued[m]!=(segmentInfo.allowed[m]&&!constants[m]))return 12;
                if(dispatchArguments.words[8]!=(jpegEntropyCount()+63)/64)return 13;
                // An extra padded lane must return before fetching a work ID.
                encodeMCUs({jpegEntropyCount(),0,0});
                runIndexShaders(jpegWorkCount(),SEGMENT_SCAN_THREADS);bytes=segmentInfo[mcuCount*2];
                if(work==0 && dispatchArguments.words[0]!=0)return 10;
                if(bytes<4 || (pass==1 && bytes!=4)) return 7;
                for(uint word=0;word<(bytes+3)/4;word++) packJPEG({word,0,0});
                std::cout.write((char *)&bytes,4);std::cout.write((char *)jpegOutput.words.data(),bytes);std::cout.flush();
                segmentInfo.check=false;tileState.w=0;dirtyTiles=flags;
                entropySegments.forbidden.clear();
            }

        }
    } catch (const std::exception &error) {std::cerr<<error.what();return 4;}
}
