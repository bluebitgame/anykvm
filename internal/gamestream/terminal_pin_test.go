package gamestream

import (
	"context"
	"io"
	"testing"
	"time"
)

func TestTerminalPINCancelAndReuse(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	prompt := TerminalPIN(r)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := prompt(ctx, PairingRequest{}); err == nil {
		t.Fatal("cancelled prompt returned PIN")
	}
	done := make(chan string, 1)
	go func() {
		pin, err := prompt(context.Background(), PairingRequest{})
		if err != nil {
			done <- "error"
		} else {
			done <- pin
		}
	}()
	if _, err := io.WriteString(w, "1234\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case pin := <-done:
		if pin != "1234" {
			t.Fatalf("PIN: %q", pin)
		}
	case <-time.After(time.Second):
		t.Fatal("next prompt did not read PIN")
	}
}
