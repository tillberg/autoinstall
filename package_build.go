package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tillberg/ansi-log"
	"github.com/tillberg/bismuth2"
	"github.com/tillberg/util/randstr"
)

func (p *Package) build() {
	ctx := bismuth2.New()
	ctx.Verbose = Opts.Verbose

	timer := alog.NewTimer()
	// Just in case it gets deleted for some reason:
	absPath := filepath.Join(srcRoot, p.Name)

	if beVerbose() {
		alog.Printf("@(dim:Building) %s@(dim:...)\n", p.Name)
	}
	tmpTargetPath := filepath.Join(tmpdir, randstr.RandStr(20))
	fail := func() {
		os.Remove(tmpTargetPath)
		buildFailure <- p
	}
	args := []string{"go", "build", "-o", tmpTargetPath}
	var err error
	if beVerbose() {
		err = ctx.QuoteCwd("go-build", absPath, args...)
	} else {
		_, _, err = ctx.RunCwd(absPath, args...)
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if waitStatus, ok := exitErr.Sys().(syscall.WaitStatus); ok {
				if beVerbose() {
					alog.Printf("@(error:Failed to build) %s @(dim)(status=%d)@(r)\n", p.Name, waitStatus.ExitStatus())
				}
				fail()
				return
			}
		}
		alog.Printf("@(error:Failed to install) %s@(error:: %s)\n", p.Name, err)
		fail()
		return
	}
	err = os.Chtimes(tmpTargetPath, time.Now(), p.UpdateStartTime)
	if err != nil {
		alog.Printf("@(error:Error setting atime/mtime of) %s@(error::) %v\n", tmpTargetPath, err)
		fail()
		return
	}
	targetPath := p.getAbsTargetPath()
	targetDir := filepath.Dir(targetPath)
	err = os.MkdirAll(targetDir, 0750)
	if err != nil {
		alog.Printf("@(error:Error creating directory %s for build target: %v)\n", targetDir, err)
		fail()
		return
	}
	err = os.Rename(tmpTargetPath, targetPath)
	if err != nil {
		alog.Printf("@(error:Error renaming %q to %q: %v)\n", tmpTargetPath, targetPath, err)
		fail()
		return
	}
	durationStr := timer.FormatElapsedColor(2*time.Second, 10*time.Second)
	if beVerbose() {
		alog.Printf("@(dim:[)%s@(dim:]) @(green:Successfully built) %s @(dim:->) @(time:%s)\n", durationStr, p.Name, p.UpdateStartTime.Format(DATE_FORMAT))
	} else {
		alog.Printf("@(dim:[)%s@(dim:]) @(green:Successfully built) %s\n", durationStr, p.Name)
	}

	buildSuccess <- p
}
