// Shared by Progressive/RemoteFX and JPEG. Adapters define the sampling,
// geometry, baseline storage and flag destination. Alpha never marks a tile.
#ifndef ANYKVM_COMPARE_GROUP
#define ANYKVM_COMPARE_GROUP(group) group
#endif
#ifndef ANYKVM_COMPARE_STATE
#define ANYKVM_COMPARE_STATE uint
#define ANYKVM_COMPARE_BEGIN(state,lane) state=0
#define ANYKVM_COMPARE_SAMPLE(state,x,y,pixel)
#define ANYKVM_COMPARE_END(state,group,lane,dirty)
#endif
groupshared uint groupDirty;
[numthreads(64,1,1)]
void compareTile(uint3 dispatchGroup : SV_GroupID,uint lane : SV_GroupIndex) {
    uint3 group=ANYKVM_COMPARE_GROUP(dispatchGroup);
    uint force=compareForce(),width=compareWidth(),height=compareHeight();
    if (lane==0) groupDirty=force;
    ANYKVM_COMPARE_STATE state;
    ANYKVM_COMPARE_BEGIN(state,lane);
    GroupMemoryBarrierWithGroupSync();
    uint changed=0;
    for (uint i=lane;i<4096;i+=64) {
        uint x=group.x*64+i%64,y=group.y*64+i/64;
        if (x>=width||y>=height) continue;
        uint pixel=comparePixel(x,y),offset=(y*width+x)*4;
        // Read before storing: JPEG uses one resident baseline buffer; RDP
        // keeps separate current/previous buffers until its wire commit.
        if (force==0 && ((pixel^comparePrevious(offset))&0x00ffffff)!=0) changed=1;
        compareStore(offset,pixel);
        ANYKVM_COMPARE_SAMPLE(state,x,y,pixel);
    }
    if (changed!=0) InterlockedOr(groupDirty,1);
    GroupMemoryBarrierWithGroupSync();
    ANYKVM_COMPARE_END(state,group,lane,groupDirty);
    if (lane==0) compareFlag(group.y*((width+63)/64)+group.x,groupDirty);
}
