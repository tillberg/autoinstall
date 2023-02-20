package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
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
	if pkg.Target.Mode == "dll" {
		buildDLLPackage(pkg)
	} else if Opts.BuildPlugins && !packageIsProgram(pkg) {
		buildPluginPackage(pkg)
	} else {
		buildProgramPackage(pkg)
	}
}

func quotedIfNeeded(str string) string {
	if strings.ContainsAny(str, " \n\t\"") {
		return strconv.Quote(str)
	} else {
		return str
	}
}

func logBuildCommand(logger *alog.Logger, dir string, env []string, args []string) {
	if Opts.Verbose {
		var s strings.Builder
		fmt.Fprintf(&s, "cd %s &&", dir)
		for _, v := range env {
			fmt.Fprintf(&s, " %s", quotedIfNeeded(v))
		}
		for _, v := range args {
			fmt.Fprintf(&s, " %s", quotedIfNeeded(v))
		}
		logger.Log(s.String())
	}
}

func buildDLLPackage(pkg *Package) {
	buildTimer := alog.NewTimer()
	logPrefix := fmt.Sprintf("@(dim:[%s]) ", path.Base(pkg.Name))
	logger := alog.New(alog.DefaultLogger, alog.Colorify(logPrefix), 0)
	outdir := filepath.Join(goPath, "bin", pkg.Target.OSArch.String())
	alog.BailIf(os.MkdirAll(outdir, 0755))
	outpath := filepath.Join(outdir, filepath.Base(pkg.Name+".dll"))
	cmd := exec.Command("go", "build", "-v", "-buildmode=c-shared", "-o", outpath)
	cmd.Dir = filepath.Join(goPath, "src", pkg.Name)
	cmd.Args = append(cmd.Args, getExtraBuildArgs()...)
	cmd.Stdout = logger
	cmd.Stderr = logger
	env := getBuildEnv(pkg)
	cmd.Env = append(os.Environ(), env...)
	logBuildCommand(logger, cmd.Dir, env, cmd.Args)
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
func buildProgramPackage(pkg *Package) {
	buildTimer := alog.NewTimer()
	logPrefix := fmt.Sprintf("@(dim:[%s]) ", path.Base(pkg.Name))
	logger := alog.New(alog.DefaultLogger, alog.Colorify(logPrefix), 0)
	cmd := exec.Command("go", "install", "-v")
	cmd.Dir = filepath.Join(goPath, "src", pkg.Name)
	cmd.Args = append(cmd.Args, getExtraBuildArgs()...)
	cmd.Stdout = logger
	cmd.Stderr = logger
	env := getBuildEnv(pkg)
	cmd.Env = append(os.Environ(), env...)
	logBuildCommand(logger, cmd.Dir, env, cmd.Args)
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
	logPrefix := fmt.Sprintf("@(dim:[plugin-%s]) ", path.Base(pkg.Name))
	logger := alog.New(alog.DefaultLogger, alog.Colorify(logPrefix), 0)
	cmd := exec.Command("go", "install", "-v", "-buildmode=plugin")
	cmd.Dir = filepath.Join(goPath, "src", pkg.Name)
	cmd.Args = append(cmd.Args, getExtraBuildArgs()...)
	cmd.Stdout = logger
	cmd.Stderr = logger
	env := getBuildEnv(pkg)
	cmd.Env = append(os.Environ(), env...)
	logBuildCommand(logger, cmd.Dir, env, cmd.Args)
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

func getBuildEnv(pkg *Package) (env []string) {
	osArch := pkg.Target.OSArch
	if !osArch.IsLocal() {
		env = append(env, "GOOS="+osArch.OS)
		env = append(env, "GOARCH="+osArch.Arch)
		env = append(env, "CGO_ENABLED=1")
		if Opts.ZigCC {
			target := fmt.Sprintf("%s-%s", osArch.ZigArchStr(), osArch.OS)
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
