//go:build e2e
// +build e2e

package e2e

import (
	"fmt"
	"net"
)

type KubevipManifestValues struct {
	ControlPlaneVIP      string
	ImagePath            string
	ConfigPath           string
	SvcEnable            string
	SvcElectionEnable    string
	EnableEndpointslices string
	ControlPlaneEnable   string
	BGPAS                uint32
	BGPPeers             string
}

// type BGPConfigValues struct {
// 	AS     uint
// 	IP     string
// 	IPv4   string
// 	IPv6   string
// 	PeerAS uint
// 	Port   uint
// }

type BGPPeerValues struct {
	IP string
	AS uint32
}

func (pv *BGPPeerValues) String() string {
	tmpIP := pv.IP
	ip := net.ParseIP(tmpIP)
	if ip == nil {
		return ""
	}

	if ip.To4() == nil {
		tmpIP = fmt.Sprintf("[%s]", tmpIP)
	}

	return fmt.Sprintf("%s:%d::false", tmpIP, pv.AS)
}
