//go:build e2e
// +build e2e

package e2e

type KubevipManifestValues struct {
	ControlPlaneVIP      string
	ImagePath            string
	ConfigPath           string
	SvcEnable            string
	SvcElectionEnable    string
	EnableEndpointslices string
	ControlPlaneEnable   string
	GobgpConfig          GoBGPConfigValues
}

type GoBGPConfigValues struct {
	AS              uint
	IP              string
	IPv4            string
	IPv6            string
	NeighborAddress string
	PeerAS          uint
	Port            uint
}
