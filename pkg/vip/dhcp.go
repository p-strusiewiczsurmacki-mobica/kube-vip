package vip

import "context"

type DHCPClient interface {
	ErrorChannel() chan error
	IPChannel() chan string
	Start(ctx context.Context) error
	Stop()
	WithHostName(hostname string) DHCPClient
}

func Test(c DHCPClient) {
	c.ErrorChannel()
}
