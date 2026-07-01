package vip

import (
	"encoding/binary"
	"fmt"
	log "log/slog"
	"net"
	"strconv"
	"strings"

	gnft "github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"github.com/google/nftables/userdata"
	iptables "github.com/kube-vip/kube-vip/pkg/iptables"
	"github.com/kube-vip/kube-vip/pkg/nftables"
	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
)

type NftEgress struct {
	client  *nftables.Client
	comment string
}

func CreateNftablesClient(namespace string, protocol iptables.Protocol) (*NftEgress, error) {
	family := gnft.TableFamilyIPv4
	if protocol != iptables.ProtocolIPv4 {
		family = gnft.TableFamilyIPv6
	}

	log.Info("[egress] creating an nftables client", "protocol", family)
	e := new(NftEgress)
	var err error

	e.client, err = nftables.NewClient(family)
	if err != nil {
		return nil, fmt.Errorf("failed to create nftables client: %w", err)
	}
	e.comment = Comment + "-" + namespace
	return e, err
}

func (n *NftEgress) CheckMangleChain(name string) (bool, error) {
	return n.client.CheckChain("mangle", name), nil
}

func (n *NftEgress) DeleteMangleReturnForNetwork(name, network string) error {
	comment := fmt.Sprintf("%s - %s", n.comment, network)
	return n.deleteRule("mangle", name, comment)
}

func (n *NftEgress) DeleteMangleMarking(podIP, name string) error {
	comment := fmt.Sprintf("%s - %s - mark", n.comment, fmt.Sprintf("%s/32", podIP))
	return n.deleteRule("mangle", name, comment)
}

func (n *NftEgress) DeleteMangleMarkingForNetwork(podIP, name, network string) error {
	comment := fmt.Sprintf("%s - %s - %s - mark", n.comment, podIP, network)
	return n.deleteRule("mangle", name, comment)
}

func (n *NftEgress) DeleteSourceNat(podIP, vip string) error {
	return n.DeleteSourceNatForDestinationPort(podIP, vip, "", "")
}

func (n *NftEgress) DeleteSourceNatForDestinationPort(podIP, vip, port, proto string) error {
	comment := fmt.Sprintf("%s - %s - %s - snat", n.comment, vip, podIP)

	if port != "" {
		comment = fmt.Sprintf("%s - %s - %s - %s - %s - snat", n.comment, vip, podIP, port, proto)
	}

	return n.deleteRule("nat", "POSTROUTING", comment)
}

func (n *NftEgress) deleteRule(table, chain, comment string) error {
	ch, err := n.client.GetChain(table, chain)
	if err != nil {
		return fmt.Errorf("failed find chain %q in table %q: %w", table, chain, err)
	}

	rule, err := n.client.FindRuleByComment(ch.Table, ch, comment)
	if err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("failed to find rule %q in chain %q, table %q", comment, chain, table)
	}

	if rule != nil {
		if err := n.client.DeleteRule(rule); err != nil {
			return fmt.Errorf("failed to delete rule %q in chain %q, table %q: %w", comment, chain, table, err)
		}
	}

	n.client.Flush()

	return nil
}

func (n *NftEgress) CreateMangleChain(name string) error {
	t := n.client.GetTable("mangle")
	ch := &gnft.Chain{
		Name:  name,
		Table: t,
	}
	_ = n.client.AddChain(ch)
	n.client.Flush()
	return nil
}
func (n *NftEgress) AppendReturnRulesForDestinationSubnet(name, subnet string) error {
	_, ipNet, err := net.ParseCIDR(subnet)
	if err != nil {
		return fmt.Errorf("failed to parse CIDR %q: %w", subnet, err)
	}

	chain, err := n.client.GetChain("mangle", name)
	if err != nil {
		return fmt.Errorf("failed to get chain %q in table 'mangle': %w", name, err)
	}

	ip := ipNet.IP
	if ip.To4() != nil {
		ip = ip.To4()
	} else {
		ip = ip.To16()
	}

	rule := &gnft.Rule{
		Table:    chain.Table,
		Chain:    chain,
		UserData: nftables.UserDataComment(fmt.Sprintf("%s - %s", n.comment, subnet)),
		Exprs: []expr.Any{
			&expr.Payload{
				DestRegister: 1,
				Base:         expr.PayloadBaseNetworkHeader,
				Offset:       n.client.GetDstOffset(),
				Len:          n.client.GetLen(),
			},
			&expr.Bitwise{
				SourceRegister: 1,
				DestRegister:   1,
				Len:            n.client.GetLen(),
				Mask:           ipNet.Mask,
				Xor:            make([]byte, n.client.GetLen()),
			},
			&expr.Cmp{
				Register: 1,
				Op:       expr.CmpOpEq,
				Data:     ip,
			},
			&expr.Counter{},
			&expr.Verdict{
				Kind: expr.VerdictReturn,
			},
		},
	}

	_, err = n.client.AddUnique(rule)
	if err != nil {
		return fmt.Errorf("failed to add rule %q: %w", n.comment, err)
	}

	n.client.Flush()

	return nil
}

