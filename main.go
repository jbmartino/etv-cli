// Command etv manages and backs up ErsatzTV channels over its HTTP API.
//
//	etv plan           show what apply would change
//	etv apply          push your channel setup to a live instance
//	etv validate       check the schedules locally, no server needed
//	etv status         what the server currently has
//
// It needs only a URL and an API key, so it works the same against a desktop, a container,
// or a cluster.
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/jbmartino/etv-cli/internal/apply"
	"github.com/jbmartino/etv-cli/internal/etv"
	"github.com/jbmartino/etv-cli/internal/export"
	"github.com/jbmartino/etv-cli/internal/manifest"
	"github.com/jbmartino/etv-cli/internal/validate"
	"github.com/spf13/cobra"
)

var (
	flagURL      string
	flagKey      string
	flagManifest string
)

func client() *etv.Client {
	url := flagURL
	if url == "" {
		url = os.Getenv("ETV_URL")
	}
	key := flagKey
	if key == "" {
		key = os.Getenv("ETV_API_KEY")
	}
	if url == "" {
		fmt.Fprintln(os.Stderr, "error: no server url (set --url or ETV_URL)")
		os.Exit(2)
	}
	return etv.New(url, key)
}

func loadManifest() *manifest.Manifest {
	m, err := manifest.Load(flagManifest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	return m
}

// plan is a read-only pass: it reports what apply would do, and what exists on the server that the
// manifest never names.
func plan(c *etv.Client, m *manifest.Manifest) (*apply.Result, error) {
	res, err := apply.Apply(c, m, apply.Options{
		DryRun: true,
		Out:    func(f string, a ...any) { fmt.Printf(f+"\n", a...) },
	})
	if err != nil {
		return nil, err
	}
	if len(res.Unmanaged) > 0 {
		fmt.Println("\nnot in the manifest, left alone:")
		for _, u := range res.Unmanaged {
			fmt.Printf("  %s\n", u)
		}
	}
	return res, nil
}

func confirm(prompt string) (bool, error) {
	fmt.Printf("%s [y/N] ", prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	answer := strings.TrimSpace(strings.ToLower(line))
	return answer == "y" || answer == "yes", nil
}

func main() {
	root := &cobra.Command{
		Use:           "etv",
		Short:         "Manage and back up your ErsatzTV setup over its HTTP API",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&flagURL, "url", "", "ErsatzTV base url (or ETV_URL)")
	root.PersistentFlags().StringVar(&flagKey, "key", "", "API key (or ETV_API_KEY)")
	root.PersistentFlags().StringVarP(&flagManifest, "file", "f", "etv.yaml", "manifest path")

	var dryRun bool
	var autoApprove bool

	applyCmd := &cobra.Command{
		Use:   "apply",
		Short: "Push your channel setup to the server",
		Long: "Push your channel setup to the server.\n\n" +
			"Shows the plan first and asks before changing anything, because repointing a playout deletes\n" +
			"and rebuilds it, which discards that channel's existing guide data. Use -y in CI or a git hook.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			m := loadManifest()
			c := client()

			p, err := plan(c, m)
			if err != nil {
				return err
			}
			if p.Changed == 0 {
				fmt.Println("already up to date")
				return nil
			}

			if dryRun {
				fmt.Printf("\n%d change(s) would be applied\n", p.Changed)
				return nil
			}

			if !autoApprove {
				ok, err := confirm(fmt.Sprintf("\napply %d change(s)?", p.Changed))
				if err != nil {
					return err
				}
				if !ok {
					fmt.Println("aborted")
					return nil
				}
				fmt.Println()
			}

			// Recomputed against the server rather than replaying the plan: the server is the state,
			// and a stale plan is how you apply something you did not just look at.
			res, err := apply.Apply(c, m, apply.Options{
				Out: func(f string, a ...any) { fmt.Printf(f+"\n", a...) },
			})
			if err != nil {
				return err
			}
			fmt.Printf("\n%d change(s) applied\n", res.Changed)
			return nil
		},
	}
	applyCmd.Flags().BoolVar(&dryRun, "dry-run", false, "show changes without making them (same as plan)")
	applyCmd.Flags().BoolVarP(&autoApprove, "yes", "y", false, "do not ask for confirmation")

	planCmd := &cobra.Command{
		Use:     "plan",
		Aliases: []string{"diff"},
		Short:   "Show what apply would change",
		RunE: func(cmd *cobra.Command, _ []string) error {
			m := loadManifest()
			res, err := plan(client(), m)
			if err != nil {
				return err
			}
			if res.Changed == 0 {
				fmt.Println("no changes")
			} else {
				fmt.Printf("\n%d change(s) would be applied\n", res.Changed)
			}
			return nil
		},
	}

	validateCmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate schedules locally (no server needed)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			m := loadManifest()
			bad := 0
			for _, ch := range m.Channels {
				yaml, err := m.ReadSchedule(ch.Schedule)
				if err != nil {
					return err
				}
				if errs := validate.Schedule(yaml); len(errs) > 0 {
					bad++
					fmt.Printf("INVALID %s\n", ch.Schedule)
					for _, e := range errs {
						fmt.Printf("    %s\n", e)
					}
					continue
				}
				fmt.Printf("ok      %s\n", ch.Schedule)
			}
			if bad > 0 {
				return fmt.Errorf("%d schedule(s) invalid", bad)
			}
			return nil
		},
	}

	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show what the server currently has",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := client()
			v, err := c.Version()
			if err != nil {
				return err
			}
			fmt.Printf("ErsatzTV %v\n\n", v["appVersion"])

			playouts, err := c.Playouts()
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "CHANNEL\tKIND\tSCHEDULE\tLAST BUILD")
			for _, p := range playouts {
				built := "-"
				if p.LastBuildSuccess != nil {
					if *p.LastBuildSuccess {
						built = "ok"
					} else {
						built = "FAILED"
					}
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", p.ChannelName, p.ScheduleKind, p.ScheduleFile, built)
			}
			return w.Flush()
		},
	}

	var exportDir string
	var exportForce bool

	exportCmd := &cobra.Command{
		Use:   "export",
		Short: "Write the server's channels, schedules, and collections to a manifest directory",
		Long: "Write the server's channels, schedules, and collections to a manifest directory that apply\n" +
			"can push back, so a rebuilt server can be restored from files.\n\n" +
			"Only channels running a single managed Sequential schedule can be represented in the manifest;\n" +
			"anything else is reported and skipped.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return export.Export(client(), export.Options{
				Dir:   exportDir,
				Force: exportForce,
				Out:   func(f string, a ...any) { fmt.Printf(f+"\n", a...) },
			})
		},
	}
	exportCmd.Flags().StringVarP(&exportDir, "out", "o", ".", "directory to write the manifest into")
	exportCmd.Flags().BoolVar(&exportForce, "force", false, "overwrite an existing etv.yaml")

	root.AddCommand(applyCmd, planCmd, validateCmd, statusCmd, exportCmd)

	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
