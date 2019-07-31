package main

import (
	"bytes"
	"os/exec"
	"sync"

	"github.com/tillberg/alog"
	"github.com/tillberg/stringset"
)

var stdPackageSet *stringset.StringSet
var stdPackageSetOnce sync.Once

func initStandardPackages() {
	stdPackageSet = stringset.New("C")
	cmd := exec.Command("go", "list", "std")
	cmd.Dir = goPath
	buf, err := cmd.CombinedOutput()
	alog.BailIf(err)
	for _, line := range bytes.Split(buf, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		stdPackageSet.Add(string(line))
	}
}

func isStandardPkg(name string) bool {
	stdPackageSetOnce.Do(initStandardPackages)
	return stdPackageSet.Has(name)
}
