// Copyright 2021 Synology Inc.

package webapi

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"

	log "github.com/sirupsen/logrus"
	"github.com/SynologyOpenSource/synology-csi/pkg/utils"
	"github.com/SynologyOpenSource/synology-csi/pkg/logger"
)

type ShareInfo struct {
	Name                string `json:"name"`                        // required
	VolPath             string `json:"vol_path"`                    // required
	Desc                string `json:"desc"`
	EnableShareCow      bool   `json:"enable_share_cow"`            // field for create
	EnableRecycleBin    bool   `json:"enable_recycle_bin"`
	RecycleBinAdminOnly bool   `json:"recycle_bin_admin_only"`
	Encryption          int    `json:"encryption"`                  // field for create
	QuotaForCreate      *int64 `json:"share_quota,omitempty"`
	QuotaValueInMB      int64  `json:"quota_value"`                 // field for get
	SupportSnapshot     bool   `json:"support_snapshot"`            // field for get
	Uuid                string `json:"uuid"`                        // field for get
	NameOrg             string `json:"name_org"`                    // required for clone
}

type ShareUpdateInfo struct {
	Name                string `json:"name"`                        // required
	VolPath             string `json:"vol_path"`                    // required
	QuotaForCreate      *int64 `json:"share_quota,omitempty"`
	// Add properties you want to update to shares here
}

type ShareSnapshotInfo struct {
	Uuid             string `json:"ruuid"`
	Time             string `json:"time"`
	Desc             string `json:"desc"`
	SnapSize         string `json:"snap_size"` // the complete size of the snapshot
	Lock             bool   `json:"lock"`
	ScheduleSnapshot bool   `json:"schedule_snapshot"`
}

type ShareCreateSpec struct {
	Name      string
	ShareInfo ShareInfo
}

type ShareCloneSpec struct {
	Name      string
	Snapshot  string
	ShareInfo ShareInfo
}

type ShareSnapshotCreateSpec struct {
	ShareName string
	Desc      string
	IsLocked  bool
}

type SharePermissionSetSpec struct {
	Name          string
	UserGroupType string            // "local_user"/"local_group"/"system"
	Permissions   []*SharePermission
}

type SharePermission struct {
	Name       string `json:"name"`
	IsReadonly bool   `json:"is_readonly"`
	IsWritable bool   `json:"is_writable"`
	IsDeny     bool   `json:"is_deny"`
	IsCustom   bool   `json:"is_custom,omitempty"`
	IsAdmin    bool   `json:"is_admin,omitempty"` // field for list
}

func shareErrCodeMapping(errCode int, oriErr error) error {
	switch errCode {
	case 402: // No such share
		return utils.NoSuchShareError("")
	case 403: // Invalid input value
		return utils.BadParametersError("")
	case 3301: // already exists
		return utils.AlreadyExistError("")
	case 3309:
		return utils.ShareReachMaxCountError("")
	case 3328:
		return utils.ShareSystemBusyError("")
	}

	if errCode >= 3300 {
		return utils.ShareDefaultError{errCode}
	}
	return oriErr
}

// ----------------------- Share APIs -----------------------
func (dsm *DSM) ShareGet(shareName string) (ShareInfo, error) {
	params := url.Values{}
	params.Add("api", "SYNO.Core.Share")
	params.Add("method", "get")
	params.Add("version", "1")
	params.Add("additional", "[\"encryption\", \"enable_share_cow\", \"recyclebin\", \"support_snapshot\", \"share_quota\"]")
	params.Add("name", strconv.Quote(shareName))

	info := ShareInfo{}

	resp, err := dsm.sendRequest("", &info, params, "webapi/entry.cgi")

	return info, shareErrCodeMapping(resp.ErrorCode, err)
}

func (dsm *DSM) ShareList() ([]ShareInfo, error) {
	params := url.Values{}
	params.Add("api", "SYNO.Core.Share")
	params.Add("method", "list")
	params.Add("version", "1")
	params.Add("additional", "[\"encryption\", \"enable_share_cow\", \"recyclebin\", \"support_snapshot\", \"share_quota\"]")

	type ShareInfos struct {
		Shares []ShareInfo `json:"shares"`
	}

	resp, err := dsm.sendRequest("", &ShareInfos{}, params, "webapi/entry.cgi")
	if err != nil {
		return nil, shareErrCodeMapping(resp.ErrorCode, err)
	}

	infos, ok := resp.Data.(*ShareInfos)
	if !ok {
		return nil, fmt.Errorf("Failed to assert response to %T", &ShareInfos{})
	}

	return infos.Shares, nil
}

