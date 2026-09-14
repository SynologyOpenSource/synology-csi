// Copyright 2021 Synology Inc.

package webapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// privilegeDSM is a stand-in for a DSM whose NFS privilege save answers success
// without necessarily applying the rules -- the behaviour that makes the driver
// mount shares that were never exported.
type privilegeDSM struct {
	mu sync.Mutex
	// applyAfter is the number of saves that are silently dropped before one
	// takes effect. 0 means every save works.
	applyAfter int
	saves      int
	loads      int
	rules      []PrivilegeRule
	loadFails  int
	// 2370 is retried inside nfsPrivilegeRequest, so a test that wants the
	// read-back to actually fail has to use a code that is not.
	loadErrCode int
}

func (f *privilegeDSM) handler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	w.Header().Set("Content-Type", "application/json")

	if strings.Contains(r.URL.Path, "auth.cgi") {
		w.Write([]byte(`{"success":true,"data":{"sid":"test-sid"}}`))
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	switch q.Get("method") {
	case "save":
		f.saves++
		if f.saves > f.applyAfter {
			var rules []PrivilegeRule
			if err := json.Unmarshal([]byte(q.Get("rule")), &rules); err != nil {
				w.Write([]byte(`{"success":false,"error":{"code":2301}}`))
				return
			}
			f.rules = rules
		}
		// Reports success either way, exactly like the DSM defect.
		w.Write([]byte(`{"success":true}`))
	case "load":
		f.loads++
		if f.loads <= f.loadFails {
			code := f.loadErrCode
			if code == 0 {
				code = 2370
			}
			w.Write([]byte(`{"success":false,"error":{"code":` + strconv.Itoa(code) + `}}`))
			return
		}
		body, _ := json.Marshal(map[string]interface{}{
			"success": true,
			"data":    SharePrivilege{ShareName: "k8s-csi-test", Rule: f.rules},
		})
		w.Write(body)
	default:
		w.Write([]byte(`{"success":true}`))
	}
}

func newPrivilegeDSM(t *testing.T, fake *privilegeDSM) (*DSM, func()) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(fake.handler))

	trimmed := strings.TrimPrefix(server.URL, "http://")
	host, portStr, ok := strings.Cut(trimmed, ":")
	if !ok {
		server.Close()
		t.Fatalf("unexpected test server URL %q", server.URL)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		server.Close()
		t.Fatalf("bad port in %q: %v", server.URL, err)
	}

	dsm := &DSM{Ip: host, Port: port, Username: "admin", Password: "secret"}
	if err := dsm.Login(); err != nil {
		server.Close()
		t.Fatalf("login to fake DSM failed: %v", err)
	}
	return dsm, server.Close
}

func wantPrivilege() SharePrivilege {
	return SharePrivilege{
		ShareName: "k8s-csi-test",
		Rule: []PrivilegeRule{
			{Client: "10.0.0.1", Privilege: "RW"},
			{Client: "10.0.0.2", Privilege: "RW"},
		},
	}
}

// withShortVerifyBudget keeps the failing cases from taking the full production
// budget. Tests in this file must not run in parallel because of it.
func withShortVerifyBudget(t *testing.T, d time.Duration) {
	t.Helper()
	original := nfsPrivilegeVerifyTimeout
	nfsPrivilegeVerifyTimeout = d
	t.Cleanup(func() { nfsPrivilegeVerifyTimeout = original })
}

// The point of the whole change: DSM says the save worked, the rules are not
// there, and the driver must not believe it.
func TestSaveThatSilentlyDidNothingIsNotReportedAsSuccess(t *testing.T) {
	withShortVerifyBudget(t, 2*time.Second)

	fake := &privilegeDSM{applyAfter: 1 << 30} // never applies
	dsm, done := newPrivilegeDSM(t, fake)
	defer done()

	err := dsm.ShareNfsPrivilegeSave(wantPrivilege())
	if err == nil {
		t.Fatal("expected an error when the rules never took effect; " +
			"reporting success here is what leaves a share unexported and its pod stuck")
	}
	if !strings.Contains(err.Error(), "10.0.0.1") {
		t.Errorf("expected the error to name the clients that are missing, got: %v", err)
	}
	if fake.saves < 2 {
		t.Errorf("expected the save to be retried, it ran %d time(s)", fake.saves)
	}
}

