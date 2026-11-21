// Copyright 2021 Synology Inc.

package driver

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/SynologyOpenSource/synology-csi/pkg/interfaces"
	"github.com/SynologyOpenSource/synology-csi/pkg/utils"
	log "github.com/sirupsen/logrus"
)

const (
	// recoveryRetryInterval is how often a session that could not take the
	// replacement timeout yet is tried again.
	recoveryRetryInterval = 30 * time.Second

	// The startup reconcile waits reconcileBackoffMin after the first failure to
	// learn which sessions are the driver's, then doubles up to
	// reconcileBackoffMax.
	reconcileBackoffMin = 10 * time.Second
	reconcileBackoffMax = 5 * time.Minute
)

// sessionTuning is the state behind the replacement timeout that is not the
// configured value itself. Its fields are replaceable so tests need neither
// real time nor a real sysfs.
type sessionTuning struct {
	ctx context.Context
	// after is time.After.
	after func(time.Duration) <-chan time.Time
	// retryInterval is the pause between attempts on a recovering session.
	retryInterval time.Duration
	// writeSysfs writes value to the sysfs attribute at path.
	writeSysfs func(path, value string) error

	mu sync.Mutex
	// retrying holds the sessions that have a retry running, so staging many
	// volumes does not start one per call.
	retrying map[sessionKey]struct{}
}

// sessionKey identifies a session across time. The kernel hands out session ids
// in increasing order, but the id alone says nothing about which target it is
// logged in to, so the target is part of the identity.
type sessionKey struct {
	id  int32
	iqn string
}

func newSessionTuning() *sessionTuning {
	return &sessionTuning{
		ctx:           context.Background(),
		after:         time.After,
		retryInterval: recoveryRetryInterval,
		writeSysfs:    writeSysfsAttribute,
		retrying:      map[sessionKey]struct{}{},
	}
}

