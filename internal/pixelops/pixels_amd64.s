#include "textflag.h"

TEXT ·runByteSSE2(SB), NOSPLIT, $0-32
	MOVQ p_base+0(FP), SI
	MOVQ p_len+8(FP), CX
	XORQ R8, R8
	TESTQ CX, CX
	JE byte_sse_done
	MOVBLZX (SI), BX
	MOVL BX, AX
	IMULL $0x01010101, AX
	MOVL AX, X2
	PSHUFL $0, X2, X2
byte_sse_vector:
	CMPQ CX, $16
	JB byte_sse_tail
	MOVOU (SI), X0
	PCMPEQB X2, X0
	PMOVMSKB X0, DX
	CMPL DX, $65535
	JNE byte_sse_partial
	ADDQ $16, SI
	ADDQ $16, R8
	SUBQ $16, CX
	JMP byte_sse_vector
byte_sse_partial:
	XORL $65535, DX
	BSFL DX, AX
	ADDQ AX, R8
	JMP byte_sse_done
byte_sse_tail:
	TESTQ CX, CX
	JE byte_sse_done
	MOVBLZX (SI), AX
	CMPL AX, BX
	JNE byte_sse_done
	INCQ SI
	INCQ R8
	DECQ CX
	JMP byte_sse_tail
byte_sse_done:
	MOVQ R8, ret+24(FP)
	RET

TEXT ·runByteAVX2(SB), NOSPLIT, $0-32
	MOVQ p_base+0(FP), SI
	MOVQ p_len+8(FP), CX
	XORQ R8, R8
	TESTQ CX, CX
	JE byte_avx_done
	MOVBLZX (SI), BX
	MOVL BX, X2
	VPBROADCASTB X2, Y2
byte_avx_vector:
	CMPQ CX, $32
	JB byte_avx_tail
	VMOVDQU (SI), Y0
	VPCMPEQB Y2, Y0, Y0
	VPMOVMSKB Y0, DX
	CMPL DX, $-1
	JNE byte_avx_partial
	ADDQ $32, SI
	ADDQ $32, R8
	SUBQ $32, CX
	JMP byte_avx_vector
byte_avx_partial:
	XORL $-1, DX
	BSFL DX, AX
	ADDQ AX, R8
	JMP byte_avx_done
byte_avx_tail:
	TESTQ CX, CX
	JE byte_avx_done
	MOVBLZX (SI), AX
	CMPL AX, BX
	JNE byte_avx_done
	INCQ SI
	INCQ R8
	DECQ CX
	JMP byte_avx_tail
byte_avx_done:
	VZEROUPPER
	MOVQ R8, ret+24(FP)
	RET

// These kernels load only complete vector groups; scalar tails never read or
// write past a pixel buffer. All loads accept unaligned addresses.

TEXT ·equalBlockSSE2(SB), NOSPLIT, $0-73
	MOVQ a_base+0(FP), SI
	MOVQ b_base+24(FP), DI
	MOVQ stride+48(FP), R8
	MOVQ width+56(FP), R9
	MOVQ height+64(FP), R10
	PXOR X7, X7
	TESTQ R10, R10
	JE eq_sse_true
eq_sse_row:
	MOVQ SI, R11
	MOVQ DI, R12
	MOVQ R9, CX
eq_sse_vector:
	CMPQ CX, $4
	JB eq_sse_tail
	MOVOU (SI), X0
	MOVOU (DI), X1
	PXOR X1, X0
	PSLLL $8, X0
	PCMPEQL X7, X0
	PMOVMSKB X0, AX
	CMPL AX, $65535
	JNE eq_sse_false
	ADDQ $16, SI
	ADDQ $16, DI
	SUBQ $4, CX
	JMP eq_sse_vector
eq_sse_tail:
	TESTQ CX, CX
	JE eq_sse_next
	MOVL (SI), AX
	XORL (DI), AX
	ANDL $0xffffff, AX
	JNE eq_sse_false
	ADDQ $4, SI
	ADDQ $4, DI
	DECQ CX
	JMP eq_sse_tail
