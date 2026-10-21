// Five groups encode one 4:2:0 MCU: four Y blocks and one Cb/Cr pair.
// group.xy selects the MCU; group.z selects Y00, Y10, Y01, Y11 or chroma.
static const uint BLOCK_SIZE = 8;
static const uint BLOCK_COEFFICIENTS = BLOCK_SIZE * BLOCK_SIZE;
static const uint COEFFICIENT_WORDS = BLOCK_COEFFICIENTS / 2;
static const uint MCU_SIZE = 16;
static const uint BLOCKS_PER_MCU = 6;
static const uint MCU_WORDS = BLOCKS_PER_MCU * (COEFFICIENT_WORDS + 2);
static const uint LUMA_BLOCKS_PER_MCU = 4;
static const uint CB_BLOCK = 4;
static const uint CR_BLOCK = 5;
static const uint LUMA_DC_TABLE = 0;
static const uint CHROMA_DC_TABLE = 2;
static const uint PACK_THREADS = 64;
static const uint WORD_BYTES = 4;
static const uint SEGMENT_SCAN_THREADS = 256;
static const uint REGION_HEADER_BYTES = 12;
static const uint SOLID_COLOR_FLAG = 1u << 24;
static const uint SOLID_MCU_TAG = 1u << 31;
static const uint SOLID_DC_HIGH = 1u << 30;

static const uint zigzag[64] = {
    0, 1, 8, 16, 9, 2, 3, 10, 17, 24, 32, 25, 18, 11, 4, 5,
    12, 19, 26, 33, 40, 48, 41, 34, 27, 20, 13, 6, 7, 14, 21, 28,
    35, 42, 49, 56, 57, 50, 43, 36, 29, 22, 15, 23, 30, 37, 44, 51,
    58, 59, 52, 45, 38, 31, 39, 46, 53, 60, 61, 54, 47, 55, 62, 63
};
static const uint coefficientScanIndex[64] = {
    0, 1, 5, 6, 14, 15, 27, 28,
    2, 4, 7, 13, 16, 26, 29, 42,
    3, 8, 12, 17, 25, 30, 41, 43,
    9, 11, 18, 24, 31, 40, 44, 53,
    10, 19, 23, 32, 39, 45, 52, 54,
    20, 22, 33, 38, 46, 51, 55, 60,
    21, 34, 37, 47, 50, 56, 59, 61,
    35, 36, 48, 49, 57, 58, 62, 63
};

#ifdef __spirv__
#define BINDING(index) [[vk::binding(index, 0)]]
#else
#define BINDING(index)
#endif

BINDING(0) Texture2D<float4> sourceTexture : register(t0);
BINDING(1) RWStructuredBuffer<uint> outputCoefficients : register(u0);
BINDING(3) RWByteAddressBuffer entropySegments : register(u1);
BINDING(4) RWStructuredBuffer<uint> segmentInfo : register(u2);
BINDING(5) RWByteAddressBuffer jpegOutput : register(u3);
BINDING(6) ByteAddressBuffer jpegHeader : register(t1);
BINDING(7) RWByteAddressBuffer dispatchArguments : register(u4);
BINDING(8) RWByteAddressBuffer previousPixels : register(u5);
BINDING(9) RWStructuredBuffer<uint> dirtyTiles : register(u6);

// Must match bk_jpeg_parameters in gpuconv/jpeg_layout.h.
BINDING(2) cbuffer Parameters : register(b0) {
    uint2 sourceSize;
    uint2 outputSize;
    uint rotationDegrees;
    uint mcuColumns;
    uint jpegHeaderBytes;
    uint mcuCount;
    uint2 cropOrigin;
    uint2 cropSize;
    // Natural-order inverse Y/CbCr quantizers with AAN scaling folded in.
    // float4 avoids constant-buffer array padding.
    float4 quantizationReciprocals[32];
    // Standard DC/AC tables, then two 1024-entry combined small-AC tables.
    uint4 huffmanCodes[768];
    // x forces initialization; y enables incremental encoding; z selects compact
    // changed-MCU records; w enables compact indirect MCU scheduling. Kept last so
    // the existing quantizer/Huffman ABI offsets remain unchanged.
    uint4 tileState;
};

// MCU tile gate. Geometry changes and any table/quality changes force all tiles.
bool jpegMCUDirty(uint mcu) {
    if (tileState.y == 0) return true;
    uint tile = (mcu / mcuColumns / 4) * ((mcuColumns + 3) / 4) + (mcu % mcuColumns / 4);
    return dirtyTiles[tile] != 0;
}

// Dirty flags are followed by three count/MCU-ID queues: changed MCUs for
// record packing, DCT MCUs, then Huffman MCUs. Candidates follow all three.
uint jpegWorkBase() { return ((outputSize.x + 63) / 64) * ((outputSize.y + 63) / 64); }
uint jpegWorkCount() { return tileState.w != 0 ? dirtyTiles[jpegWorkBase()] : mcuCount; }
uint jpegWorkMCU(uint slot) { return tileState.w != 0 ? dirtyTiles[jpegWorkBase() + 1 + slot] : slot; }
uint jpegComputeBase() { return jpegWorkBase() + 1 + mcuCount; }
uint jpegComputeCount() { return tileState.w != 0 ? dirtyTiles[jpegComputeBase()] : mcuCount; }
uint jpegComputeMCU(uint slot) { return tileState.w != 0 ? dirtyTiles[jpegComputeBase() + 1 + slot] : slot; }
uint jpegEntropyBase() { return jpegComputeBase() + 1 + mcuCount; }
uint jpegEntropyCount() { return tileState.w != 0 ? dirtyTiles[jpegEntropyBase()] : mcuCount; }
uint jpegEntropyMCU(uint slot) { return tileState.w != 0 ? dirtyTiles[jpegEntropyBase() + 1 + slot] : slot; }

