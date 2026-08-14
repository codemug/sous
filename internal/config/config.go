// Package config holds startup configuration.
//
// There is deliberately no default listen address, and 0.0.0.0 is rejected
// outright: Sous generates container configuration and runs it, which makes it
// root-equivalent on this node by construction. The protection is the network
// boundary, so binding everything would remove the only mitigation there is.
package config

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"strings"
)

type Config struct {
	Listen   string
	DataDir  string
	ModelDir string
	PortLow  int
	PortHigh int
	Reserve  float64
}

func FromFlags(args []string) (Config, error) {
	var c Config
	fs := flag.NewFlagSet("sous", flag.ContinueOnError)
	fs.StringVar(&c.Listen, "listen", "", "host:port to bind (the tailnet IP; never 0.0.0.0)")
	fs.StringVar(&c.DataDir, "data", "/var/lib/sous", "data directory")
	fs.StringVar(&c.ModelDir, "models", "", "host path holding model weights")
	fs.IntVar(&c.PortLow, "port-low", 18000, "low end of the deploy port range")
	fs.IntVar(&c.PortHigh, "port-high", 18100, "high end of the deploy port range")
	fs.Float64Var(&c.Reserve, "reserve-gib", 24,
		"memory reserved for OS, containers and CUDA contexts")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if c.Listen == "" {
		return c, errors.New("config: -listen is required")
	}
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return c, fmt.Errorf("config: -listen must be host:port: %w", err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" || strings.EqualFold(host, "[::]") {
		return c, fmt.Errorf("config: refusing to bind %q; "+
			"an endpoint that starts and stops models must not be reachable from everywhere", host)
	}
	if c.ModelDir == "" {
		return c, errors.New("config: -models is required")
	}
	if c.PortLow > c.PortHigh {
		return c, errors.New("config: -port-low is above -port-high")
	}
	return c, nil
}

// Host returns the bind address without the port.
func (c Config) Host() string {
	h, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return c.Listen
	}
	return h
}