func (n *NftEgress) AppendReturnRulesForMarking(name, subnet string) error {
	chain, err := n.client.GetChain("mangle", name)
	if err != nil {
		return fmt.Errorf("failed to get chain %q in table 'mangle': %w", name, err)
	}

	srcIP, srcNet, err := net.ParseCIDR(subnet)
	if err != nil {
		return fmt.Errorf("failed to parse CIDR for IP %q: %w", subnet, err)
	}

	if srcIP.To4() != nil {
		srcIP = srcIP.To4()
	} else {
		srcIP = srcIP.To16()
	}

	mask := srcNet.Mask

	mark := uint32(0x40)

	rule := &gnft.Rule{
		Table:    chain.Table,
		Chain:    chain,
		UserData: nftables.UserDataComment(fmt.Sprintf("%s - %s - mark", n.comment, subnet)),
		Exprs: []expr.Any{
			&expr.Payload{
				DestRegister: 1,
				Base:         expr.PayloadBaseNetworkHeader,
				Offset:       n.client.GetSrcOffset(),
				Len:          n.client.GetLen(),
			},
			&expr.Bitwise{
				SourceRegister: 1,
				DestRegister:   1,
				Len:            n.client.GetLen(),
				Mask:           mask,
				Xor:            make([]byte, n.client.GetLen()),
			},
			&expr.Cmp{
				Register: 1,
				Op:       expr.CmpOpEq,
				Data:     srcIP,
			},

			&expr.Counter{},
			&expr.Meta{
				Key:            expr.MetaKeyMARK,
				Register:       1,
				SourceRegister: false,
			},
			&expr.Bitwise{
				SourceRegister: 1,
				DestRegister:   1,
				Len:            4,
				Mask:           binaryutil.NativeEndian.PutUint32(^mark),
				Xor:            binaryutil.NativeEndian.PutUint32(mark),
			},
			&expr.Meta{
				Key:            expr.MetaKeyMARK,
				Register:       1,
				SourceRegister: true,
			},
		},
	}

	if _, err := n.client.AddUnique(rule); err != nil {
		return fmt.Errorf("failed to add rule %q: %w", n.comment, err)
	}

	n.client.Flush()

	return nil
}

func (n *NftEgress) AppendReturnRulesForMarkingForNetwork(name, subnet, destination string) error {
	chain, err := n.client.GetChain("mangle", name)
	if err != nil {
		return fmt.Errorf("failed to get chain %q in table 'mangle': %w", name, err)
	}

	srcIP, srcNet, err := net.ParseCIDR(subnet)
	if err != nil {
		return fmt.Errorf("failed to parse IP %q: %w", subnet, err)
	}

	srcMask := srcNet.Mask

	if srcIP.To4() != nil {
		srcIP = srcIP.To4()
	} else {
		srcIP = srcIP.To16()
	}

	_, dstNet, err := net.ParseCIDR(destination)
	if err != nil {
		return fmt.Errorf("failed to parse CIDR %q: %w", destination, err)
	}

	dstMask := dstNet.Mask

	dstIP := dstNet.IP

	if dstIP.To4() != nil {
		dstIP = dstIP.To4()
	} else {
		dstIP = dstIP.To16()
	}

	mark := uint32(0x40)

	rule := &gnft.Rule{
		Table:    chain.Table,
		Chain:    chain,
		UserData: nftables.UserDataComment(fmt.Sprintf("%s - %s - %s - mark", n.comment, subnet, destination)),
		Exprs: []expr.Any{
			&expr.Payload{
				DestRegister: 1,
				Base:         expr.PayloadBaseNetworkHeader,
				Offset:       n.client.GetSrcOffset(),
				Len:          n.client.GetLen(),
			},
			&expr.Bitwise{
				SourceRegister: 1,
				DestRegister:   1,
				Len:            n.client.GetLen(),
				Mask:           srcMask,
				Xor:            make([]byte, n.client.GetLen()),
			},
			&expr.Cmp{
				Register: 1,
				Op:       expr.CmpOpEq,
				Data:     srcIP,
			},
			&expr.Payload{
				DestRegister: 1,
				Base:         expr.PayloadBaseNetworkHeader,
				Offset:       n.client.GetDstOffset(),
				Len:          n.client.GetLen(),
			},
			&expr.Bitwise{
				SourceRegister: 1,
				DestRegister:   1,
				Len:            n.client.GetLen(),
				Mask:           dstMask,
				Xor:            make([]byte, n.client.GetLen()),
			},
			&expr.Cmp{
				Register: 1,
				Op:       expr.CmpOpEq,
				Data:     dstIP,
			},
			&expr.Counter{},
			&expr.Meta{
				Key:            expr.MetaKeyMARK,
				Register:       1,
				SourceRegister: false,
			},
			&expr.Bitwise{
				SourceRegister: 1,
				DestRegister:   1,
				Len:            4,
				Mask:           binaryutil.NativeEndian.PutUint32(^mark),
				Xor:            binaryutil.NativeEndian.PutUint32(mark),
			},
			&expr.Meta{
				Key:            expr.MetaKeyMARK,
				Register:       1,
				SourceRegister: true,
			},
		},
	}

	if _, err := n.client.AddUnique(rule); err != nil {
		return fmt.Errorf("failed to add rule %q: %w", n.comment, err)
	}

	n.client.Flush()

	return nil
}

