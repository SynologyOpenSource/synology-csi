// Copyright 2021 Synology Inc.

package webapi

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	log "github.com/sirupsen/logrus"
	"github.com/SynologyOpenSource/synology-csi/pkg/logger"
	"github.com/SynologyOpenSource/synology-csi/pkg/utils"
)

type LunInfo struct {
	Name             string         `json:"name"`
	Uuid             string         `json:"uuid"`
	LunType          int            `json:"type"`
	Location         string         `json:"location"`
	Size             uint64         `json:"size"`
	Used             uint64         `json:"allocated_size"`
	Status           string         `json:"status"`
	FlashcacheStatus string         `json:"flashcache_status"`
	IsActionLocked   bool           `json:"is_action_locked"`
	DevAttribs       []LunDevAttrib `json:"dev_attribs"`
}

type MappedLun struct {
	LunUuid      string `json:"lun_uuid"`
	MappingIndex int    `json:"mapping_index"`
}

type ConncetedSession struct {
	Iqn string `json:"iqn"`
	Ip  string `json:"ip"`
}

type NetworkPortal struct {
	ControllerId  int    `json:"controller_id"`
	InterfaceName string `json:"interface_name"`
}

type TargetInfo struct {
	Name              string             `json:"name"`
	Iqn               string             `json:"iqn"`
	Status            string             `json:"status"`
	MaxSessions       int                `json:"max_sessions"`
	MappedLuns        []MappedLun        `json:"mapped_luns"`
	ConnectedSessions []ConncetedSession `json:"connected_sessions"`
	NetworkPortals    []NetworkPortal    `json:"network_portals"`
	TargetId          int                `json:"target_id"`
}

type SnapshotInfo struct {
	Name              string             `json:"name"`
	Uuid              string             `json:"uuid"`
	ParentUuid        string             `json:"parent_uuid"`
	Status            string             `json:"status"`
	TotalSize         int64              `json:"total_size"`
	CreateTime        int64              `json:"create_time"`
	RootPath          string             `json:"root_path"`
}

type LunDevAttrib struct {
	DevAttrib string `json:"dev_attrib"`
	Enable    int    `json:"enable"`
}

type LunCreateSpec struct {
	Name        string
	Description string
	Location    string
	Size        int64
	Type        string
	DevAttribs  []LunDevAttrib
}

type LunUpdateSpec struct {
	Uuid    string
	NewSize uint64
}

type LunCloneSpec struct {
	Name            string
	SrcLunUuid      string
	Location        string
}

type TargetCreateSpec struct {
	Name string
	Iqn  string
}

type SnapshotCreateSpec struct {
	Name        string
	LunUuid     string
	Description string
	TakenBy     string
	IsLocked    bool
}

type SnapshotCloneSpec struct {
	Name            string
	SrcLunUuid      string
	SrcSnapshotUuid string
}

func errCodeMapping(errCode int, oriErr error) error {
	switch errCode {
	case 18990002: // Out of free space
		return utils.OutOfFreeSpaceError("")
	case 18990531: // No such LUN
		return utils.NoSuchLunError("")
	case 18990538: // Duplicated LUN name
		return utils.AlreadyExistError("")
	case 18990541:
		return utils.LunReachMaxCountError("")
	case 18990542:
		return utils.TargetReachMaxCountError("")
	case 18990744: // Duplicated Target name
		return utils.AlreadyExistError("")
	case 18990532:
		return utils.NoSuchSnapshotError("")
	case 18990500:
		return utils.BadLunTypeError("")
	case 18990543:
		return utils.SnapshotReachMaxCountError("")
	}

	if errCode > 18990000 {
		return utils.IscsiDefaultError{errCode}
	}
	return oriErr
}

func (dsm *DSM) LunList() ([]LunInfo, error) {
	params := url.Values{}
	params.Add("api", "SYNO.Core.ISCSI.LUN")
	params.Add("method", "list")
	params.Add("version", "1")
	params.Add("types", "[\"BLOCK\", \"FILE\", \"THIN\", \"ADV\", \"SINK\", \"CINDER\", \"CINDER_BLUN\", \"CINDER_BLUN_THICK\", \"BLUN\", \"BLUN_THICK\", \"BLUN_SINK\", \"BLUN_THICK_SINK\"]")
	params.Add("additional", "[\"allocated_size\",\"status\",\"flashcache_status\", \"is_action_locked\"]")

	type LunInfos struct {
		Luns []LunInfo `json:"luns"`
	}

	resp, err := dsm.sendRequest("", &LunInfos{}, params, "webapi/entry.cgi")
	if err != nil {
		return nil, errCodeMapping(resp.ErrorCode, err)
	}

	lunInfos, ok := resp.Data.(*LunInfos)
	if !ok {
		return nil, fmt.Errorf("Failed to assert response to %T", &LunInfos{})
	}
	return lunInfos.Luns, nil
}

