package relay

// UDP is a relay for UDP influxdb writes
type UDP struct {
	config *UDPConfig
}

func NewUDP(config UDPConfig) (Relay, error) {
	panic("unimplemented")
}
