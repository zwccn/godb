//go:build windows && amd64

#include "textflag.h"

// func rdtsc() uint64
//
// LFENCE 不能省：RDTSC 本身不是序列化指令，CPU 可以把它提到前面的指令之前执行，
// 那样测出来的区间会短得莫名其妙（恰恰在几百纳秒这个量级最明显）。
// Intel 的推荐组合就是 LFENCE; RDTSC。
TEXT ·rdtsc(SB), NOSPLIT, $0-8
	LFENCE
	RDTSC
	SHLQ $32, DX
	ORQ  DX, AX
	MOVQ AX, ret+0(FP)
	RET

// func cpuid(eax, ecx uint32) (a, b, c, d uint32)
//
// 只用来读两个叶：0x80000000（最大扩展叶）和 0x80000007（恒定 TSC 标志）。
// Go 1.17 起 BX 不再是保留寄存器（g 用 R14），NOSPLIT 里可以随便用。
TEXT ·cpuid(SB), NOSPLIT, $0-24
	MOVL eax+0(FP), AX
	MOVL ecx+4(FP), CX
	CPUID
	MOVL AX, a+8(FP)
	MOVL BX, b+12(FP)
	MOVL CX, c+16(FP)
	MOVL DX, d+20(FP)
	RET
