<<<<<<<< HEAD:pkg/cluster/instance.go
package cluster
========
package services
>>>>>>>> 9aff693 (Moved endpoint-related code from pkg/manager to pkg/endpoints):pkg/services/instance.go

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
<<<<<<<< HEAD:pkg/cluster/instance.go
========
	"time"
>>>>>>>> 9aff693 (Moved endpoint-related code from pkg/manager to pkg/endpoints):pkg/services/instance.go

	log "log/slog"

	"github.com/vishvananda/netlink"
	v1 "k8s.io/api/core/v1"

	"github.com/kube-vip/kube-vip/pkg/arp"
	"github.com/kube-vip/kube-vip/pkg/kubevip"
	"github.com/kube-vip/kube-vip/pkg/networkinterface"
	"github.com/kube-vip/kube-vip/pkg/vip"
)

const (
	// Hardware address of the host that has the VIP
	HWAddrKey = "kube-vip.io/hwaddr"

	// The IP address that is requested
	RequestedIP = "kube-vip.io/requestedIP"

	LoadbalancerHostname     = "kube-vip.io/loadbalancerHostname"
	ServiceInterface         = "kube-vip.io/serviceInterface"
	LoadbalancerIPAnnotation = "kube-vip.io/loadbalancerIPs"
)

// Instance defines an instance of everything needed to manage vips
type Instance struct {
	// Virtual IP / Load Balancer configuration
<<<<<<<< HEAD:pkg/cluster/instance.go
	VIPConfigs []*kubevip.Config

	// cluster instances
	Clusters []*Cluster

	// Service uses DHCP
	IsDHCP              bool
	DHCPInterface       string
	DHCPInterfaceHwaddr string
	DHCPInterfaceIP     string
	DHCPHostname        string
	DHCPClient          *vip.DHCPClient

	// External Gateway IP the service is forwarded from
	UPNPGatewayIPs []string
========
	VipConfigs []*kubevip.Config

	// cluster instances
	Clusters []*cluster.Cluster

	// Service uses DHCP
	IsDHCP              bool
	DhcpInterface       string
	DhcpInterfaceHwaddr string
	DhcpInterfaceIP     string
	dhcpHostname        string
	DhcpClient          *vip.DHCPClient
	HasEndpoints        bool

	// External Gateway IP the service is forwarded from
	UpnpGatewayIPs []string
>>>>>>>> 9aff693 (Moved endpoint-related code from pkg/manager to pkg/endpoints):pkg/services/instance.go

	// Kubernetes service mapping
	ServiceSnapshot *v1.Service
}

type Port struct {
	Port uint16
	Type string
}