func (n *NftEgress) InsertMangeTableIntoPrerouting(name string) error {
	t := n.client.GetTable("mangle")

	chain, err := n.client.GetChain("mangle", "PREROUTING")
	policy := gnft.ChainPolicyAccept

	if err != nil {
		chain = &gnft.Chain{
			Table:    t,
			Name:     "PREROUTING",
			Type:     gnft.ChainTypeFilter,
			Hooknum:  gnft.ChainHookPrerouting,
			Priority: gnft.ChainPriorityMangle,
			Policy:   &policy,
		}

		n.client.AddChain(chain)

		n.client.Flush()
	}

	rule := &gnft.Rule{
		Table:    chain.Table,
		Chain:    chain,
		UserData: nftables.UserDataComment(n.comment),
		Exprs: []expr.Any{
			&expr.Counter{},
			&expr.Verdict{
				Kind:  expr.VerdictJump,
				Chain: name,
			},
		},
	}

	if _, err := n.client.InsertUnique(rule); err != nil {
		return fmt.Errorf("failed to insert mangle into %q: %w", name, err)
	}

	n.client.Flush()

	return nil
}

func (n *NftEgress) InsertSourceNat(vip, pod string) error {
	return n.InsertSourceNatForDestinationPort(vip, pod, "", "")
}

