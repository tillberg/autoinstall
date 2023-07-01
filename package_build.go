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

func quoteEnvIfNeeded(str string) string {
	if strings.ContainsAny(str, " \n\t\"") {
		name, value, _ := strings.Cut(str, "=")
		return fmt.Sprintf("%s=%q", name, value)
	} else {
		return str
	}
}

func quoteArgIfNeeded(str string) string {
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
			fmt.Fprintf(&s, " %s", quoteEnvIfNeeded(v))
		}
		for _, v := range args {
			fmt.Fprintf(&s, " %s", quoteArgIfNeeded(v))
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
	var overrides []string
	cmd.Env, overrides = getBuildEnv(pkg)
	logBuildCommand(logger, cmd.Dir, overrides, cmd.Args)
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
	var overrides []string
	cmd.Env, overrides = getBuildEnv(pkg)
	logBuildCommand(logger, cmd.Dir, overrides, cmd.Args)
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
	var overrides []string
	cmd.Env, overrides = getBuildEnv(pkg)
	logBuildCommand(logger, cmd.Dir, overrides, cmd.Args)
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

func getBuildEnv(pkg *Package) (env, overrides []string) {
	osArch := pkg.Target.OSArch
	for _, line := range os.Environ() {
		if !strings.HasPrefix(line, "GO111MODULE=") {
			env = append(env, line)
		}
	}
	overrides = append(overrides, "GO111MODULE=auto")
	if !osArch.IsLocal() {
		overrides = append(overrides, "GOOS="+osArch.OS)
		overrides = append(overrides, "GOARCH="+osArch.Arch)
		overrides = append(overrides, "CGO_ENABLED=1")
		if Opts.ZigCC {
			target := fmt.Sprintf("%s-%s", osArch.ZigArchStr(), osArch.OS)
			overrides = append(overrides, "CC=zig cc -target "+target)
			overrides = append(overrides, "CXX=zig c++ -target "+target)
		}
	}
	env = append(env, overrides...)
	return env, overrides
}

func getExtraBuildArgs() []string {
	var args []string
	if Opts.LDFlags != "" {
		args = append(args, "-ldflags")
		args = append(args, Opts.LDFlags)
	}
	if Opts.Tags != "" {
		args = append(args, "-tags")
		args = append(args, Opts.Tags)
	}
	return args
}