eq_sse_next:
	LEAQ (R11)(R8*1), SI
	LEAQ (R12)(R8*1), DI
	DECQ R10
	JNE eq_sse_row
eq_sse_true:
	MOVB $1, ret+72(FP)
	RET
eq_sse_false:
	MOVB $0, ret+72(FP)
	RET

TEXT ·equalBlockAVX2(SB), NOSPLIT, $0-73
	MOVQ a_base+0(FP), SI
	MOVQ b_base+24(FP), DI
	MOVQ stride+48(FP), R8
	MOVQ width+56(FP), R9
	MOVQ height+64(FP), R10
	VPXOR Y7, Y7, Y7
	TESTQ R10, R10
	JE eq_avx_true
eq_avx_row:
	MOVQ SI, R11
	MOVQ DI, R12
	MOVQ R9, CX
eq_avx_vector:
	CMPQ CX, $8
	JB eq_avx_tail
	VMOVDQU (SI), Y0
	VPXOR (DI), Y0, Y0
	VPSLLD $8, Y0, Y0
	VPCMPEQD Y7, Y0, Y0
	VPMOVMSKB Y0, AX
	CMPL AX, $-1
	JNE eq_avx_false
	ADDQ $32, SI
	ADDQ $32, DI
	SUBQ $8, CX
	JMP eq_avx_vector
eq_avx_tail:
	TESTQ CX, CX
	JE eq_avx_next
	MOVL (SI), AX
	XORL (DI), AX
	ANDL $0xffffff, AX
	JNE eq_avx_false
	ADDQ $4, SI
	ADDQ $4, DI
	DECQ CX
	JMP eq_avx_tail
eq_avx_next:
	LEAQ (R11)(R8*1), SI
	LEAQ (R12)(R8*1), DI
	DECQ R10
	JNE eq_avx_row
eq_avx_true:
	VZEROUPPER
	MOVB $1, ret+72(FP)
	RET
eq_avx_false:
	VZEROUPPER
	MOVB $0, ret+72(FP)
	RET

TEXT ·runBGRSSE2(SB), NOSPLIT, $0-32
	MOVQ pixels_base+0(FP), SI
	MOVQ pixels_len+8(FP), CX
	SHRQ $2, CX
	XORQ R8, R8
	TESTQ CX, CX
	JE run_sse_done
	MOVL (SI), BX
	ANDL $0xffffff, BX
	MOVL BX, AX
	SHLL $8, AX
	MOVL AX, X2
	PSHUFL $0, X2, X2
run_sse_vector:
	CMPQ CX, $4
	JB run_sse_tail
	MOVOU (SI), X0
	PSLLL $8, X0
	PCMPEQL X2, X0
	PMOVMSKB X0, DX
	CMPL DX, $65535
	JNE run_sse_partial
	ADDQ $16, SI
	ADDQ $4, R8
	SUBQ $4, CX
	JMP run_sse_vector
run_sse_partial:
	XORL $65535, DX
	BSFL DX, AX
	SHRL $2, AX
	ADDQ AX, R8
	JMP run_sse_done
run_sse_tail:
	TESTQ CX, CX
	JE run_sse_done
	MOVL (SI), AX
	ANDL $0xffffff, AX
	CMPL AX, BX
	JNE run_sse_done
	ADDQ $4, SI
	INCQ R8
	DECQ CX
	JMP run_sse_tail
run_sse_done:
	MOVQ R8, ret+24(FP)
	RET

TEXT ·runBGRAVX2(SB), NOSPLIT, $0-32
	MOVQ pixels_base+0(FP), SI
	MOVQ pixels_len+8(FP), CX
	SHRQ $2, CX
	XORQ R8, R8
	TESTQ CX, CX
	JE run_avx_done
	MOVL (SI), BX
	ANDL $0xffffff, BX
	MOVL BX, AX
	SHLL $8, AX
	MOVL AX, X2
	VPBROADCASTD X2, Y2
run_avx_vector:
	CMPQ CX, $8
	JB run_avx_tail
	VMOVDQU (SI), Y0
	VPSLLD $8, Y0, Y0
	VPCMPEQD Y2, Y0, Y0
	VPMOVMSKB Y0, DX
	CMPL DX, $-1
	JNE run_avx_partial
	ADDQ $32, SI
	ADDQ $8, R8
	SUBQ $8, CX
	JMP run_avx_vector
