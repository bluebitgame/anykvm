//go:build darwin && cgo

package airplay

/*
#include <dns_sd.h>
#include <arpa/inet.h>
#include <poll.h>
#include <stdlib.h>
#include <errno.h>

typedef struct {
    DNSServiceRef ref;
    int registered;
    DNSServiceErrorType result;
} anykvm_airplay_ad;

static void anykvm_airplay_registered(DNSServiceRef ref, DNSServiceFlags flags,
    DNSServiceErrorType error, const char *name, const char *type,
    const char *domain, void *context) {
    anykvm_airplay_ad *ad = context;
    ad->registered = 1;
    ad->result = error;
}

static int anykvm_airplay_process(anykvm_airplay_ad *ad, int timeout) {
    struct pollfd fd = { DNSServiceRefSockFD(ad->ref), POLLIN, 0 };
    int n = poll(&fd, 1, timeout);
    if (n < 0) return errno == EINTR ? 0 : -1;
    if (n == 0) return 0;
    if (!(fd.revents & POLLIN)) return -1;
    DNSServiceErrorType error = DNSServiceProcessResult(ad->ref);
    if (error != kDNSServiceErr_NoError) return error;
    return ad->result;
}

static anykvm_airplay_ad *anykvm_airplay_register(const char *name,
    const char *type, unsigned int index, unsigned short port,
    const void *txt, unsigned short length, int *error) {
    anykvm_airplay_ad *ad = calloc(1, sizeof(*ad));
    if (!ad) { *error = kDNSServiceErr_NoMemory; return NULL; }
    *error = DNSServiceRegister(&ad->ref, kDNSServiceFlagsNoAutoRename,
        index, name, type, "local.", NULL, htons(port), length, txt,
        anykvm_airplay_registered, ad);
    if (*error != kDNSServiceErr_NoError) { free(ad); return NULL; }
    return ad;
}

static void anykvm_airplay_free(anykvm_airplay_ad *ad) {
    DNSServiceRefDeallocate(ad->ref);
    free(ad);
}
*/
import "C"

import (
	"fmt"
	"log"
	"net"
	"sync"
	"time"
	"unsafe"
)

// Register with the system mDNSResponder. It owns multicast sockets, correct
// interface address records and registration conflict handling on macOS.
type bonjourAdvertisement struct {
	handle *C.anykvm_airplay_ad
	stop   chan struct{}
	once   sync.Once
	wg     sync.WaitGroup
}

func registerService(name, service string, port int, records []string, ifaces []net.Interface) (advertisement, error) {
	var group advertisementGroup
	for _, iface := range ifaces {
		ad, err := registerBonjour(name, service, port, records, iface.Index)
		if err != nil {
			group.Shutdown()
			return nil, fmt.Errorf("Bonjour interface %s: %w", iface.Name, err)
		}
		group = append(group, ad)
	}
	return group, nil
}

func registerBonjour(name, service string, port int, records []string, index int) (*bonjourAdvertisement, error) {
	cname, ctype := C.CString(name), C.CString(service)
	defer C.free(unsafe.Pointer(cname))
	defer C.free(unsafe.Pointer(ctype))
	txt := encodeTXT(records)
	ctxt := C.CBytes(txt)
	defer C.free(ctxt)
	var code C.int
	handle := C.anykvm_airplay_register(cname, ctype, C.uint(index), C.ushort(port), ctxt, C.ushort(len(txt)), &code)
	if handle == nil {
		return nil, fmt.Errorf("DNSServiceRegister error %d", code)
	}
	// Ready means mDNSResponder acknowledged registration, not just that the
	// local IPC socket opened. Keep callback state in C, never a retained Go pointer.
	for deadline := time.Now().Add(3 * time.Second); handle.registered == 0; {
		if code := C.anykvm_airplay_process(handle, 200); code != 0 {
			C.anykvm_airplay_free(handle)
			return nil, fmt.Errorf("Bonjour registration error %d", code)
		}
		if time.Now().After(deadline) {
			C.anykvm_airplay_free(handle)
			return nil, fmt.Errorf("Bonjour registration acknowledgement timed out")
		}
	}
	ad := &bonjourAdvertisement{handle: handle, stop: make(chan struct{})}
	ad.wg.Add(1)
	go func() {
		defer ad.wg.Done()
		defer C.anykvm_airplay_free(handle)
		for {
			select {
			case <-ad.stop:
				return
			default:
			}
			if code := C.anykvm_airplay_process(handle, 200); code != 0 {
				log.Printf("airplay: Bonjour service %q ended: error %d", name, code)
				return
			}
		}
	}()
	return ad, nil
}

func (a *bonjourAdvertisement) Shutdown() {
	a.once.Do(func() { close(a.stop) })
	a.wg.Wait()
}
