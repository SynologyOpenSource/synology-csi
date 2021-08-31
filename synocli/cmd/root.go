/*
 * Copyright 2021 Synology Inc.
 */
package cmd

import (
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use: "synocli",
	Run: func(cmd *cobra.Command, args []string) {
		cmd.Help()
	},
}

func init() {
	rootCmd.AddCommand(cmdDsm)
}

func Execute() {
	rootCmd.Execute()
}