package gamestream

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"anykvm/internal/bridge"
	"anykvm/internal/capture"
)

func TestClientOSIgnoresUnverifiedHints(t *testing.T) {
	c := newClient(t)
	fp := fingerprint(c.leaf)
	s := &Server{state: identity{Clients: map[string]string{fp: string(c.pem)}}}
	for _, agent := range []string{"", "Mozilla/5.0", "okhttp/4.12.0", "Moonlight/1 (macOS)", "NotDarwin/24.2.0", "Darwin/"} {
		r := httptest.NewRequest("GET", "/launch?clientos=macos&devicename=MacBook&uniqueid=macos", nil)
		r.Header.Set("X-AnyKVM-Client-OS", "macos")
		r.Header.Set("User-Agent", agent)
		r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{c.leaf}}
		got := s.sessionClientInfo(r)
		if got.OS != ClientOSUnknown || got.OSSource != "unknown" || got.UsesAppleShortcuts() || got.UserAgent != agent || got.CertificateFingerprint != fp {
			t.Fatalf("unverified client hints selected OS: %+v", got)
		}
	}
	r := httptest.NewRequest("GET", "/launch", nil)
	if got := s.sessionClientInfo(r); got.CertificateFingerprint != "" {
		t.Fatal("plaintext request resolved a paired client")
	}
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{newClient(t).leaf}}
	if got := s.sessionClientInfo(r); got.CertificateFingerprint != "" {
		t.Fatal("unpaired certificate resolved client info")
	}
}

func TestOfficialIOSDarwinUserAgent(t *testing.T) {
	// Actual Moonlight iOS request captured in the upstream Sunshine report:
	// https://github.com/LizardByte/community/issues/626 (2024-12-30).
	const agent = "Moonlight/1 CFNetwork/1568.300.101 Darwin/24.2.0"
	c := newClient(t)
	fp := fingerprint(c.leaf)
	s := &Server{state: identity{Clients: map[string]string{fp: string(c.pem)}}}
	r := httptest.NewRequest("GET", "/launch", nil)
	r.Header.Set("User-Agent", agent)
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{c.leaf}}
	info := s.sessionClientInfo(r)
	if info.OS != ClientOSDarwin || info.OSSource != "user-agent" || !info.UsesAppleShortcuts() {
		t.Fatalf("official iOS UA did not select Apple family: %+v", info)
	}
	// No UA cannot be interpreted as evidence that the client is not Darwin.
	r.Header.Del("User-Agent")
	if info := s.sessionClientInfo(r); info.OS != ClientOSUnknown || info.OSSource != "unknown" {
		t.Fatalf("absent UA was classified: %+v", info)
	}
	r.Header.Set("User-Agent", agent)
	for _, os := range []string{"unknown", "windows", "linux", "macos", "ios", "darwin"} {
		s.cfg.ClientOS = map[string]string{fp: os}
		if info := s.sessionClientInfo(r); info.OS != ClientOS(os) || info.OSSource != "config" {
			t.Fatalf("UA overrode server OS assignment: %+v", info)
		}
	}
}

func TestClientAppleShortcutConvention(t *testing.T) {
	for _, os := range []ClientOS{ClientOSUnknown, ClientOSDarwin, ClientOSMacOS, ClientOSIOS, ClientOSWindows, ClientOSLinux, ClientOSAndroid} {
		info := ClientInfo{OS: os, OSSource: "config"}
		want := os == ClientOSDarwin || os == ClientOSMacOS || os == ClientOSIOS
		if info.UsesAppleShortcuts() != want {
			t.Fatalf("Apple shortcut convention for %s = %v", os, info.UsesAppleShortcuts())
		}
	}
}

func TestClientOSOverrides(t *testing.T) {
	fp := strings.Repeat("ab", 32)
	input := map[string]string{strings.ToUpper(fp): " MacOS "}
	got, err := ParseClientOSOverrides(input)
	if err != nil || got[fp] != "macos" {
		t.Fatalf("normalized = %v, error = %v", got, err)
	}
	input[strings.ToUpper(fp)] = "windows"
	if got[fp] != "macos" {
		t.Fatal("overrides retained caller map")
	}
	for _, invalid := range []map[string]string{
		{"0123456789ABCDEF": "macos"},
		{strings.Repeat("g", 64): "macos"},
		{fp: "automatic"},
		{fp: "macos", strings.ToUpper(fp): "windows"},
	} {
		if _, err := ParseClientOSOverrides(invalid); err == nil {
			t.Fatalf("accepted invalid overrides %v", invalid)
		}
		if _, err := NewServer(Config{ClientOS: invalid}); err == nil {
			t.Fatal("NewServer accepted invalid overrides")
		}
	}
}

type clientInfoHID struct {
	bridge.HIDSender
	events []string
}

func (h *clientInfoHID) KeyDown(k uint8) error {
	h.events = append(h.events, fmt.Sprintf("down:%x", k))
	return nil
}
func (h *clientInfoHID) KeyRelease(k uint8) error {
	h.events = append(h.events, fmt.Sprintf("up:%x", k))
	return nil
}
func (h *clientInfoHID) MouseMoveRel(x, y int) error {
	h.events = append(h.events, fmt.Sprintf("mouse:%d,%d", x, y))
	return nil
}

func clientKeyPacket(vk uint16, down bool) []byte {
	p := make([]byte, 14)
	binary.BigEndian.PutUint32(p, 10)
	kind := uint32(4)
	if down {
		kind = 3
	}
	binary.LittleEndian.PutUint32(p[4:], kind)
	binary.LittleEndian.PutUint16(p[9:], vk)
	return p
}

