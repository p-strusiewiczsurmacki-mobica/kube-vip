package e2e

type KubevipManifestValues struct {
	ControlPlaneVIP      string
	ImagePath            string
	ConfigPath           string
	SvcEnable            string
	SvcElectionEnable    string
	EnableEndpointslices string
}