// writeSysfsAttribute opens the attribute without O_CREATE so a session that
// has gone away fails instead of leaving a stray file behind.
func writeSysfsAttribute(path, value string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(value); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func iscsiSessionDir(sessionId int32) string {
	return filepath.Join(sysfsRoot, "class", "iscsi_session", fmt.Sprintf("session%d", sessionId))
}

// setSessionRecoveryTimeout writes the replacement timeout into a live session.
//
// open-iscsi reads the node record once, when it logs in, and keeps that
// configuration for the life of the session, including across reconnects after
// a failure. Updating the record therefore only affects the next login. The
// kernel's copy, /sys/class/iscsi_session/session<sid>/recovery_tmo, is what
// the recovery timer uses and it is writable at runtime, which lets a session
// that already exists pick up a new value without a logout.
//
// The kernel refuses the write with EBUSY while the session is not logged in
// (FREE or FAILED, i.e. while its connection is down or recovering); see
// retryRecoveryTimeout. A session that is already in recovery keeps the timer
// it started with; the new value is used from its next failure.
//
// sysfsRoot is the container's /sys; in a privileged pod that is the host's
// sysfs and iscsi_session is not network-namespaced, the same assumption
// verifyIscsiDevice makes when it reads the session's targetname.
func (t *tools) setSessionRecoveryTimeout(sessionId int32, seconds int) error {
	path := filepath.Join(iscsiSessionDir(sessionId), "recovery_tmo")
	return t.tuning.writeSysfs(path, strconv.Itoa(seconds))
}

// sessionTargetName reads the target a session is logged in to from sysfs.
func sessionTargetName(sessionId int32) (string, error) {
	data, err := os.ReadFile(filepath.Join(iscsiSessionDir(sessionId), "targetname"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// sessionHeldByMultipath reports whether one of the session's SCSI disks is
// part of a device-mapper multipath map.
//
// multipathd manages the recovery timeout of the paths it owns: it sets
// recovery_tmo from fast_io_fail_tmo (5s unless configured), so that I/O fails
// over to another path quickly. A replacement timeout of minutes written over
// that would delay the failover, so such sessions are left to multipathd.
func sessionHeldByMultipath(sessionId int32) (bool, error) {
	pattern := filepath.Join(iscsiSessionDir(sessionId), "device", "target*", "*", "block", "*")
	disks, err := filepath.Glob(pattern)
	if err != nil {
		return false, err
	}

	for _, disk := range disks {
		holders, err := os.ReadDir(filepath.Join(sysfsRoot, "block", filepath.Base(disk), "holders"))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return false, err
		}
		for _, h := range holders {
			if !strings.HasPrefix(h.Name(), "dm-") {
				continue
			}
			uuid, err := os.ReadFile(filepath.Join(sysfsRoot, "block", h.Name(), "dm", "uuid"))
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				return false, err
			}
			if strings.HasPrefix(strings.TrimSpace(string(uuid)), "mpath-") {
				return true, nil
			}
		}
	}
	return false, nil
}

// applyReplacementTimeout brings sessions that already exist in line with the
// configured replacement timeout: the node record, so the next login of the
// target uses the value, and the live session. Failures are logged and
// skipped, because this is a tuning knob and must not get in the way of
// staging or starting the plugin.
//
// startup only picks how loudly a success is logged: staging calls this for
// every volume that is already logged in, which would repeat the line.
func (t *tools) applyReplacementTimeout(sessions []iscsiSession, startup bool) {
	if t.iscsiReplacementTimeout <= 0 {
		return
	}

	for _, s := range sessions {
		// The answer is worked out afresh for every target, and again after the
		// node record update: iscsiadm blocks for a while, and a second
		// session or a multipath map can appear in that time.
		if !t.tunableNow(s) {
			continue
		}

		if err := t.iscsiadm_update_node_replacement_timeout(s.Iqn, s.Portal, t.iscsiReplacementTimeout); err != nil {
			log.Warnf("Failed to update replacement_timeout of the node record for target [%s] portal [%s]: %v",
				s.Iqn, s.Portal, err)
		}

		if !t.tunableNow(s) {
			continue
		}

		err := t.setSessionRecoveryTimeout(s.Id, t.iscsiReplacementTimeout)
		switch {
		case err == nil:
			t.logRecoveryTimeoutSet(s, startup)
		case errors.Is(err, syscall.EBUSY):
			log.Infof("Session %d, target [%s], is not logged in right now (its connection is down or recovering), "+
				"so it can't take the replacement timeout yet. Retrying every %s.", s.Id, s.Iqn, t.tuning.retryInterval)
			t.startRecoveryRetry(s)
		case errors.Is(err, fs.ErrNotExist):
			log.Debugf("Session %d, target [%s], is gone, no recovery_tmo to set.", s.Id, s.Iqn)
		default:
			log.Warnf("Failed to set recovery_tmo of session %d for target [%s]: %v", s.Id, s.Iqn, err)
		}
	}
}

// tunableNow lists the sessions afresh and reports whether s passes
// targetTunable. Anything it can't establish, such as failing to list the
// sessions or to look at the disks, counts as not tunable and is logged.
func (t *tools) tunableNow(s iscsiSession) bool {
	live, err := t.iscsiadm_session()
	if err != nil {
		log.Warnf("Couldn't list the iSCSI sessions, leaving the replacement timeout of session %d, target [%s], alone: %v",
			s.Id, s.Iqn, err)
		return false
	}

	ok, err := t.targetTunable(s, live)
	if errors.Is(err, errSessionNotListed) {
		log.Debugf("Session %d, target [%s], is not listed by iscsiadm, leaving its replacement timeout alone.", s.Id, s.Iqn)
		return false
	}
	if err != nil {
		log.Warnf("Couldn't tell whether session %d, target [%s], is a multipath path, leaving its replacement timeout alone: %v",
			s.Id, s.Iqn, err)
		return false
	}
	return ok
}

func (t *tools) logRecoveryTimeoutSet(s iscsiSession, startup bool) {
	logf := log.Debugf
	if startup {
		logf = log.Infof
	}
	logf("Set replacement timeout of session %d, target [%s], to %ds.", s.Id, s.Iqn, t.iscsiReplacementTimeout)
}

// errSessionNotListed means iscsiadm doesn't list the session. The session is
// gone, or iscsiadm failed; either way it can't be shown to be tunable.
var errSessionNotListed = errors.New("session is not in the iscsiadm session list")

// targetTunable is the one test every write of the replacement timeout goes
// through: staging an existing session, the startup reconcile and each attempt
// of the retry worker, so they can't disagree. It is checked again before every
// write because the answer changes: while the node boots, or multipathd
// rebuilds its maps, the sessions to a target are all there before their disks
// have a device-mapper holder.
//
// A target is left to multipathd, and not tuned, when it has more than one live
// session (which includes more than one portal), or when a disk of one of its
// sessions is part of a multipath map. multipathd sets the recovery timeout of
// the paths it owns from fast_io_fail_tmo, and a replacement timeout of minutes
// written later over that would delay the failover.
//
// live is the iscsiadm session list. It returns errSessionNotListed if s is not
// in it.
func (t *tools) targetTunable(s iscsiSession, live []iscsiSession) (bool, error) {
	var mine []iscsiSession
	listed := false
	for _, l := range live {
		if l.Iqn != s.Iqn {
			continue
		}
		mine = append(mine, l)
		if l.Id == s.Id {
			listed = true
		}
	}
	if !listed {
		return false, errSessionNotListed
	}

	if len(mine) > 1 {
		log.Infof("Not setting the replacement timeout of session %d, target [%s]: the target has %d sessions, "+
			"so multipath is in use or being assembled and multipathd decides the failover timing. "+
			"Tune multipathd (fast_io_fail_tmo, dev_loss_tmo, no_path_retry) instead.", s.Id, s.Iqn, len(mine))
		return false, nil
	}

	held, err := sessionHeldByMultipath(s.Id)
	if err != nil {
		return false, err
	}
	if held {
		log.Infof("Not setting the replacement timeout of session %d, target [%s]: its disk is part of a multipath map, "+
			"and multipathd sets the session's recovery timeout. Tune multipathd (fast_io_fail_tmo, dev_loss_tmo, "+
			"no_path_retry) instead.", s.Id, s.Iqn)
		return false, nil
	}
	return true, nil
}

// startRecoveryRetry tries a session again in the background, unless a retry
// for it is already running.
func (t *tools) startRecoveryRetry(s iscsiSession) {
	key := sessionKey{id: s.Id, iqn: s.Iqn}
	st := t.tuning

	st.mu.Lock()
	if _, ok := st.retrying[key]; ok {
		st.mu.Unlock()
		return
	}
	st.retrying[key] = struct{}{}
	st.mu.Unlock()

	go func() {
		defer func() {
			st.mu.Lock()
			delete(st.retrying, key)
			st.mu.Unlock()
		}()
		t.retryRecoveryTimeout(st.ctx, s)
	}()
}

// retryRecoveryTimeout keeps writing the replacement timeout to a session that
// refused it with EBUSY, until the write succeeds, the session goes away or
// turns out to be a different one, it turns out to be a multipath path or one of several sessions of its target, the
// write fails for another reason, or ctx is cancelled.
//
// There is deliberately no upper bound on the number of attempts: a session
// that is down because the NAS is being serviced can stay down for hours, and
// is exactly the one that needs the longer timeout once it is back. The loop
// ends with the session, and an attempt is a single sysfs write.
func (t *tools) retryRecoveryTimeout(ctx context.Context, s iscsiSession) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.tuning.after(t.tuning.retryInterval):
		}

		// Both cases can be ready at once, and select picks one at random.
		if ctx.Err() != nil || t.retryRecoveryTimeoutOnce(s) {
			return
		}
	}
}