func (n *NftEgress) InsertSourceNatForDestinationPort(vip, pod, port, proto string) error {
	if port != "" {
		log.Info("adding source nat", "from", pod, "to", vip, "port", port, "protocol", proto)
	} else {
		log.Info("adding source nat", "from", pod, "to", vip)
	}

	table := "nat"
	chain := "POSTROUTING"

	ch, err := n.client.GetChain(table, chain)
	if err != nil {
		return fmt.Errorf("failed to get chain %q in table %q: %w", chain, table, err)
	}

	podIP := net.ParseIP(pod)
	family := unix.NFPROTO_IPV4
	if podIP.To4() != nil {
		podIP = podIP.To4()
	} else {
		podIP = podIP.To16()
		family = unix.NFPROTO_IPV6
	}

	vipIP := net.ParseIP(vip)
	if vipIP.To4() != nil {
		vipIP = vipIP.To4()
	} else {
		vipIP = vipIP.To16()
	}

	var protocol int

	if proto != "" {
		protocol = unix.IPPROTO_TCP
		switch proto {
		case "udp":
			protocol = unix.IPPROTO_UDP
		case "sctp":
			protocol = unix.IPPROTO_SCTP
		}
	}

	mark := uint32(0x40)

	var p uint64
	portByte := make([]byte, 2)

	comment := fmt.Sprintf("%s - %s - %s - snat", n.comment, vip, pod)

	if port != "" {
		comment = fmt.Sprintf("%s - %s - %s - %s - %s", n.comment, vip, pod, port, proto)
		p, err = strconv.ParseUint(port, 10, 16)
		if err != nil {
			return fmt.Errorf("failed to parse port %q: %w", port, err)
		}
		binary.BigEndian.PutUint16(portByte, uint16(p))
	}

	exprs := []expr.Any{
		&expr.Payload{
			DestRegister: 1,
			Base:         expr.PayloadBaseNetworkHeader,
			Offset:       n.client.GetSrcOffset(),
			Len:          n.client.GetLen(),
		},
		&expr.Cmp{
			Register: 1,
			Op:       expr.CmpOpEq,
			Data:     podIP,
		},
		&expr.Meta{
			Key:            expr.MetaKeyMARK,
			Register:       1,
			SourceRegister: false,
		},
		&expr.Bitwise{
			SourceRegister: 1,
			DestRegister:   1,
			Len:            4,
			Mask:           binaryutil.NativeEndian.PutUint32(mark),
			Xor:            make([]byte, 4),
		},
		&expr.Cmp{
			Register: 1,
			Op:       expr.CmpOpEq,
			Data:     binaryutil.NativeEndian.PutUint32(mark),
		},
	}

	if port != "" {
		exprs = append(exprs,
			&expr.Meta{
				Key:      expr.MetaKeyL4PROTO,
				Register: 1,
			},
			&expr.Cmp{
				Register: 1,
				Op:       expr.CmpOpEq,
				Data:     []byte{uint8(protocol)},
			},
			&expr.Payload{
				DestRegister: 1,
				Base:         expr.PayloadBaseTransportHeader,
				Offset:       2,
				Len:          2,
			},
			&expr.Cmp{
				Register: 1,
				Op:       expr.CmpOpEq,
				Data:     portByte,
			})
	}

	exprs = append(exprs,
		&expr.Counter{},
		&expr.Immediate{
			Register: 1,
			Data:     vipIP,
		},
		&expr.NAT{
			Type:       expr.NATTypeSourceNAT,
			Family:     uint32(family),
			RegAddrMin: 1,
			RegAddrMax: 1,
		},
	)

	rule := &gnft.Rule{
		Table:    ch.Table,
		Chain:    ch,
		UserData: nftables.UserDataComment(comment),
		Exprs:    exprs,
	}

	if _, err := n.client.InsertUnique(rule); err != nil {
		return fmt.Errorf("failed to add rule %q: %w", n.comment, err)
	}

	n.client.Flush()

	return nil
}

func (n *NftEgress) CleanIPtables() error {
	ch, err := n.client.GetChain("nat", "POSTROUTING")
	if err != nil {
		return fmt.Errorf("failed to get chain 'POSTROUTING' in table 'nat': %w", err)
	}

	if err := n.clearChain(ch, n.comment); err != nil {
		return fmt.Errorf("failed to clear chain %q in table %q: %w", ch.Name, ch.Table.Name, err)
	}

	exists, err := n.CheckMangleChain(MangleChainName)
	if err != nil {
		log.Debug("[egress] No Mangle chain exists", "err", err)
	}

	if exists {
		ch, err := n.client.GetChain("mangle", MangleChainName)
		if err != nil {
			return fmt.Errorf("failed to get chain %q in table 'mangle': %w", MangleChainName, err)
		}
		if err := n.clearChain(ch, n.comment); err != nil {
			return fmt.Errorf("failed to clear chain %q in table %q: %w", ch.Name, ch.Table.Name, err)
		}
	} else {
		log.Warn("No existing mangle chain exists", "chain name", MangleChainName)
	}

	return nil
}

func (n *NftEgress) clearChain(chain *gnft.Chain, data string) error {
	natRules, err := n.client.List(chain.Table, chain)
	if err != nil {
		return fmt.Errorf("failed to get existing nat rules: %w", err)
	}

	foundNatRules := findRules(natRules, data)
	log.Warn("[egress] cleaning existing chain for data", "count", len(foundNatRules), "data", data)
	for _, r := range foundNatRules {
		if err := n.client.DeleteRule(r); err != nil {
			return fmt.Errorf("failed to remove rule: %w", err)
		}
	}

	n.client.Flush()

	return nil
}

func (n *NftEgress) Close() error {
	if err := n.client.Close(); err != nil {
		return fmt.Errorf("failed to close nftables client: %w", err)
	}
	return nil
}

// stub functions

func (n *NftEgress) DumpChain(name string) error {
	return nil
}

// utility functions

func findRules(rules []*gnft.Rule, data string) []*gnft.Rule {
	var foundRules []*gnft.Rule
	for _, rule := range rules {
		comment, _ := userdata.GetString(rule.UserData, userdata.TypeComment)
		if strings.Contains(comment, data) {
			foundRules = append(foundRules, rule)
		}
	}
	return foundRules
}