func (dsm *DSM) LunCreate(spec LunCreateSpec) (string, error) {
	params := url.Values{}
	params.Add("api", "SYNO.Core.ISCSI.LUN")
	params.Add("method", "create")
	params.Add("version", "1")
	params.Add("name", strconv.Quote(spec.Name))
	params.Add("size", strconv.FormatInt(int64(spec.Size), 10))
	params.Add("type", spec.Type)
	params.Add("location", spec.Location)
	params.Add("description", spec.Description)

	js, err := json.Marshal(spec.DevAttribs)
	if err != nil {
		return "", err
	}
	params.Add("dev_attribs", string(js))

	type LunCreateResp struct {
		Uuid string `json:"uuid"`
	}

	resp, err := dsm.sendRequest("", &LunCreateResp{}, params, "webapi/entry.cgi")
	if err != nil {
		return "", errCodeMapping(resp.ErrorCode, err)
	}

	lunResp, ok := resp.Data.(*LunCreateResp)
	if !ok {
		return "", fmt.Errorf("Failed to assert response to %T", &LunCreateResp{})
	}

	return lunResp.Uuid, nil
}

func (dsm *DSM) LunUpdate(spec LunUpdateSpec) error {
	params := url.Values{}
	params.Add("api", "SYNO.Core.ISCSI.LUN")
	params.Add("method", "set")
	params.Add("version", "1")
	params.Add("uuid", strconv.Quote(spec.Uuid))
	params.Add("new_size", strconv.FormatInt(int64(spec.NewSize), 10))

	resp, err := dsm.sendRequest("", &struct{}{}, params, "webapi/entry.cgi")
	if err != nil {
		return errCodeMapping(resp.ErrorCode, err)
	}

	return nil
}

func (dsm *DSM) LunGet(uuid string) (LunInfo, error) {
	params := url.Values{}
	params.Add("api", "SYNO.Core.ISCSI.LUN")
	params.Add("method", "get")
	params.Add("version", "1")
	params.Add("uuid", strconv.Quote(uuid))
	params.Add("additional", "[\"allocated_size\",\"status\",\"flashcache_status\", \"is_action_locked\"]")

	type Info struct {
		Lun LunInfo `json:"lun"`
	}
	info := Info{}

	resp, err := dsm.sendRequest("", &info, params, "webapi/entry.cgi")
	if err != nil {
		return LunInfo{}, errCodeMapping(resp.ErrorCode, err)
	}

	return info.Lun, nil
}

func (dsm *DSM) LunClone(spec LunCloneSpec) (string, error) {
	params := url.Values{}
	params.Add("api", "SYNO.Core.ISCSI.LUN")
	params.Add("method", "clone")
	params.Add("version", "1")
	params.Add("src_lun_uuid", strconv.Quote(spec.SrcLunUuid))
	params.Add("dst_lun_name", strconv.Quote(spec.Name))
	params.Add("dst_location", strconv.Quote(spec.Location))

	type LunCloneResp struct {
		Uuid string `json:"dst_lun_uuid"`
	}

	resp, err := dsm.sendRequest("", &LunCloneResp{}, params, "webapi/entry.cgi")
	if err != nil {
		return "", errCodeMapping(resp.ErrorCode, err)
	}

	cloneLunResp, ok := resp.Data.(*LunCloneResp)
	if !ok {
		return "", fmt.Errorf("Failed to assert response to %T", &LunCloneResp{})
	}

	return cloneLunResp.Uuid, nil
}