// Keep coefficient blocks 128-byte aligned; masks occupy a separate tail.
uint acMaskOffset(uint blockOffset) {
    return mcuCount * BLOCKS_PER_MCU * COEFFICIENT_WORDS + (blockOffset / COEFFICIENT_WORDS) * 2;
}

// Shared DCT workspace.
groupshared float blockSamples[BLOCK_COEFFICIENTS];
groupshared float rowCoefficients[BLOCK_COEFFICIENTS];
groupshared float crSamples[BLOCK_COEFFICIENTS];
groupshared float crRowCoefficients[BLOCK_COEFFICIENTS];
// Low/high scan masks for Y or Cb, then low/high for the paired Cr block.
groupshared uint nonzeroAC[4];

void markNonzeroAC(uint coefficientIndex, int value, inout uint lowMask, inout uint highMask) {
    if (coefficientIndex == 0 || value == 0) return;
    uint scanIndex = coefficientScanIndex[coefficientIndex];
    uint bit = 1u << (scanIndex & 31);
    if (scanIndex < 32) lowMask |= bit;
    else highMask |= bit;
}

uint2 sourcePosition(uint2 outputPosition) {
    // Extend incomplete edge blocks by repeating the last output pixel.
    outputPosition = min(outputPosition, outputSize - 1);
    bool swapsAxes = rotationDegrees == 90 || rotationDegrees == 270;
    uint2 displayedSize = swapsAxes ? cropSize.yx : cropSize;

    // Nearest-neighbor scaling, sampled at output pixel centers.
    float2 scaledPosition = (float2(outputPosition) + 0.5) * float2(displayedSize) / float2(outputSize);
    uint2 displayedPosition = min(uint2(scaledPosition), displayedSize - 1);

    // Undo display rotation to address the unrotated source texture.
    if (rotationDegrees == 90) {
        return cropOrigin + uint2(displayedPosition.y, displayedSize.x - 1 - displayedPosition.x);
    }
    if (rotationDegrees == 180) {
        return cropOrigin + displayedSize - 1 - displayedPosition;
    }
    if (rotationDegrees == 270) {
        return cropOrigin + uint2(displayedSize.y - 1 - displayedPosition.y, displayedPosition.x);
    }
    return cropOrigin + displayedPosition;
}

float3 loadRGB(uint2 outputPosition) {
    return sourceTexture.Load(int3(sourcePosition(outputPosition), 0)).rgb * 255.0;
}

uint compareWidth() { return outputSize.x; }
uint compareHeight() { return outputSize.y; }
uint compareForce() { return tileState.x; }
uint comparePixel(uint x,uint y) {
    // Sample exactly the same scaled/rotated/cropped RGB pixels as the DCT.
    uint3 rgb=uint3(floor(loadRGB(uint2(x,y))+0.5));
    return rgb.x|(rgb.y<<8)|(rgb.z<<16);
}
uint comparePrevious(uint offset) { return previousPixels.Load(offset); }
void compareStore(uint offset,uint pixel) { previousPixels.Store(offset,pixel); }
void compareFlag(uint index,uint dirty) {
    dirtyTiles[index]=dirty;
    if (tileState.w == 0 || dirty == 0) return;
    uint tileColumns = (outputSize.x + 63) / 64;
    uint x = (index % tileColumns) * 4, y = (index / tileColumns) * 4;
    uint columns = min(4u, mcuColumns - x);
    uint rows = min(4u, (mcuCount / mcuColumns) - y);
    uint computeCount = 0;
    for (uint row = 0; row < rows; row++) {
        for (uint column = 0; column < columns; column++) {
            uint mcu = (y + row) * mcuColumns + x + column;
            if (tileState.z == 0 || (segmentInfo[mcu * 2] & SOLID_MCU_TAG) == 0) computeCount++;
        }
    }
    uint first, computeFirst = 0;
    dispatchArguments.InterlockedAdd(16, columns * rows, first);
    if (computeCount != 0) dispatchArguments.InterlockedAdd(56, computeCount, computeFirst);
    for (uint row = 0; row < rows; row++) {
        for (uint column = 0; column < columns; column++) {
            uint mcu = (y + row) * mcuColumns + x + column;
            dirtyTiles[jpegWorkBase() + 1 + first + row * columns + column] = mcu;
            if (tileState.z == 0 || (segmentInfo[mcu * 2] & SOLID_MCU_TAG) == 0) {
                dirtyTiles[jpegComputeBase() + 1 + computeFirst++] = mcu;
            }
        }
    }
}

// JPEG solid comparison hooks. A lane owns one column of each 16x16 MCU.
// Keep its first RGB and vertical mismatch locally, then reduce across columns.
// These hooks reuse comparePixel's load; Progressive compiles them out entirely.
struct JPEGCompareState { uint first[4]; uint different; };
groupshared uint jpegFirstRGB[16];
groupshared uint jpegDifferentRGB[16];
int quantizeCoefficient(float coefficient, uint blockIndex, uint coefficientIndex);
bool storeSolidDC(uint mcu, int yDC, int cbDC, int crDC);

