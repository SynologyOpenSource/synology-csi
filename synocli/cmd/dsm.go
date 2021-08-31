/*
 * Copyright 2021 Synology Inc.
 */
package cmd

import (
	"fmt"
	"os"
	"github.com/spf13/cobra"
	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/webapi"
)

var https = false
var port = -1

var cmdDsm = &cobra.Command{
	Use:   "dsm",
	Short: "dsm",
	Long:  `dsm`,
	Run: func(cmd *cobra.Command, args []string) {
		cmd.Help()
	},
}

var cmdDsmLogin = &cobra.Command{
	Use:   "login <ip> <username> <password>",
	Short: "login dsm",
	Args:  cobra.MinimumNArgs(3),
	Run: func(cmd *cobra.Command, args []string) {
		var defaultPort = 5000
		if https {
			defaultPort = 5001
		}
		if port != -1 {
			defaultPort = port
		}

		dsmApi := &webapi.DSM{
			Ip:       args[0],
			Username: args[1],
			Password: args[2],
			Port:     defaultPort,
			Https:    https,
		}

		err := dsmApi.Login()
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
	},
}

func init() {
	cmdDsm.AddCommand(cmdDsmLogin)

	cmdDsmLogin.PersistentFlags().BoolVar(&https, "https", false, "Use HTTPS to login DSM")
	cmdDsmLogin.PersistentFlags().IntVarP(&port, "port", "p", -1, "Use assigned port to login DSM")
}