// Baseline interleaved JPEG: GPU Huffman -> IDCT -> limited BT.709 NV12.
#ifdef __spirv__
#define BINDING(index) [[vk::binding(index, 0)]]
#else
#define BINDING(index)
#endif
BINDING(0) ByteAddressBuffer entropyInput : register(t0);
BINDING(1) ByteAddressBuffer metadata : register(t1);
BINDING(2) RWStructuredBuffer<int> coefficients : register(u0);
BINDING(3) RWStructuredBuffer<float> samples : register(u1);
BINDING(4) RWByteAddressBuffer nv12Output : register(u2);
BINDING(5) RWByteAddressBuffer decodeStatus : register(u3);
uint meta(uint word) { return metadata.Load(word * 4); }
void invalidJPEG() { uint old; decodeStatus.InterlockedOr(0, 1, old); }
struct EntropyReader {
    uint position;
    uint end;
    uint wordOffset;
    uint word;
    bool valid;
};

uint bigEndianWord(uint offset) {
    uint word = entropyInput.Load(offset);
    return (word >> 24) | ((word >> 8) & 0xff00u)
        | ((word << 8) & 0xff0000u) | (word << 24);
}

// Read up to 16 bits from one or two aligned words, caching the current word.
uint peekBits(inout EntropyReader reader, uint count) {
    if (!reader.valid || count > 16 || reader.position > reader.end || count > reader.end - reader.position) {
        reader.valid = false;
        return 0;
    }
    if (count == 0) return 0;
    uint offset = (reader.position >> 3) & ~3u;
    if (offset != reader.wordOffset) {
        reader.word = bigEndianWord(offset);
        reader.wordOffset = offset;
    }
    uint shift = reader.position & 31u;
    uint bits = reader.word << shift;
    if (shift + count > 32) bits |= bigEndianWord(offset + 4) >> (32 - shift);
    return bits >> (32 - count);
}
uint readBits(inout EntropyReader reader, uint count) {
    uint value = peekBits(reader, count);
    if (reader.valid) reader.position += count;
    return value;
}

int amplitude(uint value, uint count) {
    return count == 0 ? 0 : value < (1u << (count - 1)) ? int(value) - int((1u << count) - 1) : int(value);
}

static const uint HUFFMAN_LOOKUP_BITS = 8;
static const uint HUFFMAN_LOOKUP_SIZE = 1 << HUFFMAN_LOOKUP_BITS;
groupshared uint huffmanLookup[6 * HUFFMAN_LOOKUP_SIZE];

void prepareHuffman(uint lane) {
    for (uint index = lane; index < meta(14) * 2 * HUFFMAN_LOOKUP_SIZE; index += 64) {
        uint table = index >> HUFFMAN_LOOKUP_BITS;
        uint prefix = index & (HUFFMAN_LOOKUP_SIZE - 1);
        uint base = 232 + table * 304;
        uint entry = 0;
        for (uint length = 1; length <= HUFFMAN_LOOKUP_BITS; length++) {
            uint code = prefix >> (HUFFMAN_LOOKUP_BITS - length);
            uint first = meta(base + length - 1), limit = meta(base + 16 + length - 1);
            if (code >= first && code < limit) {
                uint symbol = meta(base + 48 + meta(base + 32 + length - 1) + code - first);
                entry = (length << 8) | symbol;
                uint size = symbol & 15, total = length + size;
                if (total <= HUFFMAN_LOOKUP_BITS) {
                    uint raw = (prefix >> (HUFFMAN_LOOKUP_BITS - total)) & ((1u << size) - 1);
                    entry |= (total << 12) | ((uint(amplitude(raw, size)) & 0xffffu) << 16);
                }
                break;
            }
        }
        huffmanLookup[index] = entry;
    }
    GroupMemoryBarrierWithGroupSync();
}

struct HuffmanValue {
    uint symbol;
    int value;
};