func (dsm *DSM) ShareCreate(spec ShareCreateSpec) error {
	params := url.Values{}
	params.Add("api", "SYNO.Core.Share")
	params.Add("method", "create")
	params.Add("version", "1")
	params.Add("name", strconv.Quote(spec.Name))

	js, err := json.Marshal(spec.ShareInfo)
	if err != nil {
		return err
	}
	params.Add("shareinfo", string(js))

	resp, err := dsm.sendRequest("", &struct{}{}, params, "webapi/entry.cgi")

	return shareErrCodeMapping(resp.ErrorCode, err)
}

func (dsm *DSM) ShareClone(spec ShareCloneSpec) (string, error) {
	params := url.Values{}
	params.Add("api", "SYNO.Core.Share")
	params.Add("method", "clone")
	params.Add("version", "1")
	params.Add("name", strconv.Quote(spec.Name))

	// if clone from snapshot, the NameOrg must be the parent of the snapshot, or the webapi will return 3300
	// if the snapshot doesn't exist, it will return 3300 too.
	if spec.ShareInfo.NameOrg == "" {
		return "", fmt.Errorf("Clone failed. The source name can't be empty.")
	}

	if spec.Snapshot != "" {
		params.Add("snapshot", strconv.Quote(spec.Snapshot))
	}

	js, err := json.Marshal(spec.ShareInfo)
	if err != nil {
		return "", err
	}
	params.Add("shareinfo", string(js))

	type ShareCreateResp struct {
		Name string `json:"name"`
	}

	resp, err := dsm.sendRequest("", &ShareCreateResp{}, params, "webapi/entry.cgi")
	if err != nil {
		return "", shareErrCodeMapping(resp.ErrorCode, err)
	}

	shareResp, ok := resp.Data.(*ShareCreateResp)
	if !ok {
		return "", fmt.Errorf("Failed to assert response to %T", &ShareCreateResp{})
	}

	return shareResp.Name, nil
}

func (dsm *DSM) ShareDelete(shareName string) error {
	params := url.Values{}
	params.Add("api", "SYNO.Core.Share")
	params.Add("method", "delete")
	params.Add("version", "1")
	params.Add("name", fmt.Sprintf("[%s]", strconv.Quote(shareName)))

	resp, err := dsm.sendRequest("", &struct{}{}, params, "webapi/entry.cgi")

	return shareErrCodeMapping(resp.ErrorCode, err)
}

func (dsm *DSM) ShareSet(shareName string, updateInfo ShareUpdateInfo) error {
	params := url.Values{}
	params.Add("api", "SYNO.Core.Share")
	params.Add("method", "set")
	params.Add("version", "1")
	params.Add("name", strconv.Quote(shareName))

	js, err := json.Marshal(updateInfo)
	if err != nil {
		return err
	}
	params.Add("shareinfo", string(js))

	if logger.WebapiDebug {
		log.Debugln(params)
	}

	resp, err := dsm.sendRequest("", &struct{}{}, params, "webapi/entry.cgi")

	return shareErrCodeMapping(resp.ErrorCode, err)
}

func (dsm *DSM) SetShareQuota(shareInfo ShareInfo, newSizeInMB int64) error {
	updateInfo := ShareUpdateInfo{
		Name:           shareInfo.Name,
		VolPath:        shareInfo.VolPath,
		QuotaForCreate: &newSizeInMB,
	}
	return dsm.ShareSet(shareInfo.Name, updateInfo)
}