func TestSaveIsRetriedUntilTheRulesTakeEffect(t *testing.T) {
	withShortVerifyBudget(t, 5*time.Second)

	fake := &privilegeDSM{applyAfter: 2} // first two saves are dropped
	dsm, done := newPrivilegeDSM(t, fake)
	defer done()

	if err := dsm.ShareNfsPrivilegeSave(wantPrivilege()); err != nil {
		t.Fatalf("expected the save to succeed once a retry landed, got %v", err)
	}
	if fake.saves != 3 {
		t.Errorf("expected 3 saves (2 dropped + 1 applied), got %d", fake.saves)
	}
}

func TestSaveThatWorksIsNotRetried(t *testing.T) {
	withShortVerifyBudget(t, 5*time.Second)

	fake := &privilegeDSM{}
	dsm, done := newPrivilegeDSM(t, fake)
	defer done()

	if err := dsm.ShareNfsPrivilegeSave(wantPrivilege()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.saves != 1 {
		t.Errorf("expected exactly 1 save, got %d", fake.saves)
	}
	// One read-back to confirm. Anything more is wasted work against the API
	// whose contention caused this bug.
	if fake.loads != 1 {
		t.Errorf("expected exactly 1 read-back, got %d", fake.loads)
	}
}

// Not being able to read the rules back is not the same as knowing they are
// wrong, but it is also not a reason to claim success.
func TestSaveRetriesWhenTheRulesCannotBeReadBack(t *testing.T) {
	withShortVerifyBudget(t, 5*time.Second)

	fake := &privilegeDSM{loadFails: 1, loadErrCode: 2301}
	dsm, done := newPrivilegeDSM(t, fake)
	defer done()

	if err := dsm.ShareNfsPrivilegeSave(wantPrivilege()); err != nil {
		t.Fatalf("expected success once the read-back worked, got %v", err)
	}
	if fake.saves < 2 {
		t.Errorf("expected a retry after the read-back failed, saves=%d", fake.saves)
	}
}

func TestMissingPrivilegeRules(t *testing.T) {
	want := wantPrivilege()

	cases := []struct {
		name string
		got  SharePrivilege
		want []string
	}{
		{
			name: "every client granted",
			got:  want,
		},
		{
			name: "one client absent",
			got: SharePrivilege{Rule: []PrivilegeRule{
				{Client: "10.0.0.1", Privilege: "RW"},
			}},
			want: []string{"10.0.0.2"},
		},
		{
			name: "client present but read-only",
			got: SharePrivilege{Rule: []PrivilegeRule{
				{Client: "10.0.0.1", Privilege: "RW"},
				{Client: "10.0.0.2", Privilege: "RO"},
			}},
			want: []string{"10.0.0.2"},
		},
		{
			name: "no rules at all, which is what a lost save looks like",
			got:  SharePrivilege{},
			want: []string{"10.0.0.1", "10.0.0.2"},
		},
		{
			name: "extra clients we did not ask about are ignored",
			got: SharePrivilege{Rule: []PrivilegeRule{
				{Client: "10.0.0.1", Privilege: "RW"},
				{Client: "10.0.0.2", Privilege: "RW"},
				{Client: "192.168.1.1", Privilege: "RW"},
			}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := missingPrivilegeRules(tc.got, want)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("missingPrivilegeRules() = %v, want %v", got, tc.want)
			}
		})
	}
}

// The read-back happens inside NodeStageVolume, so the budget has to leave room
// under kubelet's own timeout for the mount that follows.
func TestVerifyBudgetLeavesRoomForTheMount(t *testing.T) {
	if nfsPrivilegeVerifyTimeout > 60*time.Second {
		t.Errorf("verify budget %v is too close to kubelet's NodeStageVolume timeout",
			nfsPrivilegeVerifyTimeout)
	}
	if nfsPrivilegeVerifyTimeout <= nfsPrivilegeRetryTimeout {
		t.Errorf("verify budget %v must exceed the per-request budget %v, "+
			"or a single slow request consumes the whole thing and nothing is ever retried",
			nfsPrivilegeVerifyTimeout, nfsPrivilegeRetryTimeout)
	}
}
