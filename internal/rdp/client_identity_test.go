package rdp

import "testing"

func TestClientBuildAndPlatformMetadata(t *testing.T) {
	core := make([]byte, 142)
	le.PutUint16(core[4:], 1920)
	le.PutUint16(core[6:], 1080)
	le.PutUint32(core[16:], 3078)
	le.PutUint16(core[140:], 0x100)
	data, err := parseConnect(dpiConnectPacket(core))
	if err != nil || data.clientBuild != 3078 {
		t.Fatalf("client build=%d: %v", data.clientBuild, err)
	}
	for _, os := range []uint16{0, 1, 3, 4, 5, 6, 7, 0xffff} {
		general := make([]byte, 20)
		le.PutUint16(general, os)
		le.PutUint16(general[2:], 6)
		cap := capability(1, general)
		packet := func(cap []byte, count int) []byte {
			return shareControl(3, join(u32(shareID), u16(userChannel), u16(0), u16(len(cap)+4), u16(count), u16(0), cap))
		}
		caps, err := confirmActiveCapabilities(packet(cap, 1))
		if err != nil || caps.osMajor != os || caps.osMinor != 6 {
			t.Fatalf("platform=%d/%d: %v", caps.osMajor, caps.osMinor, err)
		}
		if _, err := confirmActiveCapabilities(packet(join(cap, cap), 2)); err == nil {
			t.Fatal("accepted ambiguous duplicate General Capability Set")
		}
	}
}