HuffmanValue readCoefficient(uint table, inout EntropyReader reader) {
    HuffmanValue result = (HuffmanValue)0;
    uint base = 232 + table * 304, code = 0, length = 0;
    if (reader.valid && reader.end - reader.position >= HUFFMAN_LOOKUP_BITS) {
        code = peekBits(reader, HUFFMAN_LOOKUP_BITS);
        uint entry = huffmanLookup[table * HUFFMAN_LOOKUP_SIZE + code];
        if (entry != 0) {
            uint total = (entry >> 12) & 15;
            reader.position += total != 0 ? total : (entry >> 8) & 15;
            result.symbol = entry & 255u;
            if (total != 0) result.value = int(entry) >> 16;
            else result.value = amplitude(readBits(reader, result.symbol & 15), result.symbol & 15);
            return result;
        }
        reader.position += HUFFMAN_LOOKUP_BITS;
        length = HUFFMAN_LOOKUP_BITS;
    }
    for (; length < 16; length++) {
        code = (code << 1) | readBits(reader, 1);
        if (!reader.valid) return result;
        uint first = meta(base + length), limit = meta(base + 16 + length);
        if (code >= first && code < limit) {
            result.symbol = meta(base + 48 + meta(base + 32 + length) + code - first);
            uint size = result.symbol & 15;
            result.value = amplitude(readBits(reader, size), size);
            return result;
        }
    }
    reader.valid = false;
    return result;
}

