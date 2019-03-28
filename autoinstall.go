package main

import (
	"go/build"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/jessevdk/go-flags"
	"github.com/tillberg/alog"
	"github.com/tillberg/autoinstall/dedupingchan"
	"github.com/tillberg/notifywrap"
	"github.com/tillberg/stringset"
)

const (
	enableSanityChecks = false
	DATE_FORMAT        = "2006-01-02T15:04:05.000000"
	maxBuilders        = 1 // kind of by design
)

var Opts struct {
	Verbose        bool   `short:"v" long:"verbose" description:"Show verbose debug information"`
	VeryVerbose    bool   `long:"very-verbose" description:"Show verbose debug information"`
	NoColor        bool   `long:"no-color" description:"Disable ANSI colors"`
	RunTests       bool   `long:"run-tests" description:"Run tests after building packages (after initial pass)"`
	Tags           string `long:"tags" description:"-tags parameter to pass to go-build"`
	TestArgShort   bool   `long:"test-arg-short" description:"Pass the -short flag to go test"`
	TestArgRun     string `long:"test-arg-run" description:"Pass the -run flag to go test with this value"`
	TestArgTimeout string `long:"test-arg-timeout" description:"Pass the -timeout flag to go test with this value"`
	LDFlags        string `long:"ldflags" description:"Pass the -ldflags to go install with this value"`
}

var goPath = (func() string {
	p := os.Getenv("GOPATH")
	if p == "" {
		p = build.Default.GOPATH
	}
	return p
})()

type BuildResult struct {
	*Package
	Success bool
	Retry   bool
}

var goPathSrcRoot = (func() string {
	p, err := filepath.EvalSymlinks(filepath.Join(goPath, "src"))
	alog.BailIf(err)
	return p
})()

var packages = map[string]*Package{}
var buildQueue []*Package
var buildDone = make(chan []BuildResult)
var moduleUpdateChan = make(chan *Package)
var finishedInitialPass = false
var numBuildSucesses = 0
var numBuildFailures = 0
var numUnready = 0
var numBuildsActive = 0
var numPackageBuildsActive = 0
var startupLogger *alog.Logger
var neverChan <-chan time.Time
var lastModuleUpdateTime time.Time

func numWorkersActive() int {
	return numBuildsActive
}

var startupFormatStrBase = alog.Colorify("@(green:%d) @(dim:package%s built.)")
var startupFormatStrChecking = alog.Colorify(" @(dim:Checking) @(green:%d)@(dim:...)")
var startupFormatStrNotReady = alog.Colorify(" @(warn:%d) @(dim:package%s could not be built.)\n")

func pluralize(num int, s string) string {
	if num == 1 {
		return ""
	}
	return s
}

func updateStartupText(final bool) {
	if !final {
		if !Opts.VeryVerbose {
			return
		}
		format := startupFormatStrBase + startupFormatStrChecking
		checkingTotal := len(buildQueue) + numPackageBuildsActive
		startupLogger.Replacef(format, numBuildSucesses, pluralize(numBuildSucesses, "s"), checkingTotal)
	} else {
		format := startupFormatStrBase + startupFormatStrNotReady
		startupLogger.Replacef(format, numBuildSucesses, pluralize(numBuildSucesses, "s"), numUnready, pluralize(numUnready, "s"))
		startupLogger.Close()
	}
}

type DispatchState int

const (
	DispatchIdle DispatchState = iota
	DispatchCanTriggerBuild
	DispatchWaitingForWork
	DispatchMaybeFinishedInitialPass
)

func getDispatchState() DispatchState {
	if len(buildQueue) > 0 {
		if numWorkersActive() < maxBuilders {
			return DispatchCanTriggerBuild
		}
	} else if !finishedInitialPass && numWorkersActive() == 0 {
		return DispatchMaybeFinishedInitialPass
	}
	if numWorkersActive() > 0 {
		return DispatchWaitingForWork
	}
	return DispatchIdle
}

