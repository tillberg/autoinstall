package main

import (
	"fmt"
	"go/build"
	"path/filepath"
	"strings"
	"time"

	"github.com/tillberg/stringset"
)

type Package struct {
	Name       string       // Full path of import within GOPATH, including .../vendor/ if present
	ImportName string       // Import path which this package provides, i.e. Name, minus .../vendor/
	State      PackageState // Current state of this Package (idle/building/ready/etc)

	IsStandard bool

	Imports    *stringset.StringSet // List of package dependencies as specified in import statements
	Deps       DepSlice             // List of all recursive package dependencies with BuildIds (sorted by name)
	IsProgram  bool                 // True if the target is an executable
	HasTests   bool                 // True if the package has any _test.go files
	AllSources []string             // All source files, for input into BuildId

	BuiltModTime    time.Time // The time that the most recent build was started; should also be the ModTime on the target file
	SourceModTime   time.Time // The ModTime of the most recently updated source file in this package
	RecentSrcName   string    // Name of the most recent source file; its ModTime equals the package's SourceModTime
	UpdateStartTime time.Time // The time that the current update started
	UpdateError     error     // Error encountered during update, if any (only set on pUpdate objects)
	RemovePackage   bool      // Set to true if this package should be removed from the index
	WasUpdated      bool      // Set to true once this package has been Updated once

	DesiredBuildID string
	CurrentBuildID string

	LastBuildInputsModTime time.Time // Safety valve to prevent repeated build attempts without real updates to its inputs

	// PackageDeps *stringset.StringSet // Actual Packages resolved in most recent build attempt
}

func NewPackage(name string) *Package {
	return &Package{
		Name: name,
	}
}

func (p *Package) init() {
	vendorIndex := strings.LastIndex(p.Name, "/vendor/")
	if vendorIndex >= 0 {
		p.ImportName = p.Name[vendorIndex+len("/vendor/"):]
	} else if strings.HasPrefix(p.Name, "vendor/") {
		p.ImportName = p.Name[len("vendor/"):]
	} else {
		p.ImportName = p.Name
	}
}

func (p *Package) isVendored() bool {
	return p.Name != p.ImportName
}

// We shouldn't build programs inside vendor/ trees
func (p *Package) shouldBuild() bool {
	return !p.isVendored() || !p.IsProgram
}

func (p *Package) getAbsSrcPath() string {
	var root string
	if p.IsStandard {
		root = build.Default.GOROOT
	} else {
		root = build.Default.GOPATH
	}
	return filepath.Join(root, "src", p.Name)
}

func (p *Package) getAbsTargetPath() string {
	if p.IsProgram {
		// XXX This is too simple, as there is some special casing for go tools at
		// https://github.com/golang/go/blob/003a68bc7fcb917b5a4d92a5c2244bb1adf8f690/src/cmd/go/pkg.go#L693-L715
		return filepath.Join(goPath, "bin", filepath.Base(p.Name))
	} else {
		var root string
		if p.IsStandard {
			root = build.Default.GOROOT
		} else {
			root = build.Default.GOPATH
		}
		pkgArch := fmt.Sprintf("%s_%s", build.Default.GOOS, build.Default.GOARCH)
		return filepath.Join(root, "pkg", pkgArch, p.Name+".a")
	}
}
