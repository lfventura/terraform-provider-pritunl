package pritunl

type Host struct {
	ID                string `json:"id,omitempty"`
	Name              string `json:"name"`
	Hostname          string `json:"hostname"`
	PublicAddr        string `json:"public_addr"`
	PublicAddress     string `json:"public_address"`
	PublicAddr6       string `json:"public_addr6"`
	RoutedSubnet6     string `json:"routed_subnet6"`
	RoutedSubnet6WG   string `json:"routed_subnet6_wg"`
	LocalAddr         string `json:"local_addr"`
	LocalAddr6        string `json:"local_addr6"`
	AvailabilityGroup string `json:"availability_group"`
	LinkAddr          string `json:"link_addr"`
	SyncAddress       string `json:"sync_address"`
	Status            string `json:"status"`
}

// HostUpdate is the payload PUT /host/{id} takes: the managed settings,
// always sent together so an omitted one never clears the other.
type HostUpdate struct {
	Name          string `json:"name"`
	PublicAddress string `json:"public_address"`
}