func (dsm *DSM) TargetList() ([]TargetInfo, error) {
	params := url.Values{}
	params.Add("api", "SYNO.Core.ISCSI.Target")
	params.Add("method", "list")
	params.Add("version", "1")
	params.Add("additional", "[\"mapped_lun\", \"connected_sessions\"]")

	type TargetInfos struct {
		Targets []TargetInfo `json:"targets"`
	}

	resp, err := dsm.sendRequest("", &TargetInfos{}, params, "webapi/entry.cgi")
	if err != nil {
		return nil, errCodeMapping(resp.ErrorCode, err)
	}

	trgInfos, ok := resp.Data.(*TargetInfos)
	if !ok {
		return nil, fmt.Errorf("Failed to assert response to %T", &TargetInfos{})
	}
	return trgInfos.Targets, nil
}

func (dsm *DSM) TargetGet(targetId string) (TargetInfo, error) {
	params := url.Values{}
	params.Add("api", "SYNO.Core.ISCSI.Target")
	params.Add("method", "get")
	params.Add("version", "1")
	params.Add("target_id", strconv.Quote(targetId))
	params.Add("additional", "[\"mapped_lun\", \"connected_sessions\"]")

	type Info struct {
		Target TargetInfo `json:"target"`
	}
	info := Info{}

	resp, err := dsm.sendRequest("", &info, params, "webapi/entry.cgi")
	if err != nil {
		return TargetInfo{}, errCodeMapping(resp.ErrorCode, err)
	}

	return info.Target, nil
}

// Enable muti session
func (dsm *DSM) TargetSet(targetId string, maxSession int) error {
	params := url.Values{}
	params.Add("api", "SYNO.Core.ISCSI.Target")
	params.Add("method", "set")
	params.Add("version", "1")
	params.Add("target_id", strconv.Quote(targetId))
	params.Add("max_sessions", strconv.Itoa(maxSession))

	resp, err := dsm.sendRequest("", &struct{}{}, params, "webapi/entry.cgi")
	if err != nil {
		return errCodeMapping(resp.ErrorCode, err)
	}

	return nil
}

func (dsm *DSM) TargetCreate(spec TargetCreateSpec) (string, error) {
	params := url.Values{}
	params.Add("api", "SYNO.Core.ISCSI.Target")
	params.Add("method", "create")
	params.Add("version", "1")
	params.Add("name", spec.Name)
	params.Add("auth_type", "0")
	params.Add("iqn", spec.Iqn)

	type TrgCreateResp struct {
		TargetId int `json:"target_id"`
	}

	resp, err := dsm.sendRequest("", &TrgCreateResp{}, params, "webapi/entry.cgi")
	if err != nil {
		return "", errCodeMapping(resp.ErrorCode, err)
	}

	trgResp, ok := resp.Data.(*TrgCreateResp)
	if !ok {
		return "", fmt.Errorf("Failed to assert response to %T", &TrgCreateResp{})
	}

	return strconv.Itoa(trgResp.TargetId), nil
}

func (dsm *DSM) LunMapTarget(targetIds []string, lunUuid string) error {
	params := url.Values{}
	params.Add("api", "SYNO.Core.ISCSI.LUN")
	params.Add("method", "map_target")
	params.Add("version", "1")
	params.Add("uuid", strconv.Quote(lunUuid))
	params.Add("target_ids", fmt.Sprintf("[%s]", strings.Join(targetIds, ",")))

	if logger.WebapiDebug {
		log.Debugln(params)
	}

	resp, err := dsm.sendRequest("", &struct{}{}, params, "webapi/entry.cgi")
	if err != nil {
		return errCodeMapping(resp.ErrorCode, err)
	}
	return nil
}

func (dsm *DSM) LunDelete(lunUuid string) error {
	params := url.Values{}
	params.Add("api", "SYNO.Core.ISCSI.LUN")
	params.Add("method", "delete")
	params.Add("version", "1")
	params.Add("uuid", strconv.Quote(lunUuid))

	resp, err := dsm.sendRequest("", &struct{}{}, params, "webapi/entry.cgi")
	if err != nil {
		return errCodeMapping(resp.ErrorCode, err)
	}
	return nil
}

func (dsm *DSM) TargetDelete(targetName string) error {
	params := url.Values{}
	params.Add("api", "SYNO.Core.ISCSI.Target")
	params.Add("method", "delete")
	params.Add("version", "1")
	params.Add("target_id", strconv.Quote(targetName))

	resp, err := dsm.sendRequest("", &struct{}{}, params, "webapi/entry.cgi")
	if err != nil {
		return errCodeMapping(resp.ErrorCode, err)
	}
	return nil
}

