package videoenc

import "sync"

// Codec declarations are queried lazily by the enabled protocol. They are not
// validated by encoding synthetic pictures; real driver failures belong to the
// actual stream and do not change the cached declaration.
type codecCapability struct {
	once      sync.Once
	supported bool
}

func (c *codecCapability) get(codec string, query func(string) bool) bool {
	c.once.Do(func() { c.supported = query(codec) })
	return c.supported
}

var av1Availability codecCapability

func SupportsAV1() bool { return av1Availability.get("av1", platformSupportsCodec) }
