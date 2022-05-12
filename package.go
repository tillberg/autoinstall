package main

import (
	"fmt"

	"github.com/tillberg/alog"
	"github.com/tillberg/stringset"
)

type Package struct {
	Name            string // Full path of import within GOPATH, including .../vendor/ if present
	OSArch          OSArch
	State           PackageState // Current state of this Package (idle/building/ready/etc)
	ShouldBuild     bool
	PossibleImports *stringset.StringSet // List of all possible imports (vendor-exploded)
	FileChange      bool
}

func (p *Package) Key() string {
	archStr := p.OSArch.String()
	if archStr == "local" {
		return p.Name
	}
	return fmt.Sprintf("%s:%s", p.Name, p.OSArch.String())
}

type OSArch struct {
	OS   string
	Arch string
}

func (o OSArch) IsLocal() bool {
	return o.OS == "" && o.Arch == ""
}

func (o OSArch) String() string {
	if o.IsLocal() {
		return "local"
	}
	return fmt.Sprintf("%s_%s", o.OS, o.Arch)
}

func (o OSArch) ZigArchStr() string {
	switch o.Arch {
	case "amd64":
		return "x86_64"
	default:
		alog.Panicf("Unknown arch %s", o.Arch)
	}
	panic("unreachable")
}
