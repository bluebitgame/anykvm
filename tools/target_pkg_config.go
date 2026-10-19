//go:build ignore

// target_pkg_config exposes only the prepared target SDL2 SDK to CGO.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	cflags, libs, sdl := false, false, false
	for _, arg := range os.Args[1:] {
		switch arg {
		case "--cflags":
			cflags = true
		case "--libs":
			libs = true
		case "sdl2":
			sdl = true
		case "--":
		default:
			fmt.Fprintln(os.Stderr, "Only SDL2 --cflags/--libs queries are supported by this target SDK.")
			os.Exit(1)
		}
	}
	prefix, triple := os.Getenv("ANYKVM_BUILD_SDL_PREFIX"), os.Getenv("ANYKVM_BUILD_SDL_TRIPLE")
	if !sdl || prefix == "" {
		fmt.Fprintln(os.Stderr, "The target SDL2 SDK is not configured.")
		os.Exit(1)
	}
	var flags []string
	if cflags {
		flags = append(flags, "-I"+filepath.Join(prefix, "include", "SDL2"))
		if triple != "" {
			flags = append(flags, "-I"+filepath.Join(prefix, "include", triple, "SDL2"))
		}
	}
	if libs {
		flags = append(flags, "-L"+filepath.Join(prefix, "lib", triple), "-lSDL2")
	}
	for i, flag := range flags {
		flags[i] = "'" + strings.ReplaceAll(flag, "'", "'\"'\"'") + "'"
	}
	fmt.Println(strings.Join(flags, " "))
}
