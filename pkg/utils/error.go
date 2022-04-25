// Copyright 2021 Synology Inc.

package utils

import (
	"fmt"
)

type OutOfFreeSpaceError string
type AlreadyExistError string
type BadParametersError string
type NoSuchLunError string
type LunReachMaxCountError string
type TargetReachMaxCountError string
type NoSuchSnapshotError string
type BadLunTypeError string
type SnapshotReachMaxCountError string
type IscsiDefaultError struct {
	ErrCode int
}
type NoSuchShareError string
type ShareReachMaxCountError string
type ShareSystemBusyError string
type ShareDefaultError struct {
	ErrCode int
}

func (_ OutOfFreeSpaceError) Error() string {
	return "Out of free space"
}
func (_ AlreadyExistError) Error() string {
	return "Already Existed"
}
func (_ BadParametersError) Error() string {
	return "Invalid input value"
}

// ISCSI errors
func (_ NoSuchLunError) Error() string {
	return "No such LUN"
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

func (e IscsiDefaultError) Error() string {
	return fmt.Sprintf("ISCSI API error. Error code: %d", e.ErrCode)
}

// Share errors
func (_ NoSuchShareError) Error() string {
	return "No such share"
}

func (_ ShareReachMaxCountError) Error() string {
	return "Number of share reach limit"
}

func (_ ShareSystemBusyError) Error() string {
	return "Share system is temporary busy"
}

func (e ShareDefaultError) Error() string {
	return fmt.Sprintf("Share API error. Error code: %d", e.ErrCode)
}
