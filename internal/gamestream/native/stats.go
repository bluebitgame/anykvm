package native

// PeerStats describes ENet's estimates for the reliable control connection.
// These are not video UDP loss statistics or one-way input latency.
type PeerStats struct {
	RTTMillis, RTTVariationMillis uint32
	ReliableLossPercent           float64
}
