// Five groups encode one 4:2:0 MCU: four Y blocks and one Cb/Cr pair.
// group.xy selects the MCU; group.z selects Y00, Y10, Y01, Y11 or chroma.
static const uint BLOCK_SIZE = 8;
static const uint BLOCK_COEFFICIENTS = BLOCK_SIZE * BLOCK_SIZE;
static const uint BLOCK_WORDS = BLOCK_COEFFICIENTS / 2;
static const uint MCU_SIZE = 16;
static const uint BLOCKS_PER_MCU = 6;
static const uint LUMA_BLOCKS_PER_MCU = 4;
static const uint CB_BLOCK = 4;
static const uint CR_BLOCK = 5;
static const uint LUMA_DC_TABLE = 0;
static const uint CHROMA_DC_TABLE = 2;
static const uint PACK_THREADS = 64;
static const uint WORD_BYTES = 4;

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
    // Natural-order inverse Y table, then Cb/Cr; float4 avoids array padding.
    float4 quantizationReciprocals[32];
    // Standard DC/AC tables, then two 1024-entry combined small-AC tables.
    uint4 huffmanCodes[768];
};

// Orthonormal DCT basis: dctBasis[frequency][sample].
static const float dctBasis[8][8] = {
    { 0.3535533906, 0.3535533906, 0.3535533906, 0.3535533906, 0.3535533906, 0.3535533906, 0.3535533906, 0.3535533906 },
    { 0.4903926402, 0.4157348062, 0.2777851165, 0.0975451610, -0.0975451610, -0.2777851165, -0.4157348062, -0.4903926402 },
    { 0.4619397663, 0.1913417162, -0.1913417162, -0.4619397663, -0.4619397663, -0.1913417162, 0.1913417162, 0.4619397663 },
    { 0.4157348062, -0.0975451610, -0.4903926402, -0.2777851165, 0.2777851165, 0.4903926402, 0.0975451610, -0.4157348062 },
    { 0.3535533906, -0.3535533906, -0.3535533906, 0.3535533906, 0.3535533906, -0.3535533906, -0.3535533906, 0.3535533906 },
    { 0.2777851165, -0.4903926402, 0.0975451610, 0.4157348062, -0.4157348062, -0.0975451610, 0.4903926402, -0.2777851165 },
    { 0.1913417162, -0.4619397663, 0.4619397663, -0.1913417162, -0.1913417162, 0.4619397663, -0.4619397663, 0.1913417162 },
    { 0.0975451610, -0.2777851165, 0.4157348062, -0.4903926402, 0.4903926402, -0.4157348062, 0.2777851165, -0.0975451610 }
};

groupshared float blockSamples[BLOCK_COEFFICIENTS];
groupshared float rowCoefficients[BLOCK_COEFFICIENTS];
groupshared float crSamples[BLOCK_COEFFICIENTS];
groupshared float crRowCoefficients[BLOCK_COEFFICIENTS];

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

