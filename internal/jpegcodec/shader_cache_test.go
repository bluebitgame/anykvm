package jpegcodec

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestD3D11ShaderOptimizationModes(t *testing.T) {
	compiler, err := exec.LookPath("c++")
	if err != nil {
		t.Skip("C++ preprocessor unavailable")
	}
	for _, mode := range []struct {
		name    string
		defines []string
		fastopt bool
	}{
		{"offline", nil, false},
		{"runtime", []string{"-DANYKVM_RUNTIME_COMPILE=1"}, true},
		{"vulkan", []string{"-D__spirv__=1"}, false},
		{"vulkan_runtime_define", []string{"-D__spirv__=1", "-DANYKVM_RUNTIME_COMPILE=1"}, false},
	} {
		t.Run(mode.name, func(t *testing.T) {
			args := append([]string{"-E", "-P", "-x", "c++"}, mode.defines...)
			output, err := exec.Command(compiler, append(args, "shaders/jpeg420.hlsl")...).CombinedOutput()
			if err != nil {
				t.Fatalf("preprocess shader: %v\n%s", err, output)
			}
			if got := strings.Contains(string(output), "[fastopt]"); got != mode.fastopt {
				t.Fatalf("entropy fastopt=%v, want %v", got, mode.fastopt)
			}
		})
	}
}

// Run the native cache and compile_stage on any host. Mock COM blobs poison
// their bytes on release, so reconnects must use owned bytecode rather than
// retaining pointers into a released compiler DLL/blob.
func TestD3D11ShaderCacheReconnect(t *testing.T) {
	compiler, err := exec.LookPath("c++")
	if err != nil {
		t.Skip("C++ compiler unavailable")
	}
	raw, err := os.ReadFile("d3d11_windows.c")
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	start := strings.Index(source, "typedef HRESULT (WINAPI *compile_fn)")
	end := strings.Index(source, "int bk_jpeg_gpu_prepare_shaders(")
	if start < 0 || end <= start {
		t.Fatal("native shader cache section not found")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "shader_cache.hpp"), []byte(source[start:end]), 0600); err != nil {
		t.Fatal(err)
	}
	// This header is a test-only stand-in for FXC output, never a build asset.
	var header strings.Builder
	header.WriteString("typedef struct { const char *entry; const unsigned char *bytes; size_t size; } bk_jpeg_dxbc_stage;\n")
	header.WriteString("static const bk_jpeg_dxbc_stage bk_jpeg_dxbc_stages[] = {\n")
	for _, entry := range []string{"selectEntropy", "prepareEntropy", "prepareWork", "compareTiles", "main", "encodeMCUs", "indexSegments", "scanSegmentGroups", "addSegmentOffsets", "packJPEG"} {
		fmt.Fprintf(&header, "{\"%s\", (const unsigned char *)\"DXBC offline\", 12},\n", entry)
	}
	header.WriteString("};\n")
	if err := os.WriteFile(filepath.Join(dir, "jpeg420_dxbc.h"), []byte(header.String()), 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"runtime", "precompiled"} {
		t.Run(mode, func(t *testing.T) {
			program := filepath.Join(dir, "shader-cache-"+mode)
			args := []string{"-std=c++17", "-O2", "-pthread", "-I" + dir}
			if mode == "precompiled" {
				args = append(args, "-DBK_JPEG_PRECOMPILED")
			}
			command := exec.Command(compiler, append(args, "testdata/shader_cache.cpp", "-o", program)...)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("compile shader cache fixture: %v\n%s", err, output)
			}
			output, err := exec.Command(program).CombinedOutput()
			if err != nil {
				t.Fatalf("shader cache reconnect: %v\n%s", err, output)
			}
			for _, diagnostic := range []string{"compile started", "compile finished in", "bytecode cache hit", "CreateComputeShader finished"} {
				if !strings.Contains(string(output), diagnostic) {
					t.Fatalf("missing debug diagnostic %q: %s", diagnostic, output)
				}
			}
			if mode == "precompiled" && !strings.Contains(string(output), "precompiled DXBC") {
				t.Fatalf("missing precompiled shader diagnostic: %s", output)
			}
		})
	}
}
