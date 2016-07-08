package main

//go:generate stringer -type=PackageState

type PackageState int

const (
	PackageDirtyIdle PackageState = iota
	PackageUpdateQueued
	PackageUpdating
	PackageUpdatingButDirty
	PackageBuildQueued
	PackageBuilding
	PackageBuildingButDirty
	PackageReady
)
