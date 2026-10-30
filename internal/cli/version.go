// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/keeptalking83/pgweir/internal/meta"
)

func newVersionCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			version, commit, date := meta.BuildInfo()
			if a.cfg.JSON {
				b, err := json.Marshal(map[string]string{
					"name": meta.Name, "version": version, "commit": commit,
					"date": date, "go": runtime.Version(),
				})
				if err != nil {
					return err
				}
				a.emit("%s", b)
				return nil
			}
			a.emit("%s %s", meta.Name, version)
			if commit != "" {
				a.say("commit %s", commit)
			}
			if date != "" {
				a.say("built %s", date)
			}
			a.say("%s", runtime.Version())
			return nil
		},
	}
}
