// Previous production writer, retained only for the FXC compile-time comparison.
// This file is never embedded into the application.
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
        ENTROPY_LOOP for (uint i = 0; i < bytes; i++) {
            uint byteValue = (packed >> (i * 8)) & 255u;
            writeByte(writer, byteValue);
            if (byteValue == 0xff) writeByte(writer, 0);
        }
    }
}

void writeBits(inout EntropyWriter writer, uint value, uint count) {
    if (count == 0) return;
    if (count > 24) {
        appendBits(writer, value >> 16, count - 16);
        value &= 0xffffu;
        count = 16;
    }
    appendBits(writer, value, count);
}
