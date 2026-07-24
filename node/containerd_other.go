//go:build !linux

package node

import (
	"context"
	"fmt"
)

type ContainerdConfig struct {
	Address     string
	Namespace   string
	Snapshotter string
	LogDir      string
}

func OpenContainerd(context.Context, ContainerdConfig) (*ContainerRuntime, error) {
	return nil, fmt.Errorf("the containerd runtime adapter requires Linux")
}