void jpegCompareBegin(inout JPEGCompareState state, uint lane) {
    state.different = 0;
    for (uint b = 0; b < 4; b++) state.first[b] = 0;
    if (tileState.z != 0 && lane < 16) jpegDifferentRGB[lane] = 0;
}
void jpegCompareSample(inout JPEGCompareState state, uint x, uint y, uint pixel) {
    if (tileState.z == 0) return;
    uint band = (y & 63u) / 16;
    if ((y & 15u) == 0) state.first[band] = pixel & 0xffffffu;
    else if (state.first[band] != (pixel & 0xffffffu)) state.different |= 1u << band;
}
void jpegCompareEnd(JPEGCompareState state, uint3 group, uint lane, uint dirty) {
    // Uniform group-wide branch: every lane reaches both barriers or none do.
    if (tileState.z == 0 || dirty == 0) return;
    uint x = group.x * 64 + lane;
    for (uint b = 0; b < 4; b++) {
        if (x >= outputSize.x || group.y * 64 + b * 16 >= outputSize.y) continue;
        uint index = b * 4 + lane / 16;
        if ((lane & 15u) == 0) jpegFirstRGB[index] = state.first[b];
        if ((state.different & (1u << b)) != 0) InterlockedOr(jpegDifferentRGB[index], 1u);
    }
    GroupMemoryBarrierWithGroupSync();
    for (uint b = 0; b < 4; b++) {
        if (x >= outputSize.x || group.y * 64 + b * 16 >= outputSize.y) continue;
        uint index = b * 4 + lane / 16;
        if (state.first[b] != jpegFirstRGB[index]) InterlockedOr(jpegDifferentRGB[index], 1u);
    }
    GroupMemoryBarrierWithGroupSync();
    if (lane >= 16) return;
    uint mx = group.x * 4 + lane % 4, my = group.y * 4 + lane / 4;
    if (mx >= mcuColumns || my >= (outputSize.y + 15) / 16) return;
    uint mcu = my * mcuColumns + mx;
    segmentInfo[mcu * 2] = 0; // Clear old solid tags before a textured update.
    if (jpegDifferentRGB[lane] != 0) return;
    uint pixel = jpegFirstRGB[lane];
    float3 rgb = float3(pixel & 255u, (pixel >> 8) & 255u, (pixel >> 16) & 255u);
    // A constant block's AAN DC is exactly 64*sample; all ACs are zero.
    // Reuse the same color operations, reciprocal and signed rounding as DCT.
    int yy = quantizeCoefficient((dot(rgb, float3(0.299, 0.587, 0.114)) - 128.0) * 64.0, 0, 0);
    int cb = quantizeCoefficient(dot(rgb, float3(-0.168736, -0.331264, 0.5)) * 64.0, CB_BLOCK, 0);
    int cr = quantizeCoefficient(dot(rgb, float3(0.5, -0.418688, -0.081312)) * 64.0, CR_BLOCK, 0);
    storeSolidDC(mcu, yy, cb, cr);
}
#define ANYKVM_COMPARE_STATE JPEGCompareState
#define ANYKVM_COMPARE_BEGIN(state,lane) jpegCompareBegin(state,lane)
#define ANYKVM_COMPARE_SAMPLE(state,x,y,pixel) jpegCompareSample(state,x,y,pixel)
// Publish every MCU descriptor before lane zero builds the compute queue.
#define ANYKVM_COMPARE_END(state,group,lane,dirty) jpegCompareEnd(state,group,lane,dirty); DeviceMemoryBarrierWithGroupSync()
// The CPU uploads a sorted list of possible changes from DXGI metadata. Bit 1
// in tileState.w enables its group mapping; bit 0 still selects compact MCUs.
uint3 compareCandidateGroup(uint3 group) {
    if ((tileState.w & 2u) != 0) {
        uint tile=dirtyTiles[jpegEntropyBase()+1+mcuCount+group.x];
        uint columns=(outputSize.x+63)/64;
        return uint3(tile%columns,tile/columns,0);
    }
    return group;
}
#define ANYKVM_COMPARE_GROUP(group) compareCandidateGroup(group)
// ANYKVM_TILE_COMPARE

// Comparison appends work without ordering between tile groups. This small
// stage publishes its count and indirect transform/entropy dispatch sizes.
[numthreads(1,1,1)]
void prepareWork(uint3 threadID : SV_DispatchThreadID) {
    uint count = dispatchArguments.Load(16);
    dirtyTiles[jpegWorkBase()] = count;
    uint computeCount = dispatchArguments.Load(56);
    dirtyTiles[jpegComputeBase()] = computeCount;
    dirtyTiles[jpegEntropyBase()] = 0;
    dispatchArguments.Store(20, computeCount);
    dispatchArguments.Store(24, 1);
    dispatchArguments.Store(28, 5);
    dispatchArguments.Store(32, (computeCount + 63) / 64);
    dispatchArguments.Store(36, 1);
    dispatchArguments.Store(40, 1);
    dispatchArguments.Store(44, (count + SEGMENT_SCAN_THREADS - 1) / SEGMENT_SCAN_THREADS);
    dispatchArguments.Store(48, 1);
    dispatchArguments.Store(52, 1);
}

float lumaSample(uint2 mcuOrigin, uint blockIndex, uint2 samplePosition) {
    uint2 blockOffset = uint2(blockIndex % 2, blockIndex / 2) * BLOCK_SIZE;
    float3 rgb = loadRGB(mcuOrigin + blockOffset + samplePosition);
    return dot(rgb, float3(0.299, 0.587, 0.114)) - 128.0;
}

float2 chromaSamples(uint2 mcuOrigin, uint2 samplePosition) {
    uint2 position = mcuOrigin + samplePosition * 2;
    float3 averageRGB = (loadRGB(position)
        + loadRGB(position + uint2(1, 0))
        + loadRGB(position + uint2(0, 1))
        + loadRGB(position + uint2(1, 1))) * 0.25;
    return float2(dot(averageRGB, float3(-0.168736, -0.331264, 0.5)),
        dot(averageRGB, float3(0.5, -0.418688, -0.081312)));
}

struct DCTResult {
    float values[BLOCK_SIZE];
};