// retryRecoveryTimeoutOnce makes one attempt and reports whether the retrying
// is over.
func (t *tools) retryRecoveryTimeoutOnce(s iscsiSession) (done bool) {
	iqn, err := sessionTargetName(s.Id)
	if errors.Is(err, fs.ErrNotExist) {
		log.Debugf("Session %d, target [%s], is gone, giving up on its replacement timeout.", s.Id, s.Iqn)
		return true
	}
	if err != nil {
		log.Warnf("Failed to read the target of session %d, giving up on its replacement timeout: %v", s.Id, err)
		return true
	}
	if iqn != s.Iqn {
		log.Infof("Session %d is logged in to target [%s] now, not [%s]; giving up on its replacement timeout.",
			s.Id, iqn, s.Iqn)
		return true
	}

	// More sessions may have come up for the target, or multipathd may have
	// built its map on the path, since the last attempt.
	live, err := t.iscsiadm_session()
	if err != nil {
		// Not knowing is not a reason to stop: it may be the next attempt that
		// can tell, and nothing is written meanwhile.
		log.Warnf("Couldn't list the iSCSI sessions, trying session %d, target [%s], again later: %v", s.Id, s.Iqn, err)
		return false
	}
	ok, err := t.targetTunable(s, live)
	if errors.Is(err, errSessionNotListed) {
		// The sysfs entry is there, so the session isn't gone; iscsiadm may
		// have failed this once.
		return false
	}
	if err != nil {
		log.Warnf("Couldn't tell whether session %d, target [%s], is a multipath path, giving up on its replacement timeout: %v",
			s.Id, s.Iqn, err)
		return true
	}
	if !ok {
		return true
	}

	err = t.setSessionRecoveryTimeout(s.Id, t.iscsiReplacementTimeout)
	switch {
	case err == nil:
		log.Infof("Set replacement timeout of session %d, target [%s], to %ds.", s.Id, s.Iqn, t.iscsiReplacementTimeout)
		return true
	case errors.Is(err, syscall.EBUSY):
		return false
	case errors.Is(err, fs.ErrNotExist):
		log.Debugf("Session %d, target [%s], is gone, giving up on its replacement timeout.", s.Id, s.Iqn)
		return true
	default:
		log.Warnf("Failed to set recovery_tmo of session %d for target [%s], giving up: %v", s.Id, s.Iqn, err)
		return true
	}
}

