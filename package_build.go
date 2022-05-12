package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/tillberg/alog"
)

var funcmainslice = []byte("func main() {")

func packageIsProgram(pkg *Package) bool {
	// detectTimer := alog.NewTimer()
	// defer func() {
	// 	alog.Printf("@(dim:[)%s@(dim:]) packageIsProgram? %s)\n", detectTimer.FormatElapsedColor(2*time.Second, 10*time.Second), pkg.Key())
	// }()
	packagePath := filepath.Join(goPathSrcRoot, pkg.Name)
	infos, err := ioutil.ReadDir(packagePath)
	if err != nil {
		alog.Printf("@(warn:Failed to read directory %q: %v)\n", packagePath, err)
		return true
	}
	for _, info := range infos {
		if filepath.Ext(info.Name()) == ".go" {
			fullpath := filepath.Join(packagePath, info.Name())
			f, err := os.Open(fullpath)
			if err != nil {
				alog.Printf("@(warn:Failed to open file %q: %v)\n", fullpath, err)
				continue
			}
			s := bufio.NewScanner(f)
			for s.Scan() {
				if bytes.HasPrefix(s.Bytes(), funcmainslice) {
					f.Close()
					return true
				}
			}
			err = s.Err()
			if err != nil {
				alog.Printf("@(warn:Failed to read file %q: %v)\n", fullpath, err)
				continue
			}
			f.Close()
		}
	}
	return false
}

func buildPackage(pkg *Package) {
	if Opts.BuildPlugins && !packageIsProgram(pkg) {
		buildPluginPackage(pkg)
	} else {
		buildProgramPackage(pkg)
	}
}

func buildProgramPackage(pkg *Package) {
	buildTimer := alog.NewTimer()
	logger := alog.New(alog.DefaultLogger, alog.Colorify("@(dim:[install]) "), 0)
	cmd := exec.Command("go", "install", "-v")
	cmd.Dir = filepath.Join(goPath, "src", pkg.Name)
	cmd.Args = append(cmd.Args, getExtraBuildArgs()...)
	cmd.Stdout = logger
	cmd.Stderr = logger
	cmd.Env = getBuildEnv(pkg)
	err := cmd.Run()
	success := err == nil
	if success {
		logger.Printf("@(dim:[)%s@(dim:]) success @(dim:program) @(bright,blue:%s)\n", buildTimer.FormatElapsedColor(2*time.Second, 10*time.Second), pkg.Key())
	} else {
		logger.Printf("@(dim:[)%s@(dim:])    @(dim:fail program %s)\n", buildTimer.FormatElapsedColor(2*time.Second, 10*time.Second), pkg.Key())
	}
	buildDone <- BuildResult{
		Package: pkg,
		Success: success,
	}
}

func buildPluginPackage(pkg *Package) {
	buildTimer := alog.NewTimer()
	logger := alog.New(alog.DefaultLogger, alog.Colorify("@(dim:[install]) "), 0)
	cmd := exec.Command("go", "install", "-v", "-buildmode=plugin")
	cmd.Dir = filepath.Join(goPath, "src", pkg.Name)
	cmd.Args = append(cmd.Args, getExtraBuildArgs()...)
	cmd.Stdout = logger
	cmd.Stderr = logger
	cmd.Env = getBuildEnv(pkg)
	err := cmd.Run()
	success := err == nil
	if success {
		logger.Printf("@(dim:[)%s@(dim:]) success @(dim:plugin)  @(bright,blue:%s)\n", buildTimer.FormatElapsedColor(2*time.Second, 10*time.Second), pkg.Key())
	} else {
		logger.Printf("@(dim:[)%s@(dim:])    @(dim:fail plugin  %s)\n", buildTimer.FormatElapsedColor(2*time.Second, 10*time.Second), pkg.Key())
	}
	buildDone <- BuildResult{
		Package: pkg,
		Success: success,
	}
}

func getBuildEnv(pkg *Package) []string {
	env := os.Environ()
	if !pkg.OSArch.IsLocal() {
		env = append(env, "GOOS="+pkg.OSArch.OS)
		env = append(env, "GOARCH="+pkg.OSArch.Arch)
		if Opts.ZigCC {
			target := fmt.Sprintf("%s-%s", pkg.OSArch.ZigArchStr(), pkg.OSArch.OS)
			env = append(env, "CC=zig cc -target "+target)
			env = append(env, "CXX=zig c++ -target "+target)
		}
	}
	return env
}

func getExtraBuildArgs() []string {
	var args []string
	if Opts.LDFlags != "" {
		args = append(args, "-ldflags")
		args = append(args, Opts.LDFlags)
	}
	return args
}
