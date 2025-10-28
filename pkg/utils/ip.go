package utils

import (
	"fmt"
	log "log/slog"
	"net"
)

// FormatIPWithSubnetMask takes a raw IP address and a subnet mask, and returns a formatted string in CIDR notation.
func FormatIPWithSubnetMask(rawIP string, subnetMask string) (string, error) {

	addr := fmt.Sprintf("%s/%s", rawIP, subnetMask)
	// Check if the input is valid
	_, _, err := net.ParseCIDR(addr)
	if err != nil {
		return "", fmt.Errorf("invalid CIDR: %q, %w", addr, err)
	}
	return addr, nil
}

// IsIP returns if address is an IP or not
func IsIP(address string) bool {
	ip := net.ParseIP(address)
	return ip != nil
}

// IsIPv4 returns true only if address is a valid IPv4 address
func IsIPv4(address string) bool {
	log.Debug("ISIPv4", "address", address)
	ip := net.ParseIP(address)
	if ip == nil {
		log.Debug("ip is nil")
		return false
	}
	log.Debug("checking", "ip", ip.String())
	return ip.To4() != nil
}

// IsIPv6 returns true only if address is a valid IPv6 address
func IsIPv6(address string) bool {
	ip := net.ParseIP(address)
	if ip == nil {
		return false
	}
	return ip.To4() == nil
}

func IsIPv4CIDR(cidr string) bool {
	ip, _, _ := net.ParseCIDR(cidr)
	if ip == nil {
		return false
	}
	return ip.To4() != nil
}

func IsIPv6CIDR(cidr string) bool {
	ip, _, _ := net.ParseCIDR(cidr)
	if ip == nil {
		return false
	}
	return ip.To4() == nil
}