<<<<<<<< HEAD:pkg/cluster/instance.go
func NewInstance(svc *v1.Service, config *kubevip.Config, intfMgr *networkinterface.Manager, arpMgr *arp.Manager) (*Instance, error) {
========
func NewInstance(svc *v1.Service, config *kubevip.Config) (*Instance, error) {
>>>>>>>> 9aff693 (Moved endpoint-related code from pkg/manager to pkg/endpoints):pkg/services/instance.go
	instanceAddresses := FetchServiceAddresses(svc)
	//instanceUID := string(svc.UID)

	var newVips []*kubevip.Config
	var link netlink.Link
	var err error

	for _, address := range instanceAddresses {
		// Detect if we're using a specific interface for services
		var svcInterface string
<<<<<<<< HEAD:pkg/cluster/instance.go
		svcInterface = svc.Annotations[ServiceInterface] // If the service has a specific interface defined, then use it
========
		svcInterface = svc.Annotations[kubevip.ServiceInterface] // If the service has a specific interface defined, then use it
>>>>>>>> 9aff693 (Moved endpoint-related code from pkg/manager to pkg/endpoints):pkg/services/instance.go
		if svcInterface == kubevip.Auto {
			link, err = autoFindInterface(address)
			if err != nil {
				log.Error("automatically discover network interface for annotated IP", "address", address, "err", err)
			} else {
				if link == nil {
					log.Error("automatically discover network interface for annotated IP address", "address", address)
				}
			}
			if link == nil {
				svcInterface = ""
			} else {
				svcInterface = getAutoInterfaceName(link, config.Interface)
			}
		}
		// If it is still blank then use the
		if svcInterface == "" {
			switch config.ServicesInterface {
			case kubevip.Auto:
				link, err = autoFindInterface(address)
				if err != nil {
					log.Error("failed to automatically discover network interface for address", "ip", address, "err", err, "interface", config.Interface)
				} else if link == nil {
					log.Error("failed to automatically discover network interface for address", "ip", address, "defaulting to", config.Interface)
				}
				svcInterface = getAutoInterfaceName(link, config.Interface)
			case "":
				svcInterface = config.Interface
			default:
				svcInterface = config.ServicesInterface
			}
		}

		if link == nil {
			if link, err = netlink.LinkByName(svcInterface); err != nil {
				return nil, fmt.Errorf("failed to get interface %s: %w", svcInterface, err)
			}
			if link == nil {
				return nil, fmt.Errorf("failed to get interface %s", svcInterface)
			}
		}

		cidrs := vip.Split(config.VIPSubnet)

		ipv4AutoSubnet := false
		ipv6AutoSubnet := false
		if cidrs[0] == kubevip.Auto {
			ipv4AutoSubnet = true
		}

		if len(cidrs) > 1 && cidrs[1] == kubevip.Auto {
			ipv6AutoSubnet = true
		}

		if (config.Address != "" || config.VIP != "") && (ipv4AutoSubnet || ipv6AutoSubnet) {
			return nil, fmt.Errorf("auto subnet discovery cannot be used if VIP address was provided")
		}

		subnet := ""
		var err error
		if vip.IsIPv4(address) {
			if ipv4AutoSubnet {
				subnet, err = autoFindSubnet(link, address)
				if err != nil {
					return nil, fmt.Errorf("failed to automatically find subnet for service %s/%s with IP address %s on interface %s: %w", svc.Namespace, svc.Name, address, svcInterface, err)
				}
			} else {
				if cidrs[0] != "" && cidrs[0] != kubevip.Auto {
					subnet = cidrs[0]
				} else {
					subnet = "32"
				}
			}
		} else {
			if ipv6AutoSubnet {
				subnet, err = autoFindSubnet(link, address)
				if err != nil {
					return nil, fmt.Errorf("failed to automatically find subnet for service %s/%s with IP address %s on interface %s: %w", svc.Namespace, svc.Name, address, svcInterface, err)
				}
			} else {
				if len(cidrs) > 1 && cidrs[1] != "" && cidrs[1] != kubevip.Auto {
					subnet = cidrs[1]
				} else {
					subnet = "128"
				}
			}
		}

		//log.Info("new instance", "svc", *svc, "interface", svcInterface)

		// Generate new Virtual IP configuration
		newVips = append(newVips, &kubevip.Config{
			VIP:                    address,
			Interface:              svcInterface,
			SingleNode:             true,
			EnableARP:              config.EnableARP,
			EnableBGP:              config.EnableBGP,
			VIPSubnet:              subnet,
			EnableRoutingTable:     config.EnableRoutingTable,
			RoutingTableID:         config.RoutingTableID,
			RoutingTableType:       config.RoutingTableType,
			RoutingProtocol:        config.RoutingProtocol,
			ArpBroadcastRate:       config.ArpBroadcastRate,
			EnableServiceSecurity:  config.EnableServiceSecurity,
			DNSMode:                config.DNSMode,
			DisableServiceUpdates:  config.DisableServiceUpdates,
			EnableServicesElection: config.EnableServicesElection,
			KubernetesLeaderElection: kubevip.KubernetesLeaderElection{
				EnableLeaderElection: config.EnableLeaderElection,
			},
		})
	}

	// Create new service
	instance := &Instance{
		//UID:             instanceUID,
		//VIPs:            instanceAddresses,
		ServiceSnapshot: svc,
	}
	// for _, port := range svc.Spec.Ports {
	// 	instance.ExternalPorts = append(instance.ExternalPorts, Port{
	// 		Port: uint16(port.Port), //nolint
	// 		Type: string(port.Protocol),
	// 	})
	// }

	if svc.Annotations != nil {
<<<<<<<< HEAD:pkg/cluster/instance.go
		instance.DHCPInterfaceHwaddr = svc.Annotations[HWAddrKey]
		instance.DHCPInterfaceIP = svc.Annotations[RequestedIP]
		instance.DHCPHostname = svc.Annotations[LoadbalancerHostname]
========
		instance.DhcpInterfaceHwaddr = svc.Annotations[kubevip.HwAddrKey]
		instance.DhcpInterfaceIP = svc.Annotations[kubevip.RequestedIP]
		instance.dhcpHostname = svc.Annotations[kubevip.LoadbalancerHostname]
>>>>>>>> 9aff693 (Moved endpoint-related code from pkg/manager to pkg/endpoints):pkg/services/instance.go
	}

	configPorts := make([]kubevip.Port, 0)
	for _, p := range svc.Spec.Ports {
		configPorts = append(configPorts, kubevip.Port{
			Type: string(p.Protocol),
			Port: int(p.Port),
		})
	}
	// Generate Load Balancer config
	newLB := kubevip.LoadBalancer{
		Name:      fmt.Sprintf("%s-load-balancer", svc.Name),
		Ports:     configPorts,
		BindToVip: true,
	}
	for _, vip := range newVips {
		// Add Load Balancer Configuration
		vip.LoadBalancers = append(vip.LoadBalancers, newLB)
	}
	// Create Add configuration to the new service
<<<<<<<< HEAD:pkg/cluster/instance.go
	instance.VIPConfigs = newVips
========
	instance.VipConfigs = newVips
>>>>>>>> 9aff693 (Moved endpoint-related code from pkg/manager to pkg/endpoints):pkg/services/instance.go

	// If this was purposely created with the address 0.0.0.0,
	// we will create a macvlan on the main interface and a DHCP client
	// TODO: Consider how best to handle DHCP with multiple addresses
	if len(instanceAddresses) == 1 && instanceAddresses[0] == "0.0.0.0" {
		err := instance.startDHCP()
		if err != nil {
			return nil, err
		}
		select {
<<<<<<<< HEAD:pkg/cluster/instance.go
		case err := <-instance.DHCPClient.ErrorChannel():
			return nil, fmt.Errorf("error starting DHCP for %s/%s: error: %s",
				instance.ServiceSnapshot.Namespace, instance.ServiceSnapshot.Name, err)
		case ip := <-instance.DHCPClient.IPChannel():
			instance.VIPConfigs[0].Interface = instance.DHCPInterface
			instance.VIPConfigs[0].VIP = ip
			instance.DHCPInterfaceIP = ip
		}
	}

	for _, vipConfig := range instance.VIPConfigs {
		c, err := InitCluster(vipConfig, false, intfMgr, arpMgr)
========
		case err := <-instance.DhcpClient.ErrorChannel():
			return nil, fmt.Errorf("error starting DHCP for %s/%s: error: %s",
				instance.ServiceSnapshot.Namespace, instance.ServiceSnapshot.Name, err)
		case ip := <-instance.DhcpClient.IPChannel():
			instance.VipConfigs[0].Interface = instance.DhcpInterface
			instance.VipConfigs[0].VIP = ip
			instance.DhcpInterfaceIP = ip
		}
	}

	for _, vipConfig := range instance.VipConfigs {
		c, err := cluster.InitCluster(vipConfig, false)
>>>>>>>> 9aff693 (Moved endpoint-related code from pkg/manager to pkg/endpoints):pkg/services/instance.go
		if err != nil {
			log.Error("Failed to add Service %s/%s", svc.Namespace, svc.Name)
			return nil, err
		}

		for i := range c.Network {
			c.Network[i].SetServicePorts(svc)
		}

		instance.Clusters = append(instance.Clusters, c)
		log.Info("(svcs) adding VIP", "ip", vipConfig.VIP, "interface", vipConfig.Interface, "namespace", svc.Namespace, "name", svc.Name)

	}

	return instance, nil
}

