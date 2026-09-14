/*
 * Copyright 2021 Synology Inc.
 */

package service

import (
	log "github.com/sirupsen/logrus"
)

// createRollback undoes resources a volume creation made before it failed.
//
// It matters because of who gets told about the failure: CreateVolume returns an
// error, so the CO never records the volume and will never call DeleteVolume for
// it, while the share or LUN stays on the DSM. Nothing is left that knows the
// resource exists, so only an administrator can ever remove it.
type createRollback struct {
	steps []rollbackStep
}

type rollbackStep struct {
	describe string
	undo     func() error
}

// add registers an undo action, to run only if the creation ends up failing.
//
// Register a step only for a resource this attempt actually created. Creation is
// idempotent by design: an "already exists" answer means the resource belongs to
// an earlier attempt, and deleting it here would take away a volume that is in
// use.
func (r *createRollback) add(describe string, undo func() error) {
	r.steps = append(r.steps, rollbackStep{describe: describe, undo: undo})
}

// run undoes the registered steps, newest first, so a resource is removed before
// whatever it was built on.
//
// A step that fails is reported rather than swallowed: at that point something is
// left behind that no automated path will ever reach, and the log line is the
// only way anyone finds out.
func (r *createRollback) run(dsmIp string) {
	for i := len(r.steps) - 1; i >= 0; i-- {
		step := r.steps[i]
		if err := step.undo(); err != nil {
			log.Errorf("[%s] MANUAL CLEANUP REQUIRED: couldn't roll back %s after a failed volume creation, it is still on the DSM: %v",
				dsmIp, step.describe, err)
			continue
		}
		log.Infof("[%s] Rolled back %s after a failed volume creation", dsmIp, step.describe)
	}
}
