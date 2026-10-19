package app

import (
	"log"

	"anykvm/internal/hid"
)

func effectiveMouseMode(mode string) string {
	if mode == "relative" {
		return "relative"
	}
	return "absolute"
}

func runPair(spec, mode string) {
	if err := hid.PairProfile(spec, mode); err != nil {
		log.Fatalf("AnyKVM: %v", err)
	}
}
