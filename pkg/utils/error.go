// Copyright 2021 Synology Inc.

package utils

type OutOfFreeSpaceError string
type AlreadyExistError string
type LunReachMaxCountError string
type TargetReachMaxCountError string
type NoSuchSnapshotError string
type BadLunTypeError string
type SnapshotReachMaxCountError string
type IscsiDefaultError string

func (_ OutOfFreeSpaceError) Error() string {
	return "Out of free space"
}
func (_ AlreadyExistError) Error() string {
	return "Already Existed"
}

func (_ LunReachMaxCountError) Error() string {
	return "Number of LUN reach limit"
}

func (_ TargetReachMaxCountError) Error() string {
	return "Number of target reach limit"
}

func (_ NoSuchSnapshotError) Error() string {
	return "No such snapshot uuid"
}

func (_ BadLunTypeError) Error() string {
	return "Bad LUN type"
}

func (_ SnapshotReachMaxCountError) Error() string {
	return "Number of snapshot reach limit"
}

func (_ IscsiDefaultError) Error() string {
	return "Default ISCSI error"
}