package mlbtypes

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// IPAddressPoolSpec defines the desired state of IPAddressPool.
type IPAddressPoolSpec struct {
	// A list of IP address ranges over which MetalLB has authority.
	// You can list multiple ranges in a single pool, they will all share the
	// same settings. Each range can be either a CIDR prefix, or an explicit
	// start-end range of IPs.
	Addresses []string `json:"addresses"`

	// AutoAssign flag used to prevent MetallB from automatic allocation
	// for a pool.
	// +optional
	// +kubebuilder:default:=true
	AutoAssign *bool `json:"autoAssign,omitempty"`

	// AvoidBuggyIPs prevents addresses ending with .0 and .255
	// to be used by a pool.
	// +optional
	// +kubebuilder:default:=false
	AvoidBuggyIPs bool `json:"avoidBuggyIPs,omitempty"`
}

// IPAddressPoolStatus defines the observed state of IPAddressPool.
type IPAddressPoolStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
}

// IPAddressPool represents a pool of IP addresses that can be allocated
// to LoadBalancer services.
type IPAddressPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   IPAddressPoolSpec   `json:"spec"`
	Status IPAddressPoolStatus `json:"status,omitempty"`
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new SriovNetwork.
func (pool *IPAddressPool) DeepCopy() *IPAddressPool {
	if pool == nil {
		return nil
	}

	out := new(IPAddressPool)

	return out
}

// DeepCopyObject is an autogenerated deepcopy function, copying the receiver, creating a new runtime.Object.
func (pool *IPAddressPool) DeepCopyObject() runtime.Object { //nolint:ireturn
	if c := pool.DeepCopy(); c != nil {
		return c
	}

	return nil
}

// +kubebuilder:object:root=true

// IPAddressPoolList contains a list of IPAddressPool.
type IPAddressPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IPAddressPool `json:"items"`
}
