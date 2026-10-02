//go:build windows

package oci

import "github.com/bojanrajkovic/runny/internal/tart"

func refuseHostDiskFormat(cfg *tart.Config) error { return cfg.RefuseASIFOnHyperV() }
