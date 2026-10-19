// Each group encodes one 8x8 block. A 4:2:0 MCU contains Y00, Y10,
// Y01, Y11, Cb, Cr; group.xy selects the MCU and group.z selects its block.
static const uint BLOCK_SIZE = 8;
static const uint BLOCK_COEFFICIENTS = BLOCK_SIZE * BLOCK_SIZE;
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
BINDING(1) RWStructuredBuffer<int> outputCoefficients : register(u0);
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
    // Natural-order Y table, then Cb/Cr table. uint4 avoids scalar-array padding.
    uint4 quantizationTables[32];
    // DC Y, AC Y, DC Cb/Cr, AC Cb/Cr; each entry packs length:16 | code:16.
    uint4 huffmanCodes[256];
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

float chromaSample(uint2 mcuOrigin, uint blockIndex, uint2 samplePosition) {
    uint2 position = mcuOrigin + samplePosition * 2;
    float3 averageRGB = (loadRGB(position)
        + loadRGB(position + uint2(1, 0))
        + loadRGB(position + uint2(0, 1))
        + loadRGB(position + uint2(1, 1))) * 0.25;
    float3 weights = blockIndex == CB_BLOCK
        ? float3(-0.168736, -0.331264, 0.5)
        : float3(0.5, -0.418688, -0.081312);
    // Chroma is already centered at zero; luma subtracts its 128 offset above.
    return dot(averageRGB, weights);
}

int quantizeCoefficient(float coefficient, uint blockIndex, uint coefficientIndex) {
    uint tableOffset = blockIndex < LUMA_BLOCKS_PER_MCU ? 0 : BLOCK_COEFFICIENTS;
    uint tableIndex = tableOffset + coefficientIndex;
    float divisor = quantizationTables[tableIndex / 4][tableIndex % 4];
    int quantized = (int)(sign(coefficient) * floor(abs(coefficient) / divisor + 0.5));
    // Baseline JPEG permits 11-bit DC differences and 10-bit AC magnitudes.
    int minimum = coefficientIndex == 0 ? -1024 : -1023;
    return clamp(quantized, minimum, 1023);
}

[numthreads(8, 8, 1)]
void main(uint3 groupID : SV_GroupID, uint3 threadID : SV_GroupThreadID) {
    uint column = threadID.x;
    uint row = threadID.y;
    uint coefficientIndex = row * BLOCK_SIZE + column;
    uint blockIndex = groupID.z;
    uint2 mcuOrigin = groupID.xy * MCU_SIZE;

    // Each thread loads one centered Y, Cb or Cr sample.
    if (blockIndex < LUMA_BLOCKS_PER_MCU) {
        blockSamples[coefficientIndex] = lumaSample(mcuOrigin, blockIndex, threadID.xy);
    } else {
        blockSamples[coefficientIndex] = chromaSample(mcuOrigin, blockIndex, threadID.xy);
    }
    GroupMemoryBarrierWithGroupSync();

    // Row DCT: column selects horizontal frequency.
    float horizontalCoefficient = 0;
    [unroll]
    for (uint sampleColumn = 0; sampleColumn < BLOCK_SIZE; sampleColumn++) {
        horizontalCoefficient += dctBasis[column][sampleColumn] * blockSamples[row * BLOCK_SIZE + sampleColumn];
    }
    rowCoefficients[coefficientIndex] = horizontalCoefficient;
    GroupMemoryBarrierWithGroupSync();

    // Column DCT: row selects vertical frequency.
    float coefficient = 0;
    [unroll]
    for (uint sampleRow = 0; sampleRow < BLOCK_SIZE; sampleRow++) {
        coefficient += dctBasis[row][sampleRow] * rowCoefficients[sampleRow * BLOCK_SIZE + column];
    }

    // The entropy pass consumes raster MCUs, six blocks each, in natural order.
    uint mcuIndex = groupID.y * mcuColumns + groupID.x;
    uint blockOffset = (mcuIndex * BLOCKS_PER_MCU + blockIndex) * BLOCK_COEFFICIENTS;
    outputCoefficients[blockOffset + coefficientIndex] = quantizeCoefficient(coefficient, blockIndex, coefficientIndex);
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

void writeBits(inout EntropyWriter writer, uint value, uint count) {
    if (count == 0) {
        return;
    }
    writer.pendingBits = (writer.pendingBits << count) | (value & ((1u << count) - 1));
    writer.bitCount += count;
    while (writer.bitCount >= 8) {
        writer.bitCount -= 8;
        uint valueByte = (writer.pendingBits >> writer.bitCount) & 0xff;
        writeByte(writer, valueByte);
        // JPEG escapes FF data bytes so they cannot be mistaken for markers.
        if (valueByte == 0xff) {
            writeByte(writer, 0);
        }
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

void writeMagnitude(inout EntropyWriter writer, int value, uint count) {
    // Negative magnitudes use the low bits of the one's complement of abs(value).
    writeBits(writer, (uint)(value < 0 ? value - 1 : value), count);
}

void encodeBlock(inout EntropyWriter writer, uint blockOffset, uint dcTable, inout int previousDC) {
    uint acTable = dcTable + 1;
    int dc = outputCoefficients[blockOffset];
    int difference = dc - previousDC;
    previousDC = dc;
    uint count = magnitudeBitCount(difference);
    writeSymbol(writer, dcTable, count);
    writeMagnitude(writer, difference, count);

    uint zeroRun = 0;
    for (uint index = 1; index < BLOCK_COEFFICIENTS; index++) {
        int coefficient = outputCoefficients[blockOffset + zigzag[index]];
        if (coefficient == 0) {
            zeroRun++;
            continue;
        }
        while (zeroRun >= 16) {
            writeSymbol(writer, acTable, 0xf0);
            zeroRun -= 16;
        }
        count = magnitudeBitCount(coefficient);
        writeSymbol(writer, acTable, (zeroRun << 4) | count);
        writeMagnitude(writer, coefficient, count);
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
    uint mcuOffset = mcu * BLOCKS_PER_MCU * BLOCK_COEFFICIENTS;
    for (uint block = 0; block < LUMA_BLOCKS_PER_MCU; block++) {
        encodeBlock(writer, mcuOffset + block * BLOCK_COEFFICIENTS, LUMA_DC_TABLE, previousY);
    }
    encodeBlock(writer, mcuOffset + CB_BLOCK * BLOCK_COEFFICIENTS, CHROMA_DC_TABLE, previousCb);
    encodeBlock(writer, mcuOffset + CR_BLOCK * BLOCK_COEFFICIENTS, CHROMA_DC_TABLE, previousCr);

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