func autoFindInterface(ip string) (netlink.Link, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, fmt.Errorf("failed to list network interfaces: %w", err)
	}

	address := net.ParseIP(ip)

	family := netlink.FAMILY_V4

	if address.To4() == nil {
		family = netlink.FAMILY_V6
	}

	for _, link := range links {
		addr, err := netlink.AddrList(link, family)
		if err != nil {
			return nil, fmt.Errorf("failed to get IP addresses for interface %s: %w", link.Attrs().Name, err)
		}
		for _, a := range addr {
			if a.IPNet.Contains(address) {
				return link, nil
			}
		}
	}

	return nil, nil
}

func autoFindSubnet(link netlink.Link, ip string) (string, error) {
	address := net.ParseIP(ip)

	family := netlink.FAMILY_V4
	if address.To4() == nil {
		family = netlink.FAMILY_V6
	}

	addr, err := netlink.AddrList(link, family)
	if err != nil {
		return "", fmt.Errorf("failed to get IP addresses for interface %s: %w", link.Attrs().Name, err)
	}
	for _, a := range addr {
		if a.IPNet.Contains(address) {
			m, _ := a.IPNet.Mask.Size()
			return strconv.Itoa(m), nil
		}
	}
	return "", fmt.Errorf("failed to find suitable subnet for address %s", ip)
}

