package hid

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
)

// Run the real reader/writer against a fragmented firmware peer. Discovery,
// USB and BLE use the same capability/status/ACK sequence.
func serialPeer(t *testing.T, caps byte, slots byte, rejectMode bool, versions ...byte) *serialHIDDevice {
	t.Helper()
	host, peer := net.Pipe()
	version := byte(serialProtocolVersion)
	if len(versions) != 0 {
		version = versions[0]
	}
	d := newSerialDeviceRW(host)
	t.Cleanup(func() { peer.Close(); d.closeLink() })
	go func() {
		defer peer.Close()
		mode := byte(0)
		reply := func(cmd byte, data [4]byte) bool {
			frame := serialHIDFrame(cmd, data)
			if _, err := peer.Write(frame[:3]); err != nil {
				return false
			}
			_, err := peer.Write(frame[3:])
			return err == nil
		}
		for {
			var frame [8]byte
			if _, err := io.ReadFull(peer, frame[:]); err != nil {
				return
			}
			cmd := frame[1]
			result := byte(0)
			if cmd == 4 {
				if rejectMode {
					result = 3
				} else if frame[2] == 8 {
					mode = 0x20
				} else {
					mode = 0
				}
			}
			if cmd == 0x22 || cmd == 4 {
				if !reply(0x83, [4]byte{version, caps, slots, 3}) {
					return
				}
				if !reply(0x80, [4]byte{slots}) {
					return
				}
				if !reply(0x81, [4]byte{mode}) {
					return
				}
			}
			if !reply(0x84, [4]byte{cmd, result}) {
				return
			}
		}
	}()
	return d
}

func TestSerialRejectsUnversionedAndFutureFirmware(t *testing.T) {
	for _, version := range []byte{0, 2} {
		d := serialPeer(t, capabilityKeyboard|capabilityRelative|capabilityUSB, 0, false, version)
		if err := probeSerialDevice(d); err == nil {
			t.Fatalf("accepted protocol version %d", version)
		}
	}
}

func TestSerialRejectsUnsupportedMouseMode(t *testing.T) {
	d := serialPeer(t, capabilityKeyboard|capabilityRelative|capabilityUSB, 0, false)
	if err := probeSerialDevice(d); err != nil {
		t.Fatal(err)
	}
	if err := configureSerialSession(d, true, SlotUnset); err == nil {
		t.Fatal("selected absolute mode without the capability")
	}
}

func TestUnifiedSerialUSBAndBLE(t *testing.T) {
	for _, tc := range []struct {
		name        string
		caps, slots byte
	}{
		{"USB", capabilityKeyboard | capabilityRelative | capabilityAbsolute | capabilityUSB, 0},
		{"BLE", capabilityKeyboard | capabilityRelative | capabilityAbsolute | capabilityBLE | capabilityProfiles, 1},
		{"ESP32-S3 four profiles", capabilityKeyboard | capabilityRelative | capabilityAbsolute | capabilityBLE | capabilityProfiles, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := serialPeer(t, tc.caps, tc.slots, false)
			if err := probeSerialDevice(d); err != nil {
				t.Fatal(err)
			}
			if err := configureSerialSession(d, true, SlotUnset); err != nil {
				t.Fatal(err)
			}
			if d.lastStatus().MouseMode != "absolute" {
				t.Fatal("mode not synchronized")
			}
			_, ble := (&Device{impl: d}).BLE()
			if ble != (tc.slots > 0) {
				t.Fatalf("BLE capability = %v", ble)
			}
			if tc.slots > 0 {
				if err := d.Pair(0); err != nil {
					t.Fatal(err)
				}
				if err := d.ProfileSelect(int(tc.slots) - 1); err != nil {
					t.Fatal(err)
				}
				if err := d.ProfileClear(int(tc.slots)); err == nil {
					t.Fatal("accepted a slot past the advertised count")
				}
			} else if err := d.ProfileSelect(0); err == nil {
				t.Fatal("USB accepted BLE profile")
			}
		})
	}
}

func TestSerialModeErrorPropagates(t *testing.T) {
	d := serialPeer(t, capabilityKeyboard|capabilityRelative|capabilityAbsolute|capabilityUSB, 0, true)
	if err := probeSerialDevice(d); err != nil {
		t.Fatal(err)
	}
	if err := configureSerialSession(d, true, SlotUnset); err == nil {
		t.Fatal("device error was ignored")
	}
}

func TestSerialNativeKeycodesAndCoordinates(t *testing.T) {
	var buf bytes.Buffer
	d := newTestNRFDevice(&buf)
	for _, key := range []byte{4, 0x68, 0xe0, 0xe7} {
		buf.Reset()
		if err := d.KeyDown(key); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(buf.Bytes(), serialHIDFrame(0x11, [4]byte{key, 1})) {
			t.Fatalf("key %x translated", key)
		}
	}
	for _, value := range []int{-1, 0, 16384, 32767, 40000} {
		buf.Reset()
		if err := d.MouseMove(value, value); err != nil {
			t.Fatal(err)
		}
		want := clamp(value, 0, 32767)
		frame := buf.Bytes()
		if int(binary.BigEndian.Uint16(frame[2:4])) != want || int(binary.BigEndian.Uint16(frame[4:6])) != want {
			t.Fatalf("coordinates %d encoded as %x", value, frame)
		}
	}
}

func TestSerialReplyRecoversAfterNoise(t *testing.T) {
	var buf bytes.Buffer
	d := newTestNRFDevice(&buf)
	frame := serialHIDFrame(0x83, [4]byte{1, 0x1f, 1, 3})
	for _, b := range append([]byte{0xff, 0x33, 0x11, 0}, frame...) {
		d.replyRxByte(b)
	}
	if st := d.lastStatus(); st.Version != 1 || st.Model != 3 || st.SlotCount != 1 {
		t.Fatalf("status: %+v", st)
	}
}

func TestPS2RelativeSession(t *testing.T) {
	d := serialPeer(t, capabilityKeyboard|capabilityRelative|capabilityPS2, 0, false)
	if err := probeSerialDevice(d); err != nil {
		t.Fatal(err)
	}
	if err := configureSerialSession(d, false, SlotUnset); err != nil {
		t.Fatal(err)
	}
	if err := configureSerialSession(d, true, SlotUnset); err == nil {
		t.Fatal("PS/2 accepted absolute mouse mode")
	}
}

func TestDiscoveryRecognizesPS2Output(t *testing.T) {
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	go func() {
		var request [8]byte
		if _, err := io.ReadFull(peer, request[:]); err != nil {
			return
		}
		if request[1] != 0x22 {
			return
		}
		for _, frame := range [][]byte{
			serialHIDFrame(0x83, [4]byte{1, capabilityKeyboard | capabilityRelative | capabilityPS2, 0, 1}),
			serialHIDFrame(0x80, [4]byte{}),
			serialHIDFrame(0x81, [4]byte{}),
		} {
			if _, err := peer.Write(frame); err != nil {
				return
			}
		}
	}()
	if !probeHIDBridgeConnection(client) {
		t.Fatal("PS/2 adapter was not discovered")
	}
}
