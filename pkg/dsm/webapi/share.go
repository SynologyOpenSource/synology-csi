// Copyright 2021 Synology Inc.

package webapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/SynologyOpenSource/synology-csi/pkg/utils"
	"github.com/SynologyOpenSource/synology-csi/pkg/logger"
	"github.com/cenkalti/backoff/v4"
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

type NfsInfo struct {
	EnableNfs           bool   `json:"enable_nfs"`
	EnableNfsV4         bool   `json:"enable_nfs_v4"`
	NfsV4Domain         string `json:"nfs_v4_domain"`
	ReadSize            int    `json:"read_size"`
	SupportEncryptShare int    `json:"support_encrypt_share"`
	SupportMajorVer     int    `json:"support_major_ver"`
	SupportMinorVer     int    `json:"support_minor_ver"`
	UnixPriEnable       bool   `json:"unix_pri_enable"`
	WriteSize           int    `json:"write_size"`
}

// DSM rejects share operations while its share subsystem is busy (error 3328).
// That happens whenever several volumes are created or removed at once, and it
// clears on its own, so retry instead of failing an operation the caller has no
// way to act on. Kept below the CSI sidecar provision timeout so the retries
// finish before the caller gives up.
const shareBusyRetryTimeout = 30 * time.Second

// shareRequest issues a share API request, maps DSM error codes, and retries
// while the share subsystem reports it is busy.
func (dsm *DSM) shareRequest(apiTemplate interface{}, params url.Values) (Response, error) {
	var resp Response

	err := backoff.Retry(func() error {
		var reqErr error
		resp, reqErr = dsm.sendRequest("", apiTemplate, params, "webapi/entry.cgi")
		mapped := shareErrCodeMapping(resp.ErrorCode, reqErr)
		if mapped == nil {
			return nil
		}
		if errors.Is(mapped, utils.ShareSystemBusyError("")) {
			log.Infof("DSM[%s] share subsystem is busy, retrying: %v", dsm.Ip, mapped)
			return mapped
		}
		return backoff.Permanent(mapped)
	}, shareBusyBackOff())

	return resp, err
}

// 2370 is WEBAPI_CORE_ERR_NFS_SHARE_LOAD_FAIL: the NFS privilege API could not
// look the share up. DSM raises it from two situations it does not tell apart —
// the share is not in the share database, or the shared lock on smb.conf could
// not be taken within its own five second limit. Only the second one is worth
// waiting for, and a share that is gone never comes back, so retrying here buys
// very little: of 169 of these seen in one run, 165 were for a share that had
// already been deleted.
//
// It is kept, and kept short, for the one case it does cover: a share being
// deleted at the very moment we ask about it. Anything longer only delays the
// error without changing it.
const nfsPrivilegeShareLoadFailErrCode = 2370

const nfsPrivilegeRetryTimeout = 10 * time.Second

// nfsPrivilegeRequest issues an NFS share-privilege request, retrying briefly
// when DSM cannot look the share up. Errors are returned unmapped: the share
// error-code table belongs to another API namespace.
func (dsm *DSM) nfsPrivilegeRequest(apiTemplate interface{}, params url.Values, retryTimeout time.Duration) (Response, error) {
	var resp Response

	err := backoff.Retry(func() error {
		var reqErr error
		resp, reqErr = dsm.sendRequest("", apiTemplate, params, "webapi/entry.cgi")
		if reqErr == nil {
			return nil
		}
		if resp.ErrorCode == nfsPrivilegeShareLoadFailErrCode {
			log.Infof("DSM[%s] couldn't look up the share for its NFS privilege (error %d), retrying", dsm.Ip, resp.ErrorCode)
			return reqErr
		}
		return backoff.Permanent(reqErr)
	}, busyBackOff(retryTimeout))

	return resp, err
}

func shareBusyBackOff() backoff.BackOff {
	return busyBackOff(shareBusyRetryTimeout)
}