func getTimeout(state DispatchState) <-chan time.Time {
	switch state {
	case DispatchIdle:
		return nil
	case DispatchCanTriggerBuild:
		// Debounce filesystem events a little (though `watcher` already should be doing that more aggressively).
		// In particular, this delay ensures that we process all messages waiting in the various queues before
		// pushing work to workers.
		return time.After(1 * time.Millisecond)
	case DispatchWaitingForWork:
		// Periodically output messages about outstanding builds, both to inform the user about progress on very
		// slow builds as well as to help debug "stuck" builds.
		return time.After(120 * time.Second)
	case DispatchMaybeFinishedInitialPass:
		// When we can wait a whole second with empty queues and no active work, then we call the initial pass
		// complete. This is kind of kludgy but is functional; a more "correct" solution would require `watcher`
		// informing us when *it* had completed a first full walk of the directory tree.
		return time.After(1 * time.Second)
	}
	alog.Panicf("getTimeout received unexpected DispatchState %s", state)
	return nil
}

func dispatcher() {
	startupLogger = alog.New(os.Stderr, "@(dim:{isodate}) ", 0)

	for {
		if !finishedInitialPass {
			updateStartupText(false)
		}
		dispatchState := getDispatchState()
		timeout := getTimeout(dispatchState)

		select {
		case buildResults := <-buildDone:
			numBuildsActive--
			numPackageBuildsActive -= len(buildResults)
			for _, buildResult := range buildResults {
				p := buildResult.Package
				if buildResult.Retry {
					switch p.State {
					case PackageBuilding:
						queueBuild(p, "need to retry build")
					case PackageBuildingButDirty:
						queueBuild(p, "sources changed during build that needed retry anyway")
					default:
						alog.Panicf("buildResult with state %s", p.State)
					}
				} else if buildResult.Success {
					numBuildSucesses++
					switch p.State {
					case PackageBuilding:
						chState(p, PackageReady)
					case PackageBuildingButDirty:
						queueBuild(p, "sources changed during successful build")
					default:
						alog.Panicf("buildResult with state %s", p.State)
					}
				} else {
					numBuildFailures++
					switch p.State {
					case PackageBuilding:
						chState(p, PackageDirtyIdle)
					case PackageBuildingButDirty:
						queueBuild(p, "sources changed during failed build")
					default:
						alog.Panicf("buildResult with state %s", p.State)
					}
				}
				if !p.ShouldBuild {
					removeFromIndex(p.Name)
				}
			}
		case pNew := <-moduleUpdateChan:
			lastModuleUpdateTime = time.Now()
			p := packages[pNew.Name]
			if p == nil {
				if !pNew.ShouldBuild {
					// Don't add non-command packages to the index. Just trigger dependencies.
					triggerDependentPackages(pNew.Name, pNew.FileChange)
					continue
				}
				p = pNew
				p.State = PackageDirtyIdle
				packages[pNew.Name] = p
				numUnready++
			} else {
				p.ShouldBuild = pNew.ShouldBuild
				p.PossibleImports = pNew.PossibleImports
			}
			if pNew.FileChange {
				switch p.State {
				case PackageReady, PackageDirtyIdle:
					if p.ShouldBuild {
						queueBuild(p, "sources changed")
					} else {
						removeFromIndex(p.Name)
					}
				case PackageBuilding:
					chState(p, PackageBuildingButDirty)
				case PackageBuildingButDirty, PackageBuildQueued:
					// Already have a build queued (or will), no need to change
				default:
					alog.Panicf("moduleUpdateChan encountered unexpected state %s", p.State)
				}
			}
		case <-timeout:
			switch dispatchState {
			case DispatchMaybeFinishedInitialPass:
				// We've reached the conclusion of the initial pass
				finishedInitialPass = true
				printStartupSummary()
			case DispatchCanTriggerBuild:
				pushBuildWork()
			case DispatchWaitingForWork:
				for _, p := range packages {
					switch p.State {
					case PackageBuilding, PackageBuildingButDirty:
						alog.Printf("@(dim:Still building %s...)\n", p.Name)
					}
				}
			default:
				alog.Panicf("dispatch hit timeout with unexpected dispatchState %s", dispatchState)
			}
		}
	}
}

