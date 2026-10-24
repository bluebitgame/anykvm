#ifndef ANYKVM_PROGRESSIVE_QUALITY_H
#define ANYKVM_PROGRESSIVE_QUALITY_H

/* Microsoft WindowsProtocolTestSuites RdpegfxTileUtils progressive profiles.
 * Columns: LL3, HL3, LH3, HH3, HL2, LH2, HH2, HL1, LH1, HH1. */
static const unsigned bk_progressive_quality[2][3][10] = {
    {{1,1,1,1,2,2,2,1,1,1}, {1,1,1,1,2,2,2,1,1,1}, {1,1,1,1,2,2,2,1,1,0}},
    {{0,1,1,1,1,1,1,0,0,0}, {0,1,1,1,2,1,1,0,0,0}, {0,1,1,1,2,1,1,1,0,0}}
};
static unsigned bk_progressive_band(unsigned i) {
    if(i<1023)return 7;if(i<2046)return 8;if(i<3007)return 9;
    if(i<3279)return 4;if(i<3551)return 5;if(i<3807)return 6;
    if(i<3879)return 1;if(i<3951)return 2;if(i<4015)return 3;return 0;
}
static unsigned bk_progressive_qbit(unsigned stage, unsigned c, unsigned band) {
    return stage>=0x102 ? 0 : bk_progressive_quality[stage-0x100][c][band];
}
static int bk_progressive_official(unsigned stage) { return stage>=0x100 && stage<=0x102; }
static int bk_progressive_valid(unsigned stage,int upgrade) {
    return bk_progressive_official(stage) && upgrade==(stage!=0x100);
}
#endif
