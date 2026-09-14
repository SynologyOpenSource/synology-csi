/*
 * Copyright 2021 Synology Inc.
 */

package service

import (
	"errors"
	"testing"
)

func TestRollbackUndoesNewestFirst(t *testing.T) {
	var order []string
	var rollback createRollback

	// A volume is built up in order, so it has to be taken apart in reverse: the
	// target maps the LUN, and removing the LUN first would leave a mapping
	// pointing at nothing.
	rollback.add("LUN", func() error { order = append(order, "LUN"); return nil })
	rollback.add("target", func() error { order = append(order, "target"); return nil })

	rollback.run("1.2.3.4")

	if len(order) != 2 || order[0] != "target" || order[1] != "LUN" {
		t.Errorf("expected the target to be removed before the LUN, got %v", order)
	}
}

func TestRollbackKeepsGoingAfterAStepFails(t *testing.T) {
	var ran []string
	var rollback createRollback

	rollback.add("LUN", func() error { ran = append(ran, "LUN"); return nil })
	rollback.add("target", func() error {
		ran = append(ran, "target")
		return errors.New("DSM is busy")
	})

	rollback.run("1.2.3.4")

	// Giving up on the first failure would strand the LUN as well, turning one
	// leftover resource into two.
	if len(ran) != 2 {
		t.Errorf("expected every step to be attempted even after one failed, got %v", ran)
	}
}

func TestRollbackDoesNothingWhenNoStepsWereRegistered(t *testing.T) {
	// This is the "already exists" case: the resource came from an earlier
	// attempt, so nothing was registered and nothing may be deleted.
	var rollback createRollback

	rollback.run("1.2.3.4") // must not panic

	if len(rollback.steps) != 0 {
		t.Errorf("expected no steps, got %d", len(rollback.steps))
	}
}

// createLikeFunc mirrors how the create paths use the rollback: register a step
// only for what this attempt created, and run the steps only if it ends up
// failing.
func createLikeFunc(created bool, fail bool, deleted *bool) (err error) {
	var rollback createRollback
	defer func() {
		if err != nil {
			rollback.run("1.2.3.4")
		}
	}()

	if created {
		rollback.add("share", func() error { *deleted = true; return nil })
	}

	if fail {
		return errors.New("a later step failed")
	}
	return nil
}

func TestCreatedResourceIsRemovedWhenALaterStepFails(t *testing.T) {
	deleted := false

	if err := createLikeFunc(true, true, &deleted); err == nil {
		t.Fatal("expected the creation to fail")
	}

	if !deleted {
		t.Error("the share this attempt created was left on the DSM, where nothing will ever delete it")
	}
}

func TestSucceedingCreationKeepsItsResource(t *testing.T) {
	deleted := false

	if err := createLikeFunc(true, false, &deleted); err != nil {
		t.Fatalf("expected the creation to succeed, got %v", err)
	}

	if deleted {
		t.Error("rolled back a volume that was created successfully")
	}
}

func TestPreExistingResourceIsNeverRemoved(t *testing.T) {
	deleted := false

	// created=false stands for the "already exists" answer: the resource belongs
	// to an earlier attempt and may still be in use.
	if err := createLikeFunc(false, true, &deleted); err == nil {
		t.Fatal("expected the creation to fail")
	}

	if deleted {
		t.Error("deleted a pre-existing volume while rolling back, which takes away someone else's data")
	}
}