// Eight-point AAN butterfly. Inputs and outputs are in natural order.
// The two-dimensional output scale is removed by quantizationReciprocals.
DCTResult aanDCT(float4 first, float4 last) {
    float sum0 = first.x + last.w, difference0 = first.x - last.w;
    float sum1 = first.y + last.z, difference1 = first.y - last.z;
    float sum2 = first.z + last.y, difference2 = first.z - last.y;
    float sum3 = first.w + last.x, difference3 = first.w - last.x;

    float evenSum0 = sum0 + sum3, evenDifference0 = sum0 - sum3;
    float evenSum1 = sum1 + sum2, evenDifference1 = sum1 - sum2;
    float evenRotation = (evenDifference0 + evenDifference1) * 0.7071067812f;

    float oddSum0 = difference3 + difference2;
    float oddSum1 = difference2 + difference1;
    float oddSum2 = difference1 + difference0;
    float sharedRotation = (oddSum0 - oddSum2) * 0.3826834324f;
    float oddRotation0 = oddSum0 * 0.5411961001f + sharedRotation;
    float oddRotation1 = oddSum1 * 0.7071067812f;
    float oddRotation2 = oddSum2 * 1.3065629649f + sharedRotation;
    float oddPlus = difference0 + oddRotation1;
    float oddMinus = difference0 - oddRotation1;

    DCTResult result;
    result.values[0] = evenSum0 + evenSum1;
    result.values[1] = oddPlus + oddRotation2;
    result.values[2] = evenDifference0 + evenRotation;
    result.values[3] = oddMinus - oddRotation0;
    result.values[4] = evenSum0 - evenSum1;
    result.values[5] = oddMinus + oddRotation0;
    result.values[6] = evenDifference0 - evenRotation;
    result.values[7] = oddPlus - oddRotation2;
    return result;
}

void transformRow(uint row, bool cr) {
    uint start = row * BLOCK_SIZE;
    float4 first, last;
    if (cr) {
        first = float4(crSamples[start], crSamples[start+1], crSamples[start+2], crSamples[start+3]);
        last = float4(crSamples[start+4], crSamples[start+5], crSamples[start+6], crSamples[start+7]);
    } else {
        first = float4(blockSamples[start], blockSamples[start+1], blockSamples[start+2], blockSamples[start+3]);
        last = float4(blockSamples[start+4], blockSamples[start+5], blockSamples[start+6], blockSamples[start+7]);
    }
    DCTResult result = aanDCT(first, last);
    [unroll] for (uint frequency = 0; frequency < BLOCK_SIZE; frequency++) {
        if (cr) crRowCoefficients[start + frequency] = result.values[frequency];
        else rowCoefficients[start + frequency] = result.values[frequency];
    }
}

DCTResult transformColumn(uint column, bool cr) {
    float4 first, last;
    if (cr) {
        first = float4(crRowCoefficients[column], crRowCoefficients[8+column], crRowCoefficients[16+column], crRowCoefficients[24+column]);
        last = float4(crRowCoefficients[32+column], crRowCoefficients[40+column], crRowCoefficients[48+column], crRowCoefficients[56+column]);
    } else {
        first = float4(rowCoefficients[column], rowCoefficients[8+column], rowCoefficients[16+column], rowCoefficients[24+column]);
        last = float4(rowCoefficients[32+column], rowCoefficients[40+column], rowCoefficients[48+column], rowCoefficients[56+column]);
    }
    return aanDCT(first, last);
}

int quantizeCoefficient(float coefficient, uint blockIndex, uint coefficientIndex) {
    uint tableOffset = blockIndex < LUMA_BLOCKS_PER_MCU ? 0 : BLOCK_COEFFICIENTS;
    uint tableIndex = tableOffset + coefficientIndex;
    float reciprocal = quantizationReciprocals[tableIndex / 4][tableIndex % 4];
    int quantized = (int)(sign(coefficient) * floor(abs(coefficient) * reciprocal + 0.5));
    // Baseline JPEG permits 11-bit DC differences and 10-bit AC magnitudes.
    int minimum = coefficientIndex == 0 ? -1024 : -1023;
    return clamp(quantized, minimum, 1023);
}

// One writer owns two adjacent columns, so packed int16 stores never overlap.
// Reuse all eight AAN outputs and merge each writer's masks only once.
void storeColumnPair(uint column, uint blockIndex, uint blockOffset, bool cr) {
    DCTResult first = transformColumn(column, cr);
    DCTResult second = transformColumn(column + 1, cr);
    uint lowMask = 0, highMask = 0;
    [unroll] for (uint frequency = 0; frequency < BLOCK_SIZE; frequency++) {
        uint index = frequency * BLOCK_SIZE + column;
        int low = quantizeCoefficient(first.values[frequency], blockIndex, index);
        int high = quantizeCoefficient(second.values[frequency], blockIndex, index + 1);
        outputCoefficients[blockOffset + index / 2] = (uint(low) & 0xffffu) | ((uint(high) & 0xffffu) << 16);
        markNonzeroAC(index, low, lowMask, highMask);
        markNonzeroAC(index + 1, high, lowMask, highMask);
    }
    uint maskWord = cr ? 2 : 0;
    if (lowMask != 0) InterlockedOr(nonzeroAC[maskWord], lowMask);
    if (highMask != 0) InterlockedOr(nonzeroAC[maskWord + 1], highMask);
}

