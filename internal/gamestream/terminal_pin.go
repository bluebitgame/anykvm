package gamestream

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"sync"

	"anykvm/internal/logging"
)

// One reader owns stdin for the process lifetime. Cancelling a request must
// not leave additional scanners racing to consume the next pairing PIN.
// TerminalPIN returns a cancellable PIN prompt sharing one input reader.
func TerminalPIN(input io.Reader) func(context.Context, PairingRequest) (string, error) {
	var once sync.Once
	lines := make(chan string, 1)
	gate := make(chan struct{}, 1)
	return func(ctx context.Context, request PairingRequest) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		select {
		case gate <- struct{}{}:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		defer func() { <-gate }()
		once.Do(func() {
			go func() {
				defer close(lines)
				scanner := bufio.NewScanner(input)
				for scanner.Scan() {
					lines <- strings.TrimSpace(scanner.Text())
				}
			}()
		})
		logging.Infof("GameStream pairing from %q (%s). Enter the four-digit PIN shown in the client, then press Enter:", request.ClientName, request.Address)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case pin, ok := <-lines:
			if err := ctx.Err(); err != nil {
				return "", err
			}
			if !ok {
				return "", errors.New("pairing terminal closed")
			}
			return pin, nil
		}
	}
}
