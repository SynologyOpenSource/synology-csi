/*
 * Copyright 2021 Synology Inc.
 */

package common

import (
	"os"

	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
)

type ClientInfo struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Https    bool   `yaml:"https"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type SynoInfo struct {
	Clients []ClientInfo `yaml:"clients"`
}

func LoadConfig(configPath string) (*SynoInfo, error) {
	file, err := os.ReadFile(configPath)
	if err != nil {
		log.Errorf("Unable to open config file: %v", err)
		return nil, err
	}

	info := SynoInfo{}
	err = yaml.Unmarshal(file, &info)
	if err != nil {
		log.Errorf("Failed to parse config: %v", err)
		return nil, err
	}

	return &info, nil
}
