/*
 * Copyright 2021 Synology Inc.
 */

package service

import (
	"strconv"
	"testing"

	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/webapi"
)

// The DSM login retry runs in the background, so it writes to dsms while the gRPC
// handlers are already reading it. Every reader has to go through the lock.
//
// Run with -race: on any of the paths the loop below calls, a `range service.dsms`
// that should have been `range service.listDsms()` is reported here.
//
// The writes stand in for AddDsm's, which cannot be reached without a DSM to log in
// to. The DSMs point at a closed port, so the webapi calls the readers make fail
// immediately without reaching the network.
func TestDsmServiceReadsAreSafeWhileDsmsAreAdded(t *testing.T) {
	service := NewDsmService()

	// Keep only a few DSMs registered - the readers below dial every one of them -
	// but keep writing for as long as the readers run.
	stop := make(chan struct{})
	written := make(chan struct{})
	go func() {
		defer close(written)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			service.mutex.Lock()
			service.dsms[strconv.Itoa(i%4)] = &webapi.DSM{Ip: "127.0.0.1", Port: 1}
			service.mutex.Unlock()
		}
	}()

	for i := 0; i < 100; i++ {
		service.GetDsmsCount()
		service.listDsms()
		service.ListVolumes()
		service.ListAllSnapshots()
	}

	close(stop)
	<-written
}
