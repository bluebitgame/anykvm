package rdp

import (
	"bytes"
	"fmt"
	"strconv"
)

// Windows CF_HTML uses byte offsets in an ASCII header. macOS supplies the
// HTML fragment itself, so wrap it when advertising and strictly unwrap it on
// receipt. Clients that send a bare fragment are also accepted.
func clipboardEncodeHTML(html []byte) []byte {
	const template = "Version:0.9\r\nStartHTML:%010d\r\nEndHTML:%010d\r\nStartFragment:%010d\r\nEndFragment:%010d\r\n"
	prefix := "<html><body><!--StartFragment-->"
	suffix := "<!--EndFragment--></body></html>"
	headerLength := len(fmt.Sprintf(template, 0, 0, 0, 0))
	startHTML := headerLength
	startFragment := startHTML + len(prefix)
	endFragment := startFragment + len(html)
	endHTML := endFragment + len(suffix)
	return append([]byte(fmt.Sprintf(template, startHTML, endHTML, startFragment, endFragment)+prefix), append(html, suffix...)...)
}

func clipboardDecodeHTML(data []byte) ([]byte, error) {
	if len(data) == 0 || len(data) > maxClipboardMessage {
		return nil, errProtocol
	}
	start, end := -1, -1
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if bytes.HasPrefix(line, []byte("StartFragment:")) {
			start, _ = strconv.Atoi(string(line[len("StartFragment:"):]))
		}
		if bytes.HasPrefix(line, []byte("EndFragment:")) {
			end, _ = strconv.Atoi(string(line[len("EndFragment:"):]))
		}
	}
	if start == -1 && end == -1 {
		return append([]byte(nil), data...), nil
	}
	if start < 0 || end < start || end > len(data) {
		return nil, errProtocol
	}
	return append([]byte(nil), data[start:end]...), nil
}