// ----------------------- Share Snapshot APIs -----------------------
func (dsm *DSM) ShareSnapshotCreate(spec ShareSnapshotCreateSpec) (string, error) {
	params := url.Values{}
	params.Add("api", "SYNO.Core.Share.Snapshot")
	params.Add("method", "create")
	params.Add("version", "1")
	params.Add("name", strconv.Quote(spec.ShareName))

	type SnapInfo struct {
		Desc     string `json:"desc"`
		IsLocked bool   `json:"lock"` // default true
	}

	snapinfo := SnapInfo{
		Desc:     spec.Desc,
		IsLocked: spec.IsLocked,
	}
	js, err := json.Marshal(snapinfo)
	if err != nil {
		return "", err
	}
	params.Add("snapinfo", string(js))

	var snapTime string
	resp, err := dsm.sendRequest("", &snapTime, params, "webapi/entry.cgi")
	if err != nil {
		return "", shareErrCodeMapping(resp.ErrorCode, err)
	}

	return snapTime, nil // "GMT+08-2022.01.14-19.18.29"
}

func (dsm *DSM) ShareSnapshotList(name string) ([]ShareSnapshotInfo, error) {
	params := url.Values{}
	params.Add("api", "SYNO.Core.Share.Snapshot")
	params.Add("method", "list")
	params.Add("version", "2")
	params.Add("name", strconv.Quote(name))
	params.Add("additional", "[\"desc\", \"lock\", \"schedule_snapshot\", \"ruuid\", \"snap_size\"]")

	type Infos struct {
		Snapshots []ShareSnapshotInfo `json:"snapshots"`
		Total     int                 `json:"total"`
	}

	resp, err := dsm.sendRequest("", &Infos{}, params, "webapi/entry.cgi")
	if err != nil {
		return nil, shareErrCodeMapping(resp.ErrorCode, err)
	}

	infos, ok := resp.Data.(*Infos)
	if !ok {
		return nil, fmt.Errorf("Failed to assert response to %T", &Infos{})
	}

	return infos.Snapshots, nil
}

func (dsm *DSM) ShareSnapshotDelete(snapTime string, shareName string) error {
	params := url.Values{}
	params.Add("api", "SYNO.Core.Share.Snapshot")
	params.Add("method", "delete")
	params.Add("version", "1")
	params.Add("name", strconv.Quote(shareName))
	params.Add("snapshots", fmt.Sprintf("[%s]", strconv.Quote(snapTime))) // ["GMT+08-2022.01.14-19.18.29"]

	var objmap []map[string]interface{}
	resp, err := dsm.sendRequest("", &objmap, params, "webapi/entry.cgi")
	if err != nil {
		return shareErrCodeMapping(resp.ErrorCode, err)
	}

	if len(objmap) > 0 {
		return fmt.Errorf("Failed to delete snapshot, API common error. snapshot: %s", snapTime)
	}

	return nil
}

// ----------------------- Share Permission APIs -----------------------
func (dsm *DSM) SharePermissionSet(spec SharePermissionSetSpec) error {
	params := url.Values{}
	params.Add("api", "SYNO.Core.Share.Permission")
	params.Add("method", "set")
	params.Add("version", "1")
	params.Add("name", strconv.Quote(spec.Name))
	params.Add("user_group_type", strconv.Quote(spec.UserGroupType))

	js, err := json.Marshal(spec.Permissions)
	if err != nil {
		return err
	}
	params.Add("permissions", string(js))

	resp, err := dsm.sendRequest("", &struct{}{}, params, "webapi/entry.cgi")

	return shareErrCodeMapping(resp.ErrorCode, err)
}

func (dsm *DSM) SharePermissionList(shareName string, userGroupType string) ([]SharePermission, error) {
	params := url.Values{}
	params.Add("api", "SYNO.Core.Share.Permission")
	params.Add("method", "list")
	params.Add("version", "1")
	params.Add("name", strconv.Quote(shareName))
	params.Add("user_group_type", strconv.Quote(userGroupType))

	type SharePermissions struct {
		Permissions []SharePermission `json:"items"`
	}

	resp, err := dsm.sendRequest("", &SharePermissions{}, params, "webapi/entry.cgi")
	if err != nil {
		return nil, shareErrCodeMapping(resp.ErrorCode, err)
	}

	infos, ok := resp.Data.(*SharePermissions)
	if !ok {
		return nil, fmt.Errorf("Failed to assert response to %T", &SharePermissions{})
	}

	return infos.Permissions, nil
}

