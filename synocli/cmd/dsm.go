/*
 * Copyright 2021 Synology Inc.
 */
package cmd

import (
	"fmt"
	"os"
	"github.com/spf13/cobra"
	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/common"
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

var cmdDsmList = &cobra.Command{
	Use:   "list",
	Short: "list DSM infos in the config file",
	Args:  cobra.MinimumNArgs(0),
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("List DSM infos in: %s\n", ConfigFile)
		dsms, err := ListDsms(-1)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		fmt.Printf("%-2s\t%-32s\t%-6s\t%-32s\t%s\n", "id", "Host", "Port", "Username", "Https")
		for i, client := range dsms {
			fmt.Printf("%-2d\t%-32s\t", i, client.Ip)
			fmt.Printf("%-6d\t", client.Port)
			fmt.Printf("%-32s\t", client.Username)
			fmt.Printf("%v\t", client.Https)
			fmt.Printf("\n")
		}
	},
}

// Always get the first client from ClientInfo for synocli testing
func LoginDsmForTest(id int) (*webapi.DSM, error) {
	dsms, err := ListDsms(id)
	if err != nil {
		return nil, fmt.Errorf("Failed to list dsms: %v", err)
	}

	if err := dsms[0].Login(); err != nil {
		return nil, fmt.Errorf("Failed to login to DSM: [%s]. err: %v", dsms[0].Ip, err)
	}

	fmt.Printf("Login DSM: %s\n", dsms[0].Ip)
	return dsms[0], nil
}

func ListDsms(id int) ([]*webapi.DSM, error) {
	var dsms []*webapi.DSM

	info, err := common.LoadConfig(ConfigFile)
	if err != nil {
		return nil, fmt.Errorf("Failed to read config[%s]: %v", ConfigFile, err)
	}
	if id < -1 || id >= len(info.Clients) {
		return nil, fmt.Errorf("Invalid dsmId: %d", id)
	}

	for i, _ := range info.Clients {
		if id != -1 && id != i {
			continue
		}

		dsm := &webapi.DSM{
			Ip:       info.Clients[i].Host,
			Port:     info.Clients[i].Port,
			Username: info.Clients[i].Username,
			Password: info.Clients[i].Password,
			Https:    info.Clients[i].Https,
		}
		dsms = append(dsms, dsm)
	}

	if len(dsms) == 0 {
		return nil, fmt.Errorf("No client in config")
	}
	return dsms, nil
}

func init() {
	cmdDsm.AddCommand(cmdDsmLogin)
	cmdDsm.AddCommand(cmdDsmList)

	cmdDsmLogin.PersistentFlags().BoolVar(&https, "https", false, "Use HTTPS to login DSM")
	cmdDsmLogin.PersistentFlags().IntVarP(&port, "port", "p", -1, "Use assigned port to login DSM")
}