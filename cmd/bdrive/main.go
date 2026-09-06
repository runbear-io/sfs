// bdrive is the BearDrive CLI: mount a folder, and its
// contents stay synchronized across devices and teammates through a
// BearDrive hub, with full per-file change history and offline support.
package main

import (
	"fmt"
	"os"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/config"
)

// version is set at release time via -ldflags "-X main.version=...".
// `go install …@vX.Y.Z` builds skip ldflags, so fall back to the module
// version Go stamps into the binary.
var version = "0.1.0-dev"

func resolvedVersion() string {
	if version != "0.1.0-dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return strings.TrimPrefix(bi.Main.Version, "v")
	}
	return version
}

func main() {
	root := &cobra.Command{
		Use:   "bdrive",
		Short: "BearDrive: a synced file system for AI agents",
		Long: `bdrive — the BearDrive CLI. A mountable, offline-first, synced file
system for AI agents.

Mount any folder and BearDrive keeps it synchronized across your devices and
teammates through a BearDrive hub (bdrive serve — self-hosted or BearDrive
Cloud). Every change is journaled — you can always see which device and
author changed which file, and when. Files are real files on disk, so
everything keeps working offline; changes sync when the remote is reachable.`,
		SilenceUsage: true,
		Version:      resolvedVersion(),
	}
	root.SetVersionTemplate("beardrive {{.Version}}\n")
	root.AddCommand(
		loginCmd(),
		logoutCmd(),
		initCmd(),
		shareCmd(),
		urlCmd(),
		stopCmd(),
		scopeCmd(),
		grepCmd(),
		staleCmd(),
		forgetCmd(),
		syncCmd(),
		readLogCmd(),
		hooksCmd(),
		resumeCmd(),
		autostartCmd(),
		statusCmd(),
		logCmd(),
		restoreCmd(),
		exportCmd(),
		importCmd(),
		webCmd(),
		desktopCmd(),
		whoamiCmd(),
		daemonCmd(),
		versionCmd(),
	)
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the bdrive version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("beardrive", resolvedVersion())
		},
	}
}

func whoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show this device's identity used in change tracking",
		RunE: func(cmd *cobra.Command, args []string) error {
			dev, err := config.LoadDevice()
			if err != nil {
				return err
			}
			home, err := config.Home()
			if err != nil {
				return err
			}
			fmt.Printf("device id:   %s\n", dev.ID)
			fmt.Printf("device name: %s\n", dev.Name)
			settings, serr := config.LoadSettings()
			if serr != nil {
				fmt.Printf("account:     unknown — cannot read settings: %v\n", serr)
			} else if settings.Email != "" {
				who := settings.Email
				if settings.Name != "" {
					who = settings.Name + " <" + settings.Email + ">"
				}
				// Name and email are whatever the hub answered at sign-in.
				fmt.Printf("account:     %s (from `bdrive login`; changes are attributed to this)\n", safeField(who, 160))
				fmt.Printf("author:      %s (git/OS fallback, used only when signed out)\n", dev.Author)
			} else {
				fmt.Printf("account:     not signed in — changes are attributed to the author below (run `bdrive login`)\n")
				fmt.Printf("author:      %s (detected from git config / OS user)\n", dev.Author)
			}
			fmt.Printf("beardrive home:    %s\n", home)
			return nil
		},
	}
}