[numthreads(8, 8, 1)]
void main(uint3 groupID : SV_GroupID, uint3 threadID : SV_GroupThreadID) {
    if (tileState.w != 0) {
        uint mcu = jpegComputeMCU(groupID.x);
        groupID.x = mcu % mcuColumns;
        groupID.y = mcu / mcuColumns;
    }
    uint column = threadID.x;
    uint row = threadID.y;
    uint coefficientIndex = row * BLOCK_SIZE + column;
    uint blockIndex = groupID.z;
    // The Cb group computes both chroma blocks; the Cr group has no work.
    if (blockIndex == CR_BLOCK) return;
    if (tileState.w == 0 && !jpegMCUDirty(groupID.y * mcuColumns + groupID.x)) return;
    if (tileState.z != 0) {
        if ((segmentInfo[(groupID.y * mcuColumns + groupID.x) * 2] & SOLID_MCU_TAG) != 0) return;
    }
    uint2 mcuOrigin = groupID.xy * MCU_SIZE;

    if (coefficientIndex < 4) nonzeroAC[coefficientIndex] = 0;

    // Each thread loads one centered Y, Cb or Cr sample.
    if (blockIndex < LUMA_BLOCKS_PER_MCU) {
        blockSamples[coefficientIndex] = lumaSample(mcuOrigin, blockIndex, threadID.xy);
    } else {
        float2 chroma = chromaSamples(mcuOrigin, threadID.xy);
        blockSamples[coefficientIndex] = chroma.x;
        crSamples[coefficientIndex] = chroma.y;
    }
    GroupMemoryBarrierWithGroupSync();

    // Eight lanes each compute a complete row; the rest wait at the barrier.
    if (coefficientIndex < BLOCK_SIZE) {
        transformRow(coefficientIndex, false);
        if (blockIndex == CB_BLOCK) transformRow(coefficientIndex, true);
    }
    GroupMemoryBarrierWithGroupSync();

    uint mcuIndex = groupID.y * mcuColumns + groupID.x;
    uint blockOffset = (mcuIndex * BLOCKS_PER_MCU + blockIndex) * COEFFICIENT_WORDS;
    // Four lanes write complete column pairs, without an extra shared pass.
    if (coefficientIndex < BLOCK_SIZE / 2) {
        uint pairColumn = coefficientIndex * 2;
        storeColumnPair(pairColumn, blockIndex, blockOffset, false);
        if (blockIndex == CB_BLOCK) {
            storeColumnPair(pairColumn, CR_BLOCK, blockOffset + COEFFICIENT_WORDS, true);
        }
    }
    // All 64 lanes must reach the mask barrier, including idle writers.
    GroupMemoryBarrierWithGroupSync();
    if (coefficientIndex < 2) {
        uint maskOffset = acMaskOffset(blockOffset);
        outputCoefficients[maskOffset + coefficientIndex] = nonzeroAC[coefficientIndex];
        if (blockIndex == CB_BLOCK) {
            outputCoefficients[maskOffset + 2 + coefficientIndex] = nonzeroAC[2 + coefficientIndex];
        }
    }
}

// One restart interval per MCU makes DC prediction and Huffman coding independent.
// Worst case: 6 * (27 DC bits + 63 * 26 AC bits), doubled for FF stuffing,
// plus padding/restart bytes, stays below 3072 bytes per MCU.
static const uint SEGMENT_CAPACITY = 3072;
static const uint PACK_GROUP_WIDTH = 1024;

struct EntropyWriter {
    uint segmentOffset;
    uint byteCount;
    uint pendingBits;
    uint bitCount;
    uint pendingWord;
    uint wordBytes;
    bool failed;
};

void writeByte(inout EntropyWriter writer, uint value) {
    if (writer.byteCount >= SEGMENT_CAPACITY) {
        writer.failed = true;
        return;
    }
    writer.pendingWord |= (value & 0xff) << (writer.wordBytes * 8);
    writer.wordBytes++;
    writer.byteCount++;
    if (writer.wordBytes == WORD_BYTES) {
        entropySegments.Store(writer.segmentOffset + writer.byteCount - WORD_BYTES, writer.pendingWord);
        writer.pendingWord = 0;
        writer.wordBytes = 0;
    }
}

// Append up to three already ordered bytes, issuing a UAV store only when
// a complete word is ready. The caller has checked that no FF needs stuffing.
void writePackedBytes(inout EntropyWriter writer, uint packed, uint count) {
    if (writer.byteCount + count > SEGMENT_CAPACITY) { writer.failed = true; return; }
    uint previousBytes = writer.wordBytes;
    writer.pendingWord |= packed << (previousBytes * 8);
    writer.wordBytes += count;
    writer.byteCount += count;
    if (writer.wordBytes >= WORD_BYTES) {
        writer.wordBytes -= WORD_BYTES;
        entropySegments.Store(writer.segmentOffset + writer.byteCount - writer.wordBytes - WORD_BYTES, writer.pendingWord);
        writer.pendingWord = packed >> ((WORD_BYTES - previousBytes) * 8);
    }
}

void appendBits(inout EntropyWriter writer, uint value, uint count) {
    writer.pendingBits = (writer.pendingBits << count) | value;
    writer.bitCount += count;
    uint bytes = writer.bitCount / 8;
    if (bytes == 0) return;
    writer.bitCount &= 7u;
    uint word = (writer.pendingBits >> writer.bitCount) & ((1u << (bytes * 8)) - 1);
    uint reversed = (word >> 24) | ((word >> 8) & 0xff00u) | ((word << 8) & 0xff0000u) | (word << 24);
    uint packed = reversed >> (32 - bytes * 8);
    uint inverted = ~packed;
    if (((inverted - 0x01010101u) & packed & 0x80808080u) == 0) {
        writePackedBytes(writer, packed, bytes);
    } else {
        for (uint i = 0; i < bytes; i++) {
            uint byteValue = (packed >> (i * 8)) & 255u;
            writeByte(writer, byteValue);
            if (byteValue == 0xff) writeByte(writer, 0);
        }
    }
}

void writeBits(inout EntropyWriter writer, uint value, uint count) {
    if (count == 0) return;
    // A combined Huffman code/amplitude can contain 27 bits. Split long codes
    // so pendingBits never overflows a 32-bit Shader Model 5 register.
    if (count > 24) {
        appendBits(writer, value >> 16, count - 16);
        appendBits(writer, value & 0xffffu, 16);
    } else {
        appendBits(writer, value, count);
    }
}

void writeSymbol(inout EntropyWriter writer, uint table, uint symbol) {
    uint index = table * 256 + symbol;
    uint code = huffmanCodes[index / 4][index % 4];
    if (code == 0) {
        writer.failed = true;
        return;
    }
    writeBits(writer, code & 0xffff, code >> 16);
}

