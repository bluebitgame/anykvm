package gamestream

import "testing"

func TestParseKeyIDSignedAndUnsigned(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  uint32
	}{
		{"0", 0}, {"42", 42}, {"2147483647", 0x7fffffff}, {"-2147483648", 0x80000000},
		{"2147483648", 0x80000000}, {"-1", 0xffffffff}, {"4294967295", 0xffffffff},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got, err := parseKeyID(tc.input)
			if err != nil || got != tc.want {
				t.Fatalf("got %#x, %v; want %#x", got, err, tc.want)
			}
		})
	}
	for _, input := range []string{"", "-2147483649", "4294967296", "18446744073709551615", "1.5", "garbage"} {
		if _, err := parseKeyID(input); err == nil {
			t.Errorf("accepted invalid key ID %q", input)
		}
	}
}
