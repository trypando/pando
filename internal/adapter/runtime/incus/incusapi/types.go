// Package incusapi is the part of the Incus REST API the Incus runtime uses:
// the types, with Incus's own JSON names, and a client for them.
//
// It is ours rather than Incus's Go client because the Go vulnerability
// database records every Incus advisory against the whole lxc/incus module,
// server flaws included, with no fixed version, so depending on any of it made
// every call reachable and the build red on every release
// (notes-incus-runtime.md, As built). The API is plain JSON over HTTPS or a
// unix socket, with websockets for exec and an HTTP upgrade for SFTP. Only
// the fields the adapter reads or writes are here; Incus ignores what a
// request leaves out and the client ignores what a response adds.
package incusapi

// InstanceType is "virtual-machine" or "container".
type InstanceType string

const (
	InstanceTypeVM        InstanceType = "virtual-machine"
	InstanceTypeContainer InstanceType = "container"
	InstanceTypeAny       InstanceType = ""
)

// Server is GET /1.0.
type Server struct {
	APIExtensions []string          `json:"api_extensions"`
	Environment   ServerEnvironment `json:"environment"`
}

// ServerEnvironment is the server's read-only environment.
type ServerEnvironment struct {
	Architectures   []string `json:"architectures"`
	Project         string   `json:"project"`
	ServerClustered bool     `json:"server_clustered"`
	ServerVersion   string   `json:"server_version"`
}

// ClusterMember is one member of a cluster.
type ClusterMember struct {
	ServerName   string   `json:"server_name"`
	Status       string   `json:"status"`
	Architecture string   `json:"architecture"`
	Groups       []string `json:"groups"`
}

// Resources is a server's hardware.
type Resources struct {
	CPU    ResourcesCPU    `json:"cpu"`
	Memory ResourcesMemory `json:"memory"`
}

// ResourcesCPU counts CPU threads.
type ResourcesCPU struct {
	Total uint64 `json:"total"`
}

// ResourcesMemory is memory in bytes.
type ResourcesMemory struct {
	Used  uint64 `json:"used"`
	Total uint64 `json:"total"`
}

// InstancePut is an instance's writable part.
type InstancePut struct {
	Description string                       `json:"description,omitempty"`
	Profiles    []string                     `json:"profiles"`
	Config      map[string]string            `json:"config"`
	Devices     map[string]map[string]string `json:"devices"`
}

// Instance is an instance.
type Instance struct {
	InstancePut
	Name     string `json:"name"`
	Type     string `json:"type"`
	Status   string `json:"status"`
	Location string `json:"location"`
	Project  string `json:"project"`
}

// Writable is the instance's writable part, as PUT takes it.
func (i *Instance) Writable() InstancePut { return i.InstancePut }

// InstanceFull is an instance with its state.
type InstanceFull struct {
	Instance
	State *InstanceState `json:"state"`
}

