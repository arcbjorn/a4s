//go:build !linux

package node

import (
	"fmt"
	"time"
)

type CNIConfig struct {
	BinDir       string
	ConfDir      string
	NetworkName  string
	Subnet       string
	NamespaceDir string
	Timeout      time.Duration
}

// OpenCNI requires Linux network namespaces. The rest of the node builds and
// tests everywhere, so the contract stays verifiable on any platform.
func OpenCNI(CNIConfig) (*Network, error) {
	return nil, fmt.Errorf("the cni network adapter requires Linux")
}
