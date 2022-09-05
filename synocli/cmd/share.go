/*
 * Copyright 2022 Synology Inc.
 */
package cmd

import (
	"fmt"
	"os"
	"strings"
	"github.com/spf13/cobra"
	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/webapi"
	"github.com/SynologyOpenSource/synology-csi/pkg/utils"
	"strconv"

	"text/tabwriter"
)

var cmdShare = &cobra.Command{
	Use:   "share",
	Short: "share API",
	Long:  `DSM share API`,
	Run: func(cmd *cobra.Command, args []string) {
		cmd.Help()
	},
}

var cmdShareGet = &cobra.Command{
	Use:   "get <name>",
	Short: "get share",
	Args:  cobra.MinimumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		dsm, err := LoginDsmForTest(DsmId)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		defer func() {
			dsm.Logout()
		}()

		share, err := dsm.ShareGet(args[0])
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		fmt.Printf("Success, ShareGet(%s) = %#v\n", args[0], share)
	},
}

var cmdShareList = &cobra.Command{
	Use:   "list",
	Short: "list shares",
	Args:  cobra.MinimumNArgs(0),
	Run: func(cmd *cobra.Command, args []string) {
		dsms, err := ListDsms(DsmId)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		shareInfos := make(map[string][]webapi.ShareInfo)
		for _, dsm := range dsms {
			if err := dsm.Login(); err != nil {
				fmt.Printf("Failed to login to DSM: [%s]. err: %v\n", dsm.Ip, err)
				os.Exit(1)
			}
			shares, err := dsm.ShareList()
			if err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
			shareInfos[dsm.Ip] = shares
			dsm.Logout()
		}

		tw := tabwriter.NewWriter(os.Stdout, 8, 0, 2, ' ', 0)
		fmt.Fprintf(tw, "%-16s\t", "Host:")
		fmt.Fprintf(tw, "%-32s\t", "Name:")
		fmt.Fprintf(tw, "%-36s\t", "Uuid:")
		fmt.Fprintf(tw, "%-10s\t", "VolPath:")
		fmt.Fprintf(tw, "%-64s\t", "Desc:")
		fmt.Fprintf(tw, "%-12s\t", "Quota(MB):")
		fmt.Fprintf(tw, "%s\t", "Cow:")
		fmt.Fprintf(tw, "%s\t", "RecycleBin:")
		fmt.Fprintf(tw, "%s\t", "RBAdminOnly:")
		fmt.Fprintf(tw, "%-10s\t", "Encryption:")
		fmt.Fprintf(tw, "%s\t", "CanSnap:")
		fmt.Fprintf(tw, "\n")
		for ip, v := range shareInfos {
			for _, info := range v {
				fmt.Fprintf(tw, "%-16s\t", ip)
				fmt.Fprintf(tw, "%-32s\t", info.Name)
				fmt.Fprintf(tw, "%-36s\t", info.Uuid)
				fmt.Fprintf(tw, "%-10s\t", info.VolPath)
				fmt.Fprintf(tw, "%-64s\t", info.Desc)
				fmt.Fprintf(tw, "%-12d\t", info.QuotaValueInMB)
				fmt.Fprintf(tw, "%v\t", info.EnableShareCow)
				fmt.Fprintf(tw, "%v\t", info.EnableRecycleBin)
				fmt.Fprintf(tw, "%v\t", info.RecycleBinAdminOnly)
				fmt.Fprintf(tw, "%-10d\t", info.Encryption)
				fmt.Fprintf(tw, "%v\t", info.SupportSnapshot)
				fmt.Fprintf(tw, "\n")
				_ = tw.Flush()
			}
		}

		fmt.Printf("Success, ShareList()\n")
	},
}