// InstanceSource is where a new instance comes from.
type InstanceSource struct {
	Type        string `json:"type"`
	Server      string `json:"server,omitempty"`
	Protocol    string `json:"protocol,omitempty"`
	Alias       string `json:"alias,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

// InstancesPost creates an instance.
type InstancesPost struct {
	InstancePut
	Name   string         `json:"name"`
	Type   InstanceType   `json:"type"`
	Source InstanceSource `json:"source"`
}

// InstanceState is what an instance is doing now.
type InstanceState struct {
	Status  string                          `json:"status"`
	CPU     InstanceStateCPU                `json:"cpu"`
	Memory  InstanceStateMemory             `json:"memory"`
	Network map[string]InstanceStateNetwork `json:"network"`
}

// InstanceStateCPU is CPU time used, in nanoseconds.
type InstanceStateCPU struct {
	Usage int64 `json:"usage"`
}

// InstanceStateMemory is memory used, in bytes.
type InstanceStateMemory struct {
	Usage int64 `json:"usage"`
}

// InstanceStateNetwork is one interface as the guest reports it.
type InstanceStateNetwork struct {
	Addresses []InstanceStateNetworkAddress `json:"addresses"`
	Hwaddr    string                        `json:"hwaddr"`
}

// InstanceStateNetworkAddress is one address on an interface.
type InstanceStateNetworkAddress struct {
	Family  string `json:"family"`
	Address string `json:"address"`
	Netmask string `json:"netmask"`
	Scope   string `json:"scope"`
}

// InstanceStatePut starts or stops an instance.
type InstanceStatePut struct {
	Action  string `json:"action"`
	Timeout int    `json:"timeout"`
	Force   bool   `json:"force"`
}

// InstanceExecPost runs a command in an instance.
type InstanceExecPost struct {
	Command     []string `json:"command"`
	WaitForWS   bool     `json:"wait-for-websocket"`
	Interactive bool     `json:"interactive"`
}

// NetworkPut is a network's writable part.
type NetworkPut struct {
	Description string            `json:"description,omitempty"`
	Config      map[string]string `json:"config"`
}

// Network is a network.
type Network struct {
	NetworkPut
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Managed bool     `json:"managed"`
	Project string   `json:"project"`
	UsedBy  []string `json:"used_by"`
}

// Writable is the network's writable part, as PUT takes it.
func (n *Network) Writable() NetworkPut { return n.NetworkPut }

// NetworkACLRule is one rule of an ACL.
type NetworkACLRule struct {
	Action          string `json:"action"`
	Source          string `json:"source,omitempty"`
	Destination     string `json:"destination,omitempty"`
	Protocol        string `json:"protocol,omitempty"`
	SourcePort      string `json:"source_port,omitempty"`
	DestinationPort string `json:"destination_port,omitempty"`
	Description     string `json:"description,omitempty"`
	State           string `json:"state"`
}

// NetworkACLPut is an ACL's writable part.
type NetworkACLPut struct {
	Description string            `json:"description,omitempty"`
	Ingress     []NetworkACLRule  `json:"ingress"`
	Egress      []NetworkACLRule  `json:"egress"`
	Config      map[string]string `json:"config"`
}

// NetworkACLPost names an ACL.
type NetworkACLPost struct {
	Name string `json:"name"`
}

// NetworkACL is an ACL.
type NetworkACL struct {
	NetworkACLPost
	NetworkACLPut
}

// NetworkACLsPost creates an ACL.
type NetworkACLsPost struct {
	NetworkACLPost
	NetworkACLPut
}

// ProfilePut is a profile's writable part.
type ProfilePut struct {
	Config  map[string]string            `json:"config"`
	Devices map[string]map[string]string `json:"devices"`
}

// Profile is a profile.
type Profile struct {
	ProfilePut
	Name string `json:"name"`
}

// StoragePool is a storage pool.
type StoragePool struct {
	Name   string `json:"name"`
	Driver string `json:"driver"`
}

// StorageVolumePut is a volume's writable part.
type StorageVolumePut struct {
	Description string            `json:"description,omitempty"`
	Config      map[string]string `json:"config"`
}

// StorageVolume is a storage volume.
type StorageVolume struct {
	StorageVolumePut
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	ContentType string   `json:"content_type"`
	UsedBy      []string `json:"used_by"`
	Location    string   `json:"location"`
}

// StorageVolumesPost creates a volume.
type StorageVolumesPost struct {
	StorageVolumePut
	Name        string `json:"name"`
	Type        string `json:"type"`
	ContentType string `json:"content_type"`
}

// ImageAliasesEntry is an image alias.
type ImageAliasesEntry struct {
	Name   string `json:"name"`
	Target string `json:"target"`
}

// ImagePut is an image's writable part.
type ImagePut struct {
	Properties map[string]string `json:"properties,omitempty"`
}

// ImagesPostSource is where a new image comes from.
type ImagesPostSource struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// ImageAlias names an image.
type ImageAlias struct {
	Name string `json:"name"`
}

// ImagesPost creates an image.
type ImagesPost struct {
	ImagePut
	Source  *ImagesPostSource `json:"source"`
	Aliases []ImageAlias      `json:"aliases,omitempty"`
}
