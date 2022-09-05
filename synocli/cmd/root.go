/*
 * Copyright 2021 Synology Inc.
 */
package cmd

import (
	"github.com/spf13/cobra"
)

var (
	ConfigFile = "./config/client-info.yml"
	DsmId = -1
)

var rootCmd = &cobra.Command{
	Use: "synocli",
	Run: func(cmd *cobra.Command, args []string) {
		cmd.Help()
	},
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&ConfigFile, "client-info", "f", ConfigFile, "path of client-info file")
	rootCmd.PersistentFlags().IntVarP(&DsmId, "id", "i", DsmId, "dsm id")

	rootCmd.AddCommand(cmdDsm)
	rootCmd.AddCommand(cmdLun)
	rootCmd.AddCommand(cmdShare)
}

func Execute() {
	rootCmd.Execute()
}