package airplay

import (
	"fmt"
	"log"
	"net"
	"strings"
	"unicode/utf8"
)

type advertisement interface{ Shutdown() }

type advertisementGroup []advertisement

func (g advertisementGroup) Shutdown() {
	for _, a := range g {
		a.Shutdown()
	}
}

func encodeTXT(records []string) []byte {
	var data []byte
	for _, record := range records {
		data = append(data, byte(len(record)))
		data = append(data, record...)
	}
	return data
}

func (r *Receiver) raopName() string {
	name := r.name
	// DNS service instance labels fit 63 bytes, including the MAC@ prefix.
	for len(name) > 50 {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	return strings.ToUpper(strings.ReplaceAll(r.deviceID, ":", "")) + "@" + name
}

func (r *Receiver) raopRecords() []string {
	records := []string{"txtvers=1", "ch=2", "cn=0,1,2,3", "sr=44100", "ss=16", "tp=UDP", "et=3,5", "pw=false", "sv=false", "da=true", "vn=65537", "vv=2", "ft=" + r.featureRecord(), "am=AppleTV3,2", "sf=0x4", "vs=220.68"}
	for _, record := range r.textRecords() {
		if strings.HasPrefix(record, "pk=") {
			records = append(records, record)
		}
	}
	return records
}

func (r *Receiver) featureBits() uint64 {
	if r.urlMedia || r.browserMedia {
		return features | 1<<0 | 1<<4
	}
	return features
}
func (r *Receiver) featureRecord() string {
	bits := r.featureBits()
	return fmt.Sprintf("0x%X,0x%X", uint32(bits), uint32(bits>>32))
}

func (r *Receiver) registerAdvertisements(port int, ifaces []net.Interface) (advertisement, error) {
	var group advertisementGroup
	for _, service := range []struct {
		name, kind string
		txt        []string
	}{
		{r.name, "_airplay._tcp", r.textRecords()},
		{r.raopName(), "_raop._tcp", r.raopRecords()},
	} {
		ad, err := registerService(service.name, service.kind, port, service.txt, ifaces)
		if err != nil {
			group.Shutdown()
			return nil, fmt.Errorf("%s: %w", service.kind, err)
		}
		group = append(group, ad)
	}
	for _, iface := range ifaces {
		log.Printf("airplay: Bonjour Mirror services on %s, control port %d", iface.Name, port)
	}
	log.Printf("airplay: advertised capabilities features=%s video=%t hls=%t web_media=%t", r.featureRecord(), r.featureBits()&(1<<0) != 0, r.featureBits()&(1<<4) != 0, r.browserMedia)
	return group, nil
}
