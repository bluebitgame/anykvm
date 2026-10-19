package rdp

import (
	"bytes"
	"crypto/tls"
	"strings"
	"testing"
)

// Decode the wire structures by their MS-RDPBCGR offsets, independently of
// logonInfo, so incorrect lengths or field placement fail interoperability tests.
func checkLogonInfo(t *testing.T, p []byte, domain, username string, infoType, sessionID uint32) {
	t.Helper()
	if len(p) < 598 || int(le.Uint16(p)) != len(p) || le.Uint16(p[2:])&15 != 7 ||
		p[14] != 38 || le.Uint32(p[18:]) != infoType {
		t.Fatalf("invalid Save Session Info header: %x", p)
	}
	p = p[22:]
	var domainData, userData []byte
	var gotSessionID uint32
	switch infoType {
	case 0:
		if len(p) != 576 || le.Uint32(p) > 52 || le.Uint32(p[56:]) > 512 {
			t.Fatal("invalid fixed logon structure")
		}
		domainData = p[4 : 4+le.Uint32(p)]
		userData = p[60 : 60+le.Uint32(p[56:])]
		gotSessionID = le.Uint32(p[572:])
	case 1:
		if le.Uint16(p) != 1 || le.Uint32(p[2:]) != 576 {
			t.Fatal("invalid long logon version or fixed size")
		}
		domainLen, userLen := int(le.Uint32(p[10:])), int(le.Uint32(p[14:]))
		if 576+domainLen+userLen != len(p) {
			t.Fatal("long logon string sizes do not match the packet")
		}
		domainData, userData = p[576:576+domainLen], p[576+domainLen:]
		gotSessionID = le.Uint32(p[6:])
	case 2:
		if len(p) != 576 || !bytes.Equal(p, make([]byte, 576)) {
			t.Fatal("invalid plain logon notification")
		}
		return
	default:
		t.Fatal("unexpected logon info type")
	}
	for i, field := range []struct {
		data []byte
		want string
	}{{domainData, domain}, {userData, username}} {
		if len(field.data) < 2 || le.Uint16(field.data[len(field.data)-2:]) != 0 {
			t.Fatalf("field %d missing null terminator", i)
		}
		got, err := utf16String(field.data[:len(field.data)-2])
		if err != nil || got != field.want {
			t.Fatalf("field %d: got %q, want %q (error=%v)", i, got, field.want, err)
		}
	}
	if gotSessionID != sessionID {
		t.Fatalf("session ID: got %d, want %d", gotSessionID, sessionID)
	}
}

func TestLogonInfoFormats(t *testing.T) {
	for _, tc := range []struct {
		name, domain, user string
		long               bool
		infoType           uint32
	}{
		{"fixed", "ANYKVM", "AnyKVM", false, 0},
		{"empty-domain", "", "anykvm", false, 0},
		{"unicode", "工作组", "user😀", false, 0},
		{"fixed-limit", strings.Repeat("d", 25), strings.Repeat("u", 255), false, 0},
		{"long", strings.Repeat("d", 26), strings.Repeat("😀", 256), true, 1},
		{"domain-overflow", strings.Repeat("d", 26), "user", false, 2},
		{"user-overflow", "domain", strings.Repeat("u", 256), false, 2},
		{"surrogate-overflow", "domain", strings.Repeat("😀", 128), false, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkLogonInfo(t, logonInfo(tc.domain, tc.user, 1234, tc.long), tc.domain, tc.user, tc.infoType, 1234)
		})
	}
}

func logonConfirmActive(long bool) []byte {
	general := make([]byte, 20)
	if long {
		le.PutUint16(general[10:], 4)
	}
	caps := capability(1, general)
	return shareControl(3, join(u32(shareID), u16(userChannel), u16(0), u16(4+len(caps)), u16(1), u16(0), caps))
}

func TestLogonCapabilityNegotiation(t *testing.T) {
	for _, long := range []bool{false, true} {
		caps, err := confirmActiveCapabilities(logonConfirmActive(long))
		if err != nil || caps.longCredentials != long {
			t.Fatalf("long=%t: %+v %v", long, caps, err)
		}
	}
	p := logonConfirmActive(true)
	p = p[:len(p)-1]
	le.PutUint16(p, uint16(len(p)))
	le.PutUint16(p[14:], uint16(len(p)-16))
	le.PutUint16(p[22:], 23)
	if _, err := confirmActiveCapabilities(p); err == nil {
		t.Fatal("accepted truncated General Capability Set")
	}
}

func TestLogonNotificationAfterFontMap(t *testing.T) {
	for _, authenticated := range []bool{false, true} {
		conn := &dvcRecordingConn{}
		c := channelTestClient()
		c.conn, c.dvcCaps, c.data.dvc = conn, false, 1004
		if authenticated {
			c.logonUser, c.logonDomain, c.logonSessionID = "AnyKVM", "ANYKVM", 77
		}
		if err := c.handleShare(shareData(39, make([]byte, 8))); err != nil {
			t.Fatal(err)
		}
		count, notifications := 0, 0
		for conn.data.Len() > 0 {
			p, fast, err := readPacket(&conn.data)
			if err != nil || fast {
				t.Fatalf("packet: %v", err)
			}
			_, payload, err := takePer(p[9:])
			if err != nil {
				t.Fatal(err)
			}
			if be.Uint16(p[6:]) == ioChannel {
				if count == 0 && payload[14] != 40 {
					t.Fatal("logon notification preceded Font Map")
				}
				if payload[14] == 38 {
					if count != 1 || !authenticated {
						t.Fatal("unexpected logon notification")
					}
					checkLogonInfo(t, payload, "ANYKVM", "AnyKVM", 0, 77)
					notifications++
				}
			}
			count++
		}
		if authenticated && notifications != 1 || !authenticated && notifications != 0 {
			t.Fatalf("authenticated=%t, notifications=%d", authenticated, notifications)
		}
		if err := c.sendLogonInfo(); err != nil || conn.data.Len() != 0 {
			t.Fatalf("repeated or unauthenticated notification: %v", err)
		}
	}
}

// Exercise the real connection state machine after authentication. In
// particular, empty Client Info after NLA must not erase the delegated identity.
func testAuthenticatedLogon(t *testing.T, conn *tls.Conn, domain, user string) {
	t.Helper()
	write := func(p []byte) {
		t.Helper()
		_, err := conn.Write(tpkt(join([]byte{2, 0xf0, 0x80, 0x64, 0, 1, 3, 0xeb, 0x70}, perLength(len(p)), p)))
		if err != nil {
			t.Fatal(err)
		}
	}
	read := func(want byte) []byte {
		t.Helper()
		p, fast, err := readPacket(conn)
		if err != nil || fast || len(p) < 9 {
			t.Fatalf("activation response: %v", err)
		}
		_, p, err = takePer(p[9:])
		if err != nil || len(p) < 18 || p[14] != want {
			t.Fatalf("expected share data type %d: %x %v", want, p, err)
		}
		return p
	}
	write(logonConfirmActive(true))
	read(31)
	read(20)
	write(shareData(39, make([]byte, 8)))
	read(40)
	checkLogonInfo(t, read(38), domain, user, 1, 1)
}