uint magnitudeBitCount(int value) {
    return value == 0 ? 0 : (uint)firstbithigh((uint)abs(value)) + 1;
}

void writeCoefficient(inout EntropyWriter writer, uint table, uint symbol, int value, uint count) {
    uint index = table * 256 + symbol;
    uint code = huffmanCodes[index / 4][index % 4];
    if (code == 0) { writer.failed = true; return; }
    uint amplitude = uint(value < 0 ? value - 1 : value) & ((1u << count) - 1);
    writeBits(writer, ((code & 0xffffu) << count) | amplitude, (code >> 16) + count);
}

int loadCoefficient(uint blockOffset, uint index) {
    uint packed = outputCoefficients[blockOffset + index / 2];
    return (index & 1) != 0 ? int(packed) >> 16 : int(packed << 16) >> 16;
}

bool encodeBlock(inout EntropyWriter writer, uint blockOffset, uint dcTable, inout int previousDC) {
    uint acTable = dcTable + 1;
    int dc = loadCoefficient(blockOffset, 0);
    int difference = dc - previousDC;
    previousDC = dc;
    uint count = magnitudeBitCount(difference);
    writeCoefficient(writer, dcTable, count, difference, count);

    uint maskOffset = acMaskOffset(blockOffset);
    uint low = outputCoefficients[maskOffset];
    uint high = outputCoefficients[maskOffset + 1];
    if ((low | high) == 0) {
        writeSymbol(writer, acTable, 0); // DC-only block: EOB, no AC loads.
        return true;
    }

    uint previousIndex = 0;
    while ((low | high) != 0) {
        uint index;
        if (low != 0) {
            index = (uint)firstbitlow(low);
            low &= low - 1;
        } else {
            index = 32 + (uint)firstbitlow(high);
            high &= high - 1;
        }
        uint zeroRun = index - previousIndex - 1;
        int coefficient = loadCoefficient(blockOffset, zigzag[index]);
        while (zeroRun >= 16) {
            writeSymbol(writer, acTable, 0xf0);
            zeroRun -= 16;
        }
        if (uint(coefficient + 31) < 63) {
            uint codeIndex = 1024 + dcTable * 512 + zeroRun * 64 + (uint(coefficient) & 63u);
            uint code = huffmanCodes[codeIndex / 4][codeIndex % 4];
            writeBits(writer, code & 0x07ffffffu, code >> 27);
        } else {
            count = magnitudeBitCount(coefficient);
            writeCoefficient(writer, acTable, (zeroRun << 4) | count, coefficient, count);
        }
        previousIndex = index;
    }
    if (previousIndex != BLOCK_COEFFICIENTS - 1) writeSymbol(writer, acTable, 0);
    return false;
}

// Color descriptors have a resident tail, separate from dynamic scan totals.
uint jpegSolidOffset(uint mcu) {
    return mcuCount * 2 + 1 + ((mcuCount + SEGMENT_SCAN_THREADS - 1) / SEGMENT_SCAN_THREADS) * 2 + mcu;
}

bool storeSolidDC(uint mcu, int yDC, int cbDC, int crDC) {
    // jpegHeader420's locally generated DQT stores Y[0] at byte 7 and C[0]
    // at byte 72. Read the exact quantizers, avoiding reciprocal float rounding.
    int yQuant = int((jpegHeader.Load(4) >> 24) & 255u);
    int cQuant = int(jpegHeader.Load(72) & 255u);
    int yy = yDC * yQuant, cb = cbDC * cQuant, cr = crDC * cQuant;
    if (min(yy, min(cb, cr)) < -2048 || max(yy, max(cb, cr)) > 2047) return false;
    // Three signed 11-bit DCs are a lossless 33-bit color description. The
    // high Cr bit shares the tagged length word; zero entropy bytes follow.
    segmentInfo[mcu * 2] = SOLID_MCU_TAG | ((uint(crDC) & 0x400u) << 20);
    segmentInfo[jpegSolidOffset(mcu)] = (uint(yDC) & 0x7ffu) | ((uint(cbDC) & 0x7ffu) << 11) | ((uint(crDC) & 0x3ffu) << 22);
    return true;
}

bool omitSolidMCU(uint mcu, uint mcuOffset) {
    // Non-uniform RGB can still become constant after quantization. Preserve
    // the post-DCT optimization for those blocks, without touching their entropy.
    for (uint block = 0; block < BLOCKS_PER_MCU; block++) {
        uint mask = acMaskOffset(mcuOffset + block * COEFFICIENT_WORDS);
        if ((outputCoefficients[mask] | outputCoefficients[mask + 1]) != 0) return false;
    }
    int yy = loadCoefficient(mcuOffset, 0);
    for (uint y = 1; y < LUMA_BLOCKS_PER_MCU; y++) {
        if (loadCoefficient(mcuOffset + y * COEFFICIENT_WORDS, 0) != yy) return false;
    }
    int cb = loadCoefficient(mcuOffset + CB_BLOCK * COEFFICIENT_WORDS, 0);
    int cr = loadCoefficient(mcuOffset + CR_BLOCK * COEFFICIENT_WORDS, 0);
    return storeSolidDC(mcu, yy, cb, cr);
}

// Compact only after all DCT groups have completed. Each lane examines one
// MCU's masks/DCs; no coefficients or entropy are read for RGB-solid MCUs.
[numthreads(64, 1, 1)]
void selectEntropy(uint3 threadID : SV_DispatchThreadID) {
    uint slot = threadID.x;
    if (slot >= jpegComputeCount()) return;
    uint mcu = jpegComputeMCU(slot);
    if (omitSolidMCU(mcu, mcu * BLOCKS_PER_MCU * COEFFICIENT_WORDS)) return;
    uint first;
    InterlockedAdd(dirtyTiles[jpegEntropyBase()], 1u, first);
    dirtyTiles[jpegEntropyBase() + 1 + first] = mcu;
}