func (dsm *DSM) SnapshotCreate(spec SnapshotCreateSpec) (string, error) {
	params := url.Values{}
	params.Add("api", "SYNO.Core.ISCSI.LUN")
	params.Add("method", "take_snapshot")
	params.Add("version", "1")
	params.Add("src_lun_uuid", strconv.Quote(spec.LunUuid))
	params.Add("snapshot_name", strconv.Quote(spec.Name))
	params.Add("description", strconv.Quote(spec.Description))
	params.Add("taken_by", strconv.Quote(spec.TakenBy))
	params.Add("is_locked", strconv.FormatBool(spec.IsLocked))
	params.Add("is_app_consistent", strconv.FormatBool(false))

	type SnapshotCreateResp struct {
		Uuid string `json:"snapshot_uuid"`
	}

	resp, err := dsm.sendRequest("", &SnapshotCreateResp{}, params, "webapi/entry.cgi")
	if err != nil {
		return "", errCodeMapping(resp.ErrorCode, err)
	}

	snapshotResp, ok := resp.Data.(*SnapshotCreateResp)
	if !ok {
		return "", fmt.Errorf("Failed to assert response to %T", &SnapshotCreateResp{})
	}

	return snapshotResp.Uuid, nil
}

func (dsm *DSM) SnapshotDelete(snapshotUuid string) error {
	params := url.Values{}
	params.Add("api", "SYNO.Core.ISCSI.LUN")
	params.Add("method", "delete_snapshot")
	params.Add("version", "1")
	params.Add("snapshot_uuid", strconv.Quote(snapshotUuid))

	resp, err := dsm.sendRequest("", &struct{}{}, params, "webapi/entry.cgi")
	if err != nil {
		return errCodeMapping(resp.ErrorCode, err)
	}
	return nil
}

func (dsm *DSM) SnapshotGet(snapshotUuid string) (SnapshotInfo, error) {
	params := url.Values{}
	params.Add("api", "SYNO.Core.ISCSI.LUN")
	params.Add("method", "get_snapshot")
	params.Add("version", "1")
	params.Add("snapshot_uuid", strconv.Quote(snapshotUuid))

	type Info struct {
		Snapshot SnapshotInfo `json:"snapshot"`
	}
	info := Info{}

	resp, err := dsm.sendRequest("", &info, params, "webapi/entry.cgi")
	if err != nil {
		return SnapshotInfo{}, errCodeMapping(resp.ErrorCode, err)
	}

	return info.Snapshot, nil
}

func (dsm *DSM) SnapshotList(lunUuid string) ([]SnapshotInfo, error) {
	params := url.Values{}
	params.Add("api", "SYNO.Core.ISCSI.LUN")
	params.Add("method", "list_snapshot")
	params.Add("version", "1")
	params.Add("src_lun_uuid", strconv.Quote(lunUuid))

	type Infos struct {
		Snapshots []SnapshotInfo `json:"snapshots"`
	}

	resp, err := dsm.sendRequest("", &Infos{}, params, "webapi/entry.cgi")
	if err != nil {
		return nil, errCodeMapping(resp.ErrorCode, err)
	}

	infos, ok := resp.Data.(*Infos)
	if !ok {
		return nil, fmt.Errorf("Failed to assert response to %T", &Infos{})
	}

	return infos.Snapshots, nil
}

func (dsm *DSM) SnapshotClone(spec SnapshotCloneSpec) (string, error) {
	params := url.Values{}
	params.Add("api", "SYNO.Core.ISCSI.LUN")
	params.Add("method", "clone_snapshot")
	params.Add("version", "1")
	params.Add("src_lun_uuid", strconv.Quote(spec.SrcLunUuid))
	params.Add("snapshot_uuid", strconv.Quote(spec.SrcSnapshotUuid))
	params.Add("cloned_lun_name", strconv.Quote(spec.Name))

	type SnapshotCloneResp struct {
		Uuid string `json:"cloned_lun_uuid"`
	}

	resp, err := dsm.sendRequest("", &SnapshotCloneResp{}, params, "webapi/entry.cgi")
	if err != nil {
		return "", errCodeMapping(resp.ErrorCode, err)
	}

	snapshotCloneResp, ok := resp.Data.(*SnapshotCloneResp)
	if !ok {
		return "", fmt.Errorf("Failed to assert response to %T", &SnapshotCloneResp{})
	}

	return snapshotCloneResp.Uuid, nil
}