func removeFromIndex(pName string) {
	alog.Printf("@(dim:Removing package %s from index.)\n", pName)
	delete(packages, pName)
	// Trigger updates of any packages that depend on this import name
	// XXX this should be modified if triggerDependentPackages is made more specific in the future
	triggerDependentPackages(pName, true)
}

func pushBuildWork() {
	if numWorkersActive() >= maxBuilders {
		return
	}
	maxPackages := 1 // was runtime.GOMAXPROCS(0)
	var numToBuild int
	if len(buildQueue) > maxPackages {
		numToBuild = maxPackages
	} else {
		numToBuild = len(buildQueue)
	}
	var packages []*Package
	packages, buildQueue = buildQueue[:numToBuild], buildQueue[numToBuild:]

	for _, pkg := range packages {
		if pkg.State != PackageBuildQueued {
			alog.Panicf("Package %s was in buildQueue but had state %s", pkg.Name, pkg.State)
		}
		if beVerbose() {
			alog.Printf("@(dim:Building) %s\n", pkg.Name)
		}
		chState(pkg, PackageBuilding)
	}
	numBuildsActive++
	numPackageBuildsActive += len(packages)
	go buildPackages(packages)
}

func chState(p *Package, state PackageState) {
	if p.State == PackageDirtyIdle {
		numUnready--
	}
	if state == PackageDirtyIdle {
		numUnready++
	}
	p.State = state
}

func queueBuild(p *Package, reason string) {
	if !p.ShouldBuild {
		return
	}
	if enableSanityChecks {
		for _, pkg := range buildQueue {
			if pkg == p {
				alog.Panicf("Package %s already in buildQueue, cannot queue twice", p.Name)
			}
		}
	}
	if beVeryVerbose() {
		alog.Printf("@(dim:Queued build for %s: %s)\n", p.Name, reason)
	}
	chState(p, PackageBuildQueued)
	buildQueue = append(buildQueue, p)
}

func triggerDependentPackages(fullImportName string, isFileChange bool) {
	if finishedInitialPass && beVerbose() {
		alog.Printf("@(dim:Triggering check of packages that import %s)\n", fullImportName)
	}
	for _, pkg := range packages {
		if pkg.PossibleImports != nil && pkg.PossibleImports.Has(fullImportName) {
			if finishedInitialPass && beVerbose() {
				alog.Printf("@(dim:Triggering check of %s)\n", pkg.Name)
			}
			packageUpdates.In <- PackageUpdateTrigger{FullImportName: pkg.Name, FileChange: isFileChange}
		}
	}
}

func possibleFullImportNames(pkgName string, importName string) []string {
	results := []string{importName}
	rootPath := pkgName + "/"
	for {
		results = append(results, rootPath+"vendor/"+importName)
		if len(rootPath) == 0 {
			break
		}
		lastIndex := strings.LastIndex(rootPath[:len(rootPath)-1], "/")
		if lastIndex == -1 {
			rootPath = ""
		} else {
			rootPath = rootPath[:lastIndex+1]
		}
	}
	return results
}

func printStartupSummary() {
	alog.Printf("@(dim:Finished initial pass of all packages.)\n")
	updateStartupText(true)
}

func beVerbose() bool {
	return finishedInitialPass || Opts.Verbose || Opts.VeryVerbose
}

func beVeryVerbose() bool {
	return (finishedInitialPass && Opts.Verbose) || Opts.VeryVerbose
}

func shouldRunTests() bool {
	return finishedInitialPass && Opts.RunTests
}

var buildExtensions = stringset.New(".go") //, ".c", ".cc", ".cxx", ".cpp", ".h", ".hh", ".hpp", ".hxx", ".s", ".swig", ".swigcxx", ".syso")

type PackageUpdateTrigger struct {
	FullImportName string
	FileChange     bool
}

var packageUpdates *dedupingchan.Chan = dedupingchan.New()

