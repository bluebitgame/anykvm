package airplay

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestMirrorDiscoveryIdentityAndTXT(t *testing.T) {
	r, err := NewReceiver("airplay:"+strings.Repeat("镜", 21), Options{})
	if err != nil {
		t.Fatal(err)
	}
	name := r.raopName()
	if len(name) > 63 || !utf8.ValidString(name) || !strings.HasPrefix(name, strings.ToUpper(strings.ReplaceAll(r.deviceID, ":", ""))+"@") {
		t.Fatal("invalid RAOP discovery identity", name)
	}
	airplay, raop := map[string]string{}, map[string]string{}
	for i, records := range [][]string{r.textRecords(), r.raopRecords()} {
		values := airplay
		if i == 1 {
			values = raop
		}
		encoded := encodeTXT(records)
		for _, expected := range records {
			if len(encoded) < 1 || int(encoded[0]) != len(expected) || len(encoded) < 1+len(expected) || string(encoded[1:1+len(expected)]) != expected {
				t.Fatal("invalid Bonjour TXT encoding")
			}
			encoded = encoded[1+len(expected):]
			key, value, _ := strings.Cut(expected, "=")
			values[key] = value
		}
		if len(encoded) != 0 {
			t.Fatal("trailing TXT data")
		}
	}
	if airplay["pk"] == "" || raop["pk"] != airplay["pk"] || raop["ft"] != airplay["features"] || raop["am"] != airplay["model"] || raop["sf"] != airplay["flags"] {
		t.Fatal("paired service discovery records disagree")
	}
	if airplay["features"] != fmt.Sprintf("0x%X,0x0", r.featureBits()) || airplay["pw"] != "false" {
		t.Fatal("screen mirroring capabilities changed")
	}
	if raop["cn"] != "0,1,2,3" || raop["tp"] != "UDP" {
		t.Fatal("companion audio sink capability missing")
	}
}
