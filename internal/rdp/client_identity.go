package rdp

import "fmt"

// clientOSName describes the platform reported in the General Capability Set.
func clientOSName(major, minor uint16) string {
	switch major {
	case 0:
		return "Unspecified"
	case 1:
		switch minor {
		case 1:
			return "Windows 3.1x"
		case 2:
			return "Windows 95"
		case 3:
			return "Windows NT"
		case 9:
			return "Windows RT"
		default:
			return "Windows"
		}
	case 2:
		return "OS/2"
	case 3:
		return "Macintosh"
	case 4:
		return "UNIX"
	case 5:
		return "iOS"
	case 6:
		return "macOS"
	case 7:
		return "Android"
	case 8:
		return "Chrome OS"
	default:
		return fmt.Sprintf("Unknown (%d/%d)", major, minor)
	}
}
