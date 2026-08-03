/*
 * Copyright 2021 Synology Inc.
 */

package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cenkalti/backoff/v4"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/SynologyOpenSource/synology-csi/pkg/driver"
	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/common"
	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/service"
	"github.com/SynologyOpenSource/synology-csi/pkg/interfaces"
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
	nvmePath       = ""
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

// loginDsms logs in to every configured DSM, retrying the ones that fail until
// they all answer or the backoff gives up.
//
// The retry matters at boot. A DSM that is still starting - after a power cut the
// NAS routinely needs several minutes longer than the nodes it serves - refuses the
// login, and a single attempt leaves the driver running with an empty DSM set: the
// pod stays Running and Ready, its probes pass, and every volume operation it is
// handed fails until someone restarts it by hand.
func loginDsms(dsmService interfaces.IDsmService, clients []common.ClientInfo, bo backoff.BackOff) error {
	remaining := clients

	login := func() error {
		var failed []common.ClientInfo
		for _, client := range remaining {
			if err := dsmService.AddDsm(client); err != nil {
				log.Warnf("Failed to add DSM: %s, error: %v", client.Host, err)
				failed = append(failed, client)
			}
		}
		remaining = failed

		if len(remaining) > 0 {
			return fmt.Errorf("%d of %d DSMs could not be logged in", len(remaining), len(clients))
		}
		return nil
	}

	notify := func(err error, duration time.Duration) {
		log.Infof("%v, retrying in %3.2f seconds .....", err, float64(duration.Seconds()))
	}

	return backoff.RetryNotify(login, bo, notify)
}

func driverStart() error {
	log.Infof("CSI Options = {%s, %s, %s}", csiNodeID, csiEndpoint, csiClientInfoPath)

	dsmService := service.NewDsmService()

	// 1. Load the DSM ClientInfo
	info, err := common.LoadConfig(csiClientInfoPath)
	if err != nil {
		log.Errorf("Failed to read config: %v", err)
		return err
	}

	defer dsmService.RemoveAllDsms()

	// 2. Create command executor
	cmdMap := map[string]string{
		"iscsiadm":   iscsiadmPath,
		"multipath":  multipathPath,
		"multipathd": multipathdPath,
		"nvme":       nvmePath,
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

	// 4. Log in to the DSMs, once the socket exists.
	//
	// Deliberately after Activate() and in the background: the node plugin's
	// teardown path (NodeUnstageVolume, NodeUnpublishVolume) is plain unmounting
	// and needs no DSM, so it must keep being served while the NAS is away -
	// otherwise an unreachable NAS would also stop pods from terminating and
	// block node drains.
	loginBackoff := backoff.NewExponentialBackOff()
	loginBackoff.InitialInterval = 1 * time.Second
	loginBackoff.MaxInterval = 5 * time.Minute // a wrong password must not hammer DSM's auto-block
	loginBackoff.MaxElapsedTime = 0            // keep retrying for as long as the driver runs
	go func() {
		if err := loginDsms(dsmService, info.Clients, loginBackoff); err != nil {
			log.Errorf("Failed to add DSM, giving up. err: %v", err)
		}
	}()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	// Block until a signal is received.
	<-c
	log.Infof("Shutting down.")
	return nil
}

func main() {
	rootCmd.FParseErrWhitelist.UnknownFlags = true
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
	cmd.PersistentFlags().StringVar(&nvmePath, "nvme-path", nvmePath, "Full path of nvme executable")

	cmd.MarkFlagRequired("endpoint")
	cmd.MarkFlagRequired("client-info")
	cmd.Flags().SortFlags = false
	cmd.PersistentFlags().SortFlags = false
}
