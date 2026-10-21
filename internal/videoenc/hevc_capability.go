package videoenc

var hevcAvailability codecCapability

func SupportsHEVC() bool { return hevcAvailability.get("hevc", platformSupportsCodec) }
