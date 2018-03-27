package main

import (
	"bufio"
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tillberg/alog"
	"github.com/tillberg/stringset"
)

var ansiDimBytes = []byte(alog.Colorify("@(dim)"))
var ansiResetBytes = []byte(alog.Colorify("@(r)"))
var newlineBytes = []byte("\n")

var cantLoadPackageBytes = []byte("can't load package: package ")
var colonBytes = []byte(":")
var srcPrefixBytes = []byte("src/")
var goBuildBytes = []byte("go build ")
var goInstallBytes = []byte("go install ")
var inBytes = []byte(" in ")
var inFileIncludedFromBytes = []byte("In file included from src/")
var recursiveFromBytes = []byte("                 from src/")
var pkgCommentBytes = []byte("# ")
var ldFailureBytes = []byte("collect2: error: ld returned ")
var pkgConfigMissingBytes = []byte("Perhaps you should add the directory containing `")
var bytesPeriod = []byte(".")

func buildPackages(packages []*Package) {
	logger := alog.New(alog.DefaultLogger, alog.Colorify("@(dim:[go-install]) "), 0)

	buildTimer := alog.NewTimer()
	cmd := exec.Command("go", "install", "-v")
	cmd.Dir = goPath
	if Opts.Tags != "" {
		cmd.Args = append(cmd.Args, "-tags")
		cmd.Args = append(cmd.Args, Opts.Tags)
	}
	if Opts.LDFlags != "" {
		cmd.Args = append(cmd.Args, "-ldflags")
		cmd.Args = append(cmd.Args, Opts.LDFlags)
	}
	cmd.Args = append(cmd.Args, "--")
	for _, pkg := range packages {
		cmd.Args = append(cmd.Args, pkg.Name)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		logger.Panicf("Failed to get stderr pipe for go-install command: %v\n", err)
	}

	err = cmd.Start()
	if err != nil {
		logger.Panicf("Failed to start go-install command: %v\n", err)
	}

	failedSet := stringset.New()

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		s := bufio.NewScanner(stderr)
		s.Split(bufio.ScanLines)
		for s.Scan() {
			line := s.Bytes()
			if true {
				logger.Write(ansiDimBytes)
				logger.Write(line)
				logger.Write(ansiResetBytes)
				logger.Write(newlineBytes)
			}
			if bytes.HasPrefix(line, cantLoadPackageBytes) {
				pName := string(bytes.SplitN(line[len(cantLoadPackageBytes):], colonBytes, 2)[0])
				if failedSet.Add(pName) {
					logger.Printf("Can't load %q\n", pName)
				}
			} else if bytes.HasPrefix(line, srcPrefixBytes) {
				srcName := string(bytes.SplitN(line[len(srcPrefixBytes):], colonBytes, 2)[0])
				pName := filepath.Dir(srcName)
				if failedSet.Add(pName) {
					logger.Printf("Can't build %q\n", pName)
				}
			} else if bytes.HasPrefix(line, goBuildBytes) {
				// e.g. "go build github.com/tillberg/util/benchmarks: no non-test Go files in ..."
				pName := string(bytes.SplitN(line[len(goBuildBytes):], colonBytes, 2)[0])
				if failedSet.Add(pName) {
					logger.Printf("Build failed on 'go build': %s\n", pName)
				}
			} else if bytes.HasPrefix(line, goInstallBytes) {
				// e.g. "go install gopkg.in/mcuadros/go-syslog.v2/example: build output "/path/to/go/bin/example" already exists and is not an object file"
				pName := string(bytes.SplitN(line[len(goInstallBytes):], colonBytes, 2)[0])
				if failedSet.Add(pName) {
					logger.Printf("Build failed on 'go install': %s\n", pName)
				}
			} else if bytes.HasPrefix(line, inFileIncludedFromBytes) || bytes.HasPrefix(line, recursiveFromBytes) {
				// recursiveFromBytes is the same length as inFileIncludedFromBytes
				// e.g. "In file included from src/github.com/go-gl-legacy/gl/attriblocation.go:7:0:"
				// e.g. "                 from src/github.com/chsc/gogl/wgl/wgl.go:81:"
				srcName := string(bytes.SplitN(line[len(inFileIncludedFromBytes):], colonBytes, 2)[0])
				pName := filepath.Dir(srcName)
				if failedSet.Add(pName) {
					logger.Printf("Can't build include for %q\n", pName)
				}
			} else if bytes.HasPrefix(line, pkgCommentBytes) {
				pName := string(line[len(pkgCommentBytes):])
				if !strings.HasPrefix(pName, "pkg-config ") {
					if failedSet.Add(pName) {
						logger.Printf("Build failed for %q\n", pName)
					}
				}
			} else if bytes.HasPrefix(line, pkgConfigMissingBytes) {
				// Kludge because go-install log doesn't give enough information to determine package name:
				pkgConfigName := string(bytes.SplitN(line[len(pkgConfigMissingBytes):], bytesPeriod, 2)[0])
				var pName string
				switch pkgConfigName {
				case "sdl2":
					pName = "github.com/veandco/go-sdl2/sdl"
				default:
					alog.Panicf("Couldn't find unknown pkg-config %q\n", pkgConfigName)
				}
				if failedSet.Add(pName) {
					logger.Printf("Couldn't find pkg-config %q for %q\n", pkgConfigName, pName)
				}
			}
		}
		if err := s.Err(); err != nil {
			alog.Printf("Error scanning go-install stderr: %v\n", err)
		}
		wg.Done()
	}()

	// Wait for stderr reader to finish
	wg.Wait()

	waitErr := cmd.Wait()
	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			logger.Printf("go-install exited with %v\n", exitErr)
		} else {
			logger.Printf("go-install command failed: %v\n", waitErr)
		}
	}
	logger.Printf("go-install finished in %s\n", buildTimer.FormatElapsedColor(2*time.Second, 10*time.Second))

	var numSuccess int
	var numRetry int
	var numFail int
	results := []BuildResult{}
	understoodFailedSet := stringset.New()
	for _, pkg := range packages {
		result := BuildResult{
			Package: pkg,
		}
		failed := false
		if failedSet.Len() != 0 {
			if failedSet.Has(pkg.Name) {
				failed = true
				understoodFailedSet.Add(pkg.Name)
			}
			failedDeps := pkg.PossibleImports.Intersection(failedSet)
			for _, fullImportName := range failedDeps.All() {
				failed = true
				understoodFailedSet.Add(fullImportName)
			}
		}

		if failed {
			numFail += 1
		} else if failedSet.Len() != 0 {
			result.Retry = true
			numRetry += 1
		} else {
			result.Success = true
			numSuccess += 1
		}
		results = append(results, result)
	}
	for _, name := range understoodFailedSet.All() {
		failedSet.Remove(name)
	}
	if failedSet.Len() != 0 {
		logger.Panicf("Got unexpected/dependency package build failures: %v\n", failedSet.All())
	}
	if waitErr != nil && numFail == 0 {
		logger.Panicf("go-install failed, but no matching package failures were captured\n")
	}

	logger.Printf("@(green:%d) success, @(dim:%d) retry, @(warn:%d) fail\n", numSuccess, numRetry, numFail)
	buildDone <- results
}
