// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/google/agentmesh/internal/skill"
	"github.com/spf13/cobra"
)

// newSkillCmd builds `agentmesh-node skill`, which teaches local AI agents how to
// bootstrap a node and use the mesh.
func newSkillCmd() *cobra.Command {
	var (
		dirFlag     string
		projectFlag bool
	)

	skillCmd := &cobra.Command{
		Use:   "skill",
		Short: "Manage the Agent Mesh skill that teaches local AI agents to use the mesh",
		Long: "Install the Agent Mesh agent skill into the directories AI coding agents scan.\n" +
			"Agents load the skill on their own and use it to bootstrap a node,\n" +
			"discover mesh services, and call remote tools.",
	}

	installCmd := &cobra.Command{
		Use:          "install",
		Short:        "Install the Agent Mesh skill for local AI agents",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			targets, home, err := resolveSkillTargets(dirFlag, projectFlag)
			if err != nil {
				return err
			}
			return installSkill(cmd.OutOrStdout(), targets, home)
		},
	}

	listCmd := &cobra.Command{
		Use:          "list",
		Short:        "Show where the Agent Mesh skill is installed and whether it is current",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			targets, home, err := resolveSkillTargets(dirFlag, projectFlag)
			if err != nil {
				return err
			}
			return listSkill(cmd.OutOrStdout(), targets, home)
		},
	}

	showCmd := &cobra.Command{
		Use:          "show",
		Short:        "Print the skill document, for agents with a different layout",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := io.WriteString(cmd.OutOrStdout(), skill.Content)
			return err
		},
	}

	for _, c := range []*cobra.Command{installCmd, listCmd} {
		c.Flags().StringVar(&dirFlag, "dir", "", "Skills directory to use instead of the per-user ones")
		c.Flags().BoolVar(&projectFlag, "project", false, "Use the skill directories of the current project instead of the per-user ones")
		c.MarkFlagsMutuallyExclusive("dir", "project")
	}

	skillCmd.AddCommand(installCmd, listCmd, showCmd)
	return skillCmd
}

// resolveSkillTargets picks the directories to act on, and the home directory
// used to shorten paths when printing them.
func resolveSkillTargets(dir string, project bool) ([]skill.Target, string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	switch {
	case dir != "":
		return []skill.Target{skill.CustomTarget(dir)}, home, nil
	case project:
		wd, err := os.Getwd()
		if err != nil {
			return nil, home, fmt.Errorf("resolving the current directory: %w", err)
		}
		return skill.ProjectTargets(wd), home, nil
	case home == "":
		return nil, home, errors.New("cannot locate the home directory; pass --dir or --project")
	default:
		return skill.UserTargets(home), home, nil
	}
}

func installSkill(w io.Writer, targets []skill.Target, home string) error {
	var errs []error
	installed := 0
	_, _ = fmt.Fprintf(w, "Installing the Agent Mesh skill (%s):\n", skill.Name)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, target := range targets {
		before, err := skill.Install(target)
		if err != nil {
			_, _ = fmt.Fprintf(tw, "  failed\t%s\t%v\n", displayPath(home, target.Path()), err)
			errs = append(errs, err)
			continue
		}
		verb := "installed"
		switch before {
		case skill.StateOutdated:
			verb = "updated"
		case skill.StateCurrent:
			verb = "up to date"
		}
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\n", verb, displayPath(home, target.Path()), target.Agent)
		installed++
	}
	if err := tw.Flush(); err != nil {
		errs = append(errs, err)
	}
	if installed > 0 {
		printSkillNextSteps(w)
	}
	return errors.Join(errs...)
}

func listSkill(w io.Writer, targets []skill.Target, home string) error {
	var errs []error
	_, _ = fmt.Fprintf(w, "Agent Mesh skill (%s):\n", skill.Name)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, target := range targets {
		state, err := skill.Inspect(target)
		if err != nil {
			_, _ = fmt.Fprintf(tw, "  unreadable\t%s\t%v\n", displayPath(home, target.Path()), err)
			errs = append(errs, err)
			continue
		}
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\n", state, displayPath(home, target.Path()), target.Agent)
	}
	if err := tw.Flush(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func printSkillNextSteps(w io.Writer) {
	_, _ = fmt.Fprint(w, `
Next steps:
  1. Start a node in the background (prints its endpoint and API token file):
       agentmesh-node run --daemonize
     If it reports the node is not enrolled, run the join command it prints
     first, then re-run it.
  2. Point your agent at http://127.0.0.1:8080/mcp, authenticated with the
     header "X-Mesh-Authentication: Bearer <token>". For example:
       Claude Code   claude mcp add --transport http agentmesh \
                       http://127.0.0.1:8080/mcp \
                       --header "X-Mesh-Authentication: Bearer <token>"
       Antigravity   add that URL as "serverUrl", with the same header, to
                       ~/.gemini/config/mcp_config.json
     Other agents: https://google.github.io/agentmesh/docs/guides/connecting-agents/
  3. Restart your agent so it picks up the skill and the mesh tools.
`)
}

// displayPath shortens a path under the home directory to ~/... so the output
// stays readable.
func displayPath(home, path string) string {
	if home == "" {
		return path
	}
	prefix := home + string(filepath.Separator)
	if !strings.HasPrefix(path, prefix) {
		return path
	}
	return filepath.Join("~", strings.TrimPrefix(path, prefix))
}
