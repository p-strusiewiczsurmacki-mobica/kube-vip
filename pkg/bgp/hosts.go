package bgp

import (
	logrus "github.com/sirupsen/logrus"
)

// AddHost will update peers of a host
func (b *Server) AddHost(addr string) (err error) {
	// ip, _, err := net.ParseCIDR(addr)
	// if err != nil {
	// 	return err
	// }

	// p := b.getPath(ip)
	// if p == nil {
	// 	return fmt.Errorf("failed to get path for %v", ip)
	// }

	// _, err = b.s.AddPath(context.Background(), &api.AddPathRequest{
	// 	Path: p,
	// })

	// if err != nil {
	// 	return err
	// }

	logrus.Infof("[BGP] Added host: %s", addr)

	return
}

// DelHost will inform peers to remove a host
func (b *Server) DelHost(addr string) (err error) {
	// ip, _, err := net.ParseCIDR(addr)
	// if err != nil {
	// 	return err
	// }
	// p := b.getPath(ip)
	// if p == nil {
	// 	return
	// }

	// return b.s.DeletePath(context.Background(), &api.DeletePathRequest{
	// 	Path: p,
	// })
	logrus.Infof("[BGP] Deleted host: %s", addr)
	return nil
}
