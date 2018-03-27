package main

//go:generate stringer -type=PackageState

type PackageState int

const (
	PackageDirtyIdle PackageState = iota
	PackageBuildQueued
	PackageBuilding
	PackageBuildingButDirty
	PackageReady
)