static const uint DC_VALUE_BITS = 12;
static const uint DC_VALUE_MASK = (1u << DC_VALUE_BITS) - 1;
static const uint BLOCK_HAS_AC = 1u << DC_VALUE_BITS;
static const uint zigzag[64] = {
0,1,8,16,9,2,3,10,17,24,32,25,18,11,4,5,
12,19,26,33,40,48,41,34,27,20,13,6,7,14,21,28,
35,42,49,56,57,50,43,36,29,22,15,23,30,37,44,51,
58,59,52,45,38,31,39,46,53,60,61,54,47,55,62,63 };
// Restart intervals allow parallel entropy workers. A stream without restart
// markers has one bounded serial worker; IDCT and output remain parallel.
[numthreads(64,1,1)]
void main(uint3 thread : SV_DispatchThreadID, uint lane : SV_GroupIndex) {
    prepareHuffman(lane);
    if (thread.x >= meta(9)) return;
    uint segment = 2056 + thread.x * 4;
    EntropyReader reader = (EntropyReader)0;
    reader.position = meta(segment) * 8;
    reader.end = reader.position + meta(segment + 1) * 8;
    reader.wordOffset = ~0u;
    reader.valid = true;
    uint first = meta(segment + 2), count = meta(segment + 3);
    int predictors[3] = {0,0,0};
    for (uint mcu = first; mcu < first + count; mcu++) {
        for (uint component = 0; component < meta(14); component++) {
            uint c = 16 + component * 8, blocks = meta(c) * meta(c+1);
            for (uint block = 0; block < blocks; block++) {
                uint base = (mcu * meta(8) + meta(c+2) + block) * 64;
                for (uint k = 0; k < 64; k++) coefficients[base+k] = 0;
                HuffmanValue dc = readCoefficient(component*2, reader);
                if (dc.symbol > 11) reader.valid = false;
                predictors[component] += dc.value;
                if (predictors[component] < -2048 || predictors[component] > 2047) reader.valid = false;
                bool hasAC = false;
                uint index = 1;
                while (index < 64 && reader.valid) {
                    HuffmanValue ac = readCoefficient(component*2+1, reader);
                    if (ac.symbol == 0) break;
                    uint zeros = ac.symbol >> 4, size = ac.symbol & 15;
                    if (size == 0) {
                        if (zeros != 15 || index+16 > 64) { reader.valid = false; break; }
                        index += 16; continue;
                    }
                    index += zeros;
                    if (index >= 64 || size > 10) { reader.valid = false; break; }
                    hasAC = true;
                    coefficients[base+zigzag[index]] = ac.value;
                    index++;
                }
                if (!reader.valid) { invalidJPEG(); return; }
                // The DC slot carries its signed 12-bit value and an AC flag.
                // IDCT can skip both matrix passes for a constant block.
                coefficients[base] = (predictors[component] & DC_VALUE_MASK) | (hasAC ? BLOCK_HAS_AC : 0);
            }
        }
    }
    uint remaining = reader.end - reader.position;
    if (remaining > 7) { invalidJPEG(); return; }
    if (readBits(reader,remaining) != ((1u<<remaining)-1u)) invalidJPEG();
}
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
groupshared float rows[64];
groupshared float transformed[64];
[numthreads(8,8,1)]
void inverseDCT(uint3 group : SV_GroupID, uint3 local : SV_GroupThreadID) {
    uint block = group.x + group.y * (meta(6)*meta(8));
    uint within = group.x % meta(8), component = 0;
    if (meta(14) == 3) {
        component = within < meta(16)*meta(17) ? 0 : within < meta(34) ? 1 : 2;
    }
    uint x = local.x, y = local.y, index = y*8+x, base = block*64;
    uint packedDC = uint(coefficients[base]);
    int dc = int(packedDC << (32 - DC_VALUE_BITS)) >> (32 - DC_VALUE_BITS);
    if ((packedDC & BLOCK_HAS_AC) == 0) {
        samples[base+index] = clamp(floor(float(dc) * float(meta(40+component*64)) * 0.125 + 128.5), 0.0, 255.0);
        return;
    }
    rows[index] = float(index == 0 ? dc : coefficients[base+index]) * float(meta(40+component*64+index));
    GroupMemoryBarrierWithGroupSync();
    float value = 0;
    for (uint u=0;u<8;u++) value += dctBasis[u][x]*rows[y*8+u];
    transformed[index] = value;
    GroupMemoryBarrierWithGroupSync();
    value=0;
    for (uint v=0;v<8;v++) value += dctBasis[v][y]*transformed[v*8+x];
    samples[base+index] = clamp(floor(value+128.5),0.0,255.0);
}
float componentSample(uint component, uint2 pixel) {
    uint2 shift = uint2(meta(4) == 2, meta(5) == 2);
    uint2 mcu = pixel >> (shift + 3);
    uint2 inside = pixel & ((8u << shift) - 1);
    uint block;
    if (component == 0) {
        block = (inside.y >> 3) * meta(4) + (inside.x >> 3);
        inside &= 7u;
    } else {
        inside >>= shift;
        block = meta(16 + component*8 + 2);
    }
    uint base = ((mcu.y*meta(6)+mcu.x)*meta(8)+block)*64;
    return samples[base + inside.y*8 + inside.x];
}
float3 yuv709(uint2 outputPixel) {
    uint2 origin = uint2(meta(12),meta(13)), fit = uint2(meta(10),meta(11));
    if (any(outputPixel < origin) || any(outputPixel-origin >= fit)) return float3(16,128,128);
    uint2 sourceSize = uint2(meta(0),meta(1));
    uint2 pixel = outputPixel - origin;
    if (any(fit != sourceSize)) pixel = min(pixel*sourceSize/fit,sourceSize-1);
    float y = componentSample(0,pixel), cb=0, cr=0;
    if (meta(14)==3) { cb=componentSample(1,pixel)-128; cr=componentSample(2,pixel)-128; }
    float3 rgb = clamp(float3(y+1.402*cr,y-0.344136*cb-0.714136*cr,y+1.772*cb),0.0,255.0);
    return float3(16+dot(rgb,float3(0.2126,0.7152,0.0722))*(219.0/255.0),
        128+dot(rgb,float3(-0.114572,-0.385428,0.5))*(224.0/255.0),
        128+dot(rgb,float3(0.5,-0.454153,-0.045847))*(224.0/255.0));
}
uint quantize(float value) { return uint(clamp(floor(value+0.5),0.0,255.0)); }
// Each thread owns a complete 32-bit word; no adjacent-byte UAV write races.
[numthreads(64,1,1)]
void packNV12(uint3 thread : SV_DispatchThreadID) {
    uint width=meta(2),height=meta(3),bytes=width*height*3/2, offset=(thread.x+thread.y*16384)*4;
    if (offset >= bytes) return;
    uint packed = 0, lumaBytes = width * height;
    if (offset < lumaBytes) {
        uint2 pixel = uint2(offset % width, offset / width);
        for (uint b = 0; b < 4; b++) {
            packed |= quantize(yuv709(pixel).x) << (b * 8);
            if (++pixel.x == width) { pixel.x = 0; pixel.y++; }
        }
    } else {
        // Each pair shares one 2x2 average between its U and V bytes.
        for (uint pair = 0; pair < 2; pair++) {
            uint c = offset - lumaBytes + pair * 2;
            if (offset + pair * 2 >= bytes) break;
            uint2 pos = uint2(c % width, c / width * 2);
            float3 avg = (yuv709(pos) + yuv709(pos + uint2(1,0))
                + yuv709(pos + uint2(0,1)) + yuv709(pos + uint2(1,1))) * 0.25;
            uint uv = quantize(avg.y) | (quantize(avg.z) << 8);
            packed |= uv << (pair * 16);
        }
    }
    nv12Output.Store(offset, packed);
}
