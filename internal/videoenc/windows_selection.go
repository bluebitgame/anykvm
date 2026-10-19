package videoenc

import (
	"errors"
	"fmt"
	"log"
)

// Vendor backends follow the build tag; Media Foundation stays available even
// when an FFmpeg-enabled build cannot load its runtime libraries.
func selectWindowsEncoder(o Options, hardware []string,
	vendor func(Options, string) (Encoder, error),
	system func(Options, bool) (Encoder, error),
	library func(Options, string, string) (Encoder, error)) (Encoder, error) {
	var failures []error
	compatibilityHardware := hardware
	if o.WindowsAdapter.Set {
		hardware = nil
		if suffix := windowsVendorEncoder(o.WindowsAdapter.VendorID); suffix != "" {
			hardware = []string{o.Codec + "_" + suffix}
		}
		log.Printf("videoenc: desktop adapter=%s; requesting exact GPU match", o.WindowsAdapter)
	}
	try := func(name string, open func() (Encoder, error)) Encoder {
		e, err := open()
		if err == nil {
			return e
		}
		log.Printf("videoenc: %s unavailable: %v", name, err)
		failures = append(failures, fmt.Errorf("%s: %w", name, err))
		return nil
	}
	for _, name := range hardware {
		if e := try(name, func() (Encoder, error) { return vendor(o, name) }); e != nil {
			log.Printf("videoenc: selected %s adapter=%s", e.Name(), o.WindowsAdapter)
			return e, nil
		}
	}
	if o.WindowsAdapter.Set {
		log.Printf("videoenc: desktop adapter=%s native texture encoder unavailable; compatibility fallback uses CPU readback/upload, encoder adapter=unbound", o.WindowsAdapter)
		// Compatibility backends do not promise an exact device binding.
		o.WindowsAdapter = WindowsAdapter{}
		for _, name := range compatibilityHardware {
			if e := try(name+" compatibility", func() (Encoder, error) { return vendor(o, name) }); e != nil {
				log.Printf("videoenc: compatibility selected %s adapter=unbound; desktop GPU import is not guaranteed", e.Name())
				return e, nil
			}
		}
	}
	if e := try("Media Foundation hardware", func() (Encoder, error) { return system(o, true) }); e != nil {
		return e, nil
	}
	if o.Codec == "h264" {
		if e := try("Media Foundation software", func() (Encoder, error) { return system(o, false) }); e != nil {
			return e, nil
		}
		if e := try("libx264", func() (Encoder, error) { return library(o, "libx264", "") }); e != nil {
			return e, nil
		}
	}
	return nil, errors.Join(failures...)
}

func windowsVendorEncoder(vendor uint32) string {
	switch vendor {
	case 0x10de:
		return "nvenc"
	case 0x1002:
		return "amf"
	case 0x8086:
		return "qsv"
	}
	return ""
}