func TestClientOSSessionAndInputHook(t *testing.T) {
	s := testServer(t)
	first, second := pairClient(t, s), pairClient(t, s)
	client := tlsClient(t, s, &first.cert)
	other := tlsClient(t, s, &second.cert)
	fp := fingerprint(first.leaf)
	s.cfg.ClientOS = map[string]string{fp: "macos", fingerprint(second.leaf): "linux"}
	s.cfg.OnStreamStart = func(int, capture.VideoStreamOptions) error { return nil }
	base := &clientInfoHID{}
	s.cfg.HID = base
	wrapped := make(chan *clientInfoHID, 4)
	infos := make(chan ClientInfo, 4)
	s.cfg.WrapClientHID = func(info ClientInfo, hid bridge.HIDSender) bridge.HIDSender {
		infos <- info
		wrapper := &clientInfoHID{HIDSender: hid}
		wrapped <- wrapper
		return wrapper
	}
	if r := getReply(t, client, secureURL(s), "/serverinfo", url.Values{"clientos": {"macos"}}); r.Code != 200 {
		t.Fatal(r)
	}
	// A plaintext request with the same caller-controlled uniqueid cannot
	// replace metadata belonging to the paired TLS certificate.
	getReply(t, http.DefaultClient, plainURL(s), "/serverinfo", url.Values{"clientos": {"windows"}, "uniqueid": {first.q.Get("uniqueid")}})
	getReply(t, other, secureURL(s), "/serverinfo", url.Values{"clientos": {"linux"}, "uniqueid": {first.q.Get("uniqueid")}})
	q := url.Values{"appid": {fmt.Sprint(s.apps[0].ID)}, "rikey": {strings.Repeat("00", 16)}, "rikeyid": {"1"}}
	if r := getReply(t, client, secureURL(s), "/launch", q); r.Code != 200 {
		t.Fatal(r)
	}
	x := s.currentSession()
	info := <-infos
	mapper := <-wrapped
	if info.OS != ClientOSMacOS || info.OSSource != "config" || !info.UsesAppleShortcuts() || info.CertificateFingerprint != fp || x.client != info || x.input.hid != mapper {
		t.Fatalf("session info = %+v, hook info = %+v", x.client, info)
	}
	x.input.mu.Lock()
	x.input.active = true
	x.input.mu.Unlock()
	for _, p := range [][]byte{clientKeyPacket(0x5b, true), clientKeyPacket('C', true), clientKeyPacket('C', false), relativePacket(2, 3)} {
		if err := x.input.handle(p); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(mapper.events, []string{"down:e3", "down:6", "up:6", "mouse:2,3"}) || len(base.events) != 0 {
		t.Fatalf("input route or Command+C changed: %v, base: %v", mapper.events, base.events)
	}
	getReply(t, client, secureURL(s), "/serverinfo", url.Values{"clientos": {"windows"}})
	if x.client.OS != ClientOSMacOS {
		t.Fatal("authenticated metadata update changed active input policy")
	}
	q.Set("rikey", strings.Repeat("01", 16))
	if r := getReply(t, client, secureURL(s), "/resume", q); r.Code != 200 {
		t.Fatal(r)
	}
	info, nextMapper := <-infos, <-wrapped
	if info.OS != ClientOSMacOS || nextMapper == mapper {
		t.Fatalf("resume did not create a fresh input policy: %+v", info)
	}
	if got := mapper.events[len(mapper.events)-1]; got != "up:e3" {
		t.Fatalf("resume did not release held Command through old mapper: %s", got)
	}
	getReply(t, client, secureURL(s), "/cancel", nil)
	getReply(t, client, secureURL(s), "/unpair", nil)
	if r := getReply(t, client, secureURL(s), "/launch", q); r.Code != 401 {
		t.Fatal("OS assignment authorized an unpaired client", r)
	}
}

func TestClientOSConfigOverrideAndDefaultHID(t *testing.T) {
	for _, mode := range []string{"unknown", "config", "nil-wrapper"} {
		t.Run(mode, func(t *testing.T) {
			s := testServer(t)
			c := pairClient(t, s)
			client := tlsClient(t, s, &c.cert)
			base := &clientInfoHID{}
			s.cfg.HID = base
			s.cfg.OnStreamStart = func(int, capture.VideoStreamOptions) error { return nil }
			if mode == "config" {
				s.cfg.ClientOS = map[string]string{fingerprint(c.leaf): "macos"}
			}
			if mode == "nil-wrapper" {
				s.cfg.WrapClientHID = func(ClientInfo, bridge.HIDSender) bridge.HIDSender { return nil }
			}
			q := url.Values{"appid": {fmt.Sprint(s.apps[0].ID)}, "rikey": {strings.Repeat("00", 16)}, "rikeyid": {"1"}}
			if mode == "config" {
				q.Set("clientos", "windows")
			}
			if r := getReply(t, client, secureURL(s), "/launch", q); r.Code != 200 {
				t.Fatal(r)
			}
			x := s.currentSession()
			wantOS, source := ClientOSUnknown, "unknown"
			if mode == "config" {
				wantOS, source = ClientOSMacOS, "config"
			}
			if x.client.OS != wantOS || x.client.OSSource != source || x.input.hid != base {
				t.Fatalf("default input or config priority changed: %+v", x.client)
			}
		})
	}
}
