package vip

import (
	"fmt"
	log "log/slog"
	"net"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv6"
	"github.com/insomniacslk/dhcp/dhcpv6/client6"
	"github.com/jpillora/backoff"
)

type DHCPv6Client struct {
	iface          *net.Interface
	ddnsHostName   string
	initRebootFlag bool
	requestedIP    net.IP
	stopChan       chan struct{} // used as a signal to release the IP and stop the dhcp client daemon
	releasedChan   chan struct{} // indicate that the IP has been released
	errorChan      chan error    // indicates there was an error on the IP request
	ipChan         chan string
	client         *client6.Client
	addr           *dhcpv6.OptIAAddress
}

// NewDHCPClient returns a new DHCP Client.
func NewDHCP6Client(iface *net.Interface, initRebootFlag bool, requestedIP string) (*DHCPv6Client, error) {
	client := client6.NewClient()

	return &DHCPv6Client{
		iface:          iface,
		stopChan:       make(chan struct{}),
		releasedChan:   make(chan struct{}),
		errorChan:      make(chan error),
		initRebootFlag: initRebootFlag,
		requestedIP:    net.ParseIP(requestedIP),
		ipChan:         make(chan string),
		client:         client,
	}, nil
}

func (c *DHCPv6Client) WithHostName(hostname string) *DHCPv6Client {
	c.ddnsHostName = hostname
	return c
}

// Stop state-transition process and close dhcp client
func (c *DHCPv6Client) Stop() {
	close(c.ipChan)
	close(c.stopChan)
	<-c.releasedChan
}

// Gets the IPChannel for consumption
func (c *DHCPv6Client) IPChannel() chan string {
	return c.ipChan
}

// Gets the ErrorChannel for consumption
func (c *DHCPv6Client) ErrorChannel() chan error {
	return c.errorChan
}

func (c *DHCPv6Client) Start() error {
	// REQUEST WITH BACKOFF ACTION

	addr, err := c.requestWithBackoff()

	if err != nil {
		return fmt.Errorf("DHCPv6 client failed: %w", err)
	}

	c.addr = addr

	c.initRebootFlag = false

	// Set up two ticker to renew/rebind regularly
	t1Timeout := c.addr.PreferredLifetime / 2
	t2Timeout := (c.addr.ValidLifetime / 8) * 7
	log.Debug("dhcp timeouts", "timeout1", t1Timeout, "timeoute2", t2Timeout)
	t1, t2 := time.NewTicker(t1Timeout), time.NewTicker(t2Timeout)

	for {
		select {
		case <-t1.C:
			// renew is a unicast request of the IP renewal
			// A point on renew is: the library does not return the right message (NAK)
			// on renew error due to IP Change, but instead it returns a different error
			// This way there's not much to do other than log and continue, as the renew error
			// may be an offline server, or may be an incorrect package match

			// RENEW ACTION

			addr, err := c.renew()
			if err == nil {
				c.addr = addr
				log.Info("renew", "addr", addr.IPv6Addr.String())
				t2.Reset(t2Timeout)
			} else {
				log.Error("renew failed", "err", err)
			}
		case <-t2.C:
			// rebind is just like a request, but forcing to provide a new IP address

			// REQUEST ACTION

			addr, err := c.request(true)
			if err == nil {
				c.addr = addr
				log.Info("rebind", "lease", addr)
			} else {
				log.Warn("ip may have changed", "ip", addr.IPv6Addr.String(), "err", err)
				c.initRebootFlag = false
				c.addr, err = c.requestWithBackoff()
				if err != nil {
					return fmt.Errorf("DHCPv6 rebind failed: %w", err)
				}
			}
			t1.Reset(t1Timeout)
			t2.Reset(t2Timeout)

		case <-c.stopChan:
			// release is a unicast request of the IP release.

			// RELEASE ACTION

			// if err := c.release(); err != nil {
			// 	log.Error("release lease failed", "lease", lease, "err", err)
			// } else {
			// 	log.Info("release", "lease", lease)
			// }
			// t1.Stop()
			// t2.Stop()

			// close(c.releasedChan)
			// return
		}
	}
}

func (c *DHCPv6Client) requestWithBackoff() (*dhcpv6.OptIAAddress, error) {
	backoff := backoff.Backoff{
		Factor: 2,
		Jitter: true,
		Min:    10 * time.Second,
		Max:    1 * time.Minute,
	}

	var err error
	var addr *dhcpv6.OptIAAddress

	for {
		log.Debug("trying to get a new IP", "attempt", backoff.Attempt())

		addr, err = c.request(false)

		if err != nil {
			dur := backoff.Duration()
			if backoff.Attempt() > maxBackoffAttempts-1 {
				errMsg := fmt.Errorf("failed to get an IP address after %d attempts, error %s, giving up", maxBackoffAttempts, err.Error())
				log.Error(errMsg.Error())
				c.errorChan <- errMsg
				c.Stop()
				return nil, fmt.Errorf("failed to get IPv6 address: %w", err)
			}
			log.Error("request failed", "err", err.Error(), "waiting", dur)
			time.Sleep(dur)
			continue
		}
		backoff.Reset()
		break
	}

	if c.ipChan != nil {
		log.Debug("using channel")
		c.ipChan <- addr.IPv6Addr.String()
	}

	return addr, nil
}

// type leasev6 struct {
// 	address string
// 	t1      time.Duration
// 	t2      time.Duration
// }

func (c *DHCPv6Client) request(rebind bool) (*dhcpv6.OptIAAddress, error) {
	modifiers := []dhcpv6.Modifier{}
	modifiers = append(modifiers, dhcpv6.WithClientID(&dhcpv6.DUIDEN{EnterpriseNumber: 1, EnterpriseIdentifier: []byte(c.ddnsHostName)}))
	modifiers = append(modifiers, dhcpv6.WithFQDN(4, c.ddnsHostName))

	if rebind {
		modifiers = append(modifiers, dhcpv6.WithIANA(*c.addr))
	}

	conversation, err := c.client.Exchange(c.iface.Name, modifiers...)

	if err != nil {
		return nil, fmt.Errorf("DHCPv6 request failed: %w", err)
	}

	for _, msg := range conversation {
		if msg.Type() == dhcpv6.MessageTypeReply {
			reply, err := dhcpv6.MessageFromBytes(msg.ToBytes())
			if err != nil {
				return nil, fmt.Errorf("reply conversion failed: %w", err)
			}

			if len(reply.Options.IANA()) < 1 {
				return nil, fmt.Errorf("failed to get IANA")
			}

			if len(reply.Options.IANA()[0].Options.Addresses()) < 1 {
				return nil, fmt.Errorf("failed to get addresses data")
			}

			return reply.Options.IANA()[0].Options.Addresses()[0], nil
		}
	}

	return nil, fmt.Errorf("no data available")
}

func (c *DHCPv6Client) renew() (*dhcpv6.OptIAAddress, error) {
	return c.request(false)
}
