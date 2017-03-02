package main

import (
	"os"
	"os/exec"
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
		alog.Printf("@(dim:Installing) %s@(dim:...)\n", p.Name)
	}
	targetPath := p.getAbsTargetPath()
	fail := func() {
		buildFailure <- p
	}
	args := []string{"go", "install", "-v"}
	if Opts.Tags != "" {
		args = append(args, "-tags")
		args = append(args, Opts.Tags)
	}
	var err error
	if beVerbose() {
		err = ctx.QuoteCwd("go-install:"+p.Name, absPath, args...)
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
		alog.Printf("@(error:Failed to build) %s@(error:: %s)\n", p.Name, err)
		fail()
		return
	}
	err = os.Chtimes(targetPath, time.Now(), p.UpdateStartTime)
	if err != nil {
		alog.Printf("@(error:Error setting atime/mtime of) %s@(error::) %v\n", targetPath, err)
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