var cmdShareCreate = &cobra.Command{
	Use:   "create <name> <location> [<size_bytes>]",
	Short: "create share",
	Args:  cobra.MinimumNArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		dsm, err := LoginDsmForTest(DsmId)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		defer func() {
			dsm.Logout()
		}()

		var size int64 = 0
		if len(args) >= 3 {
			size, err = strconv.ParseInt(args[2], 10, 64)
			if err != nil {
			    fmt.Println(err)
				os.Exit(1)
			}
		}
		sizeInMB := utils.BytesToMBCeil(size)
		testSpec := webapi.ShareCreateSpec{
			Name: args[0],
			ShareInfo: webapi.ShareInfo{
				Name:                args[0],
				VolPath:             args[1],
				Desc:                "Created by synocli",
				EnableShareCow:      false,
				EnableRecycleBin:    true,
				RecycleBinAdminOnly: true,
				Encryption:          0,
				QuotaForCreate:      &sizeInMB,
			},
		}

		fmt.Printf("spec = %#v, sizeInMB = %d\n", testSpec, sizeInMB)
		err = dsm.ShareCreate(testSpec)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		share, err := dsm.ShareGet(args[0])
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		fmt.Printf("Success, ShareCreate(%s) resp = %#v\n", args[0], share)
	},
}

var cmdShareDelete = &cobra.Command{
	Use:   "delete <name>",
	Short: "delete share",
	Args:  cobra.MinimumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		dsm, err := LoginDsmForTest(DsmId)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		defer func() {
			dsm.Logout()
		}()

		err = dsm.ShareDelete(args[0])
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		fmt.Printf("Success, ShareDelete(%s)\n", args[0])
	},
}

var cmdShareClone = &cobra.Command{
	Use:   "clone <new name> <src name> <is_snapshot [true|false]>",
	Short: "clone share",
	Args:  cobra.MinimumNArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		dsm, err := LoginDsmForTest(DsmId)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		defer func() {
			dsm.Logout()
		}()

		fromSnapshot := false
		if len(args) >= 3 && args[2] == "true" {
			fromSnapshot = true
		}

		newName := args[0]
		srcName := args[1]
		orgShareName := ""
		snapshot := ""
		if fromSnapshot {
			snapshot = srcName
			shares, err := dsm.ShareList()
			if err != nil {
				fmt.Println(err)
				os.Exit(1)
			}

			for _, share := range shares {
				snaps, err := dsm.ShareSnapshotList(share.Name)
				if err != nil {
					fmt.Println(err)
					os.Exit(1)
				}
				for _, snap := range snaps {
					if snap.Time != srcName {
						continue
					}
					orgShareName = share.Name
					break
				}
				if orgShareName != "" {
					break
				}
			}

			if orgShareName == "" {
				fmt.Println("Failed to find org Share of the snapshot")
				os.Exit(1)
			}
		} else {
			orgShareName = srcName
		}
		srcShare, err := dsm.ShareGet(orgShareName)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		shareSpec := webapi.ShareCloneSpec{
			Name: newName,
			Snapshot: snapshot,
			ShareInfo: webapi.ShareInfo{
				Name:                newName,
				VolPath:             srcShare.VolPath,
				Desc:                "Cloned from "+srcName+" by synocli",
				EnableRecycleBin:    srcShare.EnableRecycleBin,
				RecycleBinAdminOnly: srcShare.RecycleBinAdminOnly,
				NameOrg:             orgShareName,
			},
		}
		fmt.Printf("newName: %s, fromSnapshot: %v (%s), orgShareName: %s\n", newName, fromSnapshot, snapshot, orgShareName)
		_, err = dsm.ShareClone(shareSpec)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		share, err := dsm.ShareGet(newName)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		fmt.Printf("Success, ShareClone(%s, %s) new share = %#v\n", newName, srcName, share)
	},
}

var cmdShareSnapshotCreate = &cobra.Command{
	Use:   "snap_create <src name> <desc> <is_lock [true|false]>",
	Short: "create share snapshot (only share located in btrfs volume can take snapshots)",
	Args:  cobra.MinimumNArgs(3),
	Run: func(cmd *cobra.Command, args []string) {
		dsm, err := LoginDsmForTest(DsmId)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		defer func() {
			dsm.Logout()
		}()

		spec := webapi.ShareSnapshotCreateSpec{
			ShareName: args[0],
			Desc:      args[1],
			IsLocked:  utils.StringToBoolean(args[2]),
		}

		fmt.Printf("spec = %#v\n", spec)
		snapTime, err := dsm.ShareSnapshotCreate(spec)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		fmt.Printf("resp = %s\n", snapTime)

		snaps, err := dsm.ShareSnapshotList(spec.ShareName)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		var snapshot webapi.ShareSnapshotInfo
		for _, snap := range snaps {
			if snap.Time == snapTime {
				snapshot = snap
				break
			}
		}

		fmt.Printf("Success, ShareSnapshotCreate(%s), snap = %#v\n", args[0], snapshot)
	},
}