[numthreads(1, 1, 1)]
void prepareEntropy(uint3 threadID : SV_DispatchThreadID) {
    dispatchArguments.Store(32, (jpegEntropyCount() + 63) / 64);
}

[numthreads(64, 1, 1)]
void encodeMCUs(uint3 threadID : SV_DispatchThreadID) {
    uint slot = threadID.x;
    if (slot >= jpegEntropyCount()) return;
    uint mcu = jpegEntropyMCU(slot);
    // Keep the bounds guard separate: pre-2021 HLSL logical operators need
    // not short-circuit, so padded dispatch lanes must not read dirty flags.
    if (mcu >= mcuCount) return;
    if (tileState.w == 0 && !jpegMCUDirty(mcu)) {
        return;
    }

    uint mcuOffset = mcu * BLOCKS_PER_MCU * COEFFICIENT_WORDS;
    if (tileState.z != 0) {
        if ((segmentInfo[mcu * 2] & SOLID_MCU_TAG) != 0) return;
        if (tileState.w == 0) {
            if (omitSolidMCU(mcu, mcuOffset)) return;
        }
        segmentInfo[jpegSolidOffset(mcu)] = 0;
    }
    EntropyWriter writer = (EntropyWriter)0;
    writer.segmentOffset = mcu * SEGMENT_CAPACITY;
    int previousY = 0;
    int previousCb = 0;
    int previousCr = 0;
    for (uint block = 0; block < LUMA_BLOCKS_PER_MCU; block++) {
        encodeBlock(writer, mcuOffset + block * COEFFICIENT_WORDS, LUMA_DC_TABLE, previousY);
    }
    encodeBlock(writer, mcuOffset + CB_BLOCK * COEFFICIENT_WORDS, CHROMA_DC_TABLE, previousCb);
    encodeBlock(writer, mcuOffset + CR_BLOCK * COEFFICIENT_WORDS, CHROMA_DC_TABLE, previousCr);

    // Restart intervals end on a byte boundary, padded with one bits.
    if (writer.bitCount != 0) {
        uint paddingBits = 8 - writer.bitCount;
        writeBits(writer, (1u << paddingBits) - 1, paddingBits);
    }
    if (mcu + 1 < mcuCount) {
        writeByte(writer, 0xff);
        writeByte(writer, 0xd0 + (mcu % 8));
    }
    if (writer.wordBytes != 0 && !writer.failed) {
        entropySegments.Store(writer.segmentOffset + writer.byteCount - writer.wordBytes, writer.pendingWord);
    }
    segmentInfo[mcu * 2] = writer.failed ? 0 : writer.byteCount;
}

// segmentInfo: MCU [tagged length,offset], total bytes, group [sum,error], DC word.
// pairs. Group sums become group offsets in the second pass.
uint segmentScanGroups() { return (jpegWorkCount() + SEGMENT_SCAN_THREADS - 1) / SEGMENT_SCAN_THREADS; }
uint segmentGroupInfo(uint group) { return mcuCount * 2 + 1 + group * 2; }

groupshared uint segmentPrefix[SEGMENT_SCAN_THREADS];
groupshared uint segmentTotal;
groupshared uint segmentFailed;

// Work-efficient exclusive Blelloch scan. All lanes participate; padded lanes
// contribute zero. Dispatch boundaries provide synchronization between groups.
void scanSegmentBytes(uint lane, uint bytes, bool failed) {
    segmentPrefix[lane] = bytes;
    if (lane == 0) segmentFailed = 0;
    GroupMemoryBarrierWithGroupSync();
    if (failed) InterlockedOr(segmentFailed, 1u);

    for (uint stride = 1; stride < SEGMENT_SCAN_THREADS; stride *= 2) {
        uint right = (lane + 1) * stride * 2 - 1;
        if (right < SEGMENT_SCAN_THREADS) segmentPrefix[right] += segmentPrefix[right - stride];
        GroupMemoryBarrierWithGroupSync();
    }
    if (lane == 0) {
        segmentTotal = segmentPrefix[SEGMENT_SCAN_THREADS - 1];
        segmentPrefix[SEGMENT_SCAN_THREADS - 1] = 0;
    }
    GroupMemoryBarrierWithGroupSync();
    for (uint stride = SEGMENT_SCAN_THREADS / 2; stride != 0; stride /= 2) {
        uint right = (lane + 1) * stride * 2 - 1;
        if (right < SEGMENT_SCAN_THREADS) {
            uint left = segmentPrefix[right - stride];
            segmentPrefix[right - stride] = segmentPrefix[right];
            segmentPrefix[right] += left;
        }
        GroupMemoryBarrierWithGroupSync();
    }
}

[numthreads(SEGMENT_SCAN_THREADS, 1, 1)]
void indexSegments(uint3 groupID : SV_GroupID, uint3 threadID : SV_GroupThreadID) {
    uint slot = groupID.x * SEGMENT_SCAN_THREADS + threadID.x;
    uint mcu = 0, bytes = 0;
    bool failed = false;
    if (slot < jpegWorkCount()) {
        mcu = jpegWorkMCU(slot);
        uint length = segmentInfo[mcu * 2];
        bool regions = tileState.z != 0;
        bool selected = tileState.w != 0 || !regions || jpegMCUDirty(mcu);
        bool solid = regions && (length & SOLID_MCU_TAG) != 0;
        bool validSolid = solid && (length & ~(SOLID_MCU_TAG | SOLID_DC_HIGH)) == 0;
        failed = selected && (!validSolid && (solid || length == 0 || length > SEGMENT_CAPACITY));
        // Invalid lengths contribute zero so corrupted metadata cannot overflow
        // the scan. The error flag suppresses the entire output in pass two.
        if (selected && !failed) bytes = solid ? REGION_HEADER_BYTES : regions ? REGION_HEADER_BYTES + ((length + 3) & ~3u) : length;
    }
    scanSegmentBytes(threadID.x, bytes, failed);
    if (slot < jpegWorkCount()) segmentInfo[mcu * 2 + 1] = segmentPrefix[threadID.x];
    if (threadID.x == 0) {
        uint info = segmentGroupInfo(groupID.x);
        segmentInfo[info] = segmentTotal;
        segmentInfo[info + 1] = segmentFailed;
    }
}

