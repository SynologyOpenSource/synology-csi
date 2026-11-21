// Copyright 2021 Synology Inc.

package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/webapi"
)

// guardDSM is a fake DSM that serves the iSCSI reads DeleteVolume makes and
// records every call that changes something. Running the real DsmService and
// webapi client against it checks the sequence of calls that would reach a NAS,
// not just the helper that decides.
type guardDSM struct {
	mu sync.Mutex

	luns    []webapi.LunInfo
	targets []webapi.TargetInfo
	// failTargetListFrom makes every Target.list call from the n-th on (1-based)
	// fail. Zero disables it.
	failTargetListFrom int

	targetLists int
	mutations   []string
}

func (f *guardDSM) handler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	api, method := q.Get("api"), q.Get("method")

	f.mu.Lock()
	defer f.mu.Unlock()

	reply := func(data interface{}) {
		body, _ := json.Marshal(map[string]interface{}{"success": true, "data": data})
		w.Write(body)
	}
	unquote := func(v string) string {
		if s, err := strconv.Unquote(v); err == nil {
			return s
		}
		return v
	}

	switch api + "." + method {
	case "SYNO.API.Auth.login":
		reply(map[string]string{"sid": "sid"})
	case "SYNO.Core.Share.list":
		reply(map[string]interface{}{"shares": []webapi.ShareInfo{}})
	case "SYNO.Core.ISCSI.Target.list":
		f.targetLists++
		if f.failTargetListFrom > 0 && f.targetLists >= f.failTargetListFrom {
			w.Write([]byte(`{"success":false,"error":{"code":103}}`))
			return
		}
		reply(map[string]interface{}{"targets": f.targets})
	case "SYNO.Core.ISCSI.LUN.get":
		for _, lun := range f.luns {
			if lun.Uuid == unquote(q.Get("uuid")) {
				reply(map[string]interface{}{"lun": lun})
				return
			}
		}
		w.Write([]byte(`{"success":false,"error":{"code":18990531}}`))
	default:
		f.mutations = append(f.mutations, api+"."+method+"("+unquote(q.Get("uuid"))+unquote(q.Get("target_id"))+")")
		reply(struct{}{})
	}
}

func (f *guardDSM) lunDeletes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, m := range f.mutations {
		if strings.HasPrefix(m, "SYNO.Core.ISCSI.LUN.delete") {
			out = append(out, m)
		}
	}
	return out
}

func newGuardService(t *testing.T, f *guardDSM) *DsmService {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(server.Close)

	host, portStr, found := strings.Cut(strings.TrimPrefix(server.URL, "http://"), ":")
	if !found {
		t.Fatalf("unexpected test server URL %q", server.URL)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}

	dsm := &webapi.DSM{Ip: host, Port: port, Username: "csi", Password: "secret"}
	if err := dsm.Login(); err != nil {
		t.Fatalf("login to the fake DSM failed: %v", err)
	}

	svc := NewDsmService()
	svc.dsms[dsm.Ip] = dsm
	return svc
}

const guardLun = "uuid-lun-x"

func guardFixture(targets ...webapi.TargetInfo) *guardDSM {
	return &guardDSM{
		luns: []webapi.LunInfo{
			{Name: "k8s-csi-pvc-x", Uuid: guardLun},
			{Name: "k8s-csi-pvc-y", Uuid: "uuid-lun-y"},
		},
		targets: targets,
	}
}

func idleTarget(name string, id int, luns ...string) webapi.TargetInfo {
	t := webapi.TargetInfo{Name: name, TargetId: id}
	for _, l := range luns {
		t.MappedLuns = append(t.MappedLuns, webapi.MappedLun{LunUuid: l})
	}
	return t
}

func busyTarget(name string, id int, luns ...string) webapi.TargetInfo {
	t := idleTarget(name, id, luns...)
	t.ConnectedSessions = []webapi.ConncetedSession{{Iqn: "iqn.1993-08.org.debian:01:node1", Ip: "10.0.0.11"}}
	return t
}

func TestDeleteVolumeChecksEveryTargetTheLunIsMappedTo(t *testing.T) {
	// The LUN is mapped to targets A and B. GetVolume returns the first mapping
	// it finds, A, which has no sessions, while the node is logged in through B.
	f := guardFixture(
		idleTarget("target-a", 1, guardLun),
		busyTarget("target-b", 2, guardLun),
	)
	svc := newGuardService(t, f)

	err := svc.DeleteVolume(guardLun)

	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v (%v)", got, err)
	}
	if !strings.Contains(err.Error(), "target-b") {
		t.Errorf("expected the error to name the busy target, got %v", err)
	}
	if deletes := f.lunDeletes(); len(deletes) != 0 {
		t.Fatalf("the LUN was deleted although a session is connected through another target: %v", deletes)
	}
}

func TestDeleteVolumeDeletesAnIdleLun(t *testing.T) {
	// Sessions on a target the LUN is not mapped to are none of its business.
	f := guardFixture(
		idleTarget("target-a", 1, guardLun),
		busyTarget("target-other", 2, "uuid-lun-y"),
	)
	svc := newGuardService(t, f)

	if err := svc.DeleteVolume(guardLun); err != nil {
		t.Fatalf("expected the idle LUN to be deleted, got %v", err)
	}
	if deletes := f.lunDeletes(); len(deletes) != 1 || !strings.Contains(deletes[0], guardLun) {
		t.Errorf("expected exactly one delete of %s, got %v", guardLun, deletes)
	}
}

func TestDeleteVolumeDoesNotDeleteWhenTargetsCannotBeListed(t *testing.T) {
	// The lookup lists the targets once; the guard lists them again and fails.
	f := guardFixture(idleTarget("target-a", 1, guardLun))
	f.failTargetListFrom = 2
	svc := newGuardService(t, f)

	err := svc.DeleteVolume(guardLun)

	if got := status.Code(err); got != codes.Unavailable {
		t.Fatalf("expected Unavailable, got %v (%v)", got, err)
	}
	if deletes := f.lunDeletes(); len(deletes) != 0 {
		t.Fatalf("the LUN was deleted without knowing whether it is in use: %v", deletes)
	}
}
