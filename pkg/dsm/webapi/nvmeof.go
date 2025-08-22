// Copyright 2025 Synology Inc.

package webapi

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"

	"github.com/SynologyOpenSource/synology-csi/pkg/utils"
)

type NamespaceInfo struct {
	Uuid            string `json:"uuid"`
	Name            string `json:"name"`
	Location        string `json:"location"`
	Size            uint64 `json:"size"`
	IsThinProvision bool   `json:"is_thin_provision"`
	IsActionLocked  bool   `json:"is_action_locked"`
	BufferedIo      bool   `json:"buffered_io"`
	SupportDiscard  bool   `json:"support_discard"`
	SupportFua      bool   `json:"support_fua"`
	SubsystemUuid   string `json:"subsystem_uuid"`
}

type SubsystemInfo struct {
	Uuid        string   `json:"uuid"`
	Name        string   `json:"name"`
	Nqn         string   `json:"nqn"`
	Status      string   `json:"status"`
	Ports       []string `json:"ports"`
}

type SubsystemCreateSpec struct {
	Name   string
	Nqn    string
}

type NamespaceCreateSpec struct {
	Name             string
	Location         string
	Description      string
	Size             uint64
	ThinProvisioning bool
	Reclaim          bool
}

type NamespaceSetSpec struct {
	Uuid    string
	NewSize uint64
}

type NamespaceCloneSpec struct {
	Name            string
	SrcUuid         string
	Location        string
}

func sanErrCodeMapping(errCode int, oriErr error) error {
	switch errCode {
	case 28990002: // Out of free space
		return utils.OutOfFreeSpaceError("")
	case 28992055: // No such namespace
		return utils.NoSuchNamespaceError("")
	case 28992056: // No such snapshot
		return utils.NoSuchSnapshotError("")
	case 28992064: // Duplicated name
		return utils.AlreadyExistError("")
	case 28992067: // Reach max count
		return utils.SanReachMaxCountError("")
	case 28992068:
		return utils.SnapshotReachMaxCountError("")
	case 28993012: // Failed to get subsystem
		return utils.FailedToGetSubsystemError("")
	}

	if errCode >= 28990000 {
		return utils.SanDefaultError{errCode}
	}
	return oriErr
}

// ----------------------- Namespace APIs -----------------------
func (dsm *DSM) NamespaceCreate(spec NamespaceCreateSpec) (string, error) {
	params := url.Values{}
	params.Add("api", "SYNO.San.Nvme.Namespace")
	params.Add("method", "create")
	params.Add("version", "1")
	params.Add("name", strconv.Quote(spec.Name))
	params.Add("description", strconv.Quote(spec.Description))
	params.Add("location", spec.Location)
	params.Add("size", strconv.FormatInt(int64(spec.Size), 10))
	params.Add("is_thin_provision", strconv.FormatBool(spec.ThinProvisioning))
	params.Add("support_discard", strconv.FormatBool(spec.Reclaim))

	type NamespaceCreateResp struct {
		Uuid string `json:"uuid"`
	}

	resp, err := dsm.sendRequest("", &NamespaceCreateResp{}, params, "webapi/entry.cgi")
	if err != nil {
		return "", sanErrCodeMapping(resp.ErrorCode, err)
	}

	namespaceResp, ok := resp.Data.(*NamespaceCreateResp)
	if !ok {
		return "", fmt.Errorf("failed to assert response to %T", &NamespaceCreateResp{})
	}

	return namespaceResp.Uuid, nil
}

func (dsm *DSM) NamespaceClone(spec NamespaceCloneSpec) (string, error) {
	params := url.Values{}
	params.Add("api", "SYNO.San.Nvme.Namespace")
	params.Add("method", "clone")
	params.Add("version", "1")
	params.Add("uuid", strconv.Quote(spec.SrcUuid))
	params.Add("dst_namespace_name", strconv.Quote(spec.Name))
	params.Add("dst_location", strconv.Quote(spec.Location))

	type NamespaceCloneResp struct {
		Uuid string `json:"dst_namespace_uuid"`
	}

	resp, err := dsm.sendRequest("", &NamespaceCloneResp{}, params, "webapi/entry.cgi")
	if err != nil {
		return "", sanErrCodeMapping(resp.ErrorCode, err)
	}

	info, ok := resp.Data.(*NamespaceCloneResp)
	if !ok {
		return "", fmt.Errorf("Failed to assert response to %T", &NamespaceCloneResp{})
	}

	return info.Uuid, nil
}

func (dsm *DSM) NamespaceSet(spec NamespaceSetSpec) error {
	params := url.Values{}
	params.Add("api", "SYNO.San.Nvme.Namespace")
	params.Add("method", "set")
	params.Add("version", "1")
	params.Add("uuid", strconv.Quote(spec.Uuid))

	if spec.NewSize > 0 {
		params.Add("new_size", strconv.FormatInt(int64(spec.NewSize), 10))
	}

	resp, err := dsm.sendRequest("", &struct{}{}, params, "webapi/entry.cgi")

	return sanErrCodeMapping(resp.ErrorCode, err)
}

