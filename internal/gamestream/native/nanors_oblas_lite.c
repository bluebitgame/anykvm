//go:build cgo

#if defined(_WIN32) && (defined(__x86_64__) || defined(__i386__))
#include <cpuid.h>
#include <stdint.h>
#include <string.h>
// MinGW/Clang distributions need not ship GCC's __cpu_model runtime.
// Check both CPU features and OS register-state support before SIMD dispatch.
static int anykvm_cpu_supports(const char *feature) {
 unsigned a,b,c,d;
 if(!__get_cpuid(1,&a,&b,&c,&d))return 0;
 if(!strcmp(feature,"ssse3"))return !!(c & (1u<<9));
 uint64_t state=0;
 if((c & ((1u<<27)|(1u<<28))) == ((1u<<27)|(1u<<28))) {
  unsigned low,high;
  __asm__ volatile("xgetbv" : "=a"(low), "=d"(high) : "c"(0));
  state=((uint64_t)high<<32)|low;
 }
 if(!__get_cpuid_count(7,0,&a,&b,&c,&d))return 0;
 if(!strcmp(feature,"gfni"))return !!(c & (1u<<8));
 if(!strcmp(feature,"avx2"))return (state&6)==6 && !!(b&(1u<<5));
 if((state&0xe6)!=0xe6)return 0;
 if(!strcmp(feature,"avx512f"))return !!(b&(1u<<16));
 if(!strcmp(feature,"avx512dq"))return !!(b&(1u<<17));
 if(!strcmp(feature,"avx512bw"))return !!(b&(1u<<30));
 if(!strcmp(feature,"avx512vl"))return !!(b&(1u<<31));
 return 0;
}
#define __builtin_cpu_init() ((void)0)
#define __builtin_cpu_supports(feature) anykvm_cpu_supports(feature)
#endif
#include "third_party/nanors/deps/obl/oblas_lite.c"
