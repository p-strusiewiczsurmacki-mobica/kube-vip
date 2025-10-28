package cluster

import (
	"context"
	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/vip"
)

// StartDDNS should start go routine for dhclient to hold the lease for the IP
// StartDDNS should wait until IP is allocated from DHCP, set it to cluster.Network
// so the OnStartedLeading can continue to configure the VIP initially
// during runtime if IP changes, startDDNS don't have to do reconfigure because
// dnsUpdater already have the functionality to keep trying resolve the IP
// and update the VIP configuration if it changes
func (cluster *Cluster) StartDDNS(ctx context.Context, network vip.Network) error {
	log.Debug("processig DDNS net")
	ddnsMgr := vip.NewDDNSManager(ctx, network)
	log.Debug("starting DDNS manager")
	ip, err := ddnsMgr.Start()
	if err != nil {
		log.Debug("ddnsMgr start", "err", err)
		return err
	}
	log.Debug("ddnsMgr got", "ip", ip)
	if err = network.SetIP(ip); err != nil {
		log.Debug("ddnsMgr set ip", "err", err)
		return err
	}

	return nil
}
