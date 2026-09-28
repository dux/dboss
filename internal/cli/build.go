package cli

import (
	"flag"
	"fmt"
	"os"

	"dboss/internal/tauri"
)

func (c CLI) build(args []string) error {
	if len(args) == 0 {
		return c.help(c.Out, "build")
	}
	switch args[0] {
	case "tauri":
		return c.buildTauri(args[1:])
	case "help":
		return c.help(c.Out, "build")
	default:
		return fmt.Errorf("unknown build target %q (use tauri)", args[0])
	}
}

// buildTauri packages the app in the current folder as a desktop app.
func (c CLI) buildTauri(args []string) error {
	set := flag.NewFlagSet("build tauri", flag.ContinueOnError)
	set.SetOutput(c.Err)
	opts := tauri.Options{Stdout: c.Out, Stderr: c.Err}
	set.StringVar(&opts.Identifier, "identifier", "", "bundle identifier (default dev.dboss.<app>)")
	set.StringVar(&opts.Out, "out", "", "folder for the finished bundles (default ./dist/tauri)")
	set.StringVar(&opts.Bundles, "bundles", "", "bundle formats, comma separated: app,dmg,deb,appimage,rpm")
	set.StringVar(&opts.Icon, "icon", "", "square PNG icon (default ./icon.png, then ./public/icon.png)")
	operands, err := parseSubcommandFlags(set, args)
	if err != nil {
		return err
	}
	if len(operands) > 0 {
		return fmt.Errorf("build tauri takes no arguments, got %q", operands[0])
	}
	if opts.AppDir, err = os.Getwd(); err != nil {
		return err
	}
	bundles, err := tauri.Build(opts)
	if err != nil {
		return err
	}
	for _, bundle := range bundles {
		fmt.Fprintln(c.Out, bundle)
	}
	return nil
}
