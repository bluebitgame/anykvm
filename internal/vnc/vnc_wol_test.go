package vnc

import (
	"net"
	"testing"
	"time"

	"anykvm/internal/wol"
)

func TestVNCClientWakeOnLAN(t *testing.T) {
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	udp.SetReadDeadline(time.Now().Add(5 * time.Second))
	reserved, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserved.Addr().String()
	reserved.Close()
	srv := NewServer(Config{Addr: address, Width: 2, Height: 2, Name: "Woken desktop"})
	started := make(chan error, 1)
	go func() {
		packet := make([]byte, 200)
		n, _, err := udp.ReadFromUDP(packet)
		if err != nil {
			started <- err
			return
		}
		if n != 102 {
			started <- &net.AddrError{Err: "invalid magic packet length", Addr: udp.LocalAddr().String()}
			return
		}
		started <- srv.Start()
	}()
	client, err := NewVNCClient("vnc://" + address)
	if err != nil {
		t.Fatal(err)
	}
	client.SetWakeOnLAN(wol.Options{MAC: "02:11:22:33:44:55", Broadcast: udp.LocalAddr().String()})
	openErr := client.Open()
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if openErr != nil {
		t.Fatal(openErr)
	}
	defer client.Close()
	if client.fbName != "Woken desktop" {
		t.Fatalf("unexpected desktop: %q", client.fbName)
	}
}