run_avx_partial:
	XORL $-1, DX
	BSFL DX, AX
	SHRL $2, AX
	ADDQ AX, R8
	JMP run_avx_done
run_avx_tail:
	TESTQ CX, CX
	JE run_avx_done
	MOVL (SI), AX
	ANDL $0xffffff, AX
	CMPL AX, BX
	JNE run_avx_done
	ADDQ $4, SI
	INCQ R8
	DECQ CX
	JMP run_avx_tail
run_avx_done:
	VZEROUPPER
	MOVQ R8, ret+24(FP)
	RET

DATA ·evenColors+0(SB)/8, $0x0000000000ffffff
DATA ·evenColors+8(SB)/8, $0x0000000000ffffff
GLOBL ·evenColors(SB), RODATA|NOPTR, $16
DATA ·oddColors+0(SB)/8, $0x0000ffffff000000
DATA ·oddColors+8(SB)/8, $0x0000ffffff000000
GLOBL ·oddColors(SB), RODATA|NOPTR, $16

TEXT ·packBGRSSE2(SB), NOSPLIT, $0-48
	MOVQ dst_base+0(FP), DI
	MOVQ src_base+24(FP), SI
	MOVQ src_len+32(FP), CX
	SHRQ $2, CX
	MOVOU ·evenColors(SB), X4
	MOVOU ·oddColors(SB), X5
pack_sse_vector:
	CMPQ CX, $4
	JB pack_sse_tail
	MOVOU (SI), X0
	MOVO X0, X1
	PSRLDQ $1, X1
	PAND X4, X0
	PAND X5, X1
	POR X1, X0
	MOVQ X0, (DI)
	PSRLDQ $8, X0
	MOVL X0, 6(DI)
	PSRLDQ $4, X0
	MOVL X0, AX
	MOVW AX, 10(DI)
	ADDQ $16, SI
	ADDQ $12, DI
	SUBQ $4, CX
	JMP pack_sse_vector
pack_sse_tail:
	TESTQ CX, CX
	JE pack_sse_done
	MOVW (SI), AX
	MOVW AX, (DI)
	MOVB 2(SI), AX
	MOVB AX, 2(DI)
	ADDQ $4, SI
	ADDQ $3, DI
	DECQ CX
	JMP pack_sse_tail
pack_sse_done:
	RET

DATA ·packShuffle+0(SB)/8, $0x0908060504020100
DATA ·packShuffle+8(SB)/8, $0x808080800e0d0c0a
DATA ·packShuffle+16(SB)/8, $0x0908060504020100
DATA ·packShuffle+24(SB)/8, $0x808080800e0d0c0a
GLOBL ·packShuffle(SB), RODATA|NOPTR, $32

TEXT ·packBGRAVX2(SB), NOSPLIT, $0-48
	MOVQ dst_base+0(FP), DI
	MOVQ src_base+24(FP), SI
	MOVQ src_len+32(FP), CX
	SHRQ $2, CX
	VMOVDQU ·packShuffle(SB), Y4
pack_avx_vector:
	CMPQ CX, $8
	JB pack_avx_tail
	VMOVDQU (SI), Y0
	VPSHUFB Y4, Y0, Y0
	VEXTRACTI128 $1, Y0, X1
	VPSLLDQ $12, X1, X2
	VPOR X2, X0, X0
	VMOVDQU X0, (DI)
	VPSRLDQ $4, X1, X1
	VMOVQ X1, 16(DI)
	ADDQ $32, SI
	ADDQ $24, DI
	SUBQ $8, CX
	JMP pack_avx_vector
pack_avx_tail:
	TESTQ CX, CX
	JE pack_avx_done
	MOVW (SI), AX
	MOVW AX, (DI)
	MOVB 2(SI), AX
	MOVB AX, 2(DI)
	ADDQ $4, SI
	ADDQ $3, DI
	DECQ CX
	JMP pack_avx_tail
pack_avx_done:
	VZEROUPPER
	RET