// Mirror symmetry reduces each frequency from eight weighted samples to
// four pairs, shortening the accumulation chain without extra group barriers.
float pairedDCT(float4 first, float4 mirrored, uint frequency) {
    float4 pairs = first + ((frequency & 1) != 0 ? -mirrored : mirrored);
    float4 weights = float4(dctBasis[frequency][0], dctBasis[frequency][1],
        dctBasis[frequency][2], dctBasis[frequency][3]);
    return dot(pairs, weights);
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

// Two signed 16-bit coefficients share each output word. The column pass
// already has all row coefficients in shared memory, so each writer computes
// an adjacent pair without another barrier or overlapping UAV stores.
uint coefficientPair(uint column, uint frequency, uint blockIndex, bool cr) {
    float4 top0, bottom0, top1, bottom1;
    if (cr) {
        top0 = float4(crRowCoefficients[column], crRowCoefficients[8+column], crRowCoefficients[16+column], crRowCoefficients[24+column]);
        bottom0 = float4(crRowCoefficients[56+column], crRowCoefficients[48+column], crRowCoefficients[40+column], crRowCoefficients[32+column]);
        top1 = float4(crRowCoefficients[1+column], crRowCoefficients[9+column], crRowCoefficients[17+column], crRowCoefficients[25+column]);
        bottom1 = float4(crRowCoefficients[57+column], crRowCoefficients[49+column], crRowCoefficients[41+column], crRowCoefficients[33+column]);
    } else {
        top0 = float4(rowCoefficients[column], rowCoefficients[8+column], rowCoefficients[16+column], rowCoefficients[24+column]);
        bottom0 = float4(rowCoefficients[56+column], rowCoefficients[48+column], rowCoefficients[40+column], rowCoefficients[32+column]);
        top1 = float4(rowCoefficients[1+column], rowCoefficients[9+column], rowCoefficients[17+column], rowCoefficients[25+column]);
        bottom1 = float4(rowCoefficients[57+column], rowCoefficients[49+column], rowCoefficients[41+column], rowCoefficients[33+column]);
    }
    uint index = frequency * BLOCK_SIZE + column;
    int low = quantizeCoefficient(pairedDCT(top0, bottom0, frequency), blockIndex, index);
    int high = quantizeCoefficient(pairedDCT(top1, bottom1, frequency), blockIndex, index + 1);
    return (uint(low) & 0xffffu) | ((uint(high) & 0xffffu) << 16);
}

[numthreads(8, 8, 1)]
void main(uint3 groupID : SV_GroupID, uint3 threadID : SV_GroupThreadID) {
    uint column = threadID.x;
    uint row = threadID.y;
    uint coefficientIndex = row * BLOCK_SIZE + column;
    uint blockIndex = groupID.z;
    // The Cb group computes both chroma blocks; the Cr group has no work.
    if (blockIndex == CR_BLOCK) return;
    uint2 mcuOrigin = groupID.xy * MCU_SIZE;

    // Each thread loads one centered Y, Cb or Cr sample.
    if (blockIndex < LUMA_BLOCKS_PER_MCU) {
        blockSamples[coefficientIndex] = lumaSample(mcuOrigin, blockIndex, threadID.xy);
    } else {
        float2 chroma = chromaSamples(mcuOrigin, threadID.xy);
        blockSamples[coefficientIndex] = chroma.x;
        crSamples[coefficientIndex] = chroma.y;
    }
    GroupMemoryBarrierWithGroupSync();

    // Row DCT: column selects horizontal frequency.
    uint start = row * BLOCK_SIZE;
    float horizontalCoefficient = pairedDCT(
        float4(blockSamples[start], blockSamples[start+1], blockSamples[start+2], blockSamples[start+3]),
        float4(blockSamples[start+7], blockSamples[start+6], blockSamples[start+5], blockSamples[start+4]), column);
    rowCoefficients[coefficientIndex] = horizontalCoefficient;
    if (blockIndex == CB_BLOCK) {
        crRowCoefficients[coefficientIndex] = pairedDCT(
            float4(crSamples[start], crSamples[start+1], crSamples[start+2], crSamples[start+3]),
            float4(crSamples[start+7], crSamples[start+6], crSamples[start+5], crSamples[start+4]), column);
    }
    GroupMemoryBarrierWithGroupSync();

    // The first 32 threads write complete coefficient pairs for this block.
    if (coefficientIndex >= BLOCK_WORDS) return;
    uint pairColumn = (coefficientIndex & 3u) * 2, frequency = coefficientIndex / 4;
    uint mcuIndex = groupID.y * mcuColumns + groupID.x;
    uint blockOffset = (mcuIndex * BLOCKS_PER_MCU + blockIndex) * BLOCK_WORDS;
    outputCoefficients[blockOffset + coefficientIndex] = coefficientPair(pairColumn, frequency, blockIndex, false);
    if (blockIndex == CB_BLOCK) {
        outputCoefficients[blockOffset + BLOCK_WORDS + coefficientIndex] = coefficientPair(pairColumn, frequency, CR_BLOCK, true);
    }
}

// One restart interval per MCU makes DC prediction and Huffman coding independent.
// Worst case: 6 * (27 DC bits + 63 * 26 AC bits), doubled for FF stuffing,
// plus padding/restart bytes, stays below 3072 bytes per MCU.
static const uint SEGMENT_CAPACITY = 3072;
static const uint PACK_GROUP_WIDTH = 1024;
static const uint zigzag[64] = {
    0, 1, 8, 16, 9, 2, 3, 10, 17, 24, 32, 25, 18, 11, 4, 5,
    12, 19, 26, 33, 40, 48, 41, 34, 27, 20, 13, 6, 7, 14, 21, 28,
    35, 42, 49, 56, 57, 50, 43, 36, 29, 22, 15, 23, 30, 37, 44, 51,
    58, 59, 52, 45, 38, 31, 39, 46, 53, 60, 61, 54, 47, 55, 62, 63
};

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

void encodeBlock(inout EntropyWriter writer, uint blockOffset, uint dcTable, inout int previousDC) {
    uint acTable = dcTable + 1;
    int dc = loadCoefficient(blockOffset, 0);
    int difference = dc - previousDC;
    previousDC = dc;
    uint count = magnitudeBitCount(difference);
    writeCoefficient(writer, dcTable, count, difference, count);

    uint zeroRun = 0;
    for (uint index = 1; index < BLOCK_COEFFICIENTS; index++) {
        int coefficient = loadCoefficient(blockOffset, zigzag[index]);
        if (coefficient == 0) {
            zeroRun++;
            continue;
        }
        while (zeroRun >= 16) {
            writeSymbol(writer, acTable, 0xf0);
            zeroRun -= 16;
        }
        if (uint(coefficient + 31) < 63) {
            uint index = 1024 + dcTable * 512 + zeroRun * 64 + (uint(coefficient) & 63u);
            uint code = huffmanCodes[index / 4][index % 4];
            writeBits(writer, code & 0x07ffffffu, code >> 27);
        } else {
            count = magnitudeBitCount(coefficient);
            writeCoefficient(writer, acTable, (zeroRun << 4) | count, coefficient, count);
        }
        zeroRun = 0;
    }
    if (zeroRun != 0) {
        writeSymbol(writer, acTable, 0); // End of block.
    }
}

[numthreads(64, 1, 1)]
void encodeMCUs(uint3 threadID : SV_DispatchThreadID) {
    uint mcu = threadID.x;
    if (mcu >= mcuCount) {
        return;
    }

    EntropyWriter writer = (EntropyWriter)0;
    writer.segmentOffset = mcu * SEGMENT_CAPACITY;
    int previousY = 0;
    int previousCb = 0;
    int previousCr = 0;
    uint mcuOffset = mcu * BLOCKS_PER_MCU * BLOCK_WORDS;
    for (uint block = 0; block < LUMA_BLOCKS_PER_MCU; block++) {
        encodeBlock(writer, mcuOffset + block * BLOCK_WORDS, LUMA_DC_TABLE, previousY);
    }
    encodeBlock(writer, mcuOffset + CB_BLOCK * BLOCK_WORDS, CHROMA_DC_TABLE, previousCb);
    encodeBlock(writer, mcuOffset + CR_BLOCK * BLOCK_WORDS, CHROMA_DC_TABLE, previousCr);

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

// segmentInfo stores [length, offset] for each MCU, followed by total JPEG bytes.
[numthreads(1, 1, 1)]
void indexSegments(uint3 threadID : SV_DispatchThreadID) {
    uint byteCount = jpegHeaderBytes;
    bool failed = false;
    for (uint mcu = 0; mcu < mcuCount; mcu++) {
        uint length = segmentInfo[mcu * 2];
        if (length == 0 || length > SEGMENT_CAPACITY) {
            failed = true;
        }
        segmentInfo[mcu * 2 + 1] = byteCount;
        byteCount += length;
    }

    byteCount = failed ? 0 : byteCount + 2; // EOI marker.
    uint bytesPerGroup = PACK_THREADS * WORD_BYTES;
    uint groups = (byteCount + bytesPerGroup - 1) / bytesPerGroup;
    dispatchArguments.Store(0, min(groups, PACK_GROUP_WIDTH));
    dispatchArguments.Store(4, (groups + PACK_GROUP_WIDTH - 1) / PACK_GROUP_WIDTH);
    dispatchArguments.Store(8, 1);
    dispatchArguments.Store(12, byteCount);
    segmentInfo[mcuCount * 2] = byteCount;
}

uint findSegment(uint position) {
    uint low = 0;
    uint high = mcuCount;
    while (low + 1 < high) {
        uint middle = (low + high) / 2;
        if (segmentInfo[middle * 2 + 1] <= position) {
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
    if (position < jpegHeaderBytes) {
        return (jpegHeader.Load(position & ~3u) >> ((position & 3) * 8)) & 0xff;
    }
    if (position >= totalBytes - 2) {
        return position == totalBytes - 2 ? 0xff : 0xd9; // EOI.
    }
    while (mcu + 1 < mcuCount && segmentInfo[(mcu + 1) * 2 + 1] <= position) {
        mcu++;
    }
    uint relative = position - segmentInfo[mcu * 2 + 1];
    uint word = entropySegments.Load(mcu * SEGMENT_CAPACITY + (relative & ~3u));
    return (word >> ((relative & 3) * 8)) & 0xff;
}

// Each invocation owns a complete 32-bit output word, including segment seams.
// Indirect dispatch covers the actual JPEG length, not the worst-case allocation.
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
