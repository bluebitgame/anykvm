// Run the actual entropy/index/packing HLSL with checked CPU buffers.
#include <algorithm>
#include <cstdint>
#include <iostream>
#include <vector>
using uint=uint32_t;
using std::min;
struct uint3 {uint x,y,z;};
struct Buffer {
    std::vector<uint> words;
    uint Load(uint offset) {return words.at(offset/4);}
    void Store(uint offset,uint value) {words.at(offset/4)=value;}
} entropySegments,jpegOutput,jpegHeader,dispatchArguments;
uint huffmanCodes[768][4],mcuCount,jpegHeaderBytes;
std::vector<uint> outputCoefficients,segmentInfo;
int firstbithigh(uint value) {return value?31-__builtin_clz(value):-1;}
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
            outputCoefficients.resize(mcuCount*192);
            for (uint i=0;i<mcuCount*192;i++) {
                int32_t values[2];std::cin.read((char *)values,8);
                outputCoefficients[i]=uint(uint16_t(values[0]))|(uint(uint16_t(values[1]))<<16);
            }
            if (!std::cin || !mcuCount) return 2;
            entropySegments.words.assign(mcuCount*SEGMENT_CAPACITY/4,0);
            jpegOutput.words.assign((jpegHeaderBytes+mcuCount*SEGMENT_CAPACITY+8)/4,0);
            segmentInfo.assign(mcuCount*2+1,0);dispatchArguments.words.assign(4,0);
            for (uint m=0;m<mcuCount;m++) encodeMCUs({m,0,0});
            indexSegments({0,0,0});uint bytes=segmentInfo[mcuCount*2];
            if (!bytes) return 3;
            for (uint word=0;word<(bytes+3)/4;word++) packJPEG({word,0,0});
            std::cout.write((char *)&bytes,4);std::cout.write((char *)jpegOutput.words.data(),bytes);std::cout.flush();
        }
    } catch (const std::exception &error) {std::cerr<<error.what();return 4;}
}
