/*
 * Copyright 2021 Synology Inc.
 */

package main

import (
	"os"
	"os/signal"
	"syscall"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/SynologyOpenSource/synology-csi/pkg/driver"
	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/common"
	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/service"
	"github.com/SynologyOpenSource/synology-csi/pkg/logger"
	"github.com/SynologyOpenSource/synology-csi/pkg/utils/hostexec"
)

var (
	// CSI options
	csiNodeID         = "CSINode"
	csiEndpoint       = "unix:///var/lib/kubelet/plugins/" + driver.DriverName + "/csi.sock"
	csiClientInfoPath = "/etc/synology/client-info.yml"
	// Logging
	logLevel       = "info"
	webapiDebug    = false
	multipathForUC = true
	// Locations is tools and directories
	chrootDir      = ""
	iscsiadmPath   = ""
	multipathPath  = ""
	multipathdPath = ""
)

var rootCmd = &cobra.Command{
	Use:          "synology-csi-driver",
	Short:        "Synology CSI Driver",
	SilenceUsage: true,
	RunE: func(_ *cobra.Command, _ []string) error {
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

	// 2. Create command executor
	cmdMap := map[string]string{
		"iscsiadm":   iscsiadmPath,
		"multipath":  multipathPath,
		"multipathd": multipathdPath,
	}
	cmdExecutor, err := hostexec.New(cmdMap, chrootDir)
	if err != nil {
		log.Errorf("Failed to create command executor: %v", err)
		return err
	}
	tools := driver.NewTools(cmdExecutor)

	// 3. Create and Run the Driver
	drv, err := driver.NewControllerAndNodeDriver(csiNodeID, csiEndpoint, dsmService, tools)
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
	cmd.PersistentFlags().StringVar(&chrootDir, "chroot-dir", chrootDir, "Host directory to chroot into (empty disables chroot)")
	cmd.PersistentFlags().StringVar(&iscsiadmPath, "iscsiadm-path", iscsiadmPath, "Full path of iscsiadm executable")
	cmd.PersistentFlags().StringVar(&multipathPath, "multipath-path", multipathPath, "Full path of multipath executable")
	cmd.PersistentFlags().StringVar(&multipathdPath, "multipathd-path", multipathdPath, "Full path of multipathd executable")

	cmd.MarkFlagRequired("endpoint")
	cmd.MarkFlagRequired("client-info")
	cmd.Flags().SortFlags = false
	cmd.PersistentFlags().SortFlags = false
}
