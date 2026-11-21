package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"regexp"

	"github.com/spf13/cobra"

	"github.com/ZachFara/curia/internal/notify"
	"github.com/ZachFara/curia/internal/store"
)

var servetokenName = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

func init() {
	token := &cobra.Command{
		Use: "token <device>", Short: "Make a web device token and show it once (the user only)", Annotations: userOnlyMark(),
		Args: cobra.ExactArgs(1), RunE: userOnlyRunE("serve token", "make a web device token", runServeToken),
	}
	revoke := &cobra.Command{
		Use: "revoke <device>", Short: "Delete a web device token (the user only)", Annotations: userOnlyMark(),
		Args: cobra.ExactArgs(1), RunE: userOnlyRunE("serve revoke", "revoke a web device token", runServeRevoke),
	}
	devices := &cobra.Command{
		Use: "devices", Short: "List the web devices by name and time (the user only)", Annotations: userOnlyMark(),
		Args: cobra.NoArgs, RunE: userOnlyRunE("serve devices", "list web devices", runServeDevices),
	}
	for _, c := range rootCmd.Commands() {
		if c.Name() == "serve" {
			c.AddCommand(token, revoke, devices)
			return
		}
	}
	panic("serve command is not registered before serve_token.go's init")
}

func servetokenStore() (*store.Store, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	return openStore(cfg)
}

func runServeToken(cmd *cobra.Command, args []string) error {
	if !servetokenName.MatchString(args[0]) {
		return fmt.Errorf("device name must match %s", servetokenName)
	}
	st, err := servetokenStore()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	plain, hash, err := notify.NewToken(rand.Reader)
	if err != nil {
		return err
	}
	switch err = st.AddDevice(cmd.Context(), args[0], hash, clock()); {
	case errors.Is(err, store.ErrDeviceExists):
		return fmt.Errorf("a device named %s exists: revoke it first", args[0])
	case err != nil:
		return err
	}
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "device %s created. The token is shown once; only its hash is kept:\n\n%s\n\n", args[0], plain)
	_, _ = fmt.Fprintln(out, "Open [serve] public_url followed by /login on that device and paste it. Needs serve.web = true; revoke with curia serve revoke "+args[0])
	return nil
}

func runServeRevoke(cmd *cobra.Command, args []string) error {
	st, err := servetokenStore()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	ok, err := st.DeleteDevice(cmd.Context(), args[0])
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no device named %s", args[0])
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "device %s revoked\n", args[0])
	return nil
}

func runServeDevices(cmd *cobra.Command, _ []string) error {
	st, err := servetokenStore()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	devs, err := st.Devices(cmd.Context())
	if err != nil {
		return err
	}
	for _, d := range devs {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s\tcreated %s\n", d.Name, d.CreatedAt.Local().Format("2006-01-02 15:04"))
	}
	return nil
}