func busyBackOff(retryTimeout time.Duration) backoff.BackOff {
	b := backoff.NewExponentialBackOff()
	b.MaxElapsedTime = retryTimeout
	return b
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

	_, err := dsm.shareRequest(&info, params)

	return info, err
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

	resp, err := dsm.shareRequest(&ShareInfos{}, params)
	if err != nil {
		return nil, err
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

	_, err = dsm.shareRequest(&struct{}{}, params)

	return err
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

	resp, err := dsm.shareRequest(&ShareCreateResp{}, params)
	if err != nil {
		return "", err
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

	_, err := dsm.shareRequest(&struct{}{}, params)

	return err
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

	_, err = dsm.shareRequest(&struct{}{}, params)

	return err
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
	_, err = dsm.shareRequest(&snapTime, params)
	if err != nil {
		return "", err
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

	resp, err := dsm.shareRequest(&Infos{}, params)
	if err != nil {
		return nil, err
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
	_, err := dsm.shareRequest(&objmap, params)
	if err != nil {
		return err
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

	if logger.WebapiDebug {
		log.Debugln(params)
	}

	_, err = dsm.shareRequest(&struct{}{}, params)

	return err
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

	resp, err := dsm.shareRequest(&SharePermissions{}, params)
	if err != nil {
		return nil, err
	}

	infos, ok := resp.Data.(*SharePermissions)
	if !ok {
		return nil, fmt.Errorf("Failed to assert response to %T", &SharePermissions{})
	}

	return infos.Permissions, nil
}

// ----------------------- FileServ NFS SharePrivilege APIs -----------------------
type SecurityFlavor struct {
	Kerbros          bool `json:"kerberos"`
	KerbrosIntegrity bool `json:"kerberos_integrity"`
	KerbrosPrivacy   bool `json:"kerberos_privacy"`
	Sys              bool `json:"sys"`
}

type PrivilegeRule struct {
	Async          bool           `json:"async"`
	Client         string         `json:"client"`
	Crossmnt       bool           `json:"crossmnt"`
	Insecure       bool           `json:"insecure"`
	Privilege      string         `json:"privilege"`
	RootSquash     string         `json:"root_squash"`
	SecurityFlavor SecurityFlavor `json:"security_flavor"`
}

type SharePrivilege struct {
	ShareName string          `json:"share_name"`
	Rule      []PrivilegeRule `json:"rule"`
}

// DSM's NFS privilege save takes no lock. When several saves run at once it can
// answer {"success":true} without the rule ever reaching /etc/exports, and it
// never repairs itself afterwards -- confirmed with the DSM team on 2026-08-17,
// and reproduced outside Kubernetes at roughly a third of concurrent saves. The
// files it writes (/etc/exports, exports_syno, exports_map) were each left
// incomplete in different combinations, so there is no single one to check.
//
// A save that silently did nothing leaves the share unexported, and the client
// gets "No such file or directory" from the mount -- so a pod sits in
// ContainerCreating until it times out. Reading the rules back is the only way
// to find out, because the save itself claims to have worked.
// A var rather than a const so tests can shorten it; nothing else reassigns it.
var nfsPrivilegeVerifyTimeout = 30 * time.Second

// ShareNfsPrivilegeSave writes a share's NFS export rules and confirms they
// took effect, retrying the whole write when they did not.
func (dsm *DSM) ShareNfsPrivilegeSave(privilege SharePrivilege) error {
	attempts := 0
	return backoff.Retry(func() error {
		attempts++
		if err := dsm.shareNfsPrivilegeSaveOnce(privilege); err != nil {
			// nfsPrivilegeRequest has already retried what is worth retrying at
			// the request level; anything left is the caller's problem.
			return backoff.Permanent(err)
		}

		got, err := dsm.ShareNfsPrivilegeLoad(privilege.ShareName)
		if err != nil {
			// We cannot tell whether the save landed. Try again rather than
			// report a success we have not seen.
			log.Infof("DSM[%s] couldn't read back the NFS privilege of share(%s) to confirm the save: %v",
				dsm.Ip, privilege.ShareName, err)
			return err
		}

		if missing := missingPrivilegeRules(got, privilege); len(missing) > 0 {
			log.Warnf("DSM[%s] reported success saving the NFS privilege of share(%s) but rules for %v are not in effect (attempt %d), retrying",
				dsm.Ip, privilege.ShareName, missing, attempts)
			return fmt.Errorf("DSM[%s] did not apply the NFS export rules of share(%s) for %v",
				dsm.Ip, privilege.ShareName, missing)
		}

		if attempts > 1 {
			log.Infof("DSM[%s] NFS privilege of share(%s) took effect after %d attempts",
				dsm.Ip, privilege.ShareName, attempts)
		}
		return nil
	}, busyBackOff(nfsPrivilegeVerifyTimeout))
}

func (dsm *DSM) shareNfsPrivilegeSaveOnce(privilege SharePrivilege) error {
	params := url.Values{}
	params.Add("api", "SYNO.Core.FileServ.NFS.SharePrivilege")
	params.Add("method", "save")
	params.Add("share_name", strconv.Quote(privilege.ShareName))
	params.Add("version", "1")

	js, err := json.Marshal(privilege.Rule)
	if err != nil {
		return err
	}
	params.Add("rule", string(js))

	_, err = dsm.nfsPrivilegeRequest(&struct{}{}, params, nfsPrivilegeRetryTimeout)
	if err != nil {
		return err
	}

	return nil
}

// missingPrivilegeRules returns the clients we asked to allow that DSM does not
// actually have rules for.
//
// It compares client and privilege only. DSM normalises the remaining fields,
// and what decides whether the mount works is that the client appears with the
// access we asked for. Requesting no rules -- which the driver never does, but
// which is how the rules would be cleared -- leaves nothing to confirm.
func missingPrivilegeRules(got, want SharePrivilege) []string {
	inEffect := make(map[string]string, len(got.Rule))
	for _, rule := range got.Rule {
		inEffect[rule.Client] = rule.Privilege
	}

	var missing []string
	for _, rule := range want.Rule {
		if inEffect[rule.Client] != rule.Privilege {
			missing = append(missing, rule.Client)
		}
	}
	return missing
}

func (dsm *DSM) ShareNfsPrivilegeLoad(shareName string) (SharePrivilege, error) {
	params := url.Values{}
	params.Add("api", "SYNO.Core.FileServ.NFS.SharePrivilege")
	params.Add("method", "load")
	params.Add("share_name", strconv.Quote(shareName))
	params.Add("version", "1")

	info := SharePrivilege{}
	_, err := dsm.nfsPrivilegeRequest(&info, params, nfsPrivilegeRetryTimeout)
	if err != nil {
		return SharePrivilege{}, err
	}

	return info, nil
}

func (dsm *DSM) NfsGet() (NfsInfo, error) {
	params := url.Values{}
	params.Add("api", "SYNO.Core.FileServ.NFS")
	params.Add("method", "get")
	params.Add("version", "2")

	info := NfsInfo{}
	// SYNO.Core.FileServ.NFS namespace: share error codes do not apply.
	_, err := dsm.sendRequest("", &info, params, "webapi/entry.cgi")
	if err != nil {
		return NfsInfo{}, err
	}

	return info, nil
}

func (dsm *DSM) NfsSet(enableV3 bool, enableV4 bool, enabledMinorVer int) error {
	params := url.Values{}
	params.Add("api", "SYNO.Core.FileServ.NFS")
	params.Add("method", "set")
	params.Add("version", "2")

	params.Add("enable_nfs", strconv.FormatBool(enableV3))
	params.Add("enable_nfs_v4", strconv.FormatBool(enableV4))
	params.Add("enabled_minor_ver", strconv.Itoa(enabledMinorVer))

	// SYNO.Core.FileServ.NFS namespace: share error codes do not apply.
	_, err := dsm.sendRequest("", &struct{}{}, params, "webapi/entry.cgi")
	if err != nil {
		return err
	}

	return nil
}

