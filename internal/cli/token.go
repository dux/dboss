package cli

import (
	"errors"
	"flag"
	"fmt"
)

// token prints the webhook token of the host config: what hook pings and /metrics present.
func (c CLI) token(args []string) error {
	set := flag.NewFlagSet("token", flag.ContinueOnError)
	set.SetOutput(c.Err)
	configPath := configFlag(set)
	operands, err := parseSubcommandFlags(set, args)
	if err != nil {
		return err
	}
	if len(operands) != 0 {
		return errors.New("usage: dboss token [-c path]")
	}
	configFile, cfg, err := (&workdir{explicit: *configPath}).load()
	if err != nil {
		return err
	}
	token := cfg.Tokens.WebhookToken()
	if token == "" {
		return fmt.Errorf("%s sets neither tokens.dboss nor tokens.webhook", configFile)
	}
	fmt.Fprintln(c.Out, token)
	return nil
}