// The segment-storage limit permits at most 171 groups, fitting this one group.
[numthreads(SEGMENT_SCAN_THREADS, 1, 1)]
void scanSegmentGroups(uint3 threadID : SV_GroupThreadID) {
    uint group = threadID.x, groups = segmentScanGroups();
    uint bytes = 0;
    bool failed = false;
    if (group < groups) {
        uint info = segmentGroupInfo(group);
        bytes = segmentInfo[info];
        failed = segmentInfo[info + 1] != 0;
    }
    scanSegmentBytes(group, bytes, failed);
    bool regions = tileState.z != 0;
    uint base = regions ? 4 : jpegHeaderBytes;
    if (group < groups) segmentInfo[segmentGroupInfo(group)] = base + segmentPrefix[group];
    if (group != 0) return;

    uint byteCount = segmentFailed != 0 ? 0 : base + segmentTotal + (regions ? 0 : 2);
    uint bytesPerGroup = PACK_THREADS * WORD_BYTES;
    uint packGroups = (tileState.w != 0 && jpegWorkCount() == 0) ? 0 : (byteCount + bytesPerGroup - 1) / bytesPerGroup;
    dispatchArguments.Store(0, min(packGroups, PACK_GROUP_WIDTH));
    dispatchArguments.Store(4, (packGroups + PACK_GROUP_WIDTH - 1) / PACK_GROUP_WIDTH);
    dispatchArguments.Store(8, 1);
    dispatchArguments.Store(12, byteCount);
    segmentInfo[mcuCount * 2] = byteCount;
}

[numthreads(SEGMENT_SCAN_THREADS, 1, 1)]
void addSegmentOffsets(uint3 threadID : SV_DispatchThreadID) {
    uint slot = threadID.x;
    if (slot >= jpegWorkCount()) return;
    uint mcu = jpegWorkMCU(slot);
    segmentInfo[mcu * 2 + 1] += segmentInfo[segmentGroupInfo(slot / SEGMENT_SCAN_THREADS)];
}

uint jpegWorkOffset(uint slot) { return segmentInfo[jpegWorkMCU(slot) * 2 + 1]; }

uint findSegment(uint position) {
    uint low = 0;
    uint high = jpegWorkCount();
    while (low + 1 < high) {
        uint middle = (low + high) / 2;
        if (jpegWorkOffset(middle) <= position) {
            low = middle;
        } else {
            high = middle;
        }
    }
    return low;
}

uint loadJPEGByte(uint position, uint totalBytes, inout uint mcu) {
    if (position >= totalBytes) {
        return 0;
    }
    if (tileState.z != 0) {
        if (position < 4) return (mcuCount >> (position * 8)) & 0xff;
        // mcu denotes a compact slot until translated into its source MCU ID.
        while (mcu + 1 < jpegWorkCount()) {
            if (jpegWorkOffset(mcu + 1) > position) break;
            mcu++;
        }
        uint sourceMCU = jpegWorkMCU(mcu);
        uint relative = position - segmentInfo[sourceMCU * 2 + 1];
        uint length = segmentInfo[sourceMCU * 2];
        if (relative < 4) return (sourceMCU >> (relative * 8)) & 0xff;
        if (relative < 8) return (length >> ((relative - 4) * 8)) & 0xff;
        if (relative < REGION_HEADER_BYTES) return (segmentInfo[jpegSolidOffset(sourceMCU)] >> ((relative - 8) * 8)) & 0xff;
        if ((length & SOLID_MCU_TAG) != 0) return 0;
        relative -= REGION_HEADER_BYTES;
        if (relative >= length) return 0;
        return (entropySegments.Load(sourceMCU * SEGMENT_CAPACITY + (relative & ~3u)) >> ((relative & 3) * 8)) & 0xff;
    }
    if (position < jpegHeaderBytes) {
        return (jpegHeader.Load(position & ~3u) >> ((position & 3) * 8)) & 0xff;
    }
    if (position >= totalBytes - 2) {
        return position == totalBytes - 2 ? 0xff : 0xd9; // EOI.
    }
    while (mcu + 1 < mcuCount) {
        if (segmentInfo[(mcu + 1) * 2 + 1] > position) break;
        mcu++;
    }
    uint relative = position - segmentInfo[mcu * 2 + 1];
    uint word = entropySegments.Load(mcu * SEGMENT_CAPACITY + (relative & ~3u));
    return (word >> ((relative & 3) * 8)) & 0xff;
}

// Each invocation owns a complete 32-bit output word, including segment seams.
// Indirect dispatch covers the actual output length. In VNC mode this packs
// changed-MCU records only; it never assembles a full-frame JPEG.
[numthreads(PACK_THREADS, 1, 1)]
void packJPEG(uint3 threadID : SV_DispatchThreadID) {
    uint position = (threadID.y * PACK_GROUP_WIDTH * PACK_THREADS + threadID.x) * WORD_BYTES;
    uint totalBytes = segmentInfo[mcuCount * 2];
    if (position >= totalBytes) {
        return;
    }
    uint mcu = findSegment(position);
    uint packed = 0;
    for (uint byteIndex = 0; byteIndex < WORD_BYTES; byteIndex++) {
        packed |= loadJPEGByte(position + byteIndex, totalBytes, mcu) << (byteIndex * 8);
    }
    jpegOutput.Store(position, packed);
}
