//go:build !darwin

package rdp

func macShortcutClient(uint16) bool                { return false }
func swapLeftModifiersClient(uint16) bool          { return false }
func (i *inputState) shortcutKey(code uint8) uint8 { return code }

func (i *inputState) withMacShortcut(_ uint8, send func() error) error { return send() }
func (i *inputState) shortcutRune(r rune) rune                         { return r }