var cmdShareSnapshotDelete = &cobra.Command{
	Use:   "snap_delete <share name> <snapshot time>",
	Short: "delete share snapshot",
	Args:  cobra.MinimumNArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		dsm, err := LoginDsmForTest(DsmId)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		defer func() {
			dsm.Logout()
		}()

		if err := dsm.ShareSnapshotDelete(args[1], args[0]); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		fmt.Printf("Success, ShareSnapshotDelete(%s, %s)\n", args[1], args[0])
	},
}

func shareSnapshotListAll(dsm *webapi.DSM, shareName string) ([]webapi.ShareSnapshotInfo, error) {
	if shareName != "" {
		return dsm.ShareSnapshotList(shareName)
	}

	var infos []webapi.ShareSnapshotInfo
	shares, err := dsm.ShareList()
	if err != nil {
		fmt.Println(err)
		return nil, err
	}

	for _, share := range shares {
		snaps, err := dsm.ShareSnapshotList(share.Name)
		if err != nil {
			fmt.Println(err)
			return nil, err
		}
		infos = append(infos, snaps...)
	}

	return infos, nil
}

var cmdShareSnapshotList = &cobra.Command{
	Use:   "snap_list [<share name>]",
	Short: "list share snapshots",
	Args:  cobra.MinimumNArgs(0),
	Run: func(cmd *cobra.Command, args []string) {
		dsm, err := LoginDsmForTest(DsmId)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		defer func() {
			dsm.Logout()
		}()

		shareName := ""
		if len(args) >= 1 {
			shareName = args[0]
		}

		snaps, err := shareSnapshotListAll(dsm, shareName)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		tw := tabwriter.NewWriter(os.Stdout, 8, 0, 2, ' ', 0)

		fmt.Fprintf(tw, "id:\tTime:\tDesc:\tLock:\tScheduleSnapshot:\n")
		for i, info := range snaps {
			fmt.Fprintf(tw, "%d\t%-32s\t", i, info.Time)
			fmt.Fprintf(tw, "%-32s\t", info.Desc)
			fmt.Fprintf(tw, "%v\t", info.Lock)
			fmt.Fprintf(tw, "%v\t", info.ScheduleSnapshot)
			fmt.Fprintf(tw, "\n")

			_ = tw.Flush()
		}
		fmt.Printf("Success, ShareSnapList(%s)\n", shareName)
	},
}

func createSharePermission(name string, acl string) *webapi.SharePermission {
	var permission webapi.SharePermission
	permission.Name = name

	switch strings.ToLower(acl) {
	case "rw":
		permission.IsWritable = true
	case "ro":
		permission.IsReadonly = true
	case "no":
		permission.IsDeny = true
	default:
		fmt.Println("[ERROR] Unknown ACL")
		return nil
	}
	return &permission
}

func getShareLocalUserPermission(dsm *webapi.DSM, shareName string, userName string) (*webapi.SharePermission, error) {
	infos, err := dsm.SharePermissionList(shareName, "local_user")
	if err != nil {
		return nil, err
	}
	for _, info := range infos {
		if info.Name == userName {
			return &info, nil
		}
	}
	return nil, fmt.Errorf("Permission Not Found.")
}

