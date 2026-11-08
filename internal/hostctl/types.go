package hostctl

// Instance is the JSON a verb returns for one VM: the registry record plus what
// Tart currently says about it (its live presence and address).
type Instance struct {
	Record
	IP     string `json:"ip,omitempty"`
	InTart bool   `json:"in_tart"`
}

// Capacity is the host's slot accounting, reported by status. used counts live
// lifecycle records (including deleting tombstones); adopted counts VMs found
// in tart list that carry this controller's label but had no record; quarantined
// counts local VMs whose ownership has not been proven. A create is refused when
// used+adopted reaches total.
type Capacity struct {
	Total       int `json:"total"`
	Used        int `json:"used"`
	Adopted     int `json:"adopted"`
	Quarantined int `json:"quarantined"`
}

// Status is the health gate reported by `status --json`.
type Status struct {
	TartVersion      string     `json:"tart_version"`
	KeychainUnlocked bool       `json:"keychain_unlocked"`
	FreeDiskBytes    uint64     `json:"free_disk_bytes"`
	Drained          bool       `json:"drained"`
	Capacity         Capacity   `json:"capacity"`
	Instances        []Instance `json:"instances"`
}

// Fault is the machine-readable diagnostic a failed verb writes to stderr. The
// provider parses it to distinguish a capacity fault (returned to GARM as an
// errored ProviderInstance) from transport and identity failures (hard errors
// GARM retries).
type Fault struct {
	Fault string `json:"fault"`
	Error string `json:"error"`
}

// Fault names. They keep the provider free of string matching on prose.
const (
	FaultUsage      = "usage"
	FaultCapacity   = "capacity"
	FaultKeychain   = "keychain"
	FaultDrained    = "drained"
	FaultNotFound   = "not_found"
	FaultTart       = "tart"
	FaultValidation = "validation"
)
