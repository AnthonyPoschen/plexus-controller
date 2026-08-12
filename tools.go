//go:build tools

package tools

// This file imports controller-gen so `go mod tidy` keeps it available
// as a tool dependency. Install it with:
//
//	go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest
//
// Then use `controller-gen` directly, or run `go generate`.

import (
	_ "sigs.k8s.io/controller-tools/cmd/controller-gen"
)
