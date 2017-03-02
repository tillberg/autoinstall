package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tillberg/alog"
	"github.com/tillberg/bismuth2"
)

func (p *Package) build() {
	ctx := bismuth2.New()
	ctx.Verbose = Opts.Verbose

	timer := alog.NewTimer()
	absPath := p.getAbsSrcPath()
	if beVerbose() {
		alog.Printf("@(dim:Building) %s@(dim:...)\n", p.Name)
	}
	targetPath := p.getAbsTargetPath()
	tmpTargetPath := targetPath + "." + RandStr(10) + ".autoinstall-tmp"
	fail := func() {
		os.Remove(tmpTargetPath)
		buildFailure <- p
	}
	args := []string{"go", "build", "-o", tmpTargetPath}
	if Opts.Tags != "" {
		args = append(args, "-tags")
		args = append(args, Opts.Tags)
	}
	var err error
	if beVerbose() {
		err = ctx.QuoteCwd("go-build:"+p.Name, absPath, args...)
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
	if Opts.Verbose {
		alog.Printf("@(dim:[)%s@(dim:]) @(green:Successfully built) %s @(dim:->) @(time:%s)\n", durationStr, p.Name, p.UpdateStartTime.Format(DATE_FORMAT))
	} else {
		alog.Printf("@(dim:[)%s@(dim:]) @(green:Successfully built) %s\n", durationStr, p.Name)
	}
	if p.HasTests && shouldRunTests() {
		args := []string{"go", "test"}
		if Opts.TestArgShort {
			args = append(args, "-short")
		}
		if Opts.TestArgRun != "" {
			args = append(args, "-run")
			args = append(args, Opts.TestArgRun)
		}
		go ctx.QuoteCwd("go-test:"+p.Name, absPath, args...)
	}

	buildSuccess <- p
}