// ownedIscsiTargets returns the IQNs of the iSCSI targets of the volumes this
// driver created, as DSM lists them. ListVolumes only returns volumes named
// with the driver's prefix, so a target that something else created on the same
// NAS (a backup LUN an admin attached by hand, say) is not in it, even though
// its IQN has the same vendor prefix.
//
// When the listing is incomplete the error is returned along with the targets
// that were listed: those are known to be the driver's, but the absence of
// others proves nothing.
func ownedIscsiTargets(dsmService interfaces.IDsmService) (map[string]struct{}, error) {
	volumes, err := dsmService.ListVolumes()

	owned := map[string]struct{}{}
	for _, v := range volumes {
		if v.Protocol == utils.ProtocolIscsi && v.Target.Iqn != "" {
			owned[v.Target.Iqn] = struct{}{}
		}
	}
	return owned, err
}

// reconcileIscsiSessions applies the replacement timeout to sessions that were
// logged in before the driver was configured with it, e.g. volumes staged
// before an upgrade, which would otherwise keep the old value until they are
// unstaged and staged again.
//
// The host's session list also holds other iSCSI users' sessions, and Synology
// LUNs that an admin attached by hand, so the vendor prefix of an IQN does not
// say that the driver manages it. Only sessions to targets listOwned reports
// are touched. If DSM can't be listed, or only partly, the sessions that can't
// be proven the driver's are skipped and the reconcile is tried again with a
// growing delay, until the listing is complete or ctx is cancelled.
func (t *tools) reconcileIscsiSessions(listOwned func() (map[string]struct{}, error)) {
	if t.iscsiReplacementTimeout <= 0 {
		return
	}

	st := t.tuning
	done := map[sessionKey]struct{}{}
	backoff := reconcileBackoffMin
	for {
		var candidates []iscsiSession
		sessions, err := t.iscsiadm_session()
		if err != nil {
			log.Warnf("Couldn't list the iSCSI sessions, not reconciling their replacement timeout: %v", err)
			return
		}
		for _, s := range sessions {
			if _, ok := done[sessionKey{id: s.Id, iqn: s.Iqn}]; !ok {
				candidates = append(candidates, s)
			}
		}
		if len(candidates) == 0 {
			return
		}

		owned, err := listOwned()

		var ours []iscsiSession
		for _, s := range candidates {
			if _, ok := owned[s.Iqn]; ok {
				ours = append(ours, s)
				done[sessionKey{id: s.Id, iqn: s.Iqn}] = struct{}{}
			}
		}
		t.applyReplacementTimeout(ours, true)

		if err == nil {
			return
		}

		log.Warnf("Couldn't list this driver's volumes in full (%v); iSCSI sessions that aren't known to be its own are "+
			"left alone, retrying in %s.", err, backoff)
		select {
		case <-st.ctx.Done():
			return
		case <-st.after(backoff):
		}
		if st.ctx.Err() != nil {
			return
		}
		backoff = min(backoff*2, reconcileBackoffMax)
	}
}

// mayTuneReplacementTimeout says whether the replacement timeout may be applied
// to a target about to be logged in through portals. With more than one portal
// multipath is in use (see getPortals), and multipathd sets the sessions'
// recovery timeout from its own failover settings.
func (t *tools) mayTuneReplacementTimeout(iqn string, portals []string) bool {
	if len(portals) <= 1 {
		return true
	}
	if t.iscsiReplacementTimeout > 0 {
		log.Infof("Not setting the iSCSI replacement timeout for target [%s]: it is logged in through %d portals, "+
			"so multipath is in use and multipathd decides the failover timing. Tune multipathd (fast_io_fail_tmo, "+
			"dev_loss_tmo, no_path_retry) instead.", iqn, len(portals))
	}
	return false
}
