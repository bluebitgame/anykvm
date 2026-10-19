package videoenc

import "fmt"

// A recovery picture must be independently decodable by a newly connected
// client. Cache current parameter sets when a driver returns them separately.
func raspberryPiPacket(data, header []byte, force bool) ([]byte, []byte, error) {
	packet, err := mediaFoundationPacket(data, header, "h264")
	if err != nil {
		return nil, nil, err
	}
	units, err := mediaFoundationUnits(packet, 4)
	if err != nil {
		return nil, nil, err
	}
	var parameters []byte
	slice, idr, sps, pps := false, false, false, false
	for _, unit := range units {
		switch unit[0] & 31 {
		case 1:
			slice = true
		case 5:
			slice, idr = true, true
		case 7, 8:
			sps = sps || unit[0]&31 == 7
			pps = pps || unit[0]&31 == 8
			parameters = append(parameters, mfStartCode...)
			parameters = append(parameters, unit...)
		}
	}
	if !slice {
		return nil, nil, fmt.Errorf("encoder returned no H.264 picture")
	}
	if force && !idr {
		return nil, nil, fmt.Errorf("encoder did not honor the IDR request")
	}
	if idr && (!sps || !pps) {
		return nil, nil, fmt.Errorf("H.264 recovery picture has no SPS/PPS")
	}
	if sps && pps {
		header = parameters
	}
	return packet, header, nil
}