func getAutoInterfaceName(link netlink.Link, defaultInterface string) string {
	if link == nil {
		return defaultInterface
	}
	return link.Attrs().Name
}

func (i *Instance) startDHCP() error {
<<<<<<<< HEAD:pkg/cluster/instance.go
	if len(i.VIPConfigs) != 1 {
		return fmt.Errorf("DHCP requires exactly 1 VIP config, got: %v", len(i.VIPConfigs))
	}
	parent, err := netlink.LinkByName(i.VIPConfigs[0].Interface)
========
	if len(i.VipConfigs) != 1 {
		return fmt.Errorf("DHCP requires exactly 1 VIP config, got: %v", len(i.VipConfigs))
	}
	parent, err := netlink.LinkByName(i.VipConfigs[0].Interface)
>>>>>>>> 9aff693 (Moved endpoint-related code from pkg/manager to pkg/endpoints):pkg/services/instance.go
	if err != nil {
		return fmt.Errorf("error finding VIP Interface, for building DHCP Link : %v", err)
	}

	// Generate name from UID
	interfaceName := fmt.Sprintf("vip-%s", i.ServiceSnapshot.UID[0:8])

	// Check if the interface doesn't exist first
	iface, err := net.InterfaceByName(interfaceName)
	if err != nil {
		log.Info("creating new macvlan interface for DHCP", "interface", interfaceName)

<<<<<<<< HEAD:pkg/cluster/instance.go
		hwaddr, err := net.ParseMAC(i.DHCPInterfaceHwaddr)
		if i.DHCPInterfaceHwaddr != "" && err != nil {
========
		hwaddr, err := net.ParseMAC(i.DhcpInterfaceHwaddr)
		if i.DhcpInterfaceHwaddr != "" && err != nil {
>>>>>>>> 9aff693 (Moved endpoint-related code from pkg/manager to pkg/endpoints):pkg/services/instance.go
			return err
		} else if hwaddr == nil {
			hwaddr, err = net.ParseMAC(vip.GenerateMac())
			if err != nil {
				return err
			}
		}

		log.Info("new macvlan interface", "interface", interfaceName, "hardware address", hwaddr)
		mac := &netlink.Macvlan{
			LinkAttrs: netlink.LinkAttrs{
				Name:         interfaceName,
				ParentIndex:  parent.Attrs().Index,
				HardwareAddr: hwaddr,
			},
			Mode: netlink.MACVLAN_MODE_DEFAULT,
		}

		err = netlink.LinkAdd(mac)
		if err != nil {
			return fmt.Errorf("could not add %s: %v", interfaceName, err)
		}

		err = netlink.LinkSetUp(mac)
		if err != nil {
			return fmt.Errorf("could not bring up interface [%s] : %v", interfaceName, err)
		}

		iface, err = net.InterfaceByName(interfaceName)
		if err != nil {
			return fmt.Errorf("error finding new DHCP interface by name [%v]", err)
		}
	} else {
		log.Info("Using existing macvlan interface for DHCP", "interface", interfaceName)
	}

	var initRebootFlag bool
<<<<<<<< HEAD:pkg/cluster/instance.go
	if i.DHCPInterfaceIP != "" {
		initRebootFlag = true
	}

	client := vip.NewDHCPClient(iface, initRebootFlag, i.DHCPInterfaceIP)
========
	if i.DhcpInterfaceIP != "" {
		initRebootFlag = true
	}

	client := vip.NewDHCPClient(iface, initRebootFlag, i.DhcpInterfaceIP)
>>>>>>>> 9aff693 (Moved endpoint-related code from pkg/manager to pkg/endpoints):pkg/services/instance.go

	// Add hostname to dhcp client if annotated
	if i.DHCPHostname != "" {
		log.Info("Hostname specified for dhcp lease", "interface", interfaceName, "hostname", i.DHCPHostname)
		client.WithHostName(i.DHCPHostname)
	}

	go client.Start()

	// Set that DHCP is enabled
	i.IsDHCP = true
	// Set the name of the interface so that it can be removed on Service deletion
<<<<<<<< HEAD:pkg/cluster/instance.go
	i.DHCPInterface = interfaceName
	i.DHCPInterfaceHwaddr = iface.HardwareAddr.String()
	// Add the client so that we can call it to stop function
	i.DHCPClient = client
========
	i.DhcpInterface = interfaceName
	i.DhcpInterfaceHwaddr = iface.HardwareAddr.String()
	// Add the client so that we can call it to stop function
	i.DhcpClient = client
>>>>>>>> 9aff693 (Moved endpoint-related code from pkg/manager to pkg/endpoints):pkg/services/instance.go

	return nil
}

<<<<<<<< HEAD:pkg/cluster/instance.go
// FetchIngressAddresses tries to get the addresses from status.loadBalancerIP
func FetchLoadBalancerIngressAddresses(s *v1.Service) []string {
	// If the service has no status, return empty
	lbStatusAddresses := []string{}

	if len(s.Status.LoadBalancer.Ingress) == 0 {
		return lbStatusAddresses
	}

	for _, ingress := range s.Status.LoadBalancer.Ingress {
		if ingress.IP != "" {
			lbStatusAddresses = append(lbStatusAddresses, ingress.IP)
		}
		// TODO: Handle hostname if needed
	}

	return lbStatusAddresses
}

========
>>>>>>>> 9aff693 (Moved endpoint-related code from pkg/manager to pkg/endpoints):pkg/services/instance.go
// FetchServiceAddresses tries to get the addresses from annotations
// kube-vip.io/loadbalancerIPs, then from spec.loadbalancerIP
func FetchServiceAddresses(s *v1.Service) []string {
	annotationAvailable := false
	if s.Annotations != nil {
<<<<<<<< HEAD:pkg/cluster/instance.go
		if v, annotationAvailable := s.Annotations[LoadbalancerIPAnnotation]; annotationAvailable {
========
		if v, annotationAvailable := s.Annotations[kubevip.LoadbalancerIPAnnotation]; annotationAvailable {
>>>>>>>> 9aff693 (Moved endpoint-related code from pkg/manager to pkg/endpoints):pkg/services/instance.go
			ips := strings.Split(v, ",")
			var trimmedIPs []string
			for _, ip := range ips {
				trimmedIPs = append(trimmedIPs, strings.TrimSpace(ip))
			}
			return trimmedIPs
		}
	}

	lbStatusAddresses := []string{}
	if !annotationAvailable {
		if len(s.Status.LoadBalancer.Ingress) > 0 {
			for _, ingress := range s.Status.LoadBalancer.Ingress {
				lbStatusAddresses = append(lbStatusAddresses, ingress.IP)
			}
		}
	}

	lbIP := net.ParseIP(s.Spec.LoadBalancerIP)
	isLbIPv4 := vip.IsIPv4(s.Spec.LoadBalancerIP)

	if len(lbStatusAddresses) > 0 {
		for _, a := range lbStatusAddresses {
			if lbStatusIP := net.ParseIP(a); lbStatusIP != nil && lbIP != nil && vip.IsIPv4(a) == isLbIPv4 && !lbIP.Equal(lbStatusIP) {
				return []string{s.Spec.LoadBalancerIP}
			}
		}
		return lbStatusAddresses
	}

	if s.Spec.LoadBalancerIP != "" {
		return []string{s.Spec.LoadBalancerIP}
	}

	return []string{}
}
<<<<<<<< HEAD:pkg/cluster/instance.go
========

func FindServiceInstance(svc *v1.Service, instances []*Instance) *Instance {
	log.Debug("finding service", "UID", svc.UID)
	for i := range instances {
		log.Debug("saved service", "instance", i, "UID", instances[i].ServiceSnapshot.UID)
		if instances[i].ServiceSnapshot.UID == svc.UID {
			return instances[i]
		}
	}
	return nil
}

func FindServiceInstanceWithTimeout(ctx context.Context, svc *v1.Service, instances []*Instance) (*Instance, error) {
	log.Debug("finding service with timeout", "UID", svc.UID)

	ctxTimeout, ctxTimeoutCancel := context.WithTimeout(ctx, time.Minute)
	defer ctxTimeoutCancel()

	for {
		instance := FindServiceInstance(svc, instances)
		if instance != nil {
			return instance, nil
		}

		t := time.NewTimer(time.Millisecond * 100)
		select {
		case <-ctxTimeout.Done():
			t.Stop()
			return nil, fmt.Errorf("failed to wait for the service instance: %w", ctx.Err())
		case <-t.C:
			log.Debug("waiting for the service to be discovered", "UID", svc.UID)
		}
	}
}
>>>>>>>> 9aff693 (Moved endpoint-related code from pkg/manager to pkg/endpoints):pkg/services/instance.go