func processPackageUpdates() {
	for pkgName := range packageUpdates.Out {
		processPackageTrigger(pkgName.(PackageUpdateTrigger))
	}
}

func processPackageTrigger(trigger PackageUpdateTrigger) {
	fullImportName := trigger.FullImportName
	if beVerbose() {
		alog.Printf("Updating package %s\n", fullImportName)
	}
	pkg := &Package{
		Name:        fullImportName,
		ShouldBuild: true,
		FileChange:  trigger.FileChange,
	}
	for _, namePart := range strings.Split(fullImportName, "/") {
		if namePart == "internal" || namePart == "vendor" {
			pkg.ShouldBuild = false
			break
		}
	}
	if pkg.ShouldBuild {
		deps, isCommand := getPackageImports(fullImportName)
		if isCommand {
			allPossible := stringset.New()
			for _, importName := range deps.All() {
				for _, depFullImportName := range possibleFullImportNames(pkg.Name, importName) {
					allPossible.Add(depFullImportName)
				}
			}
			pkg.PossibleImports = allPossible
		} else {
			pkg.ShouldBuild = false
		}
	}
	if !pkg.ShouldBuild && !pkg.FileChange {
		return
	}
	moduleUpdateChan <- pkg
}

func processPathTriggers(notifyChan <-chan *notifywrap.EventInfo) {
	for pathEvent := range notifyChan {
		if !buildExtensions.Has(filepath.Ext(pathEvent.Path)) {
			continue
		}
		fullImportName, err := filepath.Rel(goPathSrcRoot, filepath.Dir(pathEvent.Path))
		if err != nil {
			alog.Bail(err)
		}
		if beVeryVerbose() {
			alog.Printf("@(dim:Triggering module) @(cyan:%s) @(dim:due to update of) @(cyan:%s)\n", fullImportName, pathEvent.Path)
		}
		packageUpdates.In <- PackageUpdateTrigger{FullImportName: fullImportName, FileChange: true}
	}
}

func main() {
	sighup := make(chan os.Signal)
	signal.Notify(sighup, syscall.SIGHUP)
	_, err := flags.ParseArgs(&Opts, os.Args)
	if err != nil {
		err2, ok := err.(*flags.Error)
		if ok && err2.Type == flags.ErrHelp {
			return
		}
		alog.Printf("Error parsing command-line options: %s\n", err)
		return
	}
	if Opts.NoColor {
		alog.DisableColor()
	} else {
		alog.AddAnsiColorCode("time", alog.ColorBlue)
	}
	alog.Printf("@(dim:autoinstall started.)\n")
	pluralProcess := ""
	if runtime.GOMAXPROCS(0) != 1 {
		pluralProcess = "es"
	}
	alog.Printf("@(dim:Building all packages in) @(dim,cyan:%s)\n", goPath, runtime.GOMAXPROCS(0), pluralProcess)
	if !beVerbose() {
		alog.Printf("@(dim:Use) --verbose @(dim:to show all messages during startup.)\n")
	}
	if len(Opts.LDFlags) >= 2 && strings.HasPrefix(Opts.LDFlags, "'") && strings.HasSuffix(Opts.LDFlags, "'") {
		Opts.LDFlags = Opts.LDFlags[1 : len(Opts.LDFlags)-1]
	}

	versionCmd := exec.Command("go", "version")
	out, err := versionCmd.CombinedOutput()
	alog.BailIf(err)
	alog.Printf("@(dim:%s)\n", out)

	watcherOpts := notifywrap.Opts{
		DebounceDuration:           200 * time.Millisecond,
		CoalesceEventTypes:         true,
		NotifyDirectoriesOnStartup: true,
		NotifyFilesOnStartup:       true,
	}
	pathEvents, err := notifywrap.WatchRecursive(goPathSrcRoot, watcherOpts)
	alog.BailIf(err)

	for i := 0; i < runtime.NumCPU(); i++ {
		go processPackageUpdates()
	}
	go dispatcher()

	go processPathTriggers(pathEvents)

	<-sighup
	startupLogger.Close()
}
