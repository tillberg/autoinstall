package main

import "github.com/tillberg/stringset"

type Package struct {
	Name            string       // Full path of import within GOPATH, including .../vendor/ if present
	State           PackageState // Current state of this Package (idle/building/ready/etc)
	ShouldBuild     bool
	PossibleImports *stringset.StringSet // List of all possible imports (vendor-exploded)
	FileChange      bool
}
