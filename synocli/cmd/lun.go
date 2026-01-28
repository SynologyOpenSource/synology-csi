/*
 * Copyright 2022 Synology Inc.
 */
package cmd

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/webapi"
	"github.com/spf13/cobra"
)

var cmdLun = &cobra.Command{
	Use:   "lun",
	Short: "lun API",
	Long:  `DSM lun API`,
	Run: func(cmd *cobra.Command, args []string) {
		cmd.Help()
	},
}

var cmdLunList = &cobra.Command{
	Use:   "list",
	Short: "list luns",
	Args:  cobra.MinimumNArgs(0),
	Run: func(cmd *cobra.Command, args []string) {
		dsms, err := ListDsms(DsmId)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		lunInfos := make(map[string][]webapi.LunInfo)
		for _, dsm := range dsms {
			if err := dsm.Login(); err != nil {
				fmt.Printf("Failed to login to DSM: [%s]. err: %v\n", dsm.Ip, err)
				os.Exit(1)
			}
			infos, err := dsm.LunList()
			if err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
			lunInfos[dsm.Ip] = infos
			dsm.Logout()
		}

		tw := tabwriter.NewWriter(os.Stdout, 8, 0, 2, ' ', 0)
		_, _ = fmt.Fprintf(tw, "%-16s\t", "Host:")
		_, _ = fmt.Fprintf(tw, "%-52s\t", "Name:")
		_, _ = fmt.Fprintf(tw, "%-36s\t", "Uuid:")
		_, _ = fmt.Fprintf(tw, "%-10s\t", "Location:")
		_, _ = fmt.Fprintf(tw, "%-12s\t", "LunType:")
		_, _ = fmt.Fprintf(tw, "%-16s\t", "Size:")
		_, _ = fmt.Fprintf(tw, "%-16s\t", "Used:")
		_, _ = fmt.Fprintf(tw, "\n")
		for ip, v := range lunInfos {
			for _, info := range v {
				_, _ = fmt.Fprintf(tw, "%-16s\t", ip)
				_, _ = fmt.Fprintf(tw, "%-52s\t", info.Name)
				_, _ = fmt.Fprintf(tw, "%-36s\t", info.Uuid)
				_, _ = fmt.Fprintf(tw, "%-10s\t", info.Location)
				_, _ = fmt.Fprintf(tw, "%-12s\t", lunTypeToString(info.LunType))
				_, _ = fmt.Fprintf(tw, "%-16d\t", info.Size)
				_, _ = fmt.Fprintf(tw, "%-16d\t", info.Used)
				_, _ = fmt.Fprintf(tw, "\n")
				_ = tw.Flush()
			}
		}

		fmt.Printf("Success, LunList()\n")
	},
}

func lunTypeToString(lunType int) string {
	switch lunType {
	case 3:
		return "FILE"
	case 15:
		return "ADV"
	case 259:
		return "BLUN_THICK"
	case 263:
		return "BLUN"
	default:
		return fmt.Sprintf("NOT_SUPPORTED(%d)", lunType)
	}
}

func init() {
	cmdLun.AddCommand(cmdLunList)
}
