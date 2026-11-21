package main

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/config"
	"github.com/ZachFara/curia/internal/model"
	"github.com/ZachFara/curia/internal/sanitize"
	"github.com/ZachFara/curia/internal/store"
	"github.com/ZachFara/curia/internal/usage"
)

// F19 resources, read side: what the monitor sampled for each live session.
// A number is shown only while its sample is current (usage.ResourceFresh,
// three intervals); otherwise it is "?" and never the old value.

var resourcesJSON bool

func init() {
	c := &cobra.Command{
		Use:   "resources",
		Short: "Show each live agent's CPU and memory, from the monitor's samples",
		Long: "Prints one line per live session and a total: the CPU percent and resident memory\n" +
			"of the pane's process tree at the monitor's last sample. A session with no current\n" +
			"sample shows ? (the monitor is not running, or has not sampled it yet). Read-only.",
		Args: cobra.NoArgs,
		RunE: runResources,
	}
	c.Flags().BoolVar(&resourcesJSON, "json", false, "print the same as one JSON object")
	rootCmd.AddCommand(c)
}

// resourceSample is one live session's latest sample and whether it is current.
type resourceSample struct {
	ID    string
	Fresh bool
	CPU   float64
	RSSMB int64
	At    time.Time
}

// liveResources returns every live session (not ended, not stale) in id
// order with its latest sample.
func liveResources(ctx context.Context, st *store.Store, cfg config.Config, now time.Time) ([]resourceSample, error) {
	sessions, err := st.ListSessions(ctx, store.ListFilter{})
	if err != nil {
		return nil, err
	}
	latest, err := st.LatestResources(ctx)
	if err != nil {
		return nil, err
	}
	var out []resourceSample
	for _, s := range sessions {
		if s.State == model.StateEnded || s.State == model.StateStale {
			continue
		}
		r := latest[s.ID]
		out = append(out, resourceSample{
			ID: s.ID, CPU: r.CPU, RSSMB: r.RSSMB, At: r.At,
			Fresh: usage.ResourceFresh(r.At, now, cfg.Resources.Interval),
		})
	}
	slices.SortFunc(out, func(a, b resourceSample) int {
		if a.ID < b.ID {
			return -1
		}
		return 1
	})
	return out, nil
}

// resourceCells is the dashboard's CPU/RAM text by session id, for current
// samples only.
func resourceCells(ctx context.Context, st *store.Store, cfg config.Config, now time.Time) (map[string]string, error) {
	rows, err := liveResources(ctx, st, cfg, now)
	cells := map[string]string{}
	ctr, _ := readContainerView(ctx, st, cfg, now).bySession()
	for _, r := range rows {
		if r.Fresh {
			cells[r.ID] = usage.ResourceCell(r.CPU, r.RSSMB*1024, r.At, now, cfg.Resources.Interval)
			if n := ctr[r.ID]; n > 0 {
				cells[r.ID] += " " + strconv.Itoa(n) + "c" // containers beside the processes
			}
		}
	}
	return cells, err
}

func runResources(cmd *cobra.Command, _ []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	now := clock()
	rows, err := liveResources(cmd.Context(), st, cfg, now)
	if err != nil {
		return err
	}
	var cpu float64
	var mb int64
	for _, r := range rows {
		if r.Fresh {
			cpu, mb = cpu+r.CPU, mb+r.RSSMB
		}
	}
	running, _ := monitorRunning(monitorLockPath(cfg))
	w := cmd.OutOrStdout()
	cv := readContainerView(cmd.Context(), st, cfg, now)
	if resourcesJSON {
		return printResourcesJSON(w, rows, cpu, mb, running, cv)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SESSION\tCPU\tRAM")
	for _, r := range rows {
		cpuCell, ramCell := "?", "?"
		if r.Fresh {
			cpuCell, ramCell = fmt.Sprintf("%.0f%%", r.CPU), usage.ResourceMem(r.RSSMB*1024)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", sanitize.Line(r.ID, 40), cpuCell, ramCell)
	}
	_, _ = fmt.Fprintf(tw, "total\t%.0f%%\t%s\n", cpu, usage.ResourceMem(mb*1024))
	if err = tw.Flush(); err != nil {
		return err
	}
	printContainers(w, cv)
	if !running {
		_, err = fmt.Fprintln(w, "resources: monitor not running (curia monitor start)")
	}
	return err
}

func printResourcesJSON(w io.Writer, rows []resourceSample, cpu float64, mb int64, running bool, cv containerView) error {
	type jsonRow struct {
		ID        string     `json:"id"`
		Fresh     bool       `json:"fresh"`
		CPU       *float64   `json:"cpu_pct"`
		RSSMB     *int64     `json:"rss_mb"`
		SampledAt *time.Time `json:"sampled_at"`
	}
	out := struct {
		Sessions       []jsonRow `json:"sessions"`
		Total          any       `json:"total"`
		MonitorRunning bool      `json:"monitor_running"`
		Containers     []any     `json:"containers"`
		DockerVM       any       `json:"docker_vm"`
	}{Sessions: []jsonRow{}, MonitorRunning: running, Containers: []any{}}
	for _, c := range cv.Rows {
		out.Containers = append(out.Containers, map[string]any{
			"id": c.ID, "name": sanitize.Line(c.Name, 120), "session": c.SessionID,
			"attributed_by": c.By, "cpu_pct": c.CPU, "mem_mb": c.MemMB,
		})
	}
	if cv.VM != nil {
		out.DockerVM = map[string]any{"used_mb": cv.VM.UsedMB, "limit_mb": cv.VM.LimitMB}
	}
	for _, r := range rows {
		j := jsonRow{ID: sanitize.Line(r.ID, 80), Fresh: r.Fresh}
		if r.Fresh {
			j.CPU, j.RSSMB, j.SampledAt = &r.CPU, &r.RSSMB, jsonTime(r.At)
		}
		out.Sessions = append(out.Sessions, j)
	}
	out.Total = map[string]any{"cpu_pct": cpu, "rss_mb": mb}
	return writeJSON(w, out, "  ")
}

// printContainers lists the containers the monitor last saw beside the
// processes, the unattributed ones as such, and the Docker VM line. Nothing
// is printed when there is nothing current.
func printContainers(w io.Writer, cv containerView) {
	if len(cv.Rows) > 0 {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "CONTAINER\tSESSION\tCPU\tRAM\tBY")
		for _, c := range cv.Rows {
			sess := "unattributed"
			if c.SessionID != "" {
				sess = sanitize.Line(c.SessionID, 40)
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%.0f%%\t%dM\t%s\n", sanitize.Line(c.Name, 40), sess, c.CPU, c.MemMB, sanitize.Line(c.By, 10))
		}
		_ = tw.Flush()
	}
	if cv.VM != nil {
		_, _ = fmt.Fprintln(w, dockerVMText(*cv.VM)+" (containers' use only)")
	}
}
