// Code generated by "stringer -type=PackageState"; DO NOT EDIT

package main

import "fmt"

const _PackageState_name = "PackageDirtyIdlePackageUpdateQueuedPackageUpdatingPackageUpdatingButDirtyPackageBuildQueuedPackageBuildingPackageBuildingButDirtyPackageReady"

var _PackageState_index = [...]uint8{0, 16, 35, 50, 73, 91, 106, 129, 141}

func (i PackageState) String() string {
	if i < 0 || i >= PackageState(len(_PackageState_index)-1) {
		return fmt.Sprintf("PackageState(%d)", i)
	}
	return _PackageState_name[_PackageState_index[i]:_PackageState_index[i+1]]
}