func (dsm *DSM) NamespaceDelete(uuid string) error {
	params := url.Values{}
	params.Add("api", "SYNO.San.Nvme.Namespace")
	params.Add("method", "delete")
	params.Add("version", "1")
	params.Add("uuid", strconv.Quote(uuid))
	params.Add("is_soft_feas_ignored", strconv.FormatBool(true))

	resp, err := dsm.sendRequest("", &struct{}{}, params, "webapi/entry.cgi")

	return sanErrCodeMapping(resp.ErrorCode, err)
}

func (dsm *DSM) NamespaceGet(uuidOrName string) (*NamespaceInfo, error) {
	params := url.Values{}
	params.Add("api", "SYNO.San.Nvme.Namespace")
	params.Add("method", "get")
	params.Add("version", "1")
	params.Add("uuid", strconv.Quote(uuidOrName))

	type NamespaceGetResp struct {
		Namespace NamespaceInfo `json:"namespace"`
	}

	resp, err := dsm.sendRequest("", &NamespaceGetResp{}, params, "webapi/entry.cgi")
	if err != nil {
		return nil, sanErrCodeMapping(resp.ErrorCode, err)
	}

	info, ok := resp.Data.(*NamespaceGetResp)
	if !ok {
		return nil, fmt.Errorf("failed to assert response to %T", &NamespaceGetResp{})
	}
	return &info.Namespace, nil
}

func (dsm *DSM) NamespaceList() ([]NamespaceInfo, error) {
	params := url.Values{}
	params.Add("api", "SYNO.San.Nvme.Namespace")
	params.Add("method", "list")
	params.Add("version", "1")

	type NamespaceInfos struct {
		Namespaces []NamespaceInfo `json:"namespaces"`
	}

	resp, err := dsm.sendRequest("", &NamespaceInfos{}, params, "webapi/entry.cgi")
	if err != nil {
		return nil, sanErrCodeMapping(resp.ErrorCode, err)
	}

	infos, ok := resp.Data.(*NamespaceInfos)
	if !ok {
		return nil, fmt.Errorf("Failed to assert response to %T", &NamespaceInfos{})
	}

	return infos.Namespaces, nil
}

func (dsm *DSM) NamespaceSnapshotList(srcUuid string) ([]SnapshotInfo, error) {
	params := url.Values{}
	params.Add("api", "SYNO.San.Nvme.Namespace")
	params.Add("method", "list_snapshot")
	params.Add("version", "1")
	params.Add("uuid", strconv.Quote(srcUuid))

	type Infos struct {
		Snapshots []SnapshotInfo `json:"snapshots"`
	}

	resp, err := dsm.sendRequest("", &Infos{}, params, "webapi/entry.cgi")
	if err != nil {
		return nil, sanErrCodeMapping(resp.ErrorCode, err)
	}

	infos, ok := resp.Data.(*Infos)
	if !ok {
		return nil, fmt.Errorf("Failed to assert response to %T", &Infos{})
	}

	return infos.Snapshots, nil
}

func (dsm *DSM) NamespaceSnapshotGet(snapshotUuid string) (*SnapshotInfo, error) {
	params := url.Values{}
	params.Add("api", "SYNO.San.Nvme.Namespace")
	params.Add("method", "get_snapshot")
	params.Add("version", "1")
	params.Add("snapshot_uuid", strconv.Quote(snapshotUuid))

	type Info struct {
		Snapshot SnapshotInfo `json:"snapshot"`
	}
	info := Info{}

	resp, err := dsm.sendRequest("", &info, params, "webapi/entry.cgi")
	if err != nil {
		return nil, sanErrCodeMapping(resp.ErrorCode, err)
	}

	return &info.Snapshot, nil
}

func (dsm *DSM) NamespaceSnapshotCreate(spec SnapshotCreateSpec) (string, error) {
	params := url.Values{}
	params.Add("api", "SYNO.San.Nvme.Namespace")
	params.Add("method", "take_snapshot")
	params.Add("version", "1")
	params.Add("uuid", strconv.Quote(spec.SrcUuid))
	params.Add("snapshot_name", strconv.Quote(spec.Name))
	params.Add("description", strconv.Quote(spec.Description))
	params.Add("taken_by", strconv.Quote(spec.TakenBy))
	params.Add("is_user_locked", strconv.FormatBool(spec.IsLocked))
	params.Add("is_app_consistent", strconv.FormatBool(false))

	type SnapshotCreateResp struct {
		Uuid string `json:"snapshot_uuid"`
	}

	resp, err := dsm.sendRequest("", &SnapshotCreateResp{}, params, "webapi/entry.cgi")
	if err != nil {
		return "", sanErrCodeMapping(resp.ErrorCode, err)
	}

	snapshotResp, ok := resp.Data.(*SnapshotCreateResp)
	if !ok {
		return "", fmt.Errorf("Failed to assert response to %T", &SnapshotCreateResp{})
	}

	return snapshotResp.Uuid, nil
}

