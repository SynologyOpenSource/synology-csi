// Copyright 2025 Synology Inc.

package webapi

type SnapshotInfo struct {
	Name              string             `json:"name"`
	Uuid              string             `json:"uuid"`
	ParentUuid        string             `json:"parent_uuid"`
	Status            string             `json:"status"`
	TotalSize         int64              `json:"total_size"`
	CreateTime        int64              `json:"create_time"`
	RootPath          string             `json:"root_path"`
}

type SnapshotCreateSpec struct {
	Name        string
	SrcUuid     string
	Description string
	TakenBy     string
	IsLocked    bool
}

type SnapshotCloneSpec struct {
	Name            string
	SrcUuid         string
	SrcSnapshotUuid string
}
