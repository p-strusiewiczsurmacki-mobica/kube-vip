package manager

import (
	"context"
	"fmt"
	"os"
	"syscall"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/bgp"
	"github.com/kube-vip/kube-vip/pkg/cluster"
	"github.com/kube-vip/kube-vip/pkg/equinixmetal"
	api "github.com/osrg/gobgp/v3/api"
	"github.com/packethost/packngo"
	"github.com/prometheus/client_golang/prometheus"
)

// Start will begin the Manager, which will start services and watch the configmap
func (sm *Manager) startBGP() error {
	var cpCluster *cluster.Cluster
	// var ns string
	var err error

	// If Equinix Metal is enabled then we can begin our preparation work
	var packetClient *packngo.Client
	if sm.config.EnableMetal {
		if sm.config.ProviderConfig != "" {
			key, project, err := equinixmetal.GetPacketConfig(sm.config.ProviderConfig)
			if err != nil {
				return err
			}
			// Set the environment variable with the key for the project
			os.Setenv("PACKET_AUTH_TOKEN", key)
			// Update the configuration with the project key
			sm.config.MetalProjectID = project

		}
		packetClient, err = packngo.NewClient()
		if err != nil {
			return err
		}

		// We're using Equinix Metal with BGP, populate the Peer information from the API
		if sm.config.EnableBGP {
			log.Info("Looking up the BGP configuration from Equinix Metal")
			err = equinixmetal.BGPLookup(packetClient, sm.config)
			if err != nil {
				return err
			}
		}
	}

	log.Info("Starting the BGP server to advertise VIP routes to BGP peers")

	sm.bgpServer, err = bgp.NewBGPServer(&sm.config.BGPConfig)
	if err != nil {
		return fmt.Errorf("creating BGP server: %w", err)
	}

	if err := sm.bgpServer.Start(func(p *api.WatchEventResponse_PeerEvent) {
		ipaddr := p.GetPeer().GetState().GetNeighborAddress()
		port := uint64(179)
		peerDescription := fmt.Sprintf("%s:%d", ipaddr, port)

		for stateName, stateValue := range api.PeerState_SessionState_value {
			metricValue := 0.0
			if stateValue == int32(p.GetPeer().GetState().GetSessionState().Number()) {
				metricValue = 1
			}

			sm.bgpServer.BGPSessionInfoGauge.With(prometheus.Labels{
				"state": stateName,
				"peer":  peerDescription,
			}).Set(metricValue)
		}
	}); err != nil {
		return err
	}

	// use a Go context so we can tell the leaderelection code when we
	// want to step down
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Defer a function to check if the bgpServer has been created and if so attempt to close it
	defer func() {
		if sm.bgpServer != nil {
			sm.bgpServer.Close()
		}
	}()

	// Shutdown function that will wait on this signal, unless we call it ourselves
	go func() {
		<-sm.signalChan
		log.Info("Received termination, signaling shutdown")
		if sm.config.EnableControlPlane {
			if cpCluster != nil {
				cpCluster.Stop()
			}
		}
		// Cancel the context, which will in turn cancel the leadership
		cancel()
	}()

	if sm.config.EnableControlPlane {
		cpCluster, err = cluster.InitCluster(sm.config, false)
		if err != nil {
			return err
		}

		clusterManager, err := initClusterManager(sm)
		if err != nil {
			return err
		}

		go func() {
			if sm.config.EnableLeaderElection {
				err = cpCluster.StartCluster(sm.config, clusterManager, sm.bgpServer)
			} else {
				err = cpCluster.StartVipService(sm.config, clusterManager, sm.bgpServer, packetClient)
			}
			if err != nil {
				log.Error("Control Plane", "err", err)
				// Trigger the shutdown of this manager instance
				sm.signalChan <- syscall.SIGINT
			}
		}()

		// Check if we're also starting the services, if not we can sit and wait on the closing channel and return here
		if !sm.config.EnableServices {
			<-sm.signalChan
			log.Info("Shutting down Kube-Vip")

			return nil
		}
	}

	err = sm.svcProcessor.ServicesWatcher(ctx, sm.svcProcessor.SyncServices)
	if err != nil {
		return err
	}

	log.Info("Shutting down Kube-Vip")

	return nil
}
