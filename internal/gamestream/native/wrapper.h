#include <stdlib.h>
#include <string.h>
#include "enet/enet.h"
#include "rs.h"
#ifndef _WIN32
#include <netdb.h>
#include <arpa/inet.h>
#endif

static int bk_addr(ENetAddress *a, const char *ip, int port) {
    memset(a,0,sizeof(*a));
    if (enet_address_set_host(a,ip)) return -1;
    return enet_address_set_port(a,(enet_uint16)port);
}
static ENetHost *bk_host(const char *ip,int port) {
    ENetAddress a;
    if(bk_addr(&a,ip,port))return NULL;
    ENetHost *h=enet_host_create(a.address.ss_family,&a,8,0x30,0,0);
    if(h){h->maximumPacketSize=65536;h->maximumWaitingData=1024*1024;}
    return h;
}
static int bk_port(ENetHost *h) {
    ENetAddress a; if(enet_socket_get_address(h->socket,&a))return 0;
    if(a.address.ss_family==AF_INET)return ntohs(((struct sockaddr_in*)&a.address)->sin_port);
    return ntohs(((struct sockaddr_in6*)&a.address)->sin6_port);
}
static void bk_address(ENetPeer *p,char *out,size_t size) {
    out[0]=0;
    getnameinfo((struct sockaddr*)&p->address.address,p->address.addressLength,out,(socklen_t)size,NULL,0,NI_NUMERICHOST);
}
static int bk_send(ENetHost *h,unsigned int peer,void *data,size_t size) {
    if(peer>=h->peerCount)return -1;
    ENetPacket *p=enet_packet_create(data,size,ENET_PACKET_FLAG_RELIABLE);
    if(!p)return -1;
    if(enet_peer_send(&h->peers[peer],0,p)){enet_packet_destroy(p);return -1;}
    enet_host_flush(h);return 0;
}
static void bk_reset(ENetHost *h,unsigned int peer) {if(peer<h->peerCount)enet_peer_disconnect_now(&h->peers[peer],0);}
static int bk_connect(ENetHost *h,const char *ip,int port,unsigned int data) {
    ENetAddress a;if(bk_addr(&a,ip,port))return -1;
    return enet_host_connect(h,&a,0x30,data)?0:-1;
}
static int bk_fec(unsigned char *buf,int data,int parity,int size,int stride,int audio,unsigned char *missing) {
    reed_solomon *rs=reed_solomon_new(data,parity);
    if(!rs)return -1;
    if(audio){unsigned char p[]={0x77,0x40,0x38,0x0e,0xc7,0xa7,0x0d,0x6c};memcpy(rs->p,p,8);}
    unsigned char *shards[255];for(int i=0;i<data+parity;i++)shards[i]=buf+(size_t)i*stride;
    int result=missing?reed_solomon_decode(rs,shards,missing,data+parity,size):reed_solomon_encode(rs,shards,data+parity,size);
    reed_solomon_release(rs);return result;
}

static int bk_peer_stats(ENetHost *h, unsigned int peer, unsigned int *rtt, unsigned int *variance, unsigned int *loss) {
    if (!h || peer >= h->peerCount || h->peers[peer].state != ENET_PEER_STATE_CONNECTED) return 0;
    ENetPeer *p = &h->peers[peer];
    *rtt = p->roundTripTime;
    *variance = p->roundTripTimeVariance;
    *loss = p->packetLoss;
    return 1;
}
