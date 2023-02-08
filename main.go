/*
 * Copyright 2021 Synology Inc.
 */

package main

import (
	"os"
	"os/signal"
	"syscall"
	"github.com/spf13/cobra"
	log "github.com/sirupsen/logrus"

	"github.com/SynologyOpenSource/synology-csi/pkg/driver"
	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/common"
	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/service"
	"github.com/SynologyOpenSource/synology-csi/pkg/logger"
)

var (
	// CSI options
	csiNodeID         = "CSINode"
	csiEndpoint       = "unix:///var/lib/kubelet/plugins/" + driver.DriverName + "/csi.sock"
	csiClientInfoPath = "/etc/synology/client-info.yml"
	// Logging
	logLevel    = "info"
	webapiDebug = false
	multipathForUC = true
)

var rootCmd = &cobra.Command{
	Use:   "synology-csi-driver",
	Short: "Synology CSI Driver",
	SilenceUsage: true,
	RunE:  func(cmd *cobra.Command, args []string) error {
		if webapiDebug {
			logger.WebapiDebug = true
			logLevel = "debug"
		}
		logger.Init(logLevel)

		if !multipathForUC {
			driver.MultipathEnabled = false
		}

		err := driverStart()
		if err != nil {
			log.Errorf("Failed to driverStart(): %v", err)
			return err
		}
		return nil
	},
}

func driverStart() error {
	log.Infof("CSI Options = {%s, %s, %s}", csiNodeID, csiEndpoint, csiClientInfoPath)

	dsmService := service.NewDsmService()

	// 1. Login DSMs by given ClientInfo
	info, err := common.LoadConfig(csiClientInfoPath)
	if err != nil {
		log.Errorf("Failed to read config: %v", err)
		return err
	}

	for _, client := range info.Clients {
		err := dsmService.AddDsm(client)
		if err != nil {
			log.Errorf("Failed to add DSM: %s, error: %v", client.Host, err)
		}
	}
	defer dsmService.RemoveAllDsms()

	// 2. Create and Run the Driver
	drv, err := driver.NewControllerAndNodeDriver(csiNodeID, csiEndpoint, dsmService)
	if err != nil {
		log.Errorf("Failed to create driver: %v", err)
		return err
	}
	drv.Activate()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	// Block until a signal is received.
	<-c
	log.Infof("Shutting down.")
	return nil
}

func main() {
	addFlags(rootCmd)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}

	os.Exit(0)
}

func addFlags(cmd *cobra.Command) {
	cmd.PersistentFlags().StringVar(&csiNodeID, "nodeid", csiNodeID, "Node ID")
	cmd.PersistentFlags().StringVarP(&csiEndpoint, "endpoint", "e", csiEndpoint, "CSI endpoint")
	cmd.PersistentFlags().StringVarP(&csiClientInfoPath, "client-info", "f", csiClientInfoPath, "Path of Synology config yaml file")
	cmd.PersistentFlags().StringVar(&logLevel, "log-level", logLevel, "Log level (debug, info, warn, error, fatal)")
	cmd.PersistentFlags().BoolVarP(&webapiDebug, "debug", "d", webapiDebug, "Enable webapi debugging logs")
	cmd.PersistentFlags().BoolVar(&multipathForUC, "multipath", multipathForUC, "Set to 'false' to disable multipath for UC")

	cmd.MarkFlagRequired("endpoint")
	cmd.MarkFlagRequired("client-info")
	cmd.Flags().SortFlags = false
	cmd.PersistentFlags().SortFlags = false
}