var cmdSharePermissionList = &cobra.Command{
	Use:   "permission_list <name> [local_user|local_group|system]",
	Short: "list permissions",
	Args:  cobra.MinimumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		dsm, err := LoginDsmForTest(DsmId)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		defer func() {
			dsm.Logout()
		}()

		userGroupType := "local_user"
		if len(args) >= 2 {
			userGroupType = args[1]
		}

		infos, err := dsm.SharePermissionList(args[0], userGroupType)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		tw := tabwriter.NewWriter(os.Stdout, 8, 0, 2, ' ', 0)

		fmt.Fprintf(tw, "%s\t%-20s\t%-10s\t%-12s\t%-12s\t%-10s\t%-10s\n", "id:", "Name:", "IsAdmin", "IsReadonly:", "IsWritable:", "IsDeny:", "IsCustom:")
		for i, info := range infos {
			fmt.Fprintf(tw, "%d\t", i)
			fmt.Fprintf(tw, "%-20s\t", info.Name)
			fmt.Fprintf(tw, "%-10v\t", info.IsAdmin)
			fmt.Fprintf(tw, "%-12v\t", info.IsReadonly)
			fmt.Fprintf(tw, "%-12v\t", info.IsWritable)
			fmt.Fprintf(tw, "%-10v\t", info.IsDeny)
			fmt.Fprintf(tw, "%-10v\t", info.IsCustom)
			fmt.Fprintf(tw, "\n")

			_ = tw.Flush()
		}

		fmt.Printf("Success, SharePermissionList(%s)\n", args[0])
	},
}

var cmdSharePermissionSet = &cobra.Command{
	Use:   "permission_set <share name> <username> <[rw|ro|no]>",
	Short: "set permission",
	Args:  cobra.MinimumNArgs(3),
	Run: func(cmd *cobra.Command, args []string) {
		dsm, err := LoginDsmForTest(DsmId)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		defer func() {
			dsm.Logout()
		}()

		shareName := args[0]
		userName := args[1]
		userGroupType := "local_user"
		permission := createSharePermission(userName, args[2])
		if permission == nil {
			fmt.Println("Failed. Invalid Argument.")
			os.Exit(1)
		}
		permissions := append([]*webapi.SharePermission{}, permission)

		spec := webapi.SharePermissionSetSpec{
			Name: shareName,
			UserGroupType: userGroupType,
			Permissions: permissions,
		}

		fmt.Printf("spec = %#v\n", spec)
		if err := dsm.SharePermissionSet(spec); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		newPermission, err := getShareLocalUserPermission(dsm, shareName, userName)
		if err != nil {
			fmt.Printf("Failed to get share local_user permission(%s, %s): %v\n", shareName, userName, err)
			os.Exit(1)
		}

		fmt.Printf("Success, SharePermissionSet(%s), newPermission: %#v\n", shareName, newPermission)
	},
}

var cmdShareSet = &cobra.Command{
	Use:   "set <share name> <new size>",
	Short: "share set (only share located in btrfs volume can be resized)",
	Args:  cobra.MinimumNArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		dsm, err := LoginDsmForTest(DsmId)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		defer func() {
			dsm.Logout()
		}()

		shareName := args[0]
		newSize, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
		    fmt.Println(err)
			os.Exit(1)
		}

		share, err := dsm.ShareGet(shareName)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		fmt.Printf("old = %#v\n", share)

		newSizeInMB := utils.BytesToMBCeil(newSize)
		updateInfo := webapi.ShareUpdateInfo{
			Name:           share.Name,
			VolPath:        share.VolPath,
			QuotaForCreate: &newSizeInMB,
		}
		if err := dsm.ShareSet(shareName, updateInfo); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		newShare, err := dsm.ShareGet(shareName)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		fmt.Printf("new = %#v\n", newShare)

		fmt.Printf("Success, ShareSet(%s), quota: %v -> %v MB\n", shareName, share.QuotaValueInMB, newShare.QuotaValueInMB)
	},
}


func init() {
	cmdShare.AddCommand(cmdShareGet)
	cmdShare.AddCommand(cmdShareCreate)
	cmdShare.AddCommand(cmdShareDelete)
	cmdShare.AddCommand(cmdShareList)
	cmdShare.AddCommand(cmdShareClone)
	cmdShare.AddCommand(cmdShareSnapshotCreate)
	cmdShare.AddCommand(cmdShareSnapshotDelete)
	cmdShare.AddCommand(cmdShareSnapshotList)

	cmdShare.AddCommand(cmdSharePermissionList)
	cmdShare.AddCommand(cmdSharePermissionSet)
	cmdShare.AddCommand(cmdShareSet)
}