func (dsm *DSM) NamespaceSnapshotClone(spec SnapshotCloneSpec) (string, error) {
	params := url.Values{}
	params.Add("api", "SYNO.San.Nvme.Namespace")
	params.Add("method", "clone_snapshot")
	params.Add("version", "1")
	params.Add("snapshot_uuid", strconv.Quote(spec.SrcSnapshotUuid))
	params.Add("cloned_namespace_name", strconv.Quote(spec.Name))

	type SnapshotCloneResp struct {
		Uuid string `json:"cloned_namespace_uuid"`
	}

	resp, err := dsm.sendRequest("", &SnapshotCloneResp{}, params, "webapi/entry.cgi")
	if err != nil {
		return "", sanErrCodeMapping(resp.ErrorCode, err)
	}

	snapshotCloneResp, ok := resp.Data.(*SnapshotCloneResp)
	if !ok {
		return "", fmt.Errorf("Failed to assert response to %T", &SnapshotCloneResp{})
	}

	return snapshotCloneResp.Uuid, nil
}

func (dsm *DSM) NamespaceSnapshotDelete(snapshotUuid string) error {
	params := url.Values{}
	params.Add("api", "SYNO.San.Nvme.Namespace")
	params.Add("method", "delete_snapshot")
	params.Add("version", "1")
	params.Add("snapshot_uuid", strconv.Quote(snapshotUuid))

	resp, err := dsm.sendRequest("", &struct{}{}, params, "webapi/entry.cgi")

	return sanErrCodeMapping(resp.ErrorCode, err)
}

// ----------------------- Subsystem APIs -----------------------
func (dsm *DSM) SubsystemCreate(spec SubsystemCreateSpec) (string, error) {
	params := url.Values{}
	params.Add("api", "SYNO.San.Nvme.Subsystem")
	params.Add("method", "create")
	params.Add("version", "1")
	params.Add("name", strconv.Quote(spec.Name))
	params.Add("nqn", strconv.Quote(spec.Nqn))

	type SubsystemCreateResp struct {
		SubsystemUuid string `json:"uuid"`
	}

	resp, err := dsm.sendRequest("", &SubsystemCreateResp{}, params, "webapi/entry.cgi")
	if err != nil {
		return "", sanErrCodeMapping(resp.ErrorCode, err)
	}

	ssResp, ok := resp.Data.(*SubsystemCreateResp)
	if !ok {
		return "", fmt.Errorf("failed to assert response to %T", &SubsystemCreateResp{})
	}

	return ssResp.SubsystemUuid, nil
}

func (dsm *DSM) SubsystemGet(uuid string) (*SubsystemInfo, error) {
	params := url.Values{}
	params.Add("api", "SYNO.San.Nvme.Subsystem")
	params.Add("method", "get")
	params.Add("version", "1")
	params.Add("uuid", strconv.Quote(uuid))

	type SubsystemGetResp struct {
		Subsystem SubsystemInfo `json:"subsystem"`
	}

	resp, err := dsm.sendRequest("", &SubsystemGetResp{}, params, "webapi/entry.cgi")
	if err != nil {
		return nil, sanErrCodeMapping(resp.ErrorCode, err)
	}

	subsystemResp, ok := resp.Data.(*SubsystemGetResp)
	if !ok {
		return nil, fmt.Errorf("failed to assert response to %T", &SubsystemGetResp{})
	}
	return &subsystemResp.Subsystem, nil
}

func (dsm *DSM) SubsystemDelete(uuid string) error {
	params := url.Values{}
	params.Add("api", "SYNO.San.Nvme.Subsystem")
	params.Add("method", "delete")
	params.Add("version", "1")
	params.Add("uuid", strconv.Quote(uuid))

	resp, err := dsm.sendRequest("", &struct{}{}, params, "webapi/entry.cgi")

	return sanErrCodeMapping(resp.ErrorCode, err)
}

func (dsm *DSM) SubsystemSetNamespaces(uuid string, namespaceUuids []string) error {
	params := url.Values{}
	params.Add("api", "SYNO.San.Nvme.Subsystem")
	params.Add("method", "set_namespaces")
	params.Add("version", "1")
	params.Add("uuid", strconv.Quote(uuid))

	js, err := json.Marshal(namespaceUuids)
	if err != nil {
		return err
	}
	params.Add("namespace_uuids", string(js))

	resp, err := dsm.sendRequest("", &struct{}{}, params, "webapi/entry.cgi")

	return sanErrCodeMapping(resp.ErrorCode, err)
}
