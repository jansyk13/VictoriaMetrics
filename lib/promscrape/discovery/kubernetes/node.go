package kubernetes

import (
	"encoding/json"
	"fmt"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/promscrape/discoveryutils"
)

// getNodesLabels returns labels for k8s nodes obtained from the given cfg
func getNodesLabels(cfg *apiConfig) []map[string]string {
	nodes := getNodes(cfg)
	var ms []map[string]string
	for _, n := range nodes {
		ms = n.appendTargetLabels(ms)
	}
	return ms
}

func getNodes(cfg *apiConfig) []*Node {
	os := cfg.aw.getObjectsByRole("node")
	ns := make([]*Node, len(os))
	for i, o := range os {
		ns[i] = o.(*Node)
	}
	return ns
}

func (n *Node) key() string {
	return n.Metadata.key()
}

func parseNodeList(data []byte) (map[string]object, ListMeta, error) {
	var nl NodeList
	if err := json.Unmarshal(data, &nl); err != nil {
		return nil, nl.Metadata, fmt.Errorf("cannot unmarshal NodeList from %q: %w", data, err)
	}
	objectsByKey := make(map[string]object)
	for _, n := range nl.Items {
		objectsByKey[n.key()] = n
	}
	return objectsByKey, nl.Metadata, nil
}

func parseNode(data []byte) (object, error) {
	var n Node
	if err := json.Unmarshal(data, &n); err != nil {
		return nil, err
	}
	return &n, nil
}

// NodeList represents NodeList from k8s API.
//
// See https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.17/#nodelist-v1-core
type NodeList struct {
	Metadata ListMeta
	Items    []*Node
}

// Node represents Node from k8s API.
//
// See https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.17/#node-v1-core
type Node struct {
	Metadata ObjectMeta
	Status   NodeStatus
}

// NodeStatus represents NodeStatus from k8s API.
//
// See https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.17/#nodestatus-v1-core
type NodeStatus struct {
	Addresses       []NodeAddress
	DaemonEndpoints NodeDaemonEndpoints
}

// NodeAddress represents NodeAddress from k8s API.
//
// See https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.17/#nodeaddress-v1-core
type NodeAddress struct {
	Type    string
	Address string
}

// NodeDaemonEndpoints represents NodeDaemonEndpoints from k8s API.
//
// See https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.17/#nodedaemonendpoints-v1-core
type NodeDaemonEndpoints struct {
	KubeletEndpoint DaemonEndpoint
}

// appendTargetLabels appends labels for the given Node n to ms and returns the result.
//
// See https://prometheus.io/docs/prometheus/latest/configuration/configuration/#node
func (n *Node) appendTargetLabels(ms []map[string]string) []map[string]string {
	addr := getNodeAddr(n.Status.Addresses)
	if len(addr) == 0 {
		// Skip node without address
		return ms
	}
	addr = discoveryutils.JoinHostPort(addr, n.Status.DaemonEndpoints.KubeletEndpoint.Port)
	m := map[string]string{
		"__address__":                 addr,
		"instance":                    n.Metadata.Name,
		"__meta_kubernetes_node_name": n.Metadata.Name,
	}
	n.Metadata.registerLabelsAndAnnotations("__meta_kubernetes_node", m)
	addrTypesUsed := make(map[string]bool, len(n.Status.Addresses))
	for _, a := range n.Status.Addresses {
		if addrTypesUsed[a.Type] {
			continue
		}
		addrTypesUsed[a.Type] = true
		ln := discoveryutils.SanitizeLabelName(a.Type)
		m["__meta_kubernetes_node_address_"+ln] = a.Address
	}
	ms = append(ms, m)
	return ms
}

func getNodeAddr(nas []NodeAddress) string {
	if addr := getAddrByType(nas, "InternalIP"); len(addr) > 0 {
		return addr
	}
	if addr := getAddrByType(nas, "InternalDNS"); len(addr) > 0 {
		return addr
	}
	if addr := getAddrByType(nas, "ExternalIP"); len(addr) > 0 {
		return addr
	}
	if addr := getAddrByType(nas, "ExternalDNS"); len(addr) > 0 {
		return addr
	}
	if addr := getAddrByType(nas, "LegacyHostIP"); len(addr) > 0 {
		return addr
	}
	if addr := getAddrByType(nas, "Hostname"); len(addr) > 0 {
		return addr
	}
	return ""
}

func getAddrByType(nas []NodeAddress, typ string) string {
	for _, na := range nas {
		if na.Type == typ {
			return na.Address
		}
	}
	return ""
}
