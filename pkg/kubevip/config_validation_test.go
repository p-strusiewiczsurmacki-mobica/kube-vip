package kubevip

import (
	"strings"
	"testing"
)

func TestValidate_HealthCheckAddress(t *testing.T) {
	tests := []struct {
		name    string
		address string
		wantErr bool
	}{
		{"empty address (disabled)", "", false},
		{"valid http URL", "http://localhost:6443/livez", false},
		{"valid https URL", "https://localhost:6443/livez", false},
		{"https with path", "https://127.0.0.1:6443/readyz?verbose", false},
		{"invalid URL", "not-a-url", true},
		{"ftp scheme", "ftp://localhost/file", true},
		{"tcp scheme", "tcp://localhost:6443", true},
		{"missing scheme", "localhost:6443/livez", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Config{ControlPlaneHealthCheck: HealthCheck{Address: tt.address}}
			err := c.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidate_InstanceName(t *testing.T) {
	tests := []struct {
		name         string
		instanceName string
		wantErr      bool
	}{
		{name: "empty uses legacy default", instanceName: "", wantErr: false},
		{name: "letters digits and separators", instanceName: "release_01.prod-a", wantErr: false},
		{name: "exact maximum length", instanceName: strings.Repeat("a", instanceNameMaxLength), wantErr: false},
		{name: "exceeds maximum length", instanceName: strings.Repeat("a", instanceNameMaxLength+1), wantErr: true},
		{name: "space", instanceName: "release a", wantErr: true},
		{name: "slash", instanceName: "namespace/release", wantErr: true},
		{name: "dollar sign", instanceName: "release$a", wantErr: true},
		{name: "at sign", instanceName: "release@a", wantErr: true},
		{name: "newline", instanceName: "release\na", wantErr: true},
		{name: "null byte", instanceName: "release\x00a", wantErr: true},
		{name: "unicode", instanceName: "rilascio-à", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := &Config{InstanceName: tt.instanceName}
			err := config.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %t", err, tt.wantErr)
			}
		})
	}
}

func TestInstanceNameLimitReservesNftablesPrefixAndFamilySuffix(t *testing.T) {
	name := strings.Repeat("a", instanceNameMaxLength)
	if got := len(egressNftablesTablePrefix + name + egressNftablesTableSuffix); got != nftablesNameMaxLength {
		t.Fatalf("family-specific table name length = %d, want %d", got, nftablesNameMaxLength)
	}
}

func TestValidate_EnablePprof(t *testing.T) {
	tests := []struct {
		name        string
		enablePprof bool
		wantErr     bool
	}{
		{name: "pprof disabled (default)", enablePprof: false, wantErr: false},
		{name: "pprof enabled", enablePprof: true, wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := &Config{
				EnablePprof: tt.enablePprof,
				// Minimal config to pass validation
				Interface: "eth0",
			}
			err := config.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() with EnablePprof=%v error = %v, wantErr %v", tt.enablePprof, err, tt.wantErr)
			}
		})
	}
}

